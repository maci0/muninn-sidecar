// This file contains the session-window decay study: an empirical check of the
// decayFactor (0.7/turn) and decayFloor (0.2) that govern how long a memory
// lingers in the multi-turn window after it was last recalled. Like the gate
// method-study (eval_study.go), it scores candidate parameters on a synthetic
// but principled model — here, memories that are recalled once and then stay
// *relevant* for a number of turns while fresh recall no longer surfaces them
// (exactly the case the carry-forward window exists to handle).
//
// The model's one assumption is the relevance-lifespan distribution: a memory
// recalled at turn t stays useful for L more turns, L ~ a small geometric-ish
// spread (most facts matter for a few turns, some longer). The decay/floor pair
// defines the window's *effective* lifespan (how many turns a non-refreshed
// memory survives before its decayed score drops below the floor); the study
// asks which pair best matches the true relevance lifespan — keeping memories
// while they help (recall) and dropping them once they don't (precision).
package inject

import (
	"math"
	"math/rand"
)

// windowMemory is one memory in a simulated session: recalled once at Intro with
// score Score, then genuinely relevant through turn Intro+Lifespan (inclusive).
type windowMemory struct {
	Intro    int
	Lifespan int
	Score    float64
}

// windowSession is a fixed number of turns plus the memories introduced in it.
type windowSession struct {
	Turns int
	Mems  []windowMemory
}

// genWindowSessions builds n deterministic sessions from seed. Each memory is
// recalled once (at Intro) and stays relevant for Lifespan more turns; fresh
// recall does not re-surface it (drift), so only the window can keep injecting
// it. Lifespan is a small geometric-ish spread; ~20% are one-off (Lifespan 0)
// that the window should drop immediately.
func genWindowSessions(seed int64, n int) []windowSession {
	rng := rand.New(rand.NewSource(seed))
	clamp := func(x float64) float64 { return math.Max(0, math.Min(1, x)) }
	sessions := make([]windowSession, 0, n)
	for i := 0; i < n; i++ {
		turns := 6 + rng.Intn(7) // 6..12 turns
		nMems := 3 + rng.Intn(5) // 3..7 memories
		mems := make([]windowMemory, 0, nMems)
		for m := 0; m < nMems; m++ {
			intro := rng.Intn(turns)
			lifespan := 0
			if rng.Float64() >= 0.2 { // 80% are multi-turn relevant
				// Geometric-ish: mostly 1-3, occasionally longer.
				lifespan = 1
				for lifespan < 6 && rng.Float64() < 0.45 {
					lifespan++
				}
			}
			score := clamp(0.6 + rng.Float64()*0.3) // [0.6, 0.9]
			mems = append(mems, windowMemory{Intro: intro, Lifespan: lifespan, Score: score})
		}
		sessions = append(sessions, windowSession{Turns: turns, Mems: mems})
	}
	return sessions
}

// scoreWindow simulates the window over every session with the given decay rate
// and returns macro-averaged precision/recall/F1 of "is this memory injected at
// this turn?" vs "is it actually relevant at this turn?".
//
// The inject condition is the PRODUCTION rule: a memory introduced at Intro is
// injected at turn t while its decayed score decay^(t-Intro)*Score clears the
// injection gate (`gate`, i.e. minScore ~0.6). The eviction floor (decayFloor)
// only governs how long a memory lingers in the window for a *future* refresh,
// not whether it is injected, so it does not enter this score — decay rate is
// the lever that sets the carry-forward injection lifespan. It is relevant at
// turn t if Intro <= t <= Intro+Lifespan.
func scoreWindow(sessions []windowSession, decay, gate float64) (prec, rec, f1 float64) {
	var sumP, sumR, sumF, count float64
	for _, s := range sessions {
		var tp, fp, fn int
		for t := 0; t < s.Turns; t++ {
			for _, m := range s.Mems {
				if t < m.Intro {
					continue
				}
				injected := m.Score*math.Pow(decay, float64(t-m.Intro)) >= gate
				relevant := t <= m.Intro+m.Lifespan
				switch {
				case injected && relevant:
					tp++
				case injected && !relevant:
					fp++
				case !injected && relevant:
					fn++
				}
			}
		}
		p, r := ratio(tp, tp+fp), ratio(tp, tp+fn)
		sumP += p
		sumR += r
		if p+r > 0 {
			sumF += 2 * p * r / (p + r)
		}
		count++
	}
	if count == 0 {
		return 0, 0, 0
	}
	return sumP / count, sumR / count, sumF / count
}

func ratio(a, b int) float64 {
	if b == 0 {
		return 0
	}
	return float64(a) / float64(b)
}

// WindowParams is one decay candidate and its score at the production gate.
type WindowParams struct {
	Decay     float64 `json:"decay"`
	Gate      float64 `json:"gate"`
	Precision float64 `json:"precision"`
	Recall    float64 `json:"recall"`
	F1        float64 `json:"f1"`
}

// RunWindowStudy sweeps the decay rate over n synthetic sessions at the
// production injection gate and returns every candidate sorted by F1 (best
// first). It is a tuning *tool*, not a mandate: the relevance-lifespan model is
// principled but uncalibrated (there is no real multi-turn relevance dataset),
// so it shows the decay precision/recall tradeoff rather than dictating a value.
func RunWindowStudy(seed int64, n int) []WindowParams {
	sessions := genWindowSessions(seed, n)
	decays := []float64{0.5, 0.6, 0.7, 0.8, 0.9}
	gate := defaultMinScore // inject while decayed score clears the gate
	var out []WindowParams
	for _, d := range decays {
		p, r, f := scoreWindow(sessions, d, gate)
		out = append(out, WindowParams{Decay: d, Gate: gate, Precision: p, Recall: r, F1: f})
	}
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j].F1 > out[j-1].F1; j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}
