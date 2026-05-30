// Command msc-bench benchmarks memory retrieval and the when/what-to-inject
// decision against a REAL MuninnDB instance — not the synthetic distributions of
// the offline study. It seeds a large labeled corpus into a dedicated vault,
// then probes with queries whose correct answer is known, measuring:
//
//   - Retrieval: does semantic recall surface the right memory (Recall@k, MRR)?
//   - When to inject: can a threshold on the recall signal separate queries that
//     SHOULD inject (a relevant memory exists) from queries that should NOT
//     (the topic is absent)? Swept over both the `score` and `vector_score`
//     fields, since real MuninnDB `score` is a recency/graph-inflated composite
//     that exceeds 1.0 while `vector_score` is the raw cosine similarity.
//
// Everything the benchmark measures is what the proxy sees in-flight, so the
// winning field+threshold can be wired straight into transparent injection.
//
//	msc-bench -seed -probe            # seed the corpus then run probes
//	msc-bench -probe                  # re-probe an already-seeded vault
//	msc-bench -n 300 -absent 100      # corpus + absent-probe sizing
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/maci0/muninn-sidecar/internal/mcpclient"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "msc-bench:", err)
		os.Exit(1)
	}
}

func run() error {
	var (
		mcpURL     = flag.String("mcp-url", envOr("MUNINN_MCP_URL", "http://127.0.0.1:8750/mcp"), "MuninnDB MCP endpoint")
		token      = flag.String("token", "", "bearer token (default ~/.muninn/mcp.token)")
		vault      = flag.String("vault", "msc-bench", "vault to seed/probe (dedicated; not 'default')")
		corpus     = flag.String("corpus", "homogeneous", "corpus generator: homogeneous | diverse | facts | squad")
		squadFile  = flag.String("squad-file", "/tmp/squad-dev.json", "path to SQuAD JSON (corpus=squad)")
		squadArts  = flag.Int("squad-articles", 12, "number of SQuAD articles to seed (rest are held out as absent)")
		hardNeg    = flag.Bool("hard-neg", false, "squad: draw negatives from held-out paragraphs of SEEDED articles (same-topic hard negatives) instead of disjoint articles")
		chunk      = flag.String("chunk", "paragraph", "SQuAD chunk granularity: paragraph | sentence")
		dumpQA     = flag.String("dump-qa", "", "write present probes as a QA JSON ([{question,answer}]) for msc-qa -dataset generic")
		mode       = flag.String("mode", "", "MuninnDB recall mode: semantic | recent | balanced | deep (empty = server default)")
		qTransform = flag.String("query-transform", "none", "query construction: none | distractors (prepend prior unrelated turns) | repeat-last")
		distractN  = flag.Int("distractors", 2, "number of prior unrelated turns to prepend (query-transform=distractors)")
		rerank     = flag.String("rerank", "none", "candidate rerank: none | lexical (vector + lambda*token-overlap)")
		rerankL    = flag.Float64("rerank-lambda", 0.3, "lexical rerank weight")
		groundCmd  = flag.String("ground-cmd", "", "LLM answer-grounding rerank via a CLI agent (e.g. \"claude -p\"); drops recalled candidates the model says don't answer the query")
		groundURL  = flag.String("ground-url", "", "LLM answer-grounding rerank via an OpenAI-compatible URL (e.g. http://127.0.0.1:11434/v1)")
		groundMod  = flag.String("ground-model", "qwen2.5:1.5b-instruct", "grounding model name (for -ground-url)")
		groundKey  = flag.String("ground-key", "", "grounding model API key (for -ground-url)")
		groundTopK = flag.Int("ground-topk", 5, "ground only the top-K candidates by cosine per probe (bounds model calls)")
		groundTO   = flag.Duration("ground-timeout", 60*time.Second, "per grounding-call timeout")
		multiRec   = flag.Bool("multi-recall", false, "split query into entity spans, recall each, merge (helps multi-hop)")
		n          = flag.Int("n", 300, "number of labeled memories to seed")
		absent     = flag.Int("absent", 100, "number of absent-topic probes (should suppress)")
		present    = flag.Int("present", 150, "number of present-topic probes (should inject)")
		seed       = flag.Bool("seed", false, "seed the corpus before probing")
		doProbe    = flag.Bool("probe", false, "run probes (default if neither -seed nor -probe given)")
		limit      = flag.Int("limit", 10, "recall limit per probe")
		timeout    = flag.Duration("timeout", 30*time.Second, "per-MCP-call timeout")
		rngSeed    = flag.Int64("rng", 1, "deterministic dataset seed")
		asJSON     = flag.Bool("json", false, "emit machine-readable JSON")
	)
	flag.Parse()
	if !*seed && !*doProbe {
		*doProbe = true
	}

	client := mcpclient.New(*mcpURL, resolveToken(*token), *timeout)
	var items []item
	var presentProbes, absentProbes []probe
	switch *corpus {
	case "squad":
		var err error
		if *hardNeg {
			items, presentProbes, absentProbes, err = genSquadHardNeg(*squadFile, *squadArts, *n, *present, *absent, *chunk)
		} else {
			items, presentProbes, absentProbes, err = genSquad(*squadFile, *squadArts, *n, *present, *absent, *chunk)
		}
		if err != nil {
			return err
		}
	case "hotpot":
		var err error
		items, presentProbes, absentProbes, err = genHotpot(*squadFile, *squadArts, *n, *present, *absent)
		if err != nil {
			return err
		}
	case "agentmem":
		items, presentProbes, absentProbes = genAgentMem(*n, *absent)
	case "facts":
		items, presentProbes, absentProbes = genFacts()
	case "diverse":
		items, presentProbes, absentProbes = genDiverse(*rngSeed, *n, *present, *absent)
	default:
		items, presentProbes, absentProbes = genDataset(*rngSeed, *n, *present, *absent)
	}
	ctx := context.Background()

	if *dumpQA != "" {
		if err := writeQA(*dumpQA, presentProbes); err != nil {
			return err
		}
		fmt.Fprintf(os.Stderr, "wrote %d QA pairs to %s\n", len(presentProbes), *dumpQA)
	}

	if *seed {
		fmt.Fprintf(os.Stderr, "seeding %d memories into vault %q...\n", len(items), *vault)
		if err := seedCorpus(ctx, client, *vault, items); err != nil {
			return err
		}
		fmt.Fprintf(os.Stderr, "seeded. waiting 3s for indexing...\n")
		time.Sleep(3 * time.Second)
	}

	if !*doProbe {
		return nil
	}

	probes := append(presentProbes, absentProbes...)
	fmt.Fprintf(os.Stderr, "probing %d queries (%d present, %d absent) mode=%q transform=%q rerank=%q...\n",
		len(probes), len(presentProbes), len(absentProbes), *mode, *qTransform, *rerank)
	opt := probeOpts{mode: *mode, transform: *qTransform, distractN: *distractN, rerank: *rerank, rerankLambda: *rerankL, multiRecall: *multiRec}
	results, err := runProbes(ctx, client, *vault, probes, *limit, opt)
	if err != nil {
		return err
	}

	report := analyze(results)
	if *asJSON {
		return json.NewEncoder(os.Stdout).Encode(report)
	}
	printReport(report, results)

	// Optional LLM answer-grounding rerank: re-measure the gate after dropping
	// recalled candidates the model says don't answer the query. This is the
	// cross-encoder precision step the cosine gate can't do (§B2). It mutates a
	// COPY of the results so the cosine report above is untouched.
	if g := buildGrounder(*groundCmd, *groundURL, *groundMod, *groundKey, *groundTO); g != nil {
		grounded := deepCopyResults(results)
		fmt.Fprintf(os.Stderr, "grounding rerank via %s (top-%d/probe)...\n", g.label(), *groundTopK)
		t0 := time.Now()
		calls := applyGrounding(ctx, g, grounded, *groundTopK)
		fmt.Fprintf(os.Stderr, "grounding: %d model calls in %s\n", calls, time.Since(t0).Round(time.Millisecond))
		// Split AFTER grounding so the partitions see the filtered Recalled sets.
		gp, ga := splitPresentAbsent(grounded)
		reportGroundedGate(g.label(), gp, ga)
	}
	return nil
}

// splitPresentAbsent partitions probe results by their Present label.
func splitPresentAbsent(results []probeResult) (present, absent []probeResult) {
	for _, r := range results {
		if r.Present {
			present = append(present, r)
		} else {
			absent = append(absent, r)
		}
	}
	return present, absent
}

// deepCopyResults copies results with independent Recalled slices so grounding
// can filter them without disturbing the cosine-gate report.
func deepCopyResults(results []probeResult) []probeResult {
	out := make([]probeResult, len(results))
	copy(out, results)
	for i := range out {
		out[i].Recalled = append([]recalledMemory(nil), results[i].Recalled...)
	}
	return out
}

// reportGroundedGate prints the gate metric after grounding. Because grounding
// already removes non-answering candidates, the gate is reported at a permissive
// cosine floor (0.30) — suppression now comes from grounding, not the threshold.
func reportGroundedGate(label string, present, absent []probeResult) {
	vec := func(m recalledMemory) float64 { return m.VectorScore }
	pts := gateSweep(present, absent, []float64{0.30}, vec)
	if len(pts) == 0 {
		return
	}
	p := pts[0]
	fmt.Printf("\nGROUNDED GATE (%s, cosine>=0.30 AND model says the passage answers the query)\n", label)
	fmt.Printf("  acc=%.2f f1=%.2f inject@should=%.2f suppress@absent=%.2f what=%.2f\n",
		p.GateAcc, p.GateF1, p.InjectWhenS, p.SuppressOK, p.WhatCorrect)
}

// --- dataset ---

// item is a labeled memory: a unique concept key plus distinctive content.
type item struct {
	Concept string
	Content string
}

// probe is a labeled query: the gold concept it should retrieve (empty if the
// topic is absent, i.e. the query should be suppressed).
type probe struct {
	Query   string
	Gold    string // gold concept; "" => absent (should suppress)
	Answer  string // gold answer span (for QA dump); optional
	Present bool
}

var (
	adjs      = []string{"amber", "cobalt", "crimson", "emerald", "golden", "ivory", "jade", "obsidian", "scarlet", "silver", "teal", "violet", "azure", "bronze", "copper", "indigo", "maroon", "onyx", "pearl", "ruby"}
	creatures = []string{"otter", "falcon", "lynx", "heron", "marmot", "gecko", "badger", "raven", "ferret", "ibis", "newt", "stoat", "tapir", "vole", "wren", "yak", "quail", "shrew", "civet", "dingo"}
	places    = []string{"Velmoor", "Drassil", "Khyber Reach", "Pellwitch", "Greyfen", "Lowmarsh", "Thornvale", "Quillhaven", "Brackwater", "Misthollow", "Caldspire", "Ostgard", "Fenwick Hollow", "Sablecliff", "Dunmere", "Wraithmoor", "Calloway Flats", "Embergrove", "Hollowmere", "Stagholt"}
	traits    = []string{"bioluminescent at dusk", "able to mimic birdsong", "immune to the local frostblight", "known to hoard polished stones", "active only during the spring thaw", "capable of swimming upstream for miles", "famous for its nine-note call", "the last of its migratory line"}
	landmarks = []string{"the Old Salt Bridge", "Cinder Lake", "the Tannery Steps", "Marrow Ridge", "the Glasswind Pass", "Harrow Mill", "the Sunken Orchard", "Pikeman's Wharf"}
)

// genDataset builds n unique memories from rare adjective+creature+place triples
// and matched present/absent probes. Probes are worded differently from the
// stored content so retrieval tests semantics, not lexical overlap.
func genDataset(rngSeed int64, n, nPresent, nAbsent int) ([]item, []probe, []probe) {
	rng := rand.New(rand.NewSource(rngSeed))
	triple := func(i int) (string, string, string) {
		a := adjs[i%len(adjs)]
		c := creatures[(i/len(adjs))%len(creatures)]
		p := places[(i/(len(adjs)*len(creatures)))%len(places)]
		return a, c, p
	}
	concept := func(a, c, p string) string {
		return strings.ToLower(fmt.Sprintf("%s-%s-%s", a, c, strings.ReplaceAll(p, " ", "")))
	}

	items := make([]item, 0, n)
	for i := 0; i < n; i++ {
		a, c, p := triple(i)
		trait := traits[rng.Intn(len(traits))]
		lm := landmarks[rng.Intn(len(landmarks))]
		count := 12 + rng.Intn(880)
		items = append(items, item{
			Concept: concept(a, c, p),
			Content: fmt.Sprintf("Field note: the %s %s of %s is %s. A census near %s counted roughly %d individuals.",
				a, c, p, trait, lm, count),
		})
	}

	// Present probes: a paraphrased question about a seeded triple.
	present := make([]probe, 0, nPresent)
	for i := 0; i < nPresent && i < n; i++ {
		idx := (i * 7) % n // spread across the corpus
		a, c, p := triple(idx)
		present = append(present, probe{
			Query:   fmt.Sprintf("What have naturalists recorded about the %s %s that lives around %s?", a, c, p),
			Gold:    concept(a, c, p),
			Present: true,
		})
	}

	// Absent probes: triples beyond the seeded range (guaranteed not stored).
	absent := make([]probe, 0, nAbsent)
	for i := 0; i < nAbsent; i++ {
		idx := n + 1 + i*3 // past the seeded indices
		a, c, p := triple(idx)
		absent = append(absent, probe{
			Query:   fmt.Sprintf("What have naturalists recorded about the %s %s that lives around %s?", a, c, p),
			Gold:    "",
			Present: false,
		})
	}
	return items, present, absent
}

// coinName builds a deterministic distinctive pseudo-word from an index, so each
// diverse memory has a unique, embedding-separable subject token.
func coinName(i int) string {
	a := []string{"Zeph", "Korb", "Vael", "Drix", "Mor", "Quil", "Tav", "Bryn", "Sol", "Hesp", "Nyx", "Orin", "Pell", "Rask", "Vund", "Wex"}
	b := []string{"yr", "al", "ix", "on", "eth", "ar", "is", "um", " os", "en", "ic", "ad", "or", "us", "el", "yn"}
	c := []string{"ia", "os", "ane", "ex", "ium", "ara", "oid", "yx", "een", "ova", "ull", "ade", "ish", "orn", "wick", "holt"}
	return a[i%len(a)] + b[(i/len(a))%len(b)] + c[(i/(len(a)*len(b)))%len(c)]
}

// genDiverse builds memories spread across distinct domains with unique
// vocabulary, so embeddings separate them well — a realistic memory store rather
// than the near-identical homogeneous corpus. Each memory has a unique coined
// subject; probes paraphrase a fact about that subject.
func genDiverse(rngSeed int64, n, nPresent, nAbsent int) ([]item, []probe, []probe) {
	rng := rand.New(rand.NewSource(rngSeed))
	mem := func(i int) item {
		x := coinName(i)
		switch i % 8 {
		case 0:
			return item{"tool-" + x, fmt.Sprintf("The %s build tool caches compiled artifacts under ~/.%s/cache and invalidates them on lockfile changes.", x, strings.ToLower(x))}
		case 1:
			return item{"person-" + x, fmt.Sprintf("%s, the lead engineer on the Halcyon project, insists that every pull request include a rollback plan.", x)}
		case 2:
			return item{"lang-" + x, fmt.Sprintf("In the %s programming language, tail calls are optimized away by the compiler into loops, so deep recursion never overflows the stack.", x)}
		case 3:
			return item{"dish-" + x, fmt.Sprintf("%s stew, a specialty of the coastal town of Marrowport, is traditionally thickened with roasted chestnut flour.", x)}
		case 4:
			return item{"moon-" + x, fmt.Sprintf("The moon %s, orbiting the gas giant Theron, is notable for its methane geysers that erupt on a seven-hour cycle.", x)}
		case 5:
			return item{"proto-" + x, fmt.Sprintf("The %s protocol authenticates clients with rotating ed25519 keys exchanged over a short-lived QUIC channel.", x)}
		case 6:
			return item{"drug-" + x, fmt.Sprintf("The compound %s treats chronic vestibular migraine by blocking the CGRP receptor in the trigeminal pathway.", x)}
		default:
			return item{"treaty-" + x, fmt.Sprintf("The Treaty of %s, signed in 1847, ended the decade-long Saltmarsh border war between the river provinces.", x)}
		}
	}
	probeFor := func(i int) probe {
		x := coinName(i)
		c := mem(i).Concept
		var q string
		switch i % 8 {
		case 0:
			q = fmt.Sprintf("Where does the %s build tool keep its compiled output?", x)
		case 1:
			q = fmt.Sprintf("What does %s require on every pull request?", x)
		case 2:
			q = fmt.Sprintf("How does the %s language avoid stack overflow on deep recursion?", x)
		case 3:
			q = fmt.Sprintf("Which ingredient thickens %s stew?", x)
		case 4:
			q = fmt.Sprintf("What is unusual about the moon %s?", x)
		case 5:
			q = fmt.Sprintf("How does the %s protocol authenticate clients?", x)
		case 6:
			q = fmt.Sprintf("What condition does the compound %s treat?", x)
		default:
			q = fmt.Sprintf("What conflict did the Treaty of %s end?", x)
		}
		return probe{Query: q, Gold: c, Present: true}
	}

	items := make([]item, 0, n)
	for i := 0; i < n; i++ {
		items = append(items, mem(i))
	}
	_ = rng
	present := make([]probe, 0, nPresent)
	for i := 0; i < nPresent && i < n; i++ {
		present = append(present, probeFor((i*7)%n))
	}
	absent := make([]probe, 0, nAbsent)
	for i := 0; i < nAbsent; i++ {
		absent = append(absent, probeFor(n+1+i*3)) // coined subjects past the seeded range
	}
	for i := range absent {
		absent[i].Gold = ""
		absent[i].Present = false
	}
	return items, present, absent
}

// --- seeding ---

func seedCorpus(ctx context.Context, c *mcpclient.Client, vault string, items []item) error {
	const batchSize = 25
	for start := 0; start < len(items); start += batchSize {
		end := start + batchSize
		if end > len(items) {
			end = len(items)
		}
		mems := make([]map[string]any, 0, end-start)
		for _, it := range items[start:end] {
			mems = append(mems, map[string]any{
				"concept": it.Concept,
				"content": it.Content,
				"summary": it.Content,
				"type":    "reference",
			})
		}
		if _, err := c.Call(ctx, "muninn_remember_batch", map[string]any{
			"vault":    vault,
			"memories": mems,
		}); err != nil {
			return fmt.Errorf("seed batch [%d:%d]: %w", start, end, err)
		}
		fmt.Fprintf(os.Stderr, "  seeded %d/%d\n", end, len(items))
	}
	return nil
}

// --- probing ---

type recalledMemory struct {
	Concept     string  `json:"concept"`
	Content     string  `json:"content"`
	Score       float64 `json:"score"`
	VectorScore float64 `json:"vector_score"`
}

// probeOpts configures query construction and reranking for an experiment run.
type probeOpts struct {
	mode         string
	transform    string  // none | distractors | repeat-last
	distractN    int     // prior unrelated turns to prepend
	rerank       string  // none | lexical
	rerankLambda float64 // lexical rerank weight
	multiRecall  bool    // split the query into entity spans, recall each, merge
}

// recallMems issues one recall and parses the memories.
func recallMems(ctx context.Context, c *mcpclient.Client, vault, query string, limit int, mode string) ([]recalledMemory, error) {
	args := map[string]any{"vault": vault, "context": []string{query}, "limit": limit, "threshold": 0.05}
	if mode != "" {
		args["mode"] = mode
	}
	resp, err := c.Call(ctx, "muninn_recall", args)
	if err != nil {
		return nil, err
	}
	return parseRecall(resp)
}

// splitQuery decomposes a query into sub-queries for multi-recall: the full
// query plus each capitalized entity span (a no-LLM proxy for the "hops" a
// multi-hop question references). Deduped, full query first.
func splitQuery(q string) []string {
	subs := []string{q}
	seen := map[string]bool{q: true}
	words := strings.Fields(q)
	var cur []string
	flush := func() {
		if len(cur) > 0 {
			s := strings.Trim(strings.Join(cur, " "), "?.,")
			if len(s) >= 3 && !seen[s] {
				seen[s] = true
				subs = append(subs, s)
			}
			cur = nil
		}
	}
	for _, w := range words {
		r := []rune(w)
		if len(r) > 0 && r[0] >= 'A' && r[0] <= 'Z' {
			cur = append(cur, w)
		} else {
			flush()
		}
	}
	flush()
	return subs
}

// recallMerged does a single recall, or (multi) splits the query, recalls each
// sub-query, and merges by best vector_score per concept — a transparent,
// no-LLM way to surface both hops of a multi-hop question.
func recallMerged(ctx context.Context, c *mcpclient.Client, vault, query string, limit int, mode string, multi bool) ([]recalledMemory, error) {
	if !multi {
		return recallMems(ctx, c, vault, query, limit, mode)
	}
	best := map[string]recalledMemory{}
	for _, sub := range splitQuery(query) {
		ms, err := recallMems(ctx, c, vault, sub, limit, mode)
		if err != nil {
			continue
		}
		for _, m := range ms {
			if cur, ok := best[m.Concept]; !ok || m.VectorScore > cur.VectorScore {
				best[m.Concept] = m
			}
		}
	}
	out := make([]recalledMemory, 0, len(best))
	for _, m := range best {
		out = append(out, m)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].VectorScore > out[j].VectorScore })
	return out, nil
}

// lexOverlap is the word-set Jaccard of two strings (cheap lexical similarity).
func lexOverlap(a, b string) float64 {
	wa := strings.Fields(strings.ToLower(a))
	wb := strings.Fields(strings.ToLower(b))
	if len(wa) == 0 || len(wb) == 0 {
		return 0
	}
	set := make(map[string]struct{}, len(wa))
	for _, w := range wa {
		set[w] = struct{}{}
	}
	inter := 0
	seen := make(map[string]struct{}, len(wb))
	for _, w := range wb {
		if _, dup := seen[w]; dup {
			continue
		}
		seen[w] = struct{}{}
		if _, ok := set[w]; ok {
			inter++
		}
	}
	union := len(set) + len(seen) - inter
	if union == 0 {
		return 0
	}
	return float64(inter) / float64(union)
}

// transformQuery builds the query actually sent, simulating how the proxy would
// construct it from conversation context.
func transformQuery(opt probeOpts, probes []probe, i int) string {
	q := probes[i].Query
	switch opt.transform {
	case "distractors":
		// Prepend N other probes' queries as prior unrelated turns, then the real
		// question last — tests whether multi-turn concatenation dilutes recall.
		var b strings.Builder
		for d := 1; d <= opt.distractN; d++ {
			j := (i + d*37) % len(probes)
			b.WriteString(probes[j].Query)
			b.WriteString("\n")
		}
		b.WriteString(q)
		return b.String()
	case "emphasis":
		// Latest turn FIRST, then prior unrelated turns — mirrors the proxy's
		// recency-emphasis query construction; should recover the retrieval that
		// plain "distractors" loses.
		var b strings.Builder
		b.WriteString(q)
		for d := 1; d <= opt.distractN; d++ {
			j := (i + d*37) % len(probes)
			b.WriteString("\n")
			b.WriteString(probes[j].Query)
		}
		return b.String()
	case "repeat-last":
		return q + "\n" + q
	default:
		return q
	}
}

// probeResult records, for one probe, the ranked recall output (by score order
// as MuninnDB returns it) and the rank of the gold concept.
type probeResult struct {
	probe
	Recalled []recalledMemory `json:"recalled"`
	// rankByScore / rankByVec: 0-based rank of gold in the result list ordered
	// by score / vector_score; -1 if gold absent from results.
	RankByScore int `json:"rank_by_score"`
	RankByVec   int `json:"rank_by_vec"`
	RankRerank  int `json:"rank_rerank"` // rank under the configured rerank (== vec when rerank=none)
	// Article-level ranks: rank of the first recalled memory from the SAME
	// source article as gold (concept prefix before '#'). For corpora without
	// '#' in concepts this equals the exact rank. Captures topic-level retrieval,
	// which is what injection needs — a sibling paragraph is usually also useful.
	RankArtScore int `json:"rank_art_score"`
	RankArtVec   int `json:"rank_art_vec"`
}

// articleOf returns the article key of a concept (the part before '#'), or the
// whole concept when there is no '#'.
func articleOf(concept string) string {
	if i := strings.IndexByte(concept, '#'); i >= 0 {
		return concept[:i]
	}
	return concept
}

// rankArticleOf returns the 0-based rank, sorted by field desc, of the first
// recalled memory sharing gold's article; -1 if gold empty or none match.
func rankArticleOf(mems []recalledMemory, gold string, field func(recalledMemory) float64) int {
	if gold == "" {
		return -1
	}
	art := articleOf(gold)
	sorted := append([]recalledMemory(nil), mems...)
	sort.SliceStable(sorted, func(i, j int) bool { return field(sorted[i]) > field(sorted[j]) })
	for i, m := range sorted {
		if articleOf(m.Concept) == art {
			return i
		}
	}
	return -1
}

func runProbes(ctx context.Context, c *mcpclient.Client, vault string, probes []probe, limit int, opt probeOpts) ([]probeResult, error) {
	out := make([]probeResult, 0, len(probes))
	var totalDur time.Duration
	var timed int
	for i, pr := range probes {
		query := transformQuery(opt, probes, i)
		t0 := time.Now()
		mems, err := recallMerged(ctx, c, vault, query, limit, opt.mode, opt.multiRecall)
		totalDur += time.Since(t0)
		timed++
		if err != nil {
			// Skip transient failures rather than aborting the whole run; a
			// dropped probe just doesn't contribute to the metrics.
			fmt.Fprintf(os.Stderr, "  warn: probe %d skipped: %v\n", i, err)
			continue
		}
		// Optional lexical rerank field: vector + lambda * token-overlap(query, content).
		vecField := func(m recalledMemory) float64 { return m.VectorScore }
		rerankField := vecField
		if opt.rerank == "lexical" {
			rerankField = func(m recalledMemory) float64 {
				return m.VectorScore + opt.rerankLambda*lexOverlap(pr.Query, m.Content)
			}
		}
		out = append(out, probeResult{
			probe:        pr,
			Recalled:     mems,
			RankByScore:  rankOf(mems, pr.Gold, func(m recalledMemory) float64 { return m.Score }),
			RankByVec:    rankOf(mems, pr.Gold, vecField),
			RankRerank:   rankOf(mems, pr.Gold, rerankField),
			RankArtScore: rankArticleOf(mems, pr.Gold, func(m recalledMemory) float64 { return m.Score }),
			RankArtVec:   rankArticleOf(mems, pr.Gold, vecField),
		})
		if (i+1)%25 == 0 {
			fmt.Fprintf(os.Stderr, "  probed %d/%d\n", i+1, len(probes))
		}
	}
	if timed > 0 {
		fmt.Fprintf(os.Stderr, "recall latency: avg %.1fms over %d calls\n",
			float64(totalDur.Microseconds())/float64(timed)/1000, timed)
	}
	return out, nil
}

func parseRecall(body []byte) ([]recalledMemory, error) {
	var rpc struct {
		Result struct {
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
		} `json:"result"`
	}
	if err := json.Unmarshal(body, &rpc); err != nil {
		return nil, err
	}
	for _, ct := range rpc.Result.Content {
		if ct.Type != "text" {
			continue
		}
		var inner struct {
			Memories []recalledMemory `json:"memories"`
		}
		if err := json.Unmarshal([]byte(ct.Text), &inner); err != nil {
			return nil, err
		}
		return inner.Memories, nil
	}
	return nil, nil
}

// rankOf returns the 0-based rank of the gold concept when results are sorted by
// the given field descending; -1 if gold is empty or not present.
func rankOf(mems []recalledMemory, gold string, field func(recalledMemory) float64) int {
	if gold == "" {
		return -1
	}
	sorted := append([]recalledMemory(nil), mems...)
	sort.SliceStable(sorted, func(i, j int) bool { return field(sorted[i]) > field(sorted[j]) })
	for i, m := range sorted {
		if m.Concept == gold {
			return i
		}
	}
	return -1
}

// --- analysis ---

type retrievalMetrics struct {
	R1, R3, R5 float64
	MRR        float64
}

type gatePoint struct {
	Threshold   float64 `json:"threshold"`
	GateAcc     float64 `json:"gate_accuracy"`
	GateF1      float64 `json:"gate_f1"`
	InjectWhenS float64 `json:"inject_when_should"`   // sensitivity: present probes that injected
	SuppressOK  float64 `json:"suppress_when_absent"` // specificity: absent probes suppressed
	WhatCorrect float64 `json:"what_correct"`         // present probes where gold is the top kept item
}

type benchReport struct {
	NPresent        int              `json:"n_present"`
	NAbsent         int              `json:"n_absent"`
	RetrievalScore  retrievalMetrics `json:"retrieval_by_score_order"`
	RetrievalVec    retrievalMetrics `json:"retrieval_by_vector_order"`
	RetrievalRerank retrievalMetrics `json:"retrieval_by_rerank"`
	RetrievalArtVec retrievalMetrics `json:"retrieval_article_by_vector"`
	GateByScore     []gatePoint      `json:"gate_by_score"`
	GateByVec       []gatePoint      `json:"gate_by_vector"`
	BestScore       gatePoint        `json:"best_by_score"`
	BestVec         gatePoint        `json:"best_by_vector"`
}

func analyze(results []probeResult) benchReport {
	var present, absent []probeResult
	for _, r := range results {
		if r.Present {
			present = append(present, r)
		} else {
			absent = append(absent, r)
		}
	}

	rep := benchReport{NPresent: len(present), NAbsent: len(absent)}
	rep.RetrievalScore = retrieval(present, func(r probeResult) int { return r.RankByScore })
	rep.RetrievalVec = retrieval(present, func(r probeResult) int { return r.RankByVec })
	rep.RetrievalRerank = retrieval(present, func(r probeResult) int { return r.RankRerank })
	rep.RetrievalArtVec = retrieval(present, func(r probeResult) int { return r.RankArtVec })

	scoreThresholds := frange(0.3, 1.2, 0.05)
	vecThresholds := frange(0.30, 0.85, 0.025)
	rep.GateByScore = gateSweep(present, absent, scoreThresholds, func(m recalledMemory) float64 { return m.Score })
	rep.GateByVec = gateSweep(present, absent, vecThresholds, func(m recalledMemory) float64 { return m.VectorScore })
	rep.BestScore = bestGate(rep.GateByScore)
	rep.BestVec = bestGate(rep.GateByVec)
	return rep
}

func retrieval(present []probeResult, rank func(probeResult) int) retrievalMetrics {
	if len(present) == 0 {
		return retrievalMetrics{}
	}
	var r1, r3, r5, mrr float64
	for _, r := range present {
		k := rank(r)
		if k < 0 {
			continue
		}
		if k == 0 {
			r1++
		}
		if k < 3 {
			r3++
		}
		if k < 5 {
			r5++
		}
		mrr += 1.0 / float64(k+1)
	}
	n := float64(len(present))
	return retrievalMetrics{R1: r1 / n, R3: r3 / n, R5: r5 / n, MRR: mrr / n}
}

// gateSweep scores the when-to-inject decision at each threshold on the given
// field. The in-flight rule is: inject iff the top result's field value >= T.
func gateSweep(present, absent []probeResult, thresholds []float64, field func(recalledMemory) float64) []gatePoint {
	pts := make([]gatePoint, 0, len(thresholds))
	for _, t := range thresholds {
		var injectWhenShould, suppressWhenAbsent, whatCorrect float64
		tp, fp, fn := 0, 0, 0
		for _, r := range present {
			top, ok := topByField(r.Recalled, field)
			injected := ok && top >= t
			if injected {
				injectWhenShould++
				tp++
				if c, ok2 := topConcept(r.Recalled, field); ok2 && c == r.Gold {
					whatCorrect++
				}
			} else {
				fn++
			}
		}
		for _, r := range absent {
			top, ok := topByField(r.Recalled, field)
			if ok && top >= t {
				fp++
			} else {
				suppressWhenAbsent++
			}
		}
		nP, nA := float64(len(present)), float64(len(absent))
		acc := (injectWhenShould + suppressWhenAbsent) / (nP + nA)
		prec, rec := 0.0, 0.0
		if tp+fp > 0 {
			prec = float64(tp) / float64(tp+fp)
		}
		if tp+fn > 0 {
			rec = float64(tp) / float64(tp+fn)
		}
		f1 := 0.0
		if prec+rec > 0 {
			f1 = 2 * prec * rec / (prec + rec)
		}
		pts = append(pts, gatePoint{
			Threshold:   t,
			GateAcc:     acc,
			GateF1:      f1,
			InjectWhenS: safeDiv(injectWhenShould, nP),
			SuppressOK:  safeDiv(suppressWhenAbsent, nA),
			WhatCorrect: safeDiv(whatCorrect, nP),
		})
	}
	return pts
}

func bestGate(pts []gatePoint) gatePoint {
	best := gatePoint{GateAcc: -1}
	for _, p := range pts {
		if p.GateAcc > best.GateAcc || (p.GateAcc == best.GateAcc && p.GateF1 > best.GateF1) {
			best = p
		}
	}
	return best
}

func topByField(mems []recalledMemory, field func(recalledMemory) float64) (float64, bool) {
	if len(mems) == 0 {
		return 0, false
	}
	max := field(mems[0])
	for _, m := range mems[1:] {
		if v := field(m); v > max {
			max = v
		}
	}
	return max, true
}

func topConcept(mems []recalledMemory, field func(recalledMemory) float64) (string, bool) {
	if len(mems) == 0 {
		return "", false
	}
	best, bestV := mems[0].Concept, field(mems[0])
	for _, m := range mems[1:] {
		if v := field(m); v > bestV {
			best, bestV = m.Concept, v
		}
	}
	return best, true
}

func printReport(rep benchReport, results []probeResult) {
	fmt.Printf("\n=== msc-bench: real MuninnDB retrieval + when-to-inject ===\n")
	fmt.Printf("present probes: %d   absent probes: %d\n\n", rep.NPresent, rep.NAbsent)

	fmt.Printf("RETRIEVAL (did recall surface the gold memory?)\n")
	fmt.Printf("  ranked by score      R@1=%.2f R@3=%.2f R@5=%.2f MRR=%.3f\n",
		rep.RetrievalScore.R1, rep.RetrievalScore.R3, rep.RetrievalScore.R5, rep.RetrievalScore.MRR)
	fmt.Printf("  ranked by vector     R@1=%.2f R@3=%.2f R@5=%.2f MRR=%.3f\n",
		rep.RetrievalVec.R1, rep.RetrievalVec.R3, rep.RetrievalVec.R5, rep.RetrievalVec.MRR)
	fmt.Printf("  reranked             R@1=%.2f R@3=%.2f R@5=%.2f MRR=%.3f\n",
		rep.RetrievalRerank.R1, rep.RetrievalRerank.R3, rep.RetrievalRerank.R5, rep.RetrievalRerank.MRR)
	fmt.Printf("  article-level (vec)  R@1=%.2f R@3=%.2f R@5=%.2f MRR=%.3f  (same-article hit = useful)\n\n",
		rep.RetrievalArtVec.R1, rep.RetrievalArtVec.R3, rep.RetrievalArtVec.R5, rep.RetrievalArtVec.MRR)

	printGate("WHEN-TO-INJECT gate on `score` (composite)", rep.GateByScore)
	printGate("WHEN-TO-INJECT gate on `vector_score` (cosine)", rep.GateByVec)

	fmt.Printf("\nBEST gate on score : T=%.3f acc=%.2f f1=%.2f (inject@should=%.2f suppress@absent=%.2f what=%.2f)\n",
		rep.BestScore.Threshold, rep.BestScore.GateAcc, rep.BestScore.GateF1, rep.BestScore.InjectWhenS, rep.BestScore.SuppressOK, rep.BestScore.WhatCorrect)
	fmt.Printf("BEST gate on vector: T=%.3f acc=%.2f f1=%.2f (inject@should=%.2f suppress@absent=%.2f what=%.2f)\n",
		rep.BestVec.Threshold, rep.BestVec.GateAcc, rep.BestVec.GateF1, rep.BestVec.InjectWhenS, rep.BestVec.SuppressOK, rep.BestVec.WhatCorrect)
}

func printGate(title string, pts []gatePoint) {
	fmt.Printf("\n%s\n", title)
	fmt.Printf("%8s %5s %5s %10s %9s %6s\n", "thresh", "acc", "f1", "inj@should", "supp@abs", "what")
	for _, p := range pts {
		fmt.Printf("%8.3f %5.2f %5.2f %10.2f %9.2f %6.2f\n",
			p.Threshold, p.GateAcc, p.GateF1, p.InjectWhenS, p.SuppressOK, p.WhatCorrect)
	}
}

// --- helpers ---

// writeQA dumps present probes as a generic QA JSON ([{question,answer}]) that
// msc-qa reads with -dataset generic.
func writeQA(path string, probes []probe) error {
	type qa struct {
		Question string `json:"question"`
		Answer   string `json:"answer"`
	}
	out := make([]qa, 0, len(probes))
	for _, p := range probes {
		if p.Present && p.Answer != "" {
			out = append(out, qa{Question: p.Query, Answer: p.Answer})
		}
	}
	data, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o644)
}

func frange(lo, hi, step float64) []float64 {
	var out []float64
	for v := lo; v <= hi+1e-9; v += step {
		out = append(out, float64(int(v*1000+0.5))/1000)
	}
	return out
}

func safeDiv(a, b float64) float64 {
	if b == 0 {
		return 0
	}
	return a / b
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func resolveToken(flagVal string) string {
	if flagVal != "" {
		return flagVal
	}
	if t := os.Getenv("MUNINN_TOKEN"); t != "" {
		return t
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	data, err := os.ReadFile(filepath.Join(home, ".muninn", "mcp.token"))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}
