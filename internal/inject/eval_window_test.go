package inject

import "testing"

// TestWindowDecayStudy runs the synthetic window-decay sweep at the production
// injection gate and logs the precision/recall tradeoff per decay rate. Unlike
// the gate method-study (whose score distribution is calibrated to real cosine
// data), the relevance-lifespan model here is principled but UNCALIBRATED — no
// real multi-turn relevance dataset exists — so this is a tuning tool, not a
// retune mandate. The test therefore only asserts the shipped decay is not
// pathological (positive, non-degenerate F1), and logs where it sits so a future
// run with real session data can retune from a ready harness.
func TestWindowDecayStudy(t *testing.T) {
	const (
		seed = 424242
		n    = 800
	)
	results := RunWindowStudy(seed, n)
	if len(results) == 0 {
		t.Fatal("no window study results")
	}

	best := results[0]
	t.Logf("window-decay study — %d sessions, gate=%.2f (seed %d)", n, defaultMinScore, seed)
	t.Logf("%-7s %8s %8s %8s", "decay", "prec", "recall", "f1")
	var prod WindowParams
	found := false
	for _, r := range results {
		marker := ""
		if r.Decay == decayFactor {
			marker = "  <- production"
			prod, found = r, true
		}
		t.Logf("%-7.2f %8.3f %8.3f %8.3f%s", r.Decay, r.Precision, r.Recall, r.F1, marker)
	}
	t.Logf("BEST decay=%.2f f1=%.3f (model lifespan ~1-3 turns; tradeoff is assumption-sensitive)", best.Decay, best.F1)

	if !found {
		t.Fatalf("production decay %.2f not in the sweep grid", decayFactor)
	}
	// Not pathological: the window must inject relevant memories and not be pure
	// noise. A degenerate decay (everything or nothing) would fail this.
	if prod.F1 <= 0.3 {
		t.Errorf("production decay %.2f scores a degenerate F1 %.3f; the window may be mis-tuned", decayFactor, prod.F1)
	}
	if prod.Recall <= 0.5 {
		t.Errorf("production decay %.2f recall %.3f too low — carry-forward is dropping relevant memories", decayFactor, prod.Recall)
	}
}

// TestRecallFloorBelowCalibrationFloor guards the invariant that the default
// recall floor (MuninnDB's composite-score pre-filter) sits at or below the
// gate's cosine calibration floor. If it drifts above, auto-calibration could
// lower MinScore beneath the recall floor and the server would silently withhold
// memories the calibrated gate would inject (see docs/experiments.md §recall floor).
func TestRecallFloorBelowCalibrationFloor(t *testing.T) {
	inj := New(Config{MCPURL: "http://unused"})
	if inj.threshold > calibMinThreshold {
		t.Errorf("default recall floor %.3f exceeds calibration floor %.3f; calibration below it would be moot", inj.threshold, calibMinThreshold)
	}
}

func TestCalibrateThresholdDetail(t *testing.T) {
	// Clearly bimodal: a noise cluster ~0.45 and a relevant cluster ~0.75.
	var scores []float64
	for i := 0; i < 40; i++ {
		scores = append(scores, 0.45, 0.75)
	}
	thr, noise, rel, sep := CalibrateThresholdDetail(scores)
	if thr <= calibMinThreshold || thr >= calibMaxThreshold {
		t.Errorf("threshold %.3f out of expected range", thr)
	}
	if !(noise < rel) {
		t.Errorf("noise mean %.3f should be below relevant mean %.3f", noise, rel)
	}
	if sep <= 0 {
		t.Errorf("expected positive cluster separation, got %.3f", sep)
	}
	// CalibrateThreshold returns the same threshold.
	if CalibrateThreshold(scores) != thr {
		t.Error("CalibrateThreshold and CalibrateThresholdDetail must agree")
	}
}

func TestScoreWindowEdges(t *testing.T) {
	// Empty sessions → zero, no panic.
	if p, r, f := scoreWindow(nil, 0.7, 0.6); p != 0 || r != 0 || f != 0 {
		t.Errorf("empty sessions should score zero, got %v/%v/%v", p, r, f)
	}
	// A high-score memory recalled at turn 0, relevant a few turns, is injected
	// while it clears the gate → positive F1.
	s := []windowSession{{Turns: 2, Mems: []windowMemory{{Intro: 0, Lifespan: 2, Score: 0.9}}}}
	if _, _, f := scoreWindow(s, 0.7, 0.6); f <= 0 {
		t.Errorf("expected positive F1 for a clearly-injected relevant memory, got %v", f)
	}
}

func TestGenWindowSessionsDeterministic(t *testing.T) {
	a := genWindowSessions(7, 10)
	b := genWindowSessions(7, 10)
	if len(a) != 10 || len(b) != 10 {
		t.Fatalf("expected 10 sessions, got %d/%d", len(a), len(b))
	}
	for i := range a {
		if a[i].Turns != b[i].Turns || len(a[i].Mems) != len(b[i].Mems) {
			t.Fatalf("nondeterministic generation at session %d", i)
		}
	}
}
