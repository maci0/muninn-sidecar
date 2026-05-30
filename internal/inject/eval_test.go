package inject

import (
	"fmt"
	"math"
	"testing"
)

func approx(a, b float64) bool { return math.Abs(a-b) < 0.01 }

func TestMetricPrimitives(t *testing.T) {
	t.Run("precision", func(t *testing.T) {
		if got := precision(2, 4); !approx(got, 0.5) {
			t.Errorf("precision(2,4)=%v want 0.5", got)
		}
		if got := precision(0, 0); got != 1.0 {
			t.Errorf("precision of empty injection should be 1.0, got %v", got)
		}
	})
	t.Run("recall", func(t *testing.T) {
		if got := recall(2, 5); !approx(got, 0.4) {
			t.Errorf("recall(2,5)=%v want 0.4", got)
		}
		if got := recall(0, 0); got != 1.0 {
			t.Errorf("recall with nothing to find should be 1.0, got %v", got)
		}
	})
	t.Run("f1", func(t *testing.T) {
		if got := f1(0.5, 0.4); !approx(got, 0.4444) {
			t.Errorf("f1(0.5,0.4)=%v want 0.4444", got)
		}
		if got := f1(0, 1); got != 0 {
			t.Errorf("f1 with zero precision should be 0, got %v", got)
		}
	})
	t.Run("wastedRatio", func(t *testing.T) {
		if got := wastedRatio(3, 10); !approx(got, 0.7) {
			t.Errorf("wastedRatio(3,10)=%v want 0.7", got)
		}
		if got := wastedRatio(0, 0); got != 0 {
			t.Errorf("wastedRatio of empty should be 0, got %v", got)
		}
	})
}

func TestNDCG(t *testing.T) {
	cases := []struct {
		name string
		rels []int
		want float64
	}{
		{"empty", []int{}, 0},
		{"perfect order", []int{1, 1, 1}, 1.0},
		{"no relevant", []int{0, 0}, 0},
		// [1,0,1]: dcg = 1 + 0 + 1/log2(4)=0.5 -> 1.5; ideal [1,1,0]: 1 + 1/log2(3)=0.6309 -> 1.6309
		{"relevant split", []int{1, 0, 1}, 0.9197},
		// [0,1]: relevant ranked last; dcg = 1/log2(3)=0.6309; ideal [1,0]: 1 -> ndcg 0.6309
		{"relevant ranked last", []int{0, 1}, 0.6309},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := ndcg(c.rels); !approx(got, c.want) {
				t.Errorf("ndcg(%v)=%v want %v", c.rels, got, c.want)
			}
		})
	}
}

// byName indexes results for assertion lookups.
func byName(results []EvalResult) map[string]EvalResult {
	m := make(map[string]EvalResult, len(results))
	for _, r := range results {
		m[r.Scenario] = r
	}
	return m
}

func TestRunScenarioBehavior(t *testing.T) {
	scenarios, err := DefaultScenarios()
	if err != nil {
		t.Fatal(err)
	}
	results := make([]EvalResult, len(scenarios))
	for i, s := range scenarios {
		results[i] = RunScenario(s)
	}
	got := byName(results)

	t.Run("strong match drops weak tail with perfect precision/recall", func(t *testing.T) {
		r := got["strong-match-with-noise"]
		if r.Metrics.NumInjected != 2 {
			t.Errorf("expected 2 injected (weak tail dropped), got %d", r.Metrics.NumInjected)
		}
		if !approx(r.Metrics.Precision, 1) || !approx(r.Metrics.Recall, 1) {
			t.Errorf("expected P=R=1, got P=%v R=%v", r.Metrics.Precision, r.Metrics.Recall)
		}
	})

	t.Run("near-duplicates removed", func(t *testing.T) {
		r := got["duplicate-heavy"]
		if r.Metrics.NumInjected != 2 {
			t.Errorf("expected 2 injected (3 dups collapse to 1 + distinct), got %d", r.Metrics.NumInjected)
		}
		if !approx(r.Metrics.Precision, 1) {
			t.Errorf("dedup should leave only relevant memories, P=%v", r.Metrics.Precision)
		}
	})

	t.Run("all-noise turn is suppressed by the gate", func(t *testing.T) {
		r := got["all-noise"]
		if r.DidInject {
			t.Errorf("all-noise should inject nothing (top score below floor), injected %d", r.Metrics.NumInjected)
		}
		if r.ShouldInject {
			t.Error("all-noise gold label should be should_inject=false")
		}
		if r.Metrics.GateAccuracy != 1 {
			t.Errorf("gate decision should be correct, GateAccuracy=%v", r.Metrics.GateAccuracy)
		}
		if r.Metrics.WastedRatio != 0 {
			t.Errorf("suppressed turn wastes no budget, got %v", r.Metrics.WastedRatio)
		}
	})

	t.Run("cold-open with no signal is suppressed", func(t *testing.T) {
		r := got["cold-open-no-signal"]
		if r.DidInject {
			t.Errorf("cold-open should be suppressed, injected %d", r.Metrics.NumInjected)
		}
		if r.Metrics.GateAccuracy != 1 {
			t.Errorf("gate should correctly suppress, GateAccuracy=%v", r.Metrics.GateAccuracy)
		}
	})

	t.Run("tight budget injects only relevant memories", func(t *testing.T) {
		r := got["mixed-relevance-tight-budget"]
		if r.Metrics.Precision != 1 {
			t.Errorf("tight-budget precision should be 1 (only relevant injected), got %v", r.Metrics.Precision)
		}
		if r.Metrics.NumInjected >= 4 {
			t.Errorf("tight budget should cap injected count, got %d", r.Metrics.NumInjected)
		}
	})
}

// TestCorpusRegression gates aggregate selection quality and prints a report so
// regressions in selection logic surface as failing CI rather than silent drift.
func TestCorpusRegression(t *testing.T) {
	scenarios, err := DefaultScenarios()
	if err != nil {
		t.Fatal(err)
	}
	results := make([]EvalResult, len(scenarios))
	for i, s := range scenarios {
		results[i] = RunScenario(s)
	}
	agg := AggregateMetrics(results)

	t.Log(report(results, agg))

	const (
		minPrecision = 0.85
		minRecall    = 0.9
		minF1        = 0.9
		minGateAcc   = 0.9
		maxWasted    = 0.1
	)
	if agg.Precision < minPrecision {
		t.Errorf("aggregate precision %.3f below floor %.2f", agg.Precision, minPrecision)
	}
	if agg.Recall < minRecall {
		t.Errorf("aggregate recall %.3f below floor %.2f", agg.Recall, minRecall)
	}
	if agg.F1 < minF1 {
		t.Errorf("aggregate F1 %.3f below floor %.2f", agg.F1, minF1)
	}
	if agg.GateAccuracy < minGateAcc {
		t.Errorf("aggregate gate accuracy %.3f below floor %.2f", agg.GateAccuracy, minGateAcc)
	}
	if agg.WastedRatio > maxWasted {
		t.Errorf("aggregate wasted-budget ratio %.3f above ceiling %.2f", agg.WastedRatio, maxWasted)
	}
}

// TestMinScoreThresholdImproves confirms the injection threshold is worth
// having: the production default must beat the threshold-off baseline on gate
// accuracy and wasted budget, and must sit on the optimal plateau of the
// MinScore sweep over the corpus. This is the corpus-level cross-check of the
// synthetic method study's conclusion.
func TestMinScoreThresholdImproves(t *testing.T) {
	scenarios, err := DefaultScenarios()
	if err != nil {
		t.Fatal(err)
	}
	scores := []float64{0.0, 0.45, 0.50, 0.55, 0.58, 0.60, 0.62, 0.65, 0.70}
	points := SweepMinScore(scenarios, scores)

	var off, def, best SweepPoint
	for _, p := range points {
		t.Logf("minscore %.2f: gate=%.0f%% prec=%.2f rec=%.2f f1=%.2f wasted=%.0f%%",
			p.MinScore, p.Metrics.GateAccuracy*100, p.Metrics.Precision, p.Metrics.Recall, p.Metrics.F1, p.Metrics.WastedRatio*100)
		if p.MinScore == 0 {
			off = p
		}
		if approx(p.MinScore, defaultMinScore) {
			def = p
		}
		if p.Metrics.GateAccuracy > best.Metrics.GateAccuracy ||
			(approx(p.Metrics.GateAccuracy, best.Metrics.GateAccuracy) && p.Metrics.F1 > best.Metrics.F1) {
			best = p
		}
	}

	if def.Metrics.GateAccuracy <= off.Metrics.GateAccuracy {
		t.Errorf("threshold (%.2f, acc %.2f) should beat threshold-off baseline (acc %.2f)",
			defaultMinScore, def.Metrics.GateAccuracy, off.Metrics.GateAccuracy)
	}
	if def.Metrics.WastedRatio >= off.Metrics.WastedRatio {
		t.Errorf("threshold should reduce wasted budget vs baseline: %.2f vs %.2f", def.Metrics.WastedRatio, off.Metrics.WastedRatio)
	}
	if !approx(def.Metrics.GateAccuracy, best.Metrics.GateAccuracy) || !approx(def.Metrics.F1, best.Metrics.F1) {
		t.Errorf("default MinScore %.2f (gate %.2f, f1 %.2f) should be on the optimal plateau (best gate %.2f, f1 %.2f)",
			defaultMinScore, def.Metrics.GateAccuracy, def.Metrics.F1, best.Metrics.GateAccuracy, best.Metrics.F1)
	}
}

// report renders a human-readable per-scenario + aggregate metrics table.
func report(results []EvalResult, agg Metrics) string {
	gate := func(r EvalResult) string {
		switch {
		case r.DidInject == r.ShouldInject:
			return "ok"
		case r.DidInject:
			return "FP"
		default:
			return "FN"
		}
	}
	s := "\nMemory injection selection — offline evaluation\n"
	s += fmt.Sprintf("%-32s %5s %5s %5s %5s %5s %8s %7s\n", "scenario", "prec", "rec", "f1", "ndcg", "gate", "inj/rel", "wasted")
	for _, r := range results {
		m := r.Metrics
		s += fmt.Sprintf("%-32s %5.2f %5.2f %5.2f %5.2f %5s %4d/%-3d %6.0f%%\n",
			r.Scenario, m.Precision, m.Recall, m.F1, m.NDCG, gate(r), m.NumInjected, m.NumRelevant, m.WastedRatio*100)
	}
	s += fmt.Sprintf("%-32s %5.2f %5.2f %5.2f %5.2f %4.0f%% %4d/%-3d %6.0f%%\n",
		"AGGREGATE (macro avg)", agg.Precision, agg.Recall, agg.F1, agg.NDCG, agg.GateAccuracy*100, agg.NumInjected, agg.NumRelevant, agg.WastedRatio*100)
	return s
}

func TestCalibrateThreshold(t *testing.T) {
	// Bimodal sample: noise ~N(0.45,0.03), relevant ~N(0.72,0.05). Otsu valley
	// should land between the clusters (~0.5-0.65).
	rng := newDetRand(7)
	var scores []float64
	for i := 0; i < 400; i++ {
		scores = append(scores, 0.45+rng()*0.03-0.015)
	}
	for i := 0; i < 300; i++ {
		scores = append(scores, 0.72+rng()*0.05-0.025)
	}
	got := CalibrateThreshold(scores)
	// Must land in the empty valley between the noise (~0.45) and relevant
	// (~0.72) clusters — i.e. above the noise mean, below the relevant mean.
	if got < 0.46 || got > 0.70 {
		t.Errorf("calibrated threshold %.3f not in the (0.46,0.70) inter-cluster valley", got)
	}
	// Tiny sample falls back to the default.
	if got := CalibrateThreshold([]float64{0.7, 0.4}); got != defaultMinScore {
		t.Errorf("small sample should fall back to default %.2f, got %.3f", defaultMinScore, got)
	}

	// Low-cosine vault (the SQuAD failure case): clusters ~0.15 and ~0.35.
	// Must adapt DOWN into the valley, not clamp up to 0.45 — otherwise the gate
	// would suppress everything.
	rng = newDetRand(11)
	var low []float64
	for i := 0; i < 300; i++ {
		low = append(low, 0.15+rng()*0.03-0.015)
	}
	for i := 0; i < 200; i++ {
		low = append(low, 0.35+rng()*0.04-0.02)
	}
	if got := CalibrateThreshold(low); got < 0.16 || got > 0.34 {
		t.Errorf("low-cosine valley %.3f not adapted into (0.16,0.34)", got)
	}

	// Unimodal sample (no real split): keep the prior, don't latch onto noise.
	rng = newDetRand(3)
	var uni []float64
	for i := 0; i < 400; i++ {
		uni = append(uni, 0.5+rng()*0.04-0.02)
	}
	if got := CalibrateThreshold(uni); got != defaultMinScore {
		t.Errorf("unimodal sample should keep default %.2f, got %.3f", defaultMinScore, got)
	}
}

// newDetRand returns a deterministic pseudo-random generator in [0,1) without
// importing math/rand into the non-test build.
func newDetRand(seed uint64) func() float64 {
	s := seed
	return func() float64 {
		s = s*6364136223846793005 + 1442695040888963407
		return float64(s>>11) / float64(1<<53)
	}
}
