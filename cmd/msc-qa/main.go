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
//
// Supporting files, one concern each: score.go (answer scoring), dataset.go
// (QA loaders), recall.go (gated MuninnDB recall), models.go (reader
// backends), stats.go (arm aggregation and bootstrap CIs), report.go (-md
// results blocks and run provenance).
package main

import (
	"context"
	"flag"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/maci0/muninn-sidecar/internal/clirun"
	"github.com/maci0/muninn-sidecar/internal/config"
	"github.com/maci0/muninn-sidecar/internal/grounding"
	"github.com/maci0/muninn-sidecar/internal/mcpclient"
)

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
		modelCmd    = flag.String("model-cmd", "", "comma-separated reader CLIs (e.g. \"claude -p,codex exec --skip-git-repo-check,grok -p\"); each reads the prompt on stdin, the last non-empty stdout line is the answer (quote a path containing spaces)")
		minScore    = flag.Float64("min-score", 0.6, "injection cosine threshold (the gate)")
		n           = flag.Int("n", 100, "number of questions to evaluate")
		sampleSeed  = flag.Int64("sample-seed", 1, "question-sampling shuffle seed: all eligible questions are shuffled deterministically then truncated to -n, so runs stay reproducible and paired across models")
		maxTokens   = flag.Int("max-tokens", 512, "model max_tokens (raise for thinking models)")
		multiRecall = flag.Bool("multi-recall", false, "split query into entity spans, recall each, merge (multi-hop)")
		timeout     = flag.Duration("timeout", 60*time.Second, "per-call timeout")
		mdFile      = flag.String("md", "", "append a results row per model to this markdown file")
		groundURL   = flag.String("ground-url", "", "add a 4th \"grounded\" arm: LLM answer-grounding filter on the injected context via an OpenAI-compatible URL")
		groundCmd   = flag.String("ground-cmd", "", "add a 4th \"grounded\" arm via a CLI agent grounder (e.g. \"claude -p\"); quote a path containing spaces")
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
	// formatInjected's default branch is a valid presentation, so a typo would
	// silently score the "bare" arm under the name of another one.
	if !slices.Contains([]string{"bare", "labeled", "scored"}, *injectFmt) {
		return fmt.Errorf("invalid -inject-format %q: must be one of bare, labeled, scored", *injectFmt)
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
	// A typo here selects formatInjected's default branch, so the injected arm
	// is compared against context formatted a way the run never asked for.
	if err := config.OneOf("-inject-format", *injectFmt, "bare", "labeled", "scored"); err != nil {
		return err
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

	for _, sec := range []struct{ flag, val, env string }{
		{"-token", *token, "MUNINN_TOKEN"},
		{"-model-key", *modelKey, "OPENAI_API_KEY"},
		{"-ground-key", *groundKey, "OPENAI_API_KEY"},
	} {
		if w := config.ArgSecretWarning(sec.flag, sec.val, sec.env); w != "" {
			fmt.Fprintln(os.Stderr, "warning:", w)
		}
	}

	questions, err := loadDataset(*dataset, *squadFile, *n, *sampleSeed)
	if err != nil {
		return err
	}
	// An empty or wrongly-shaped file would otherwise run every arm over zero
	// questions and print a table of zeros as a completed evaluation.
	if len(questions) == 0 {
		if *dataset == "squad" {
			return fmt.Errorf("no answerable questions in %s: -dataset squad expects the official SQuAD dev JSON (a top-level \"data\" array of articles)", *squadFile)
		}
		return fmt.Errorf("no answerable questions in %s: -dataset %s expects a flat JSON array of {question, answer} objects", *squadFile, *dataset)
	}
	datasetSHA, err := fileSHA256(*squadFile)
	if err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "loaded %d %s questions\n", len(questions), *dataset)

	mcp := mcpclient.New(*mcpURL, config.Token(*token), *timeout)
	defer mcp.Close()
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
	var recallFails int
	var firstRecallErr error
	for i, q := range questions {
		cands, err := recallStructuredErr(ctx, mcp, *vault, q.Question, 0, *multiRecall)
		if err != nil {
			recallFails++
			if firstRecallErr == nil {
				firstRecallErr = err
			}
		}
		ungated[i] = cands
	}
	// Every question's recall failing means MuninnDB was never reached, so the
	// injected and distractor arms below are both empty and the run would report
	// a zero delta for a reason that is not about injection at all.
	if recallFails == len(questions) {
		return fmt.Errorf("recall failed for all %d questions, so the injected and distractor arms would be empty: %w", recallFails, firstRecallErr)
	}
	if recallFails > 0 {
		fmt.Fprintf(os.Stderr, "warn: recall failed for %d/%d questions (excluded from the arms); first error: %v\n", recallFails, len(questions), firstRecallErr)
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
		if argv := clirun.SplitCommand(cmd); len(argv) > 0 {
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

func resolveToken(flagVal string) string { return config.Token(flagVal) }
