package main

import (
	"context"

	"github.com/maci0/muninn-sidecar/internal/grounding"
)

// applyGrounding filters each result's recalled candidates (top-k by cosine) to
// those the grounder accepts, in place. A probe whose every candidate is rejected
// ends up with an empty Recalled set, so the gate suppresses it — which is the
// whole point for same-topic hard negatives. Returns the number of model calls.
// The grounding judge itself lives in internal/grounding (shared with the live
// injector and msc-qa); this is the bench-specific glue over recalledMemory.
func applyGrounding(ctx context.Context, g grounding.Grounder, results []probeResult, topK int) int {
	calls := 0
	for ri := range results {
		mems := results[ri].Recalled
		if len(mems) == 0 {
			continue
		}
		sortByVecDesc(mems) // ground the candidates a gate would consider first
		n := topK
		if n <= 0 || n > len(mems) {
			n = len(mems)
		}
		kept := make([]recalledMemory, 0, n)
		for i := 0; i < n; i++ {
			calls++
			if g.Grounded(ctx, results[ri].Query, mems[i].Content) {
				kept = append(kept, mems[i])
			}
		}
		results[ri].Recalled = kept
	}
	return calls
}

func sortByVecDesc(mems []recalledMemory) {
	for i := 1; i < len(mems); i++ {
		for j := i; j > 0 && mems[j].VectorScore > mems[j-1].VectorScore; j-- {
			mems[j], mems[j-1] = mems[j-1], mems[j]
		}
	}
}
