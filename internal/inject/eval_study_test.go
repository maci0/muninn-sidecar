package inject

import (
	"fmt"
	"slices"
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

	// The tuned absolute threshold must validate the production default
	// (defaultMinScore = 0.6), not just the method shape. CV tunes per fold;
	// the mean should land near that default.
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

// TestMethodStudySeeds checks the cross-seed aggregation: every method present,
// rows sorted best-first, Best matching the top row, a single-seed run agreeing
// with RunMethodStudy (std 0), and determinism across calls.
func TestMethodStudySeeds(t *testing.T) {
	const (
		n = 120
		k = 3
	)
	rep := RunMethodStudySeeds([]int64{1, 2, 3}, n, k)

	// The study must report exactly this set of methods. Comparing the count
	// against len(candidateMethods()) would stay equal if a method were dropped
	// from both sides, so the names are spelled out here instead.
	want := []string{
		"fixed-topk", "absolute", "relative-only", "absfloor+relative",
		"absolute+sepgate", "absolute+zgate", "absfloor+gapcut", "absfloor+margin",
		"absolute+capN",
	}
	got := make(map[string]bool, len(rep.Methods))
	for _, m := range rep.Methods {
		got[m.Name] = true
	}
	for _, name := range want {
		if !got[name] {
			t.Errorf("method %q missing from the study", name)
		}
	}
	for _, m := range rep.Methods {
		if !slices.Contains(want, m.Name) {
			t.Errorf("unexpected method %q in the study", m.Name)
		}
	}
	if len(rep.Methods) != len(want) {
		t.Errorf("got %d methods, want %d", len(rep.Methods), len(want))
	}
	if rep.Best != rep.Methods[0].Name {
		t.Errorf("Best %q != top-ranked %q", rep.Best, rep.Methods[0].Name)
	}
	for i, m := range rep.Methods {
		if m.F1Mean < 0 || m.F1Mean > 1 || m.F1Std < 0 {
			t.Errorf("%s: implausible f1 mean %.3f std %.3f", m.Name, m.F1Mean, m.F1Std)
		}
		if i > 0 && rep.Methods[i-1].F1Mean < m.F1Mean {
			t.Errorf("methods not sorted by F1Mean at index %d", i)
		}
	}

	single := RunMethodStudy(7, n, k)
	singleF1 := make(map[string]float64, len(single.Methods))
	for _, m := range single.Methods {
		singleF1[m.Name] = m.F1Mean
	}
	for _, m := range RunMethodStudySeeds([]int64{7}, n, k).Methods {
		if m.F1Mean != singleF1[m.Name] || m.F1Std != 0 {
			t.Errorf("%s: single-seed run diverges from RunMethodStudy (f1 %.3f vs %.3f, std %.3f)",
				m.Name, m.F1Mean, singleF1[m.Name], m.F1Std)
		}
	}

	again := RunMethodStudySeeds([]int64{1, 2, 3}, n, k)
	if again.Best != rep.Best || again.Methods[0].F1Mean != rep.Methods[0].F1Mean {
		t.Error("RunMethodStudySeeds is not deterministic for identical inputs")
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
