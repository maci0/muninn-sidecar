// Command msc-eval evaluates the memory-injection selection pipeline.
//
// Offline mode (default) runs labeled scenarios through the production
// selection logic and reports precision/recall/F1, nDCG, gate accuracy, and
// budget efficiency — deterministic, no MuninnDB needed. The -sweep flag charts
// those metrics across MinScore thresholds; -compare runs the cross-validated
// study comparing when+what methods on synthetic data, across a fixed set of
// generator seeds by default (mean ± std of held-out F1 per method) or on a
// single seed via -study-seed.
//
// Live mode (-live) seeds a real MuninnDB vault and exercises the full
// recall + selection path, reporting how many expected concepts were injected.
//
//	msc-eval                          # offline report on the built-in corpus
//	msc-eval -sweep                   # + MinScore when+what sweep
//	msc-eval -compare                 # + cross-validated method study (multi-seed)
//	msc-eval -compare -study-seed 7   # + method study on one generator seed
//	msc-eval -file scenarios.json     # offline report on a custom corpus
//	msc-eval -json                    # machine-readable output
//	msc-eval -live -live-file live.json -vault msc-eval
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/maci0/muninn-sidecar/internal/config"
	"github.com/maci0/muninn-sidecar/internal/inject"
	"github.com/maci0/muninn-sidecar/internal/report"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "msc-eval:", err)
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
		file       = flag.String("file", "", "offline scenario JSON file (default: built-in corpus)")
		minScore   = flag.Float64("min-score", 0, fmt.Sprintf("override injection threshold for all scenarios (0 = per-scenario/default %v)", inject.DefaultMinScore))
		budget     = flag.Int("budget", 0, "override token budget for all scenarios (0 = per-scenario/default 2048)")
		sweep      = flag.Bool("sweep", false, "also run a MinScore (when+what) sweep and print the tradeoff table")
		compare    = flag.Bool("compare", false, "run the cross-validated method study (compares when+what strategies on synthetic data)")
		studySeed  = flag.Int64("study-seed", 0, "single generator seed for the -compare study (0 = run the fixed multi-seed set and report cross-seed variance)")
		studyN     = flag.Int("study-n", 600, "synthetic scenarios per seed for the -compare study")
		studyFolds = flag.Int("study-folds", 5, "cross-validation folds for the -compare study")
		asJSON     = flag.Bool("json", false, "emit machine-readable JSON instead of a table")
		live       = flag.Bool("live", false, "run live end-to-end evaluation against a real MuninnDB")
		liveFile   = flag.String("live-file", "", "live scenario JSON file (required with -live)")
		mcpURL     = flag.String("mcp-url", config.MCPURL(""), "MuninnDB MCP endpoint (live mode)")
		token      = flag.String("token", "", "MuninnDB bearer token (live mode; default $MUNINN_TOKEN_FILE, else ~/.muninn/mcp.token)")
		vault      = flag.String("vault", "msc-eval", "vault to seed/probe (live mode)")
		settle     = flag.Duration("settle", 750*time.Millisecond, "delay after seeding before probing (live mode)")
		timeout    = flag.Duration("timeout", 5*time.Second, "per-MCP-call timeout (live mode)")
	)
	parseFlags()

	if *compare && (*studyN <= 0 || *studyFolds < 2 || *studyFolds > *studyN) {
		return config.Usagef("-study-n must be > 0 and 2 <= -study-folds <= -study-n")
	}
	// 0 keeps the per-scenario threshold; anything else overrides it and is
	// compared against an embedding cosine in [0,1]. Out-of-range or NaN values
	// would inject everything or nothing and silently misreport the scenario
	// score, so reject them. Positive range check, so NaN is caught too.
	if *minScore != 0 && !(*minScore > 0 && *minScore <= 1) {
		return config.Usagef("invalid -min-score %v: must be 0 (per-scenario default) or in (0,1]", *minScore)
	}
	if *live {
		// The flag default is already the resolved endpoint, but an explicitly
		// written `-mcp-url=` leaves the flag empty, and the flag package does
		// not fall back to its default for that. Resolve again so an empty value
		// means "not configured" (env, then default) as it does everywhere else,
		// instead of an undialable empty endpoint.
		endpoint := config.MCPURL(*mcpURL)
		if err := config.ValidateURL("MuninnDB URL", endpoint); err != nil {
			return err
		}
		if w := config.ArgSecretWarning("-token", *token, "MUNINN_TOKEN"); w != "" {
			fmt.Fprintln(os.Stderr, "warning:", w)
		}
		return runLive(*liveFile, endpoint, config.Token(*token), *vault, *minScore, *budget, *settle, *timeout, *asJSON)
	}
	return runOffline(*file, *minScore, *budget, *sweep, *compare, *asJSON, studyOpts{seed: *studySeed, n: *studyN, folds: *studyFolds})
}

// studyOpts configures the -compare method study; seed 0 means "run the fixed
// multi-seed set" so seed-to-seed variation is reported, not hidden.
type studyOpts struct {
	seed  int64
	n     int
	folds int
}

// fixedStudySeeds are the distinct generator seeds a multi-seed -compare run
// uses, fixed so results stay reproducible across invocations.
var fixedStudySeeds = []int64{20240529, 42, 1337, 271828, 3141592}

func runOffline(file string, minScore float64, budget int, sweep, compare, asJSON bool, study studyOpts) error {
	scenarios, err := loadOfflineScenarios(file)
	if err != nil {
		return err
	}
	for i := range scenarios {
		if minScore > 0 {
			scenarios[i].MinScore = minScore
		}
		if budget > 0 {
			scenarios[i].Budget = budget
		}
	}

	results := make([]inject.EvalResult, len(scenarios))
	for i, s := range scenarios {
		results[i] = inject.RunScenario(s)
	}
	agg := inject.AggregateMetrics(results)

	var sweepPoints []inject.SweepPoint
	if sweep {
		sweepPoints = inject.SweepMinScore(scenarios, []float64{0.0, 0.40, 0.44, 0.46, 0.48, 0.50, 0.52, 0.55, 0.60})
	}
	var studyRep *inject.StudyReport
	var seedStudy *inject.SeedStudyReport
	if compare {
		if study.seed != 0 {
			s := inject.RunMethodStudy(study.seed, study.n, study.folds)
			studyRep = &s
		} else {
			s := inject.RunMethodStudySeeds(fixedStudySeeds, study.n, study.folds)
			seedStudy = &s
		}
	}

	if asJSON {
		return emitJSON(map[string]any{
			"results":    results,
			"aggregate":  agg,
			"sweep":      sweepPoints,
			"study":      studyRep,
			"seed_study": seedStudy,
		})
	}

	printOfflineReport(results, agg)
	if sweep {
		printSweep(sweepPoints)
	}
	if studyRep != nil {
		printStudy(*studyRep)
	}
	if seedStudy != nil {
		printSeedStudy(*seedStudy)
	}
	return nil
}

func runLive(liveFile, mcpURL, token, vault string, minScore float64, budget int, settle, timeout time.Duration, asJSON bool) error {
	if liveFile == "" {
		return config.Usagef("-live requires -live-file")
	}
	data, err := os.ReadFile(liveFile)
	if err != nil {
		return fmt.Errorf("read live scenarios: %w", err)
	}
	scenarios, err := inject.ParseLiveScenarios(data)
	if err != nil {
		return err
	}

	cfg := inject.Config{
		MCPURL:   mcpURL,
		Token:    token,
		Vault:    vault,
		Timeout:  timeout,
		MinScore: minScore,
		Budget:   budget,
	}
	fmt.Fprintf(os.Stderr, "live eval: seeding vault %q at %s\n", vault, mcpURL)

	results, err := inject.RunLive(context.Background(), cfg, scenarios, settle)
	if err != nil {
		// Print whatever completed before the failure, then surface the error.
		if len(results) > 0 && !asJSON {
			printLiveReport(results)
		}
		return err
	}

	if asJSON {
		return emitJSON(results)
	}
	printLiveReport(results)
	return nil
}

func loadOfflineScenarios(file string) ([]inject.EvalScenario, error) {
	if file == "" {
		return inject.DefaultScenarios()
	}
	data, err := os.ReadFile(file)
	if err != nil {
		return nil, fmt.Errorf("read scenarios: %w", err)
	}
	return inject.ParseScenarios(data)
}

func emitJSON(v any) error {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

func printOfflineReport(results []inject.EvalResult, agg inject.Metrics) {
	fmt.Println("\nMemory injection selection — offline evaluation")
	fmt.Printf("%-32s %5s %5s %5s %5s %5s %8s %7s\n", "scenario", "prec", "rec", "f1", "ndcg", "gate", "inj/rel", "wasted")
	fmt.Println(strings.Repeat("-", 84))
	for _, r := range results {
		m := r.Metrics
		fmt.Printf("%-32s %5.2f %5.2f %5.2f %5.2f %5s %4d/%-3d %6.0f%%\n",
			report.Trunc(r.Scenario, 32), m.Precision, m.Recall, m.F1, m.NDCG, gateMark(r), m.NumInjected, m.NumRelevant, m.WastedRatio*100)
	}
	fmt.Println(strings.Repeat("-", 84))
	fmt.Printf("%-32s %5.2f %5.2f %5.2f %5.2f %4.0f%% %4d/%-3d %6.0f%%\n",
		"AGGREGATE (macro avg)", agg.Precision, agg.Recall, agg.F1, agg.NDCG, agg.GateAccuracy*100, agg.NumInjected, agg.NumRelevant, agg.WastedRatio*100)
}

// gateMark shows whether the turn-level inject/suppress decision matched the
// gold label: ok, or the kind of mistake (FP = injected when it shouldn't,
// FN = suppressed when it should have injected).
func gateMark(r inject.EvalResult) string {
	if r.DidInject == r.ShouldInject {
		return "ok"
	}
	if r.DidInject {
		return "FP"
	}
	return "FN"
}

func printSweep(points []inject.SweepPoint) {
	fmt.Println("\nMinScore sweep — when+what to inject (aggregate over corpus)")
	fmt.Printf("%8s %6s %5s %5s %5s %7s   (0.00 = threshold off)\n", "minscore", "gate", "prec", "rec", "f1", "wasted")
	fmt.Println(strings.Repeat("-", 58))
	for _, p := range points {
		m := p.Metrics
		fmt.Printf("%8.2f %5.0f%% %5.2f %5.2f %5.2f %6.0f%%\n",
			p.MinScore, m.GateAccuracy*100, m.Precision, m.Recall, m.F1, m.WastedRatio*100)
	}
}

func printStudy(rep inject.StudyReport) {
	fmt.Printf("\nMethod study — %d synthetic scenarios, %d-fold cross-validation (seed %d)\n", rep.N, rep.K, rep.Seed)
	fmt.Printf("%-20s %9s %7s %6s %7s %8s\n", "method", "f1(test)", "±std", "gate", "wasted", "avg_inj")
	fmt.Println(strings.Repeat("-", 64))
	for _, m := range rep.Methods {
		fmt.Printf("%-20s %9.3f %7.3f %5.0f%% %6.0f%% %8.2f\n",
			m.Name, m.F1Mean, m.F1Std, m.GateAcc*100, m.Wasted*100, m.AvgInjected)
	}
	fmt.Printf("WINNER (highest held-out F1): %s\n", rep.Best)
}

func printSeedStudy(rep inject.SeedStudyReport) {
	fmt.Printf("\nMethod study across %d seeds %v: %d synthetic scenarios each, %d-fold cross-validation\n",
		len(rep.Seeds), rep.Seeds, rep.N, rep.K)
	fmt.Printf("%-20s %9s %7s   (mean ± std of held-out F1 across seeds)\n", "method", "f1(test)", "±std")
	fmt.Println(strings.Repeat("-", 38))
	for _, m := range rep.Methods {
		fmt.Printf("%-20s %9.3f %7.3f\n", m.Name, m.F1Mean, m.F1Std)
	}
	fmt.Printf("WINNER (highest mean held-out F1 across seeds): %s\n", rep.Best)
}

func printLiveReport(results []inject.LiveResult) {
	fmt.Println("\nMemory injection — live end-to-end evaluation")
	fmt.Printf("%-28s %8s %8s %6s %6s %8s\n", "scenario", "recalled", "expected", "hits", "extra", "hitrate")
	fmt.Println(strings.Repeat("-", 70))
	var sum float64
	for _, r := range results {
		fmt.Printf("%-28s %8d %8d %6d %6d %7.0f%%\n",
			report.Trunc(r.Scenario, 28), r.Recalled, len(r.Expected), r.Hits, r.Extra, r.HitRate*100)
		sum += r.HitRate
	}
	if len(results) > 0 {
		fmt.Println(strings.Repeat("-", 70))
		fmt.Printf("%-28s %8s %8s %6s %6s %7.0f%%\n", "MEAN", "", "", "", "", sum/float64(len(results))*100)
	}
}

// parseFlags sends -h to stdout and exits 0. A bad flag or an unexpected
// argument goes to stderr and exits 2, without the usage text.
func parseFlags() {
	flag.CommandLine.Init("msc-eval", flag.ContinueOnError)
	flag.CommandLine.SetOutput(os.Stderr)
	flag.CommandLine.Usage = func() {}
	if err := flag.CommandLine.Parse(os.Args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			usage(os.Stdout)
			os.Exit(0)
		}
		fmt.Fprintln(os.Stderr, "Run 'msc-eval -h' for usage.")
		os.Exit(2)
	}
	if flag.NArg() > 0 {
		fmt.Fprintf(os.Stderr, "msc-eval: unexpected argument %q\n", flag.Arg(0))
		fmt.Fprintln(os.Stderr, "Run 'msc-eval -h' for usage.")
		os.Exit(2)
	}
}

func usage(w io.Writer) {
	flag.CommandLine.SetOutput(w)
	fmt.Fprint(w, `msc-eval - offline and live evaluation of the memory-injection selection pipeline

Usage: msc-eval [flags]

Examples:
  msc-eval                          # offline report on the built-in corpus
  msc-eval -sweep                   # + MinScore when+what sweep
  msc-eval -compare                 # + cross-validated method study (multi-seed)
  msc-eval -compare -study-seed 7   # + method study on one generator seed
  msc-eval -file scenarios.json     # offline report on a custom corpus
  msc-eval -json                    # machine-readable output
  msc-eval -live -live-file live.json -vault msc-eval

Flags:
`)
	flag.PrintDefaults()
	flag.CommandLine.SetOutput(os.Stderr)
}
