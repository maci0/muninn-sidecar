package main

import (
	"sort"
	"strings"
)

// --- ranking the gold concept within a recall result ---

// rankOf returns the 0-based rank of the gold concept when results are sorted by
// the given field descending; -1 if gold is empty or not present.
func rankOf(mems []recalledMemory, gold string, field func(recalledMemory) float64) int {
	if gold == "" {
		return -1
	}
	sorted := append([]recalledMemory(nil), mems...)
	sort.SliceStable(sorted, func(i, j int) bool { return field(sorted[i]) > field(sorted[j]) })
	for i, m := range sorted {
		if m.Concept == gold {
			return i
		}
	}
	return -1
}

// articleOf returns the article key of a concept (the part before '#'), or the
// whole concept when there is no '#'.
func articleOf(concept string) string {
	if i := strings.IndexByte(concept, '#'); i >= 0 {
		return concept[:i]
	}
	return concept
}

// rankArticleOf returns the 0-based rank, sorted by field desc, of the first
// recalled memory sharing gold's article; -1 if gold empty or none match.
func rankArticleOf(mems []recalledMemory, gold string, field func(recalledMemory) float64) int {
	if gold == "" {
		return -1
	}
	art := articleOf(gold)
	sorted := append([]recalledMemory(nil), mems...)
	sort.SliceStable(sorted, func(i, j int) bool { return field(sorted[i]) > field(sorted[j]) })
	for i, m := range sorted {
		if articleOf(m.Concept) == art {
			return i
		}
	}
	return -1
}
