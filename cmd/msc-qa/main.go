// Command msc-qa measures the DOWNSTREAM usefulness of memory injection: does
// putting recalled context in front of the model actually improve its answers?
// It replays SQuAD questions through an OpenAI-compatible chat model under three
// arms and scores answers with SQuAD exact-match / token-F1:
//
//   - none:       question only (baseline)
//   - injected:   question + the gated recall context from a seeded vault
//   - distractor: question + a deliberately irrelevant memory (harm of a false inject)
//
// It also reports answer-coverage (did the injected context even contain the
// answer?), plus how often a gold answer appears in the reply as a contiguous
// token run (the "loose containment" line under each model).
//
// This is the gold-standard metric the proxy-level studies only proxy for. It
// needs a model endpoint; without -model-url it builds the prompts and recall
// but cannot score, and exits with guidance.
//
//	msc-qa -vault msc-squad -model-url http://localhost:1234/v1 -model gpt-4o-mini -n 100
package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"math"
	"math/rand"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/maci0/muninn-sidecar/internal/apiformat"
	"github.com/maci0/muninn-sidecar/internal/config"
	"github.com/maci0/muninn-sidecar/internal/grounding"
	"github.com/maci0/muninn-sidecar/internal/mcpclient"
)

func newReq(ctx context.Context, url, key string, body []byte) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	return req, nil
}

// maxModelResponse caps a model response body so a misbehaving endpoint can't
// exhaust memory, matching the caps used by mcpclient and grounding.
const maxModelResponse = 4 << 20 // 4 MiB

func doJSON(req *http.Request, timeout time.Duration, out any) error {
	client := &http.Client{Timeout: timeout}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("call model endpoint %s: %w", req.URL, err)
	}
	defer resp.Body.Close()
	// Read one byte past the cap so an oversized body is reported as such
	// instead of failing later as invalid JSON. A failed read is the real
	// cause, so keep it rather than reporting a parse error for a truncated body.
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxModelResponse+1))
	if err != nil {
		return fmt.Errorf("read model response from %s: %w", req.URL, err)
	}
	if int64(len(data)) > maxModelResponse {
		return fmt.Errorf("model response exceeds %d-byte limit (HTTP %d)", maxModelResponse, resp.StatusCode)
	}
	if resp.StatusCode >= 300 {
		return fmt.Errorf("model HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(data)))
	}
	return json.Unmarshal(data, out)
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "msc-qa:", err)
		os.Exit(1)
	}
}

func run() error {
	var (
		squadFile   = flag.String("squad-file", filepath.Join(os.TempDir(), "squad-dev.json"), "dataset JSON path (SQuAD or HotpotQA official format)")
		dataset     = flag.String("dataset", "squad", "dataset format: squad | hotpot | generic")
		vault       = flag.String("vault", "msc-squad", "vault to recall from (must be seeded, e.g. by msc-bench)")
		mcpURL      = flag.String("mcp-url", config.MCPURL(""), "MuninnDB MCP endpoint")
		token       = flag.String("token", "", "MuninnDB bearer token (default ~/.muninn/mcp.token)")
		modelURL    = flag.String("model-url", "", "OpenAI-compatible base URL (e.g. http://localhost:1234/v1); empty = build-only, no scoring")
		modelKey    = flag.String("model-key", "", "model API key (default $OPENAI_API_KEY)")
		model       = flag.String("model", "gpt-4o-mini", "model name(s); comma-separated to compare several")
		modelCmd    = flag.String("model-cmd", "", "comma-separated reader CLIs (e.g. \"claude -p,codex exec --skip-git-repo-check,grok -p\"); each reads the prompt on stdin, the last non-empty stdout line is the answer")
		minScore    = flag.Float64("min-score", 0.6, "injection cosine threshold (the gate)")
		n           = flag.Int("n", 100, "number of questions to evaluate")
		sampleSeed  = flag.Int64("sample-seed", 1, "question-sampling shuffle seed: all eligible questions are shuffled deterministically then truncated to -n, so runs stay reproducible and paired across models")
		maxTokens   = flag.Int("max-tokens", 512, "model max_tokens (raise for thinking models)")
		multiRecall = flag.Bool("multi-recall", false, "split query into entity spans, recall each, merge (multi-hop)")
		timeout     = flag.Duration("timeout", 60*time.Second, "per-call timeout")
		mdFile      = flag.String("md", "", "append a results row per model to this markdown file")
		groundURL   = flag.String("ground-url", "", "add a 4th \"grounded\" arm: LLM answer-grounding filter on the injected context via an OpenAI-compatible URL")
		groundCmd   = flag.String("ground-cmd", "", "add a 4th \"grounded\" arm via a CLI agent grounder (e.g. \"claude -p\")")
		groundMod   = flag.String("ground-model", "qwen2.5:7b-instruct", "grounding model name (for -ground-url)")
		groundKey   = flag.String("ground-key", "", "grounding model API key (for -ground-url)")
		groundTopK  = flag.Int("ground-topk", 5, "ground only the top-K recalled passages per question")
		injectFmt   = flag.String("inject-format", "bare", "injected context presentation: bare | labeled | scored (scored = live proxy format)")
		answerHintF = flag.String("answer-hint", "", "constrain answers to a fixed label set (e.g. \"SUPPORTS, REFUTES\") for classification regimes like FEVER; empty = extractive span")
	)
	flag.Parse()
	if err := config.ValidateURL("MuninnDB URL", *mcpURL); err != nil {
		return err
	}
	switch *dataset {
	case "squad", "hotpot", "generic":
	default:
		return fmt.Errorf("invalid -dataset %q: must be one of squad, hotpot, generic", *dataset)
	}
	if *n <= 0 {
		return fmt.Errorf("invalid -n %d: must be positive", *n)
	}
	for opt, raw := range map[string]string{"-model-url": *modelURL, "-ground-url": *groundURL} {
		if err := config.ValidateURL(opt, raw); err != nil {
			return err
		}
	}
	// The gate is compared against an embedding cosine in [0,1]. A value
	// outside that range, or NaN, injects everything or nothing and the scores
	// below describe a run nobody asked for. Written as a positive range check
	// so NaN is rejected too.
	if !(*minScore > 0 && *minScore <= 1) {
		return fmt.Errorf("invalid -min-score %v: must be in (0,1]", *minScore)
	}
	if *modelKey == "" {
		*modelKey = os.Getenv("OPENAI_API_KEY")
		// The env fallback is an OpenAI key, but -model-url may point anywhere.
		// Say so before it is sent to a third-party endpoint.
		if *modelKey != "" && *modelURL != "" {
			if u, err := url.Parse(*modelURL); err == nil && u.Hostname() != "" &&
				!config.IsOpenAIHost(u.Hostname()) && !config.IsLoopbackHost(u.Hostname()) {
				fmt.Fprintf(os.Stderr, "warning: sending OPENAI_API_KEY to non-OpenAI model endpoint %s; pass -model-key to override\n", u.Hostname())
			}
		}
	}
	answerHint = *answerHintF

	questions, err := loadDataset(*dataset, *squadFile, *n, *sampleSeed)
	if err != nil {
		return err
	}
	datasetSHA, err := fileSHA256(*squadFile)
	if err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "loaded %d %s questions\n", len(questions), *dataset)

	mcp := mcpclient.New(*mcpURL, resolveToken(*token), *timeout)
	ctx := context.Background()

	// Optional answer-grounding rerank: a 4th "grounded" arm filters the injected
	// passages by an LLM "does this answer the query?" judgment (model-independent,
	// so computed once with the injected context). Tests whether grounding helps
	// downstream where wrong-paragraph injects hurt (e.g. HotpotQA, experiments §B).
	grd := grounding.New(*groundCmd, *groundURL, *groundMod, *groundKey, *timeout)

	// Precompute recall context per question ONCE — it is model-independent, so
	// all models reuse it (recall is the expensive MCP path; only model calls
	// repeat per model).
	type prepared struct {
		q                            qaItem
		injected, distract, grounded string
	}
	prep := make([]prepared, len(questions))
	// Recall ungated (threshold 0) once per question; the gate is applied
	// in-process below. The ungated recall doubles as the distractor pool: each
	// question's distractor is the recall for the question shifted by n/2, so it
	// is sampled per question, vault-matched, answers the wrong question, and is
	// non-empty whenever the vault holds anything. Like the old fixed-query
	// distractor it bypasses the gate: a gated distractor would degenerate to
	// "none". A single question has no other question to borrow from (the shift
	// would wrap to itself, handing the arm the correct context), so that arm is
	// left empty with a warning.
	ungated := make([][]cand, len(questions))
	for i, q := range questions {
		ungated[i] = recallStructured(ctx, mcp, *vault, q.Question, 0, *multiRecall)
	}
	var coverage, distEmpty, distGold, groundCalls, groundPassages int
	for i, q := range questions {
		var scands []cand
		for _, c := range ungated[i] {
			if c.Score >= *minScore { // the gate
				scands = append(scands, c)
			}
		}
		inj := formatInjected(scands, *injectFmt)
		contents := make([]string, len(scands))
		for j, c := range scands {
			contents[j] = c.Content
		}
		dis := ""
		if len(questions) > 1 {
			dis = formatInjected(ungated[(i+len(questions)/2)%len(questions)], *injectFmt)
			switch {
			case dis == "":
				distEmpty++
			case containsAnswer(dis, q.Answers):
				// The shifted question's recall can hit the same article and
				// carry this question's gold answer; count it so contaminated
				// runs are visible instead of silently understating Δdist.
				distGold++
			}
		}
		grounded := ""
		if grd != nil && len(scands) > 0 {
			groundCalls++ // one listwise judge call per question
			groundPassages += min(*groundTopK, len(scands))
			grounded = strings.Join(grounding.Filter(ctx, grd, q.Question, contents, *groundTopK), "\n")
		}
		prep[i] = prepared{q: q, injected: inj, distract: dis, grounded: grounded}
		if containsAnswer(inj, q.Answers) {
			coverage++
		}
	}
	fmt.Fprintf(os.Stderr, "recall context contained the gold answer for %d/%d (%.0f%%)\n",
		coverage, len(questions), 100*float64(coverage)/float64(max(1, len(questions))))
	if len(questions) == 1 {
		fmt.Fprintln(os.Stderr, "warn: only 1 question loaded; the distractor arm needs another question's recall, so it degenerates to \"none\"")
	} else if distEmpty > 0 {
		fmt.Fprintf(os.Stderr, "warn: empty distractor context for %d/%d questions (vault empty?); that arm degenerates to \"none\" there\n",
			distEmpty, len(questions))
	}
	if distGold > 0 {
		fmt.Fprintf(os.Stderr, "warn: distractor context contains the gold answer for %d/%d questions (shifted question overlaps the same article); Δdist is biased toward 0 there\n",
			distGold, len(questions))
	}
	if grd != nil {
		fmt.Fprintf(os.Stderr, "grounding arm enabled via %s (%d listwise calls judging ~%d passages)\n", grd.Label(), groundCalls, groundPassages)
	}

	// Assemble reader backends: OpenAI-compatible HTTP models (-model-url) and/or
	// CLI agents (-model-cmd). Either source can be empty.
	var readers []answerer
	if *modelURL != "" {
		for _, m := range splitCSV(*model) {
			readers = append(readers, &modelClient{baseURL: strings.TrimRight(*modelURL, "/"), key: *modelKey, model: m, timeout: *timeout, maxTokens: *maxTokens})
		}
	}
	for _, cmd := range splitCSV(*modelCmd) {
		if argv := strings.Fields(cmd); len(argv) > 0 {
			readers = append(readers, &cliClient{name: cmd, argv: argv, timeout: *timeout})
		}
	}

	labels := make([]string, len(readers))
	for i, r := range readers {
		labels[i] = r.label()
	}
	manifest := reproManifest(flag.CommandLine, datasetSHA, len(questions), labels)
	fmt.Fprintln(os.Stderr, manifest)

	if len(readers) == 0 {
		fmt.Printf("BUILD-ONLY (no -model-url / -model-cmd): answer-coverage %d/%d. Supply a reader to score arms.\n", coverage, len(questions))
		return nil
	}
	// Results rows are buffered and written once at the end (see writeMDBlock):
	// a run that dies mid-way leaves the file untouched instead of a truncated
	// block, and a rerun replaces its own block instead of duplicating it.
	var mdRows []string

	// Arms are dynamic: the grounded arm appears only when a grounder is set.
	armNames := []string{"none", "injected", "distractor"}
	if grd != nil {
		armNames = append(armNames, "grounded")
	}
	fmt.Printf("\n=== msc-qa: downstream answer quality (%d questions) ===\n", len(questions))
	header := fmt.Sprintf("%-26s %12s %12s %12s", "model", "none EM/F1", "inj EM/F1", "dist EM/F1")
	if grd != nil {
		header += fmt.Sprintf(" %12s", "grnd EM/F1")
	}
	fmt.Printf("%s   %s\n", header, "Δinj F1 [95% CI]  Δdist F1 [95% CI]")
	for _, r := range readers {
		agg := make([]armAgg, len(armNames))
		for i, p := range prep {
			ctxBlocks := []string{"", p.injected, p.distract}
			if grd != nil {
				ctxBlocks = append(ctxBlocks, p.grounded)
			}
			for a := range armNames {
				ans, err := r.answer(ctx, p.q.Question, ctxBlocks[a])
				if err != nil {
					// A failed call is excluded, not scored as "": empty answers
					// would deflate the arm (injected prompts are the longest, so
					// they time out most, biasing against injection).
					fmt.Fprintf(os.Stderr, "  warn: %s q%d arm %s: %v\n", r.label(), i, armNames[a], err)
					agg[a].fail()
					continue
				}
				agg[a].add(exactMatch(ans, p.q.Answers), tokenF1(ans, p.q.Answers), containsAnswer(ans, p.q.Answers))
			}
		}
		for a, name := range armNames {
			if k := agg[a].failN(); k > 0 {
				fmt.Fprintf(os.Stderr, "  arm %s: %d/%d calls failed (excluded)\n", name, k, len(agg[a].ok))
			}
		}
		note := ""
		if unreliable(agg) {
			note = "   UNRELIABLE (an arm lost >10% of calls)"
		}
		row := fmt.Sprintf("%-26s  %4.2f/%4.2f   %4.2f/%4.2f   %4.2f/%4.2f",
			trunc(r.label(), 26), agg[0].em(), agg[0].f1(), agg[1].em(), agg[1].f1(), agg[2].em(), agg[2].f1())
		if grd != nil {
			row += fmt.Sprintf("   %4.2f/%4.2f", agg[3].em(), agg[3].f1())
		}
		fmt.Printf("%s   %s  %s%s\n", row, deltaCI(&agg[0], &agg[1]), deltaCI(&agg[0], &agg[2]), note)
		fmt.Printf("    loose answer containment: %s\n", containmentLine(agg, armNames))
		if grd != nil {
			fmt.Printf("    Δgrounded F1 = %+.2f (vs none), %+.2f (vs injected)\n", agg[3].f1()-agg[0].f1(), agg[3].f1()-agg[1].f1())
		}
		if *mdFile != "" {
			mdRows = append(mdRows, mdRow(r.label(), len(questions), [3]armAgg{agg[0], agg[1], agg[2]}))
		}
	}
	if *mdFile != "" {
		if err := writeMDBlock(*mdFile, manifest, mdRows); err != nil {
			return fmt.Errorf("write -md block: %w", err)
		}
	}
	return nil
}

func splitCSV(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func trunc(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n-1] + "…"
}

// mdRow formats one results row for a model.
func mdRow(model string, n int, agg [3]armAgg) string {
	note := ""
	if unreliable(agg[:]) {
		note = " <!-- unreliable: an arm lost >10% of calls -->"
	}
	return fmt.Sprintf("| %s | %d | %.2f/%.2f | %.2f/%.2f | %.2f/%.2f | %s | %s |%s\n",
		model, n, agg[0].em(), agg[0].f1(), agg[1].em(), agg[1].f1(), agg[2].em(), agg[2].f1(),
		deltaCI(&agg[0], &agg[1]), deltaCI(&agg[0], &agg[2]), note)
}

// mdManifestPrefix opens the provenance comment that starts each run's block
// in a -md results file.
const mdManifestPrefix = "<!-- msc-qa repro:"

// writeMDBlock records one run's results in the -md file. The file is a stream
// of blocks, each opened by its manifest comment. A rerun with the same
// configuration derives the same manifest, so its block is replaced in place
// rather than appended: the file converges to one row per configuration no
// matter how many times the run is repeated. A new configuration appends a new
// block, keeping the history of distinct runs.
func writeMDBlock(path, manifest string, rows []string) error {
	marker := mdManifestPrefix + " " + manifest + " -->"
	var b strings.Builder
	b.WriteString(marker + "\n")
	for _, r := range rows {
		b.WriteString(r)
	}
	block := b.String()

	existing, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	out := replaceMDBlock(string(existing), marker, block)
	return os.WriteFile(path, []byte(out), 0o644)
}

// replaceMDBlock swaps the block opened by marker for block, dropping whatever
// followed that block up to the next manifest comment (or end of file). When
// marker is absent, the block is appended. Returns the full new file contents.
func replaceMDBlock(content, marker, block string) string {
	lines := strings.SplitAfter(content, "\n")
	start := -1
	for i, l := range lines {
		if strings.TrimRight(l, "\n") == marker {
			start = i
			break
		}
	}
	if start < 0 {
		if content != "" && !strings.HasSuffix(content, "\n") {
			content += "\n"
		}
		return content + block
	}
	end := len(lines)
	for i := start + 1; i < len(lines); i++ {
		if strings.HasPrefix(lines[i], mdManifestPrefix) {
			end = i
			break
		}
	}
	// Keep any trailing blank line that separated this block from the next so
	// the file does not gain blank lines on each rerun.
	tail := ""
	if end < len(lines) && strings.TrimSpace(lines[end-1]) == "" {
		tail = lines[end-1]
	}
	var b strings.Builder
	b.WriteString(strings.Join(lines[:start], ""))
	b.WriteString(block)
	b.WriteString(tail)
	b.WriteString(strings.Join(lines[end:], ""))
	return b.String()
}

// fileSHA256 returns the hex SHA-256 of a file's contents.
func fileSHA256(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%x", sha256.Sum256(data)), nil
}

// reproManifest is a one-line provenance record for a run: every flag's
// effective value (secrets redacted), the dataset digest, the sampled question
// count, and the reader list. It is printed to stderr and written into -md
// output so any results row can be traced to its exact configuration.
func reproManifest(fs *flag.FlagSet, datasetSHA string, nQuestions int, readers []string) string {
	var b strings.Builder
	b.WriteString("msc-qa repro:")
	fs.VisitAll(func(f *flag.Flag) {
		v := f.Value.String()
		switch f.Name {
		case "token", "model-key", "ground-key":
			if v != "" {
				v = "<redacted>"
			}
		}
		fmt.Fprintf(&b, " -%s=%q", f.Name, v)
	})
	fmt.Fprintf(&b, " dataset-sha256=%s questions=%d readers=%q", datasetSHA, nQuestions, strings.Join(readers, ","))
	return b.String()
}

// armAgg accumulates per-question scores for one arm. Scores are kept as
// parallel slices, not running sums, so paired bootstrap CIs can resample
// question indices after the fact; failed model calls keep their index for
// pairing (ok=false) but are excluded from every aggregate.
type armAgg struct {
	ems, f1s  []float64
	ok        []bool
	contained int
}

func (a *armAgg) add(em, f1 float64, contains bool) {
	a.ems = append(a.ems, em)
	a.f1s = append(a.f1s, f1)
	a.ok = append(a.ok, true)
	if contains {
		a.contained++
	}
}

// fail records a failed model call for the next question index.
func (a *armAgg) fail() {
	a.ems = append(a.ems, 0)
	a.f1s = append(a.f1s, 0)
	a.ok = append(a.ok, false)
}

func (a *armAgg) n() int {
	n := 0
	for _, k := range a.ok {
		if k {
			n++
		}
	}
	return n
}
func (a *armAgg) failN() int  { return len(a.ok) - a.n() }
func (a *armAgg) em() float64 { return safe(sumWhere(a.ems, a.ok), float64(a.n())) }
func (a *armAgg) f1() float64 { return safe(sumWhere(a.f1s, a.ok), float64(a.n())) }

// containment is the fraction of answers that contain a gold answer as a
// contiguous token run. It is looser than exactMatch (which requires the whole
// normalized answer), so it shows how often the model got the fact right inside
// a longer sentence — the signal that separates "wrong" from "right but verbose".
func (a *armAgg) containment() float64 { return safe(float64(a.contained), float64(a.n())) }

// containmentLine renders the per-arm containment rates for one model's row.
func containmentLine(agg []armAgg, armNames []string) string {
	parts := make([]string, 0, len(agg))
	for a, name := range armNames {
		parts = append(parts, fmt.Sprintf("%s %.2f", name, agg[a].containment()))
	}
	return strings.Join(parts, "  ")
}

func sumWhere(xs []float64, ok []bool) float64 {
	s := 0.0
	for i, x := range xs {
		if ok[i] {
			s += x
		}
	}
	return s
}

func safe(x, y float64) float64 {
	if y == 0 {
		return 0
	}
	return x / y
}

// Paired bootstrap over per-question F1 differences; draws and seed are fixed
// so repeated runs report identical intervals.
const (
	bootstrapDraws = 2000
	bootstrapSeed  = 1
)

// pairedF1Deltas returns arm-minus-base per-question F1 differences over the
// questions where both arms' model calls succeeded.
func pairedF1Deltas(base, arm *armAgg) []float64 {
	var d []float64
	for i := 0; i < min(len(base.f1s), len(arm.f1s)); i++ {
		if base.ok[i] && arm.ok[i] {
			d = append(d, arm.f1s[i]-base.f1s[i])
		}
	}
	return d
}

// bootstrapCI returns the 95% percentile interval for the mean of deltas by
// resampling indices with replacement.
func bootstrapCI(deltas []float64, draws int, seed int64) (lo, hi float64) {
	if len(deltas) == 0 {
		return 0, 0
	}
	rng := rand.New(rand.NewSource(seed))
	means := make([]float64, draws)
	for d := range means {
		s := 0.0
		for range deltas {
			s += deltas[rng.Intn(len(deltas))]
		}
		means[d] = s / float64(len(deltas))
	}
	sort.Float64s(means)
	return means[int(0.025*float64(draws))], means[int(0.975*float64(draws))]
}

// deltaCI formats the mean paired F1 delta of arm vs base with its 95%
// bootstrap CI, e.g. "+0.47 [+0.30, +0.62]".
func deltaCI(base, arm *armAgg) string {
	d := pairedF1Deltas(base, arm)
	s := 0.0
	for _, x := range d {
		s += x
	}
	lo, hi := bootstrapCI(d, bootstrapDraws, bootstrapSeed)
	return fmt.Sprintf("%+.2f [%+.2f, %+.2f]", safe(s, float64(len(d))), lo, hi)
}

// unreliable reports whether any arm lost more than 10% of its model calls;
// the excluded failures could bias that row's comparison.
func unreliable(aggs []armAgg) bool {
	for _, a := range aggs {
		if total := len(a.ok); total > 0 && a.failN()*10 > total {
			return true
		}
	}
	return false
}

// --- recall (mirrors the proxy's gated selection: cosine >= minScore) ---

// splitQueryQA decomposes a query into sub-queries (full + capitalized entity
// spans) for transparent multi-recall — no LLM call.
func splitQueryQA(q string) []string {
	subs := []string{q}
	seen := map[string]bool{q: true}
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
	for _, w := range strings.Fields(q) {
		// unicode.IsUpper, not an 'A'..'Z' range test: a German noun, a Turkish
		// or Cyrillic proper noun, and any CJK entity carry no ASCII uppercase,
		// so the range test found no sub-queries for those questions.
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

// cand is a gated recall candidate with the metadata the production injection
// format carries (concept + effective cosine), so the harness can reproduce the
// real `[concept] (relevance: X.XX)\ncontent` block, not just bare content.
type cand struct {
	Concept string
	Content string
	Score   float64 // effective cosine used as the relevance shown to the reader
}

func recallContext(ctx context.Context, mcp *mcpclient.Client, vault, query string, minScore float64, multi bool) string {
	return strings.Join(recallCandidates(ctx, mcp, vault, query, minScore, multi), "\n")
}

// recallCandidates returns the gated recall passages' content in recall order
// (bare content — used for grounding and the distractor arm).
func recallCandidates(ctx context.Context, mcp *mcpclient.Client, vault, query string, minScore float64, multi bool) []string {
	cands := recallStructured(ctx, mcp, vault, query, minScore, multi)
	out := make([]string, len(cands))
	for i, c := range cands {
		out[i] = c.Content
	}
	return out
}

// recallStructured returns the gated recall candidates (cosine >= minScore) with
// concept and relevance, in MuninnDB's return order (already score-ranked; the
// multi-query path concatenates per-sub-query results, not a global ranking).
func recallStructured(ctx context.Context, mcp *mcpclient.Client, vault, query string, minScore float64, multi bool) []cand {
	if multi {
		// Dedup by content, keeping the best score across sub-queries: a memory
		// scoring low vs the full question but high vs an entity sub-query must
		// carry the high score, or a downstream gate would wrongly reject it.
		seen := map[string]int{}
		var parts []cand
		for _, sub := range splitQueryQA(query) {
			for _, c := range recallStructured(ctx, mcp, vault, sub, minScore, false) {
				if c.Content == "" {
					continue
				}
				if j, ok := seen[c.Content]; ok {
					if c.Score > parts[j].Score {
						parts[j] = c
					}
					continue
				}
				seen[c.Content] = len(parts)
				parts = append(parts, c)
			}
		}
		return parts
	}
	resp, err := mcp.Call(ctx, "muninn_recall", map[string]any{
		"vault": vault, "context": []string{query}, "limit": 5, "threshold": 0.05, "mode": "semantic",
	})
	if err != nil {
		return nil
	}
	var rpc struct {
		Result struct {
			Content []struct {
				Type, Text string
			} `json:"content"`
		} `json:"result"`
	}
	if json.Unmarshal(resp, &rpc) != nil {
		return nil
	}
	for _, c := range rpc.Result.Content {
		if c.Type != "text" {
			continue
		}
		var inner struct {
			Memories []struct {
				Concept     string  `json:"concept"`
				Content     string  `json:"content"`
				VectorScore float64 `json:"vector_score"`
				Score       float64 `json:"score"`
			} `json:"memories"`
		}
		if json.Unmarshal([]byte(c.Text), &inner) != nil {
			return nil
		}
		var parts []cand
		for _, m := range inner.Memories {
			rel := m.VectorScore
			if rel == 0 {
				rel = m.Score
			}
			if rel >= minScore { // the gate
				parts = append(parts, cand{Concept: m.Concept, Content: m.Content, Score: rel})
			}
		}
		return parts
	}
	return nil
}

// formatInjected renders gated candidates into the context body for a given
// presentation mode: "bare" joins raw content; "scored" reproduces the live
// proxy's per-entry format ("[concept] (relevance: X.XX)\ncontent"); "labeled"
// keeps the concept header but drops the relevance number.
func formatInjected(cands []cand, mode string) string {
	if len(cands) == 0 {
		return ""
	}
	switch mode {
	case "scored", "labeled":
		var sb strings.Builder
		for i, c := range cands {
			if i > 0 {
				sb.WriteString("\n\n")
			}
			sb.WriteString("[" + c.Concept + "]")
			if mode == "scored" {
				sb.WriteString(" (relevance: " + strconv.FormatFloat(c.Score, 'f', 2, 64) + ")")
			}
			sb.WriteString("\n" + c.Content)
		}
		return sb.String()
	default: // "bare"
		parts := make([]string, len(cands))
		for i, c := range cands {
			parts[i] = c.Content
		}
		return strings.Join(parts, "\n")
	}
}

// answerer is a reader backend: given a question and an optional injected
// context block, it returns a short-span answer. Both the OpenAI HTTP client and
// the CLI-agent client satisfy it, so the eval loop is backend-agnostic.
type answerer interface {
	answer(ctx context.Context, question, contextBlock string) (string, error)
	label() string
}

// --- model client (OpenAI-compatible chat completions) ---

// modelSeed pins the sampler (ollama's "seed" option) alongside temperature 0
// so repeated runs decode identically.
const modelSeed = 1

type modelClient struct {
	baseURL, key, model string
	timeout             time.Duration
	maxTokens           int
}

func (m *modelClient) label() string { return m.model }

// answer queries the model. When contextBlock is non-empty it is injected as a
// SECOND system message wrapped in the real <retrieved-context> markers —
// exactly how the proxy's injectOpenAIContext enriches an OpenAI request — so the
// eval measures the production injection path, not an ad-hoc user-prefix.
func (m *modelClient) answer(ctx context.Context, question, contextBlock string) (string, error) {
	msgs := []map[string]string{
		{"role": "system", "content": answerInstruction()},
	}
	if contextBlock != "" {
		msgs = append(msgs, map[string]string{
			"role":    "system",
			"content": apiformat.ContextPrefix + "\n" + contextBlock + "\n" + apiformat.ContextSuffix,
		})
	}
	msgs = append(msgs, map[string]string{"role": "user", "content": question})
	body, _ := json.Marshal(map[string]any{
		"model":       m.model,
		"messages":    msgs,
		"temperature": 0,
		"seed":        modelSeed,
		"max_tokens":  m.maxTokens,
	})
	req, err := newReq(ctx, m.baseURL+"/chat/completions", m.key, body)
	if err != nil {
		return "", err
	}
	var out struct {
		Choices []struct {
			Message struct{ Content string } `json:"message"`
		} `json:"choices"`
	}
	if err := doJSON(req, m.timeout, &out); err != nil {
		return "", err
	}
	if len(out.Choices) == 0 {
		return "", nil
	}
	return out.Choices[0].Message.Content, nil
}

// --- CLI agent client (claude -p / codex exec / grok -p / any command) ---

type cliClient struct {
	name    string   // display label, e.g. "claude -p"
	argv    []string // command + flags; the prompt is delivered on stdin
	timeout time.Duration
}

func (c *cliClient) label() string { return c.name }

// answer runs the CLI with a single combined prompt (system instruction +
// optional <retrieved-context> block + question), then returns the last
// non-empty stdout line. Agent CLIs print a usage footer or streaming chatter;
// the final span answer is reliably on the last content line, so we take that.
//
// The prompt goes on stdin, never argv: it carries the question and the
// recalled memory block, and /proc/<pid>/cmdline exposes argv to every user on
// the host for the life of the call. Same reasoning as the CLI grounder in
// internal/grounding.
func (c *cliClient) answer(ctx context.Context, question, contextBlock string) (string, error) {
	cctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	prompt := buildCLIPrompt(question, contextBlock)
	cmd := exec.CommandContext(cctx, c.argv[0], c.argv[1:]...)
	cmd.Stdin = strings.NewReader(prompt)
	var stdout bytes.Buffer
	cmd.Stdout = &stdout
	if err := cmd.Run(); err != nil {
		// A non-zero exit can still leave a usable answer on stdout (some agents
		// exit non-zero on warnings); prefer any captured line over the error.
		if line := lastNonEmptyLine(stdout.String()); line != "" {
			return line, nil
		}
		return "", err
	}
	return lastNonEmptyLine(stdout.String()), nil
}

// buildCLIPrompt flattens the chat arms into one prompt string for single-shot
// CLI agents, mirroring the HTTP path: the same instruction, the same
// <retrieved-context> markers, then the question.
func buildCLIPrompt(question, contextBlock string) string {
	var sb strings.Builder
	sb.WriteString(answerInstruction() + " Output only the answer, nothing else.\n")
	if contextBlock != "" {
		sb.WriteString("\n")
		sb.WriteString(apiformat.ContextPrefix + "\n" + contextBlock + "\n" + apiformat.ContextSuffix)
		sb.WriteString("\n")
	}
	sb.WriteString("\nQuestion: " + question + "\nAnswer:")
	return sb.String()
}

// answerHint, when set via -answer-hint, constrains the answer to a fixed label
// set (e.g. "SUPPORTS, REFUTES" for claim verification). The default extractive
// "shortest span" instruction does not elicit label tokens, so classification
// regimes like FEVER score 0 spuriously without it.
var answerHint string

func answerInstruction() string {
	if answerHint != "" {
		return "Answer with exactly one of: " + answerHint + ". Output only that label."
	}
	return "Answer with the shortest exact span that answers the question. If unknown, reply 'unknown'."
}

// lastNonEmptyLine returns the final non-blank line of s, trimmed.
func lastNonEmptyLine(s string) string {
	lines := strings.Split(s, "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		if t := strings.TrimSpace(lines[i]); t != "" {
			return t
		}
	}
	return ""
}

// --- SQuAD QA loader ---

type qaItem struct {
	Question string
	Answers  []string
}

func loadSquadQA(path string, n int) ([]qaItem, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read squad: %w", err)
	}
	var sq struct {
		Data []struct {
			Paragraphs []struct {
				QAs []struct {
					Question     string `json:"question"`
					IsImpossible bool   `json:"is_impossible"`
					Answers      []struct {
						Text string `json:"text"`
					} `json:"answers"`
				} `json:"qas"`
			} `json:"paragraphs"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &sq); err != nil {
		return nil, fmt.Errorf("parse squad: %w", err)
	}
	var out []qaItem
	for _, art := range sq.Data {
		for _, p := range art.Paragraphs {
			for _, qa := range p.QAs {
				if qa.IsImpossible || len(qa.Answers) == 0 {
					continue
				}
				golds := make([]string, 0, len(qa.Answers))
				for _, a := range qa.Answers {
					golds = append(golds, a.Text)
				}
				out = append(out, qaItem{Question: qa.Question, Answers: golds})
				if len(out) >= n {
					return out, nil
				}
			}
		}
	}
	return out, nil
}

// loadFlatQA reads a flat [{question, answer}] JSON, keeping the first n
// questions that carry both. label names the source in the error text. Callers
// pass the official HotpotQA array format ({question, answer, context,
// supporting_facts}, multi-hop questions with a single gold answer including
// yes/no, as seeded by `msc-bench -corpus hotpot`) or the dump format
// `msc-bench -dump-qa` produces for an arbitrary seeded vault.
func loadFlatQA(path, label string, n int) ([]qaItem, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", label, err)
	}
	var data []struct {
		Question string `json:"question"`
		Answer   string `json:"answer"`
	}
	if err := json.Unmarshal(raw, &data); err != nil {
		return nil, fmt.Errorf("parse %s: %w", label, err)
	}
	var out []qaItem
	for _, d := range data {
		if d.Question == "" || d.Answer == "" {
			continue
		}
		out = append(out, qaItem{Question: d.Question, Answers: []string{d.Answer}})
		if len(out) >= n {
			break
		}
	}
	return out, nil
}

// loadDataset loads every eligible question, shuffles deterministically with
// seed, then truncates to n. Datasets are grouped in file order (SQuAD by
// article), so taking the first n unshuffled would cover only 1-2 articles.
func loadDataset(dataset, path string, n int, seed int64) ([]qaItem, error) {
	var qs []qaItem
	var err error
	switch dataset {
	case "hotpot":
		qs, err = loadFlatQA(path, "hotpot", math.MaxInt)
	case "generic":
		qs, err = loadFlatQA(path, "generic qa", math.MaxInt)
	default:
		qs, err = loadSquadQA(path, math.MaxInt)
	}
	if err != nil {
		return nil, err
	}
	rng := rand.New(rand.NewSource(seed))
	rng.Shuffle(len(qs), func(i, j int) { qs[i], qs[j] = qs[j], qs[i] })
	if len(qs) > n {
		qs = qs[:n]
	}
	return qs, nil
}

func resolveToken(flagVal string) string { return config.Token(flagVal) }
