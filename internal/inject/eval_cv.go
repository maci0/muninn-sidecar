// This file contains the cross-validation engine and the exported entry point
// for the empirical method study. It scores each candidate method on held-out
// data so the comparison reflects generalization, not memorization.
package inject

import (
	"math"
	"sort"
)

// studyScore holds the metrics for one method evaluated on one scenario set.
type studyScore struct {
	F1          float64 // macro-averaged across scenarios (primary objective)
	GateAcc     float64 // fraction of correct inject/suppress decisions
	Wasted      float64 // fraction of injected memories that were irrelevant
	AvgInjected float64 // mean memories injected per scenario
}

// scoreSet runs a selector with fixed params over a scenario set and returns the
// aggregate metrics. F1 uses the same vacuous conventions as the rest of the
// harness, so a correctly-suppressed noise-only turn scores F1=1 — this is how a
// single metric captures both "when" (suppression) and "what" (selection).
func scoreSet(scenarios [][]studyCand, sel studySelector, p studyParams) studyScore {
	if len(scenarios) == 0 {
		return studyScore{}
	}
	var sumF1, sumGate, totalInjected, totalIrrelevant, totalCount float64
	for _, cands := range scenarios {
		injected := sel.sel(cands, p)

		relevantTotal, injectedRelevant := 0, 0
		for _, c := range cands {
			if c.relevant {
				relevantTotal++
			}
		}
		for _, c := range injected {
			if c.relevant {
				injectedRelevant++
			}
		}

		prec := precision(injectedRelevant, len(injected))
		rec := recall(injectedRelevant, relevantTotal)
		sumF1 += f1(prec, rec)

		didInject := len(injected) > 0
		shouldInject := relevantTotal > 0
		if didInject == shouldInject {
			sumGate++
		}

		totalInjected += float64(len(injected))
		totalIrrelevant += float64(len(injected) - injectedRelevant)
		totalCount++
	}

	wasted := 0.0
	if totalInjected > 0 {
		wasted = totalIrrelevant / totalInjected
	}
	return studyScore{
		F1:          sumF1 / totalCount,
		GateAcc:     sumGate / totalCount,
		Wasted:      wasted,
		AvgInjected: totalInjected / totalCount,
	}
}

// gridSearch returns the params maximizing macro-F1 over the training set. Ties
// are broken toward lower wasted budget, then fewer injected memories (prefer
// the simpler, leaner decision when quality is equal).
func gridSearch(train [][]studyCand, sel studySelector) studyParams {
	best := sel.grid[0]
	bestScore := studyScore{F1: -1}
	for _, p := range sel.grid {
		sc := scoreSet(train, sel, p)
		if betterTrain(sc, bestScore) {
			best, bestScore = p, sc
		}
	}
	return best
}

func betterTrain(a, b studyScore) bool {
	const eps = 1e-9
	if math.Abs(a.F1-b.F1) > eps {
		return a.F1 > b.F1
	}
	if math.Abs(a.Wasted-b.Wasted) > eps {
		return a.Wasted < b.Wasted
	}
	return a.AvgInjected < b.AvgInjected
}

// MethodResult is the held-out (cross-validated) performance of one method.
type MethodResult struct {
	Name        string      `json:"name"`
	F1Mean      float64     `json:"f1_mean"` // mean held-out macro-F1 across folds
	F1Std       float64     `json:"f1_std"`  // std across folds (stability)
	GateAcc     float64     `json:"gate_accuracy"`
	Wasted      float64     `json:"wasted"`
	AvgInjected float64     `json:"avg_injected"`
	TunedParams studyParams `json:"-"` // mean tuned hyperparameters across folds
	TunedFloor  float64     `json:"tuned_floor"`
	TunedRel    float64     `json:"tuned_rel"`
	TunedAbs    float64     `json:"tuned_abs"`
	TunedK      float64     `json:"tuned_k"`
}

// StudyReport is the full empirical comparison.
type StudyReport struct {
	Seed    int64          `json:"seed"`
	N       int            `json:"n"`
	K       int            `json:"k"`
	Methods []MethodResult `json:"methods"`
	Best    string         `json:"best"` // method with the highest held-out F1
}

// crossValidate runs k-fold CV for one method: per fold, tune hyperparameters on
// the other folds and score on the held-out fold. Returns held-out metrics
// averaged across folds, plus the mean tuned hyperparameters.
func crossValidate(scenarios [][]studyCand, sel studySelector, k int) MethodResult {
	f1s := make([]float64, 0, k)
	var sumGate, sumWasted, sumInj float64
	var sumFloor, sumRel, sumAbs, sumK float64

	for fold := 0; fold < k; fold++ {
		var train, test [][]studyCand
		for i, s := range scenarios {
			if i%k == fold {
				test = append(test, s)
			} else {
				train = append(train, s)
			}
		}
		p := gridSearch(train, sel)
		sc := scoreSet(test, sel, p)

		f1s = append(f1s, sc.F1)
		sumGate += sc.GateAcc
		sumWasted += sc.Wasted
		sumInj += sc.AvgInjected
		sumFloor += p.floor
		sumRel += p.rel
		sumAbs += p.abs
		sumK += float64(p.k)
	}

	kf := float64(k)
	mean := meanOf(f1s)
	return MethodResult{
		Name:        sel.name,
		F1Mean:      mean,
		F1Std:       stdOf(f1s, mean),
		GateAcc:     sumGate / kf,
		Wasted:      sumWasted / kf,
		AvgInjected: sumInj / kf,
		TunedFloor:  sumFloor / kf,
		TunedRel:    sumRel / kf,
		TunedAbs:    sumAbs / kf,
		TunedK:      sumK / kf,
	}
}

// RunMethodStudy generates n synthetic scenarios from seed and compares every
// candidate when+what method by k-fold cross-validation. The winner is the
// method with the highest mean held-out macro-F1.
func RunMethodStudy(seed int64, n, k int) StudyReport {
	scenarios := genScenarios(seed, n)
	methods := candidateMethods()

	results := make([]MethodResult, 0, len(methods))
	for _, m := range methods {
		results = append(results, crossValidate(scenarios, m, k))
	}
	sort.SliceStable(results, func(i, j int) bool { return results[i].F1Mean > results[j].F1Mean })

	best := ""
	if len(results) > 0 {
		best = results[0].Name
	}
	return StudyReport{Seed: seed, N: n, K: k, Methods: results, Best: best}
}

func meanOf(xs []float64) float64 {
	if len(xs) == 0 {
		return 0
	}
	var s float64
	for _, x := range xs {
		s += x
	}
	return s / float64(len(xs))
}

func stdOf(xs []float64, mean float64) float64 {
	if len(xs) < 2 {
		return 0
	}
	var s float64
	for _, x := range xs {
		d := x - mean
		s += d * d
	}
	return math.Sqrt(s / float64(len(xs)))
}
