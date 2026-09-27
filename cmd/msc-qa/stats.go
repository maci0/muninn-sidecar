package main

import (
	"fmt"
	"math/rand"
	"sort"
	"strings"
)

// armAgg accumulates per-question scores for one arm. Scores are kept as
// parallel slices, not running sums, so paired bootstrap CIs can resample
// question indices after the fact; failed model calls keep their index for
// pairing (ok=false) but are excluded from every aggregate.
type armAgg struct {
	ems, f1s  []float64
	ok        []bool
	contained int
}

func (a *armAgg) add(em, f1 float64, contains bool) {
	a.ems = append(a.ems, em)
	a.f1s = append(a.f1s, f1)
	a.ok = append(a.ok, true)
	if contains {
		a.contained++
	}
}

// fail records a failed model call for the next question index.
func (a *armAgg) fail() {
	a.ems = append(a.ems, 0)
	a.f1s = append(a.f1s, 0)
	a.ok = append(a.ok, false)
}

func (a *armAgg) n() int {
	n := 0
	for _, k := range a.ok {
		if k {
			n++
		}
	}
	return n
}
func (a *armAgg) failN() int  { return len(a.ok) - a.n() }
func (a *armAgg) em() float64 { return safe(sumWhere(a.ems, a.ok), float64(a.n())) }
func (a *armAgg) f1() float64 { return safe(sumWhere(a.f1s, a.ok), float64(a.n())) }

// containment is the fraction of answers that contain a gold answer as a
// contiguous token run. It is looser than exactMatch (which requires the whole
// normalized answer), so it shows how often the model got the fact right inside
// a longer sentence — the signal that separates "wrong" from "right but verbose".
func (a *armAgg) containment() float64 { return safe(float64(a.contained), float64(a.n())) }

// containmentLine renders the per-arm containment rates for one model's row.
func containmentLine(agg []armAgg, armNames []string) string {
	parts := make([]string, 0, len(agg))
	for a, name := range armNames {
		parts = append(parts, fmt.Sprintf("%s %.2f", name, agg[a].containment()))
	}
	return strings.Join(parts, "  ")
}

func sumWhere(xs []float64, ok []bool) float64 {
	s := 0.0
	for i, x := range xs {
		if ok[i] {
			s += x
		}
	}
	return s
}

func safe(x, y float64) float64 {
	if y == 0 {
		return 0
	}
	return x / y
}

// Paired bootstrap over per-question F1 differences; draws and seed are fixed
// so repeated runs report identical intervals.
const (
	bootstrapDraws = 2000
	bootstrapSeed  = 1
)

// pairedF1Deltas returns arm-minus-base per-question F1 differences over the
// questions where both arms' model calls succeeded.
func pairedF1Deltas(base, arm *armAgg) []float64 {
	var d []float64
	for i := 0; i < min(len(base.f1s), len(arm.f1s)); i++ {
		if base.ok[i] && arm.ok[i] {
			d = append(d, arm.f1s[i]-base.f1s[i])
		}
	}
	return d
}

// bootstrapCI returns the 95% percentile interval for the mean of deltas by
// resampling indices with replacement.
func bootstrapCI(deltas []float64, draws int, seed int64) (lo, hi float64) {
	if len(deltas) == 0 {
		return 0, 0
	}
	rng := rand.New(rand.NewSource(seed))
	means := make([]float64, draws)
	for d := range means {
		s := 0.0
		for range deltas {
			s += deltas[rng.Intn(len(deltas))]
		}
		means[d] = s / float64(len(deltas))
	}
	sort.Float64s(means)
	return means[int(0.025*float64(draws))], means[int(0.975*float64(draws))]
}

// deltaCI formats the mean paired F1 delta of arm vs base with its 95%
// bootstrap CI, e.g. "+0.47 [+0.30, +0.62]".
func deltaCI(base, arm *armAgg) string {
	d := pairedF1Deltas(base, arm)
	s := 0.0
	for _, x := range d {
		s += x
	}
	lo, hi := bootstrapCI(d, bootstrapDraws, bootstrapSeed)
	return fmt.Sprintf("%+.2f [%+.2f, %+.2f]", safe(s, float64(len(d))), lo, hi)
}

// unreliable reports whether any arm lost more than 10% of its model calls;
// the excluded failures could bias that row's comparison.
func unreliable(aggs []armAgg) bool {
	for _, a := range aggs {
		if total := len(a.ok); total > 0 && a.failN()*10 > total {
			return true
		}
	}
	return false
}
