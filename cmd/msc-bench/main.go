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
//
// Supporting files, one concern each: dataset.go and friends (corpus
// generators), probe.go (seeding, recall, query construction), rank.go (gold
// ranking), analysis.go (retrieval and gate metrics), report.go (printing and
// -dump-qa).
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/maci0/muninn-sidecar/internal/config"
	"github.com/maci0/muninn-sidecar/internal/grounding"
	"github.com/maci0/muninn-sidecar/internal/mcpclient"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "msc-bench:", err)
		// 2 for a bad flag value, matching what the flag package already
		// returns for an unparseable command line, and what msc exits with.
		if config.IsUsageError(err) {
			os.Exit(2)
		}
		os.Exit(1)
	}
}

func run() error {
	var (
		mcpURL     = flag.String("mcp-url", config.MCPURL(""), "MuninnDB MCP endpoint")
		token      = flag.String("token", "", "bearer token (default $MUNINN_TOKEN_FILE, else ~/.muninn/mcp.token)")
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
		groundCmd  = flag.String("ground-cmd", "", "LLM answer-grounding rerank via a CLI agent (e.g. \"claude -p\"); drops recalled candidates the model says don't answer the query (quote a path containing spaces)")
		groundURL  = flag.String("ground-url", "", "LLM answer-grounding rerank via an OpenAI-compatible URL (e.g. http://127.0.0.1:11434/v1)")
		groundMod  = flag.String("ground-model", "qwen2.5:1.5b-instruct", "grounding model name (for -ground-url)")
		groundKey  = flag.String("ground-key", "", "grounding model API key (for -ground-url)")
		groundTopK = flag.Int("ground-topk", 5, "ground only the top-K candidates by cosine per probe (bounds model calls; must be positive)")
		groundTO   = flag.Duration("ground-timeout", 60*time.Second, "per grounding-call timeout")
		rewriteCmd = flag.String("rewrite-cmd", "", "LLM query rewrite/decomposition before recall via a CLI agent (e.g. \"claude -p\"); the prompt is delivered on stdin (quote a path containing spaces)")
		rewriteURL = flag.String("rewrite-url", "", "LLM query rewrite via an OpenAI-compatible URL")
		rewriteMod = flag.String("rewrite-model", grounding.DefaultModel, "rewrite model name (for -rewrite-url)")
		rewriteKey = flag.String("rewrite-key", "", "rewrite model API key (for -rewrite-url)")
		rewriteN   = flag.Int("rewrite-n", 4, "max sub-queries per probe, must be positive (including the original; each one costs a recall)")
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
	// The flag default is already the resolved endpoint, but an explicitly
	// written `-mcp-url=` leaves the flag empty, and the flag package does not
	// fall back to its default for that. Resolve again so an empty value means
	// "not configured" (env, then default) as it does everywhere else, instead
	// of an undialable empty endpoint.
	endpoint := config.MCPURL(*mcpURL)
	if err := config.ValidateURL("MuninnDB URL", endpoint); err != nil {
		return err
	}
	if err := config.OneOf("-corpus", *corpus, "homogeneous", "diverse", "facts", "squad", "hotpot", "agentmem"); err != nil {
		return err
	}
	if err := config.OneOf("-mode", *mode, "", "semantic", "recent", "balanced", "deep"); err != nil {
		return err
	}
	// The remaining enum flags select a code path in a switch deeper in the run
	// (chunk in the SQuAD generators, transform in transformQuery, rerank in
	// runProbes) where the default branch is itself a valid mode. A typo would
	// run the baseline configuration under another one's name, and the report
	// header echoes the value back, so nothing downstream notices.
	if err := config.OneOf("-chunk", *chunk, "paragraph", "sentence"); err != nil {
		return err
	}
	if err := config.OneOf("-query-transform", *qTransform, "none", "distractors", "emphasis", "repeat-last"); err != nil {
		return err
	}
	if err := config.OneOf("-rerank", *rerank, "none", "lexical"); err != nil {
		return err
	}
	for opt, raw := range map[string]string{"-ground-url": *groundURL, "-rewrite-url": *rewriteURL} {
		if err := config.ValidateURL(opt, raw); err != nil {
			return err
		}
	}
	// Both flags bound one model call: the judge grades every candidate it is
	// given in a single request, and the rewriter recalls each sub-query
	// separately. Zero is not "none" in either case, it is "all of them".
	for _, c := range []struct {
		opt string
		val int
	}{{"-ground-topk", *groundTopK}, {"-rewrite-n", *rewriteN}} {
		if c.val <= 0 {
			return config.Usagef("invalid %s %d: must be positive", c.opt, c.val)
		}
	}
	if !*seed && !*doProbe {
		*doProbe = true
	}

	for _, sec := range []struct{ flag, val, env string }{
		{"-token", *token, "MUNINN_TOKEN"},
		{"-ground-key", *groundKey, "OPENAI_API_KEY"},
		{"-rewrite-key", *rewriteKey, "OPENAI_API_KEY"},
	} {
		if w := config.ArgSecretWarning(sec.flag, sec.val, sec.env); w != "" {
			fmt.Fprintln(os.Stderr, "warning:", w)
		}
	}

	client := mcpclient.New(endpoint, config.Token(*token), *timeout)
	defer client.Close()
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
