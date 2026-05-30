// Command msc-eval evaluates the memory-injection selection pipeline.
//
// Offline mode (default) runs labeled scenarios through the production
// selection logic and reports precision/recall/F1, nDCG, gate accuracy, and
// budget efficiency — deterministic, no MuninnDB needed. The -sweep flag charts
// those metrics across MinScore thresholds; -compare runs the cross-validated
// study comparing when+what methods on synthetic data.
//
// Live mode (-live) seeds a real MuninnDB vault and exercises the full
// recall + selection path, reporting how many expected concepts were injected.
//
//	msc-eval                          # offline report on the built-in corpus
//	msc-eval -sweep                   # + MinScore when+what sweep
//	msc-eval -compare                 # + cross-validated method study
//	msc-eval -file scenarios.json     # offline report on a custom corpus
//	msc-eval -json                    # machine-readable output
//	msc-eval -live -live-file live.json -vault msc-eval
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/maci0/muninn-sidecar/internal/inject"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "msc-eval:", err)
		os.Exit(1)
	}
}

func run() error {
	var (
		file     = flag.String("file", "", "offline scenario JSON file (default: built-in corpus)")
		minScore = flag.Float64("min-score", 0, "override injection threshold for all scenarios (0 = per-scenario/default 0.5)")
		budget   = flag.Int("budget", 0, "override token budget for all scenarios (0 = per-scenario/default 2048)")
		sweep    = flag.Bool("sweep", false, "also run a MinScore (when+what) sweep and print the tradeoff table")
		compare  = flag.Bool("compare", false, "run the cross-validated method study (compares when+what strategies on synthetic data)")
		asJSON   = flag.Bool("json", false, "emit machine-readable JSON instead of a table")
		live     = flag.Bool("live", false, "run live end-to-end evaluation against a real MuninnDB")
		liveFile = flag.String("live-file", "", "live scenario JSON file (required with -live)")
		mcpURL   = flag.String("mcp-url", defaultMCPURL(), "MuninnDB MCP endpoint (live mode)")
		token    = flag.String("token", "", "MuninnDB bearer token (live mode; default ~/.muninn/mcp.token)")
		vault    = flag.String("vault", "msc-eval", "vault to seed/probe (live mode)")
		settle   = flag.Duration("settle", 750*time.Millisecond, "delay after seeding before probing (live mode)")
		timeout  = flag.Duration("timeout", 5*time.Second, "per-MCP-call timeout (live mode)")
	)
	flag.Parse()

	if *live {
		return runLive(*liveFile, *mcpURL, resolveToken(*token), *vault, *minScore, *budget, *settle, *timeout, *asJSON)
	}
	return runOffline(*file, *minScore, *budget, *sweep, *compare, *asJSON)
}

func runOffline(file string, minScore float64, budget int, sweep, compare, asJSON bool) error {
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
	var study *inject.StudyReport
	if compare {
		s := inject.RunMethodStudy(20240529, 600, 5)
		study = &s
	}

	if asJSON {
		return emitJSON(map[string]any{
			"results":   results,
			"aggregate": agg,
			"sweep":     sweepPoints,
			"study":     study,
		})
	}

	printOfflineReport(results, agg)
	if sweep {
		printSweep(sweepPoints)
	}
	if study != nil {
		printStudy(*study)
	}
	return nil
}

func runLive(liveFile, mcpURL, token, vault string, minScore float64, budget int, settle, timeout time.Duration, asJSON bool) error {
	if liveFile == "" {
		return fmt.Errorf("-live requires -live-file")
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
			trunc(r.Scenario, 32), m.Precision, m.Recall, m.F1, m.NDCG, gateMark(r), m.NumInjected, m.NumRelevant, m.WastedRatio*100)
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

func printLiveReport(results []inject.LiveResult) {
	fmt.Println("\nMemory injection — live end-to-end evaluation")
	fmt.Printf("%-28s %8s %8s %6s %6s %8s\n", "scenario", "recalled", "expected", "hits", "extra", "hitrate")
	fmt.Println(strings.Repeat("-", 70))
	var sum float64
	for _, r := range results {
		fmt.Printf("%-28s %8d %8d %6d %6d %7.0f%%\n",
			trunc(r.Scenario, 28), r.Recalled, len(r.Expected), r.Hits, r.Extra, r.HitRate*100)
		sum += r.HitRate
	}
	if len(results) > 0 {
		fmt.Println(strings.Repeat("-", 70))
		fmt.Printf("%-28s %8s %8s %6s %6s %7.0f%%\n", "MEAN", "", "", "", "", sum/float64(len(results))*100)
	}
}

func trunc(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n-1] + "…"
}

// --- minimal MuninnDB config resolution (mirrors cmd/msc defaults) ---

func defaultMCPURL() string {
	if u := os.Getenv("MUNINN_MCP_URL"); u != "" {
		return u
	}
	return "http://127.0.0.1:8750/mcp"
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
