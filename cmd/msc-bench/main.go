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
	"hash/fnv"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
	"unicode"

	"github.com/maci0/muninn-sidecar/internal/config"
	"github.com/maci0/muninn-sidecar/internal/grounding"
	"github.com/maci0/muninn-sidecar/internal/inject"
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
		mcpURL     = flag.String("mcp-url", config.MCPURL(""), "MuninnDB MCP endpoint")
		token      = flag.String("token", "", "bearer token (default ~/.muninn/mcp.token)")
		vault      = flag.String("vault", "msc-bench", "vault to seed/probe (dedicated; not 'default')")
		corpus     = flag.String("corpus", "homogeneous", "corpus generator: homogeneous | diverse | facts | squad | hotpot | agentmem")
		squadFile  = flag.String("squad-file", filepath.Join(os.TempDir(), "squad-dev.json"), "path to SQuAD JSON (corpus=squad)")
		squadArts  = flag.Int("squad-articles", 12, "number of SQuAD articles to seed (rest are held out as absent)")
		hardNeg    = flag.Bool("hard-neg", false, "squad: draw negatives from held-out paragraphs of SEEDED articles (same-topic hard negatives) instead of disjoint articles")
		chunk      = flag.String("chunk", "paragraph", "SQuAD chunk granularity: paragraph | sentence")
		dumpQA     = flag.String("dump-qa", "", "write present probes as a QA JSON ([{question,answer}]) for msc-qa -dataset generic")
		mode       = flag.String("mode", "", "MuninnDB recall mode: semantic | recent | balanced | deep (empty = server default)")
		qTransform = flag.String("query-transform", "none", "query construction: none | distractors (prepend prior unrelated turns) | emphasis (latest turn first) | repeat-last")
		distractN  = flag.Int("distractors", 2, "number of prior unrelated turns to prepend (query-transform=distractors)")
		rerank     = flag.String("rerank", "none", "candidate rerank: none | lexical (vector + lambda*token-overlap)")
		rerankL    = flag.Float64("rerank-lambda", 0.3, "lexical rerank weight")
		groundCmd  = flag.String("ground-cmd", "", "LLM answer-grounding rerank via a CLI agent (e.g. \"claude -p\"); drops recalled candidates the model says don't answer the query")
		groundURL  = flag.String("ground-url", "", "LLM answer-grounding rerank via an OpenAI-compatible URL (e.g. http://127.0.0.1:11434/v1)")
		groundMod  = flag.String("ground-model", "qwen2.5:1.5b-instruct", "grounding model name (for -ground-url)")
		groundKey  = flag.String("ground-key", "", "grounding model API key (for -ground-url)")
		groundTopK = flag.Int("ground-topk", 5, "ground only the top-K candidates by cosine per probe (bounds model calls)")
		groundTO   = flag.Duration("ground-timeout", 60*time.Second, "per grounding-call timeout")
		rewriteCmd = flag.String("rewrite-cmd", "", "LLM query rewrite/decomposition before recall via a CLI agent (e.g. \"claude -p\"); the prompt is delivered on stdin")
		rewriteURL = flag.String("rewrite-url", "", "LLM query rewrite via an OpenAI-compatible URL")
		rewriteMod = flag.String("rewrite-model", "qwen2.5:7b-instruct", "rewrite model name (for -rewrite-url)")
		rewriteKey = flag.String("rewrite-key", "", "rewrite model API key (for -rewrite-url)")
		rewriteN   = flag.Int("rewrite-n", 4, "max sub-queries per probe (including the original)")
		rewriteTO  = flag.Duration("rewrite-timeout", 60*time.Second, "per rewrite-call timeout")
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
	if err := config.ValidateURL("MuninnDB URL", *mcpURL); err != nil {
		return err
	}
	switch *corpus {
	case "homogeneous", "diverse", "facts", "squad", "hotpot", "agentmem":
	default:
		return fmt.Errorf("invalid -corpus %q: must be one of homogeneous, diverse, facts, squad, hotpot, agentmem", *corpus)
	}
	switch *mode {
	case "", "semantic", "recent", "balanced", "deep":
	default:
		return fmt.Errorf("invalid -mode %q: must be one of semantic, recent, balanced, deep (or empty for server default)", *mode)
	}
	for opt, raw := range map[string]string{"-ground-url": *groundURL, "-rewrite-url": *rewriteURL} {
		if err := config.ValidateURL(opt, raw); err != nil {
			return err
		}
	}
	if !*seed && !*doProbe {
		*doProbe = true
	}

	client := mcpclient.New(*mcpURL, resolveToken(*token), *timeout)
	var items []item
	var presentProbes, absentProbes []probe
	var err error
	switch *corpus {
	case "squad":
		if *hardNeg {
			items, presentProbes, absentProbes, err = genSquadHardNeg(*squadFile, *squadArts, *n, *present, *absent, *chunk)
		} else {
			items, presentProbes, absentProbes, err = genSquad(*squadFile, *squadArts, *n, *present, *absent, *chunk)
		}
	case "hotpot":
		items, presentProbes, absentProbes, err = genHotpot(*squadFile, *squadArts, *n, *present, *absent)
	case "agentmem":
		items, presentProbes, absentProbes, err = genAgentMem(*n, *absent)
	case "facts":
		items, presentProbes, absentProbes = genFacts()
	case "diverse":
		items, presentProbes, absentProbes, err = genDiverse(*rngSeed, *n, *present, *absent)
	default:
		items, presentProbes, absentProbes, err = genDataset(*rngSeed, *n, *present, *absent)
	}
	if err != nil {
		return err
	}
	ctx := context.Background()

	if *dumpQA != "" {
		nQA, err := writeQA(*dumpQA, presentProbes)
		if err != nil {
			return err
		}
		fmt.Fprintf(os.Stderr, "wrote %d QA pairs to %s\n", nQA, *dumpQA)
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
	if rw := buildRewriter(*rewriteCmd, *rewriteURL, *rewriteMod, *rewriteKey, *rewriteTO); rw != nil {
		opt.rewriter = rw
		opt.rewriteN = *rewriteN
		fmt.Fprintf(os.Stderr, "query rewrite enabled via %s (≤%d sub-queries/probe)\n", rw.label(), *rewriteN)
	}
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
	if g := grounding.New(*groundCmd, *groundURL, *groundMod, *groundKey, *groundTO); g != nil {
		grounded := deepCopyResults(results)
		fmt.Fprintf(os.Stderr, "grounding rerank via %s (top-%d/probe)...\n", g.Label(), *groundTopK)
		t0 := time.Now()
		calls := applyGrounding(ctx, g, grounded, *groundTopK)
		fmt.Fprintf(os.Stderr, "grounding: %d model calls in %s\n", calls, time.Since(t0).Round(time.Millisecond))
		// Split AFTER grounding so the partitions see the filtered Recalled sets.
		gp, ga := splitPresentAbsent(grounded)
		reportGroundedGate(g.Label(), gp, ga)
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

// --- seeding ---

// seedCorpus stores the corpus in vault. Every memory carries a content-addressed
// dedup_key (mcpclient.DedupKey), so re-running -seed is safe: the same corpus
// collapses onto the memories already there instead of doubling them. Duplicates
// would not be harmless here — they crowd recall's top-k and skew exactly the
// retrieval numbers the benchmark exists to measure.
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
				"concept":   it.Concept,
				"content":   it.Content,
				"summary":   it.Content,
				"type":      "reference",
				"dedup_key": mcpclient.DedupKey(vault, it.Concept, it.Content),
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
	transform    string   // none | distractors | emphasis | repeat-last
	distractN    int      // prior unrelated turns to prepend
	rerank       string   // none | lexical
	rerankLambda float64  // lexical rerank weight
	multiRecall  bool     // split the query into entity spans, recall each, merge
	rewriter     rewriter // optional LLM query rewrite/decomposition before recall
	rewriteN     int      // max sub-queries (incl. original) from the rewriter
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
		// unicode.IsUpper, not an 'A'..'Z' range test: a German noun ("Migrations-
		// strategie"), a Turkish or Cyrillic proper noun, and any CJK entity carry
		// no ASCII uppercase at all, so the range test silently extracted no
		// sub-queries for those queries and multi-hop recall collapsed to a single
		// recall of the whole question.
		r := []rune(w)
		if len(r) > 0 && unicode.IsUpper(r[0]) {
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
	return recallSubqueries(ctx, c, vault, splitQuery(query), limit, mode), nil
}

// recallSubqueries recalls each sub-query and merges by best vector_score per
// concept — shared by the no-LLM entity split and the LLM query-rewrite path.
func recallSubqueries(ctx context.Context, c *mcpclient.Client, vault string, subs []string, limit int, mode string) []recalledMemory {
	best := map[string]recalledMemory{}
	for _, sub := range subs {
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
	return out
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
		var mems []recalledMemory
		var err error
		if opt.rewriter != nil {
			// LLM query rewrite/decomposition → recall each sub-query, merge.
			subs := opt.rewriter.Rewrite(ctx, query, opt.rewriteN)
			mems = recallSubqueries(ctx, c, vault, subs, limit, opt.mode)
		} else {
			mems, err = recallMerged(ctx, c, vault, query, limit, opt.mode, opt.multiRecall)
		}
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
	// Held-out variants: threshold picked on the tune half (probes whose query
	// hash is even), metrics measured on the eval half (odd hash).
	// BestScore/BestVec are in-sample optima and thus optimistic.
	HeldOutScore gatePoint `json:"heldout_by_score"`
	HeldOutVec   gatePoint `json:"heldout_by_vector"`
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
	scoreOf := func(m recalledMemory) float64 { return m.Score }
	vecOf := func(m recalledMemory) float64 { return m.VectorScore }
	rep.GateByScore = gateSweep(present, absent, scoreThresholds, scoreOf)
	rep.GateByVec = gateSweep(present, absent, vecThresholds, vecOf)
	rep.BestScore = bestGate(rep.GateByScore)
	rep.BestVec = bestGate(rep.GateByVec)
	rep.HeldOutScore = heldOutBest(results, scoreThresholds, scoreOf)
	rep.HeldOutVec = heldOutBest(results, vecThresholds, vecOf)
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
		acc := safeDiv(injectWhenShould+suppressWhenAbsent, nP+nA)
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

// tuneHalf assigns a probe to the tune half of the held-out split by hashing
// its query. Result-index parity would NOT work: the synthetic generators pick
// question category by index modulo an even number, so an even/odd index split
// puts disjoint category sets in each half and the "held-out" metric would
// measure cross-category transfer instead of generalization. The hash is
// deterministic so runs stay comparable.
func tuneHalf(query string) bool {
	h := fnv.New32a()
	h.Write([]byte(query))
	return h.Sum32()%2 == 0
}

// heldOutBest counters the in-sample optimism of bestGate, which selects and
// scores the threshold on the same probes. It splits results into a tune half
// and an eval half by query hash (see tuneHalf), picks the best threshold on
// the tune half, and returns that threshold's gate metrics measured on the
// eval half.
func heldOutBest(results []probeResult, thresholds []float64, field func(recalledMemory) float64) gatePoint {
	var tuneP, tuneA, evalP, evalA []probeResult
	for _, r := range results {
		switch {
		case tuneHalf(r.Query) && r.Present:
			tuneP = append(tuneP, r)
		case tuneHalf(r.Query):
			tuneA = append(tuneA, r)
		case r.Present:
			evalP = append(evalP, r)
		default:
			evalA = append(evalA, r)
		}
	}
	best := bestGate(gateSweep(tuneP, tuneA, thresholds, field))
	return gateSweep(evalP, evalA, []float64{best.Threshold}, field)[0]
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
	fmt.Printf("BEST gate (held-out) on score : T=%.3f acc=%.2f f1=%.2f (inject@should=%.2f suppress@absent=%.2f what=%.2f)\n",
		rep.HeldOutScore.Threshold, rep.HeldOutScore.GateAcc, rep.HeldOutScore.GateF1, rep.HeldOutScore.InjectWhenS, rep.HeldOutScore.SuppressOK, rep.HeldOutScore.WhatCorrect)
	fmt.Printf("BEST gate (held-out) on vector: T=%.3f acc=%.2f f1=%.2f (inject@should=%.2f suppress@absent=%.2f what=%.2f)\n",
		rep.HeldOutVec.Threshold, rep.HeldOutVec.GateAcc, rep.HeldOutVec.GateF1, rep.HeldOutVec.InjectWhenS, rep.HeldOutVec.SuppressOK, rep.HeldOutVec.WhatCorrect)

	// Validate auto-calibration: feed the observed cosines (what the injector's
	// observeCalibration samples) to the production CalibrateThreshold and compare
	// to the empirical best. They should land close — proof the shipped online
	// calibration discovers the right per-vault gate without the labeled sweep.
	var cosines []float64
	for _, r := range results {
		for _, m := range r.Recalled {
			cosines = append(cosines, m.VectorScore)
		}
	}
	if len(cosines) > 0 {
		calT, noiseMean, relMean, sep := inject.CalibrateThresholdDetail(cosines)
		fmt.Printf("AUTO-CALIBRATED gate: T=%.3f (from %d cosines; best %.3f, |Δ|=%.3f; clusters noise=%.3f rel=%.3f sep=%.3f)\n",
			calT, len(cosines), rep.BestVec.Threshold, math.Abs(calT-rep.BestVec.Threshold), noiseMean, relMean, sep)
	}
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
// msc-qa reads with -dataset generic, returning the pair count. Probes without
// an answer span are an error: writing an empty file would silently give a
// later msc-qa run zero questions.
func writeQA(path string, probes []probe) (int, error) {
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
	if len(out) == 0 {
		return 0, fmt.Errorf("no present probes carry an answer span; -dump-qa needs a corpus with answers (squad, hotpot, agentmem)")
	}
	data, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		return 0, err
	}
	return len(out), os.WriteFile(path, data, 0o644)
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

func resolveToken(flagVal string) string { return config.Token(flagVal) }
