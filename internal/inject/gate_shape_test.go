package inject

import (
	"math"
	"testing"
)

func TestTopZScore(t *testing.T) {
	// Fewer than 2 candidates: shape is undefined, return +Inf (z-gate never fires).
	if z := topZScore(nil); !math.IsInf(z, 1) {
		t.Fatalf("empty: got %v, want +Inf", z)
	}
	if z := topZScore([]studyCand{{score: 0.7}}); !math.IsInf(z, 1) {
		t.Fatalf("single: got %v, want +Inf", z)
	}
	// All-equal scores: zero variance, return +Inf.
	if z := topZScore([]studyCand{{score: 0.5}, {score: 0.5}, {score: 0.5}}); !math.IsInf(z, 1) {
		t.Fatalf("zero-variance: got %v, want +Inf", z)
	}
	// A top hit standing clear of a low cluster has a large positive z; a bunched
	// set has a small one.
	standout := topZScore([]studyCand{{score: 0.9}, {score: 0.45}, {score: 0.44}})
	bunched := topZScore([]studyCand{{score: 0.52}, {score: 0.50}, {score: 0.49}})
	if !(standout > bunched) {
		t.Fatalf("expected standout z (%.3f) > bunched z (%.3f)", standout, bunched)
	}
	if standout <= 0 {
		t.Fatalf("standout z should be positive, got %.3f", standout)
	}
}

// TestShapeGatesReduceToAbsolute verifies the new gates collapse to the plain
// absolute method at their degenerate params (sep=0, z=0) — so cross-validation
// can never score them below absolute, and any difference is pure upside.
func TestShapeGatesReduceToAbsolute(t *testing.T) {
	scenarios := genScenarios(20240529, 300)
	methods := candidateMethods()
	var abs, sep, zg studySelector
	for _, m := range methods {
		switch m.name {
		case "absolute":
			abs = m
		case "absolute+sepgate":
			sep = m
		case "absolute+zgate":
			zg = m
		}
	}
	if abs.sel == nil || sep.sel == nil || zg.sel == nil {
		t.Fatal("expected absolute, absolute+sepgate, absolute+zgate methods present")
	}
	for _, cands := range scenarios {
		base := abs.sel(cands, studyParams{abs: 0.6})
		gotSep := sep.sel(cands, studyParams{abs: 0.6, sep: 0})
		gotZ := zg.sel(cands, studyParams{abs: 0.6, z: 0})
		if len(gotSep) != len(base) {
			t.Fatalf("sepgate(sep=0) len %d != absolute len %d", len(gotSep), len(base))
		}
		// z=0 keeps any top hit whose z >= 0, i.e. at or above the mean — always
		// true for the max — so it matches absolute too.
		if len(gotZ) != len(base) {
			t.Fatalf("zgate(z=0) len %d != absolute len %d", len(gotZ), len(base))
		}
	}
}

func TestParamsAbsSepAndZ(t *testing.T) {
	sp := paramsAbsSep([]float64{0.5, 0.6}, []float64{0.0, 0.1})
	if len(sp) != 4 {
		t.Fatalf("paramsAbsSep: got %d, want 4", len(sp))
	}
	zp := paramsAbsZ([]float64{0.5, 0.6}, []float64{0.0, 1.0})
	if len(zp) != 4 {
		t.Fatalf("paramsAbsZ: got %d, want 4", len(zp))
	}
	// Spot-check a combination is present and fields routed correctly.
	found := false
	for _, p := range sp {
		if p.abs == 0.6 && p.sep == 0.1 {
			found = true
		}
	}
	if !found {
		t.Fatal("paramsAbsSep missing {abs:0.6, sep:0.1}")
	}
}

func FuzzTopZScore(f *testing.F) {
	f.Add(0.9, 0.5, 0.4)
	f.Add(0.5, 0.5, 0.5)
	f.Fuzz(func(t *testing.T, a, b, c float64) {
		// topZScore's contract: candidate scores are clamped cosines in [0,1] (see
		// genScenarios / normalizeRelevance). Map fuzz inputs into that domain so
		// we test the real operating range, not float overflow on impossible values.
		clamp01 := func(v float64) float64 {
			if math.IsNaN(v) || math.IsInf(v, 0) {
				return 0
			}
			v = math.Abs(v)
			v -= math.Floor(v) // fractional part → [0,1)
			return v
		}
		a, b, c = clamp01(a), clamp01(b), clamp01(c)
		s := []studyCand{{score: a}, {score: b}, {score: c}}
		// Sort descending so s[0] is the top, as production guarantees.
		if s[1].score > s[0].score {
			s[0], s[1] = s[1], s[0]
		}
		if s[2].score > s[0].score {
			s[0], s[2] = s[2], s[0]
		}
		z := topZScore(s)
		if math.IsNaN(z) {
			t.Fatalf("topZScore returned NaN for %v", s)
		}
		// The top element is the max, so its deviation from the mean is >= 0;
		// z is therefore non-negative (or +Inf for a zero-variance set).
		if z < 0 {
			t.Fatalf("top z should be >= 0, got %v for %v", z, s)
		}
	})
}
