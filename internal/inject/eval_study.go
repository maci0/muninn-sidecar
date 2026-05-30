// This file contains the empirical method study: rather than tuning one knob of
// one selection strategy, it compares several candidate "when + what to inject"
// methods on a larger synthetic dataset using k-fold cross-validation, so each
// method's hyperparameters are tuned on training folds and scored on held-out
// test folds. This guards against overfitting a threshold to the same data used
// to pick it — the flaw in tuning AbsFloor on the small hand-authored corpus.
//
// The dataset is synthetic but principled: relevant and noise memories are drawn
// from overlapping score distributions (so no threshold can separate them
// perfectly), with sparse-but-relevant and noise-only scenarios included. It
// models the recall-score signal the selector actually sees in production; it
// does not model embedding-recall quality (that is the live layer's job).
package inject

import (
	"math"
	"math/rand"
	"sort"
)

// studyCand is a candidate reduced to the only two things a score-based selector
// sees (score) and the study scores against (relevant). Content/concept are
// irrelevant to the when/what decision being compared here.
type studyCand struct {
	score    float64
	relevant bool
}

// genScenarios produces n labeled scenarios from overlapping score
// distributions, deterministically from seed. Each scenario is a score-sorted
// (descending) candidate list, already filtered by the recall floor (0.4), as a
// selector would receive it in production.
//
// Distribution model (per scenario) — calibrated to the embedding cosine
// similarities observed against a real MuninnDB instance (cmd/msc-bench):
//   - ~15% are noise-only (no relevant memory exists) — the gate should suppress.
//   - relevant memories: cosine ~ N(muRel, 0.07), muRel ~ U(0.62, 0.88).
//     Low-muRel scenarios are the sparse-but-relevant case the gate must NOT drop.
//   - noise memories: cosine ~ N(muNoise, 0.04), muNoise ~ U(0.40, 0.50).
//
// Relevant and noise ranges overlap around 0.5–0.6, so that band is genuinely
// ambiguous — exactly where "when to inject" is hard, and where the real-data
// threshold (~0.6) sits.
func genScenarios(seed int64, n int) [][]studyCand {
	rng := rand.New(rand.NewSource(seed))
	clamp := func(x float64) float64 { return math.Max(0, math.Min(1, x)) }

	scenarios := make([][]studyCand, 0, n)
	for i := 0; i < n; i++ {
		var cands []studyCand

		noiseOnly := rng.Float64() < 0.15
		numRel := 0
		if !noiseOnly {
			numRel = 1 + rng.Intn(4) // 1..4 relevant
		}
		numNoise := rng.Intn(6) // 0..5 noise

		muRel := 0.62 + rng.Float64()*0.26   // [0.62, 0.88]
		muNoise := 0.40 + rng.Float64()*0.10 // [0.40, 0.50]

		for r := 0; r < numRel; r++ {
			s := clamp(rng.NormFloat64()*0.07 + muRel)
			if s >= 0.30 {
				cands = append(cands, studyCand{score: s, relevant: true})
			}
		}
		for nz := 0; nz < numNoise; nz++ {
			s := clamp(rng.NormFloat64()*0.04 + muNoise)
			if s >= 0.30 {
				cands = append(cands, studyCand{score: s, relevant: false})
			}
		}

		sort.SliceStable(cands, func(a, b int) bool { return cands[a].score > cands[b].score })
		scenarios = append(scenarios, cands)
	}
	return scenarios
}

// studyParams holds every hyperparameter any method might use; each method reads
// only the fields it cares about.
type studyParams struct {
	abs    float64 // absolute score threshold
	rel    float64 // relative cutoff as a fraction of the top score
	floor  float64 // turn-level suppression floor on the top score
	margin float64 // keep memories within this score gap below the top
	sep    float64 // min gap between top1 and top2 required to inject (separation gate)
	z      float64 // min z-score of top1 over the candidate set required to inject
	k      int     // max items
}

// studySelector is a candidate method: a name, a pure score-based selection
// function, and the grid of hyperparameters to search over.
type studySelector struct {
	name string
	sel  func(sorted []studyCand, p studyParams) []studyCand
	grid []studyParams
}

// candidateMethods returns the competing "when + what to inject" strategies.
func candidateMethods() []studySelector {
	floors := frange(0.40, 0.60, 0.02)
	return []studySelector{
		{
			// Legacy-ish: keep up to k candidates above the recall floor. Never
			// suppresses a non-empty turn — the always-inject baseline.
			name: "fixed-topk",
			sel: func(s []studyCand, p studyParams) []studyCand {
				return capN(s, p.k)
			},
			grid: paramsK([]int{3, 5, 8}),
		},
		{
			// Single absolute threshold does both jobs: keep score >= abs;
			// suppress when nothing clears it.
			name: "absolute",
			sel: func(s []studyCand, p studyParams) []studyCand {
				return keepAbove(s, p.abs)
			},
			grid: paramsAbs(frange(0.40, 0.72, 0.02)),
		},
		{
			// Relative knee only: keep score >= top*rel. Always keeps the top, so
			// it can never decide to inject nothing.
			name: "relative-only",
			sel: func(s []studyCand, p studyParams) []studyCand {
				if len(s) == 0 {
					return nil
				}
				return keepAbove(s, s[0].score*p.rel)
			},
			grid: paramsRel(frange(0.40, 0.90, 0.05)),
		},
		{
			// Current production method: suppress if top < floor, else keep the
			// relative knee. Two knobs separating "when" from "what".
			name: "absfloor+relative",
			sel: func(s []studyCand, p studyParams) []studyCand {
				if len(s) == 0 || s[0].score < p.floor {
					return nil
				}
				return keepAbove(s, s[0].score*p.rel)
			},
			grid: paramsFloorRel(floors, []float64{0.4, 0.5, 0.6, 0.7}),
		},
		{
			// Suppress if top < floor; else cut at the largest score gap (the
			// natural break between a relevant cluster and the noise tail).
			name: "absfloor+gapcut",
			sel: func(s []studyCand, p studyParams) []studyCand {
				if len(s) == 0 || s[0].score < p.floor {
					return nil
				}
				return gapCut(s)
			},
			grid: paramsFloor(floors),
		},
		{
			// Absolute threshold plus a hard item cap — does limiting how many we
			// inject (even when several clear the bar) help precision/budget?
			name: "absolute+capN",
			sel: func(s []studyCand, p studyParams) []studyCand {
				return capN(keepAbove(s, p.abs), p.k)
			},
			grid: paramsAbsK(frange(0.50, 0.66, 0.02), []int{1, 2, 3, 5}),
		},
		{
			// Suppress if top < floor; else keep memories within `margin` of the
			// top score (a relative-to-top band rather than an absolute cut).
			name: "absfloor+margin",
			sel: func(s []studyCand, p studyParams) []studyCand {
				if len(s) == 0 || s[0].score < p.floor {
					return nil
				}
				return keepAbove(s, s[0].score-p.margin)
			},
			grid: paramsFloorMargin(floors, frange(0.05, 0.30, 0.05)),
		},
		{
			// absolute WHEN-gate plus a separation requirement: even if top1 clears
			// the bar, suppress unless it stands clear of top2 by `sep`. Tests
			// whether "the top hit must visibly win" cuts false injects that a flat
			// threshold lets through. WHAT is unchanged (keep >= abs).
			name: "absolute+sepgate",
			sel: func(s []studyCand, p studyParams) []studyCand {
				if len(s) == 0 || s[0].score < p.abs {
					return nil
				}
				if len(s) > 1 && s[0].score-s[1].score < p.sep {
					return nil
				}
				return keepAbove(s, p.abs)
			},
			grid: paramsAbsSep(frange(0.50, 0.66, 0.02), frange(0.0, 0.15, 0.03)),
		},
		{
			// absolute WHEN-gate plus an adaptive z-score requirement: suppress
			// unless top1 is at least `z` standard deviations above the candidate
			// set's mean. This reads the per-query score *shape* (does the top hit
			// stand out from its own neighbourhood?) rather than a flat level.
			name: "absolute+zgate",
			sel: func(s []studyCand, p studyParams) []studyCand {
				if len(s) == 0 || s[0].score < p.abs {
					return nil
				}
				if topZScore(s) < p.z {
					return nil
				}
				return keepAbove(s, p.abs)
			},
			grid: paramsAbsZ(frange(0.50, 0.66, 0.02), frange(0.0, 1.5, 0.25)),
		},
	}
}

// topZScore returns how many standard deviations the top (first) score sits
// above the mean of all candidate scores. A lone candidate, or an all-equal set
// (zero variance), returns +Inf so the z-gate never suppresses on shape alone
// there — the absolute floor remains the sole arbiter.
func topZScore(s []studyCand) float64 {
	if len(s) < 2 {
		return math.Inf(1)
	}
	var sum float64
	for _, c := range s {
		sum += c.score
	}
	mean := sum / float64(len(s))
	var ss float64
	for _, c := range s {
		d := c.score - mean
		ss += d * d
	}
	std := math.Sqrt(ss / float64(len(s)))
	// Treat a near-zero spread as no shape signal: all scores are effectively
	// equal, so the z-gate must not fire (float rounding can leave std a tiny
	// positive instead of exactly 0, which would otherwise yield a meaningless z).
	if std < 1e-9 {
		return math.Inf(1)
	}
	return (s[0].score - mean) / std
}

func paramsAbsK(abss []float64, ks []int) []studyParams {
	var out []studyParams
	for _, a := range abss {
		for _, k := range ks {
			out = append(out, studyParams{abs: a, k: k})
		}
	}
	return out
}

func paramsAbsSep(abss, seps []float64) []studyParams {
	var out []studyParams
	for _, a := range abss {
		for _, s := range seps {
			out = append(out, studyParams{abs: a, sep: s})
		}
	}
	return out
}

func paramsAbsZ(abss, zs []float64) []studyParams {
	var out []studyParams
	for _, a := range abss {
		for _, z := range zs {
			out = append(out, studyParams{abs: a, z: z})
		}
	}
	return out
}

func paramsFloorMargin(floors, margins []float64) []studyParams {
	var out []studyParams
	for _, fl := range floors {
		for _, m := range margins {
			out = append(out, studyParams{floor: fl, margin: m})
		}
	}
	return out
}

// --- selection primitives ---

func keepAbove(s []studyCand, cut float64) []studyCand {
	out := make([]studyCand, 0, len(s))
	for _, c := range s {
		if c.score < cut {
			break // sorted descending
		}
		out = append(out, c)
	}
	return out
}

func capN(s []studyCand, k int) []studyCand {
	if k <= 0 || k >= len(s) {
		return s
	}
	return s[:k]
}

// gapCut keeps the score-sorted prefix up to the largest relative gap between
// consecutive scores, which tends to separate a relevant cluster from the noise
// tail. At least the top item is always kept.
func gapCut(s []studyCand) []studyCand {
	if len(s) <= 1 {
		return s
	}
	bestIdx, bestGap := 1, -1.0
	for i := 1; i < len(s); i++ {
		gap := s[i-1].score - s[i].score
		if gap > bestGap {
			bestGap, bestIdx = gap, i
		}
	}
	return s[:bestIdx]
}

// --- param grid builders ---

func frange(lo, hi, step float64) []float64 {
	var out []float64
	for v := lo; v <= hi+1e-9; v += step {
		out = append(out, math.Round(v*100)/100)
	}
	return out
}

func paramsAbs(vals []float64) []studyParams {
	out := make([]studyParams, len(vals))
	for i, v := range vals {
		out[i] = studyParams{abs: v}
	}
	return out
}

func paramsRel(vals []float64) []studyParams {
	out := make([]studyParams, len(vals))
	for i, v := range vals {
		out[i] = studyParams{rel: v}
	}
	return out
}

func paramsFloor(vals []float64) []studyParams {
	out := make([]studyParams, len(vals))
	for i, v := range vals {
		out[i] = studyParams{floor: v}
	}
	return out
}

func paramsK(vals []int) []studyParams {
	out := make([]studyParams, len(vals))
	for i, v := range vals {
		out[i] = studyParams{k: v}
	}
	return out
}

func paramsFloorRel(floors, rels []float64) []studyParams {
	var out []studyParams
	for _, f := range floors {
		for _, r := range rels {
			out = append(out, studyParams{floor: f, rel: r})
		}
	}
	return out
}
