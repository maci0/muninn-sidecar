package main

import (
	"encoding/json"
	"fmt"
	"math"
	"os"

	"github.com/maci0/muninn-sidecar/internal/inject"
)

// splitPresentAbsent partitions probe results by their Present label.
func splitPresentAbsent(results []probeResult) (present, absent []probeResult) {
	for _, r := range results {
		if r.Present {
			present = append(present, r)
		} else {
			absent = append(absent, r)
		}
	}
	return present, absent
}

// deepCopyResults copies results with independent Recalled slices so grounding
// can filter them without disturbing the cosine-gate report.
func deepCopyResults(results []probeResult) []probeResult {
	out := make([]probeResult, len(results))
	copy(out, results)
	for i := range out {
		out[i].Recalled = append([]recalledMemory(nil), results[i].Recalled...)
	}
	return out
}

func printReport(rep benchReport, results []probeResult) {
	fmt.Printf("\n=== msc-bench: real MuninnDB retrieval + when-to-inject ===\n")
	fmt.Printf("present probes: %d   absent probes: %d\n\n", rep.NPresent, rep.NAbsent)

	fmt.Printf("RETRIEVAL (did recall surface the gold memory?)\n")
	fmt.Printf("  ranked by score      R@1=%.2f R@3=%.2f R@5=%.2f MRR=%.3f\n",
		rep.RetrievalScore.R1, rep.RetrievalScore.R3, rep.RetrievalScore.R5, rep.RetrievalScore.MRR)
	fmt.Printf("  ranked by vector     R@1=%.2f R@3=%.2f R@5=%.2f MRR=%.3f\n",
		rep.RetrievalVec.R1, rep.RetrievalVec.R3, rep.RetrievalVec.R5, rep.RetrievalVec.MRR)
	fmt.Printf("  reranked             R@1=%.2f R@3=%.2f R@5=%.2f MRR=%.3f\n",
		rep.RetrievalRerank.R1, rep.RetrievalRerank.R3, rep.RetrievalRerank.R5, rep.RetrievalRerank.MRR)
	fmt.Printf("  article-level (vec)  R@1=%.2f R@3=%.2f R@5=%.2f MRR=%.3f  (same-article hit = useful)\n\n",
		rep.RetrievalArtVec.R1, rep.RetrievalArtVec.R3, rep.RetrievalArtVec.R5, rep.RetrievalArtVec.MRR)

	printGate("WHEN-TO-INJECT gate on `score` (composite)", rep.GateByScore)
	printGate("WHEN-TO-INJECT gate on `vector_score` (cosine)", rep.GateByVec)

	fmt.Printf("\nBEST gate on score : T=%.3f acc=%.2f f1=%.2f (inject@should=%.2f suppress@absent=%.2f what=%.2f)\n",
		rep.BestScore.Threshold, rep.BestScore.GateAcc, rep.BestScore.GateF1, rep.BestScore.InjectWhenS, rep.BestScore.SuppressOK, rep.BestScore.WhatCorrect)
	fmt.Printf("BEST gate on vector: T=%.3f acc=%.2f f1=%.2f (inject@should=%.2f suppress@absent=%.2f what=%.2f)\n",
		rep.BestVec.Threshold, rep.BestVec.GateAcc, rep.BestVec.GateF1, rep.BestVec.InjectWhenS, rep.BestVec.SuppressOK, rep.BestVec.WhatCorrect)
	fmt.Printf("BEST gate (held-out) on score : T=%.3f acc=%.2f f1=%.2f (inject@should=%.2f suppress@absent=%.2f what=%.2f)\n",
		rep.HeldOutScore.Threshold, rep.HeldOutScore.GateAcc, rep.HeldOutScore.GateF1, rep.HeldOutScore.InjectWhenS, rep.HeldOutScore.SuppressOK, rep.HeldOutScore.WhatCorrect)
	fmt.Printf("BEST gate (held-out) on vector: T=%.3f acc=%.2f f1=%.2f (inject@should=%.2f suppress@absent=%.2f what=%.2f)\n",
		rep.HeldOutVec.Threshold, rep.HeldOutVec.GateAcc, rep.HeldOutVec.GateF1, rep.HeldOutVec.InjectWhenS, rep.HeldOutVec.SuppressOK, rep.HeldOutVec.WhatCorrect)

	// Validate auto-calibration: feed the observed cosines (what the injector's
	// observeCalibration samples) to the production CalibrateThreshold and compare
	// to the empirical best. They should land close — proof the shipped online
	// calibration discovers the right per-vault gate without the labeled sweep.
	var cosines []float64
	for _, r := range results {
		for _, m := range r.Recalled {
			cosines = append(cosines, m.VectorScore)
		}
	}
	if len(cosines) > 0 {
		calT, noiseMean, relMean, sep := inject.CalibrateThresholdDetail(cosines)
		fmt.Printf("AUTO-CALIBRATED gate: T=%.3f (from %d cosines; best %.3f, |Δ|=%.3f; clusters noise=%.3f rel=%.3f sep=%.3f)\n",
			calT, len(cosines), rep.BestVec.Threshold, math.Abs(calT-rep.BestVec.Threshold), noiseMean, relMean, sep)
	}
}

func printGate(title string, pts []gatePoint) {
	fmt.Printf("\n%s\n", title)
	fmt.Printf("%8s %5s %5s %10s %9s %6s\n", "thresh", "acc", "f1", "inj@should", "supp@abs", "what")
	for _, p := range pts {
		fmt.Printf("%8.3f %5.2f %5.2f %10.2f %9.2f %6.2f\n",
			p.Threshold, p.GateAcc, p.GateF1, p.InjectWhenS, p.SuppressOK, p.WhatCorrect)
	}
}

// reportGroundedGate prints the gate metric after grounding. Because grounding
// already removes non-answering candidates, the gate is reported at a permissive
// cosine floor (0.30) — suppression now comes from grounding, not the threshold.
// A run with no probes measured nothing, so it prints no gate rather than an
// all-zero one that reads like a score: gateSweep always returns one point per
// threshold, so emptiness is a property of the inputs, not of the sweep.
func reportGroundedGate(label string, present, absent []probeResult) {
	if len(present)+len(absent) == 0 {
		return
	}
	vec := func(m recalledMemory) float64 { return m.VectorScore }
	p := gateSweep(present, absent, []float64{0.30}, vec)[0]
	fmt.Printf("\nGROUNDED GATE (%s, cosine>=0.30 AND model says the passage answers the query)\n", label)
	fmt.Printf("  acc=%.2f f1=%.2f inject@should=%.2f suppress@absent=%.2f what=%.2f\n",
		p.GateAcc, p.GateF1, p.InjectWhenS, p.SuppressOK, p.WhatCorrect)
}

// writeQA dumps present probes as a generic QA JSON ([{question,answer}]) that
// msc-qa reads with -dataset generic, returning the pair count. Probes without
// an answer span are an error: writing an empty file would silently give a
// later msc-qa run zero questions.
func writeQA(path string, probes []probe) (int, error) {
	type qa struct {
		Question string `json:"question"`
		Answer   string `json:"answer"`
	}
	out := make([]qa, 0, len(probes))
	for _, p := range probes {
		if p.Present && p.Answer != "" {
			out = append(out, qa{Question: p.Query, Answer: p.Answer})
		}
	}
	if len(out) == 0 {
		return 0, fmt.Errorf("no present probes carry an answer span; -dump-qa needs a corpus with answers (squad, hotpot, agentmem)")
	}
	data, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		return 0, err
	}
	return len(out), os.WriteFile(path, data, 0o644)
}
