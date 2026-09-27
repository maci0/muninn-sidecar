package main

import "hash/fnv"

// --- analysis ---

type retrievalMetrics struct {
	R1, R3, R5 float64
	MRR        float64
}

type gatePoint struct {
	Threshold   float64 `json:"threshold"`
	GateAcc     float64 `json:"gate_accuracy"`
	GateF1      float64 `json:"gate_f1"`
	InjectWhenS float64 `json:"inject_when_should"`   // sensitivity: present probes that injected
	SuppressOK  float64 `json:"suppress_when_absent"` // specificity: absent probes suppressed
	WhatCorrect float64 `json:"what_correct"`         // present probes where gold is the top kept item
}

type benchReport struct {
	NPresent        int              `json:"n_present"`
	NAbsent         int              `json:"n_absent"`
	RetrievalScore  retrievalMetrics `json:"retrieval_by_score_order"`
	RetrievalVec    retrievalMetrics `json:"retrieval_by_vector_order"`
	RetrievalRerank retrievalMetrics `json:"retrieval_by_rerank"`
	RetrievalArtVec retrievalMetrics `json:"retrieval_article_by_vector"`
	GateByScore     []gatePoint      `json:"gate_by_score"`
	GateByVec       []gatePoint      `json:"gate_by_vector"`
	BestScore       gatePoint        `json:"best_by_score"`
	BestVec         gatePoint        `json:"best_by_vector"`
	// Held-out variants: threshold picked on the tune half (probes whose query
	// hash is even), metrics measured on the eval half (odd hash).
	// BestScore/BestVec are in-sample optima and thus optimistic.
	HeldOutScore gatePoint `json:"heldout_by_score"`
	HeldOutVec   gatePoint `json:"heldout_by_vector"`
}

func analyze(results []probeResult) benchReport {
	present, absent := splitPresentAbsent(results)

	rep := benchReport{NPresent: len(present), NAbsent: len(absent)}
	rep.RetrievalScore = retrieval(present, func(r probeResult) int { return r.RankByScore })
	rep.RetrievalVec = retrieval(present, func(r probeResult) int { return r.RankByVec })
	rep.RetrievalRerank = retrieval(present, func(r probeResult) int { return r.RankRerank })
	rep.RetrievalArtVec = retrieval(present, func(r probeResult) int { return r.RankArtVec })

	scoreThresholds := frange(0.3, 1.2, 0.05)
	vecThresholds := frange(0.30, 0.85, 0.025)
	scoreOf := func(m recalledMemory) float64 { return m.Score }
	vecOf := func(m recalledMemory) float64 { return m.VectorScore }
	rep.GateByScore = gateSweep(present, absent, scoreThresholds, scoreOf)
	rep.GateByVec = gateSweep(present, absent, vecThresholds, vecOf)
	rep.BestScore = bestGate(rep.GateByScore)
	rep.BestVec = bestGate(rep.GateByVec)
	rep.HeldOutScore = heldOutBest(results, scoreThresholds, scoreOf)
	rep.HeldOutVec = heldOutBest(results, vecThresholds, vecOf)
	return rep
}

func retrieval(present []probeResult, rank func(probeResult) int) retrievalMetrics {
	if len(present) == 0 {
		return retrievalMetrics{}
	}
	var r1, r3, r5, mrr float64
	for _, r := range present {
		k := rank(r)
		if k < 0 {
			continue
		}
		if k == 0 {
			r1++
		}
		if k < 3 {
			r3++
		}
		if k < 5 {
			r5++
		}
		mrr += 1.0 / float64(k+1)
	}
	n := float64(len(present))
	return retrievalMetrics{R1: r1 / n, R3: r3 / n, R5: r5 / n, MRR: mrr / n}
}

// gateSweep scores the when-to-inject decision at each threshold on the given
// field. The in-flight rule is: inject iff the top result's field value >= T.
func gateSweep(present, absent []probeResult, thresholds []float64, field func(recalledMemory) float64) []gatePoint {
	pts := make([]gatePoint, 0, len(thresholds))
	for _, t := range thresholds {
		var injectWhenShould, suppressWhenAbsent, whatCorrect float64
		tp, fp, fn := 0, 0, 0
		for _, r := range present {
			top, ok := topByField(r.Recalled, field)
			injected := ok && top >= t
			if injected {
				injectWhenShould++
				tp++
				if c, ok2 := topConcept(r.Recalled, field); ok2 && c == r.Gold {
					whatCorrect++
				}
			} else {
				fn++
			}
		}
		for _, r := range absent {
			top, ok := topByField(r.Recalled, field)
			if ok && top >= t {
				fp++
			} else {
				suppressWhenAbsent++
			}
		}
		nP, nA := float64(len(present)), float64(len(absent))
		acc := safeDiv(injectWhenShould+suppressWhenAbsent, nP+nA)
		prec, rec := 0.0, 0.0
		if tp+fp > 0 {
			prec = float64(tp) / float64(tp+fp)
		}
		if tp+fn > 0 {
			rec = float64(tp) / float64(tp+fn)
		}
		f1 := 0.0
		if prec+rec > 0 {
			f1 = 2 * prec * rec / (prec + rec)
		}
		pts = append(pts, gatePoint{
			Threshold:   t,
			GateAcc:     acc,
			GateF1:      f1,
			InjectWhenS: safeDiv(injectWhenShould, nP),
			SuppressOK:  safeDiv(suppressWhenAbsent, nA),
			WhatCorrect: safeDiv(whatCorrect, nP),
		})
	}
	return pts
}

func bestGate(pts []gatePoint) gatePoint {
	best := gatePoint{GateAcc: -1}
	for _, p := range pts {
		if p.GateAcc > best.GateAcc || (p.GateAcc == best.GateAcc && p.GateF1 > best.GateF1) {
			best = p
		}
	}
	return best
}

// tuneHalf assigns a probe to the tune half of the held-out split by hashing
// its query. Result-index parity would NOT work: the synthetic generators pick
// question category by index modulo an even number, so an even/odd index split
// puts disjoint category sets in each half and the "held-out" metric would
// measure cross-category transfer instead of generalization. The hash is
// deterministic so runs stay comparable.
func tuneHalf(query string) bool {
	h := fnv.New32a()
	h.Write([]byte(query))
	return h.Sum32()%2 == 0
}

// heldOutBest counters the in-sample optimism of bestGate, which selects and
// scores the threshold on the same probes. It splits results into a tune half
// and an eval half by query hash (see tuneHalf), picks the best threshold on
// the tune half, and returns that threshold's gate metrics measured on the
// eval half.
func heldOutBest(results []probeResult, thresholds []float64, field func(recalledMemory) float64) gatePoint {
	var tuneP, tuneA, evalP, evalA []probeResult
	for _, r := range results {
		switch {
		case tuneHalf(r.Query) && r.Present:
			tuneP = append(tuneP, r)
		case tuneHalf(r.Query):
			tuneA = append(tuneA, r)
		case r.Present:
			evalP = append(evalP, r)
		default:
			evalA = append(evalA, r)
		}
	}
	best := bestGate(gateSweep(tuneP, tuneA, thresholds, field))
	return gateSweep(evalP, evalA, []float64{best.Threshold}, field)[0]
}

func topByField(mems []recalledMemory, field func(recalledMemory) float64) (float64, bool) {
	if len(mems) == 0 {
		return 0, false
	}
	max := field(mems[0])
	for _, m := range mems[1:] {
		if v := field(m); v > max {
			max = v
		}
	}
	return max, true
}

func topConcept(mems []recalledMemory, field func(recalledMemory) float64) (string, bool) {
	if len(mems) == 0 {
		return "", false
	}
	best, bestV := mems[0].Concept, field(mems[0])
	for _, m := range mems[1:] {
		if v := field(m); v > bestV {
			best, bestV = m.Concept, v
		}
	}
	return best, true
}

func frange(lo, hi, step float64) []float64 {
	var out []float64
	for v := lo; v <= hi+1e-9; v += step {
		out = append(out, float64(int(v*1000+0.5))/1000)
	}
	return out
}

func safeDiv(a, b float64) float64 {
	if b == 0 {
		return 0
	}
	return a / b
}
