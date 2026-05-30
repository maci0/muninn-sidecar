package inject

import (
	"fmt"
	"testing"
)

// TestMethodStudy runs the cross-validated empirical comparison of when+what
// injection methods and logs the held-out leaderboard. It is the authoritative
// answer to "which method is best": the winner is chosen by mean held-out F1.
// Production uses the "absolute" method (a single MinScore threshold), so the
// study must confirm "absolute" is the winner (or within noise of it) and that
// its tuned threshold matches the production default.
func TestMethodStudy(t *testing.T) {
	const (
		seed = 20240529
		n    = 600
		k    = 5
	)
	rep := RunMethodStudy(seed, n, k)

	t.Logf("\nEmpirical when+what study — %d synthetic scenarios, %d-fold CV (seed %d)", rep.N, rep.K, rep.Seed)
	t.Logf("%-20s %8s %7s %7s %7s %8s  %s", "method", "f1(test)", "±std", "gate", "wasted", "avg_inj", "tuned")
	for _, m := range rep.Methods {
		t.Logf("%-20s %8.3f %7.3f %6.0f%% %6.0f%% %8.2f  %s",
			m.Name, m.F1Mean, m.F1Std, m.GateAcc*100, m.Wasted*100, m.AvgInjected, tunedStr(m))
	}
	t.Logf("WINNER (highest held-out F1): %s", rep.Best)

	byName := make(map[string]MethodResult, len(rep.Methods))
	for _, m := range rep.Methods {
		byName[m.name()] = m
	}
	best := rep.Methods[0]

	// Production = the "absolute" single-threshold method. It must be the winner
	// or within 0.02 F1 of it; otherwise the chosen production method is wrong.
	prod, ok := byName["absolute"]
	if !ok {
		t.Fatal("production method 'absolute' missing from study")
	}
	if best.F1Mean-prod.F1Mean > 0.02 {
		t.Errorf("production method 'absolute' F1 %.3f trails the best (%s, %.3f) by >0.02; reconsider the method",
			prod.F1Mean, best.Name, best.F1Mean)
	}

	// The tuned absolute threshold must validate the production default (0.5),
	// not just the method shape. CV tunes per fold; the mean should land near 0.5.
	if prod.TunedAbs < defaultMinScore-0.05 || prod.TunedAbs > defaultMinScore+0.05 {
		t.Errorf("tuned absolute threshold %.3f is far from production default %.2f; retune defaultMinScore",
			prod.TunedAbs, defaultMinScore)
	}

	// Methods that cannot suppress (never inject nothing) must do measurably
	// worse on gate accuracy than the threshold method — empirical proof that a
	// "when to inject" decision is necessary, not just "what".
	rel := byName["relative-only"]
	if rel.GateAcc >= prod.GateAcc {
		t.Errorf("non-suppressing method gate acc %.2f should trail the threshold method %.2f", rel.GateAcc, prod.GateAcc)
	}
}

func tunedStr(m MethodResult) string {
	switch m.Name {
	case "fixed-topk":
		return fmt.Sprintf("k=%.1f", m.TunedK)
	case "absolute":
		return fmt.Sprintf("abs=%.3f", m.TunedAbs)
	case "relative-only":
		return fmt.Sprintf("rel=%.3f", m.TunedRel)
	case "absfloor+relative":
		return fmt.Sprintf("floor=%.3f rel=%.3f", m.TunedFloor, m.TunedRel)
	case "absfloor+gapcut":
		return fmt.Sprintf("floor=%.3f", m.TunedFloor)
	}
	return ""
}

func (m MethodResult) name() string { return m.Name }
