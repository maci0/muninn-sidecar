// This file contains the injection threshold: its default, the Otsu-based
// calibration that retunes it from an observed recall-score sample, and the
// rolling online calibration that tracks drift between requests.
package inject

import (
	"log/slog"
	"math"
)

// Online-calibration tuning constants.
const (
	calibCap         = 400 // rolling sample window
	calibMinSamples  = 40  // first calibration once this many scores seen
	calibRefreshEach = 30  // recalibrate every N recalls thereafter (track drift)
)

// observeCalibration feeds the effective scores of a fresh recall into the
// rolling sample and retunes minScore toward the noise/relevant valley once
// enough data is seen, then periodically to track drift. No-op unless
// auto-calibration is enabled.
func (inj *Injector) observeCalibration(mems []memory) {
	if !inj.autoCalibrate || len(mems) == 0 {
		return
	}
	inj.mu.Lock()
	defer inj.mu.Unlock()

	for _, m := range mems {
		inj.calibScores = append(inj.calibScores, m.Score)
	}
	if len(inj.calibScores) > calibCap {
		inj.calibScores = inj.calibScores[len(inj.calibScores)-calibCap:]
	}
	inj.recallsSinceCalib++

	due := (!inj.calibrated && len(inj.calibScores) >= calibMinSamples) ||
		(inj.calibrated && inj.recallsSinceCalib >= calibRefreshEach)
	if !due {
		return
	}
	newT := CalibrateThreshold(inj.calibScores)
	if newT != inj.minScore {
		slog.Info("inject: auto-calibrated injection threshold",
			"old", inj.minScore, "new", newT, "samples", len(inj.calibScores))
	}
	inj.minScore = newT
	inj.calibrated = true
	inj.recallsSinceCalib = 0
}

// Calibration bounds: a wide clamp so the gate can adapt to low-cosine
// deployments (e.g. short query vs long memory, where the relevant cluster sits
// near 0.2) as well as high-cosine ones, while still rejecting degenerate ends.
const (
	calibMinThreshold = 0.10
	calibMaxThreshold = 0.90
	// calibMinSeparation is the minimum gap between the two cluster means (in
	// score units) for the sample to count as bimodal. Below it the distribution
	// is effectively unimodal — no real noise/relevant split — so we keep the
	// default prior instead of trusting an arbitrary Otsu cut.
	calibMinSeparation = 0.08
)

// CalibrateThreshold derives an injection threshold from a sample of recall
// scores instead of relying on the hand-picked default. Recall scores tend to be
// bimodal — a noise cluster and a relevant cluster — so it finds the valley
// between them with Otsu's method (the split maximizing between-class variance).
//
// Crucially, the valley is adopted ONLY when the two clusters are meaningfully
// separated (>= calibMinSeparation); on a unimodal sample it returns
// defaultMinScore. This lets the gate self-tune to whatever the deployment's
// embedding/query shape produces — including low-cosine vaults where a fixed 0.6
// would suppress everything — without latching onto noise. Returns defaultMinScore
// for a too-small sample.
func CalibrateThreshold(scores []float64) float64 {
	t, _, _, _ := CalibrateThresholdDetail(scores)
	return t
}

// CalibrateThresholdDetail returns the calibrated threshold together with the
// two Otsu cluster means and their separation (score units). The extra values
// let the calibration-validation instrument in cmd/msc-bench see where the
// valley sits relative to the relevant cluster. threshold is already clamped
// and falls back to defaultMinScore when not confidently bimodal.
func CalibrateThresholdDetail(scores []float64) (threshold, noiseMean, relMean, sep float64) {
	if len(scores) < 20 {
		return defaultMinScore, 0, 0, 0
	}
	const bins = 50
	hist := make([]int, bins)
	for _, s := range scores {
		if math.IsNaN(s) {
			continue // math.Max/Min leave NaN alone, and int(NaN*bins) indexes hist out of range
		}
		s = math.Max(0, math.Min(1, s))
		b := int(s * bins)
		if b >= bins {
			b = bins - 1
		}
		hist[b]++
	}
	// total counts what the histogram actually holds, not len(scores): a
	// skipped sample is in neither cluster, and counting it in the foreground
	// weight would drag both Otsu means toward the background.
	var total int
	var sumAll float64
	for b, c := range hist {
		total += c
		sumAll += float64(b) * float64(c)
	}
	if total < 20 {
		return defaultMinScore, 0, 0, 0
	}

	var wB int
	var sumB float64
	bestVar, bestT, bestSep, bestMB, bestMF := -1.0, defaultMinScore, 0.0, 0.0, 0.0
	for t := 0; t < bins; t++ {
		wB += hist[t]
		if wB == 0 {
			continue
		}
		wF := total - wB
		if wF == 0 {
			break
		}
		sumB += float64(t) * float64(hist[t])
		mB := sumB / float64(wB)
		mF := (sumAll - sumB) / float64(wF)
		between := float64(wB) * float64(wF) * (mB - mF) * (mB - mF)
		if between > bestVar {
			bestVar = between
			bestT = (float64(t) + 1) / float64(bins) // upper edge of the noise bin
			bestSep = (mF - mB) / float64(bins)      // cluster-mean gap in score units
			bestMB, bestMF = mB/float64(bins), mF/float64(bins)
		}
	}
	if bestSep < calibMinSeparation {
		return defaultMinScore, bestMB, bestMF, bestSep // not confidently bimodal — keep the prior
	}
	return math.Max(calibMinThreshold, math.Min(calibMaxThreshold, bestT)), bestMB, bestMF, bestSep
}
