package main

import (
	"regexp"
	"strings"
)

// SQuAD-style answer scoring: normalize (lowercase, strip punctuation/articles,
// collapse whitespace), then exact-match and token-F1 against the gold answers,
// taking the max over multiple acceptable golds.

var (
	punctRe = regexp.MustCompile(`[^\p{L}\p{N}\s]`)
	wsRe    = regexp.MustCompile(`\s+`)
	artRe   = regexp.MustCompile(`\b(a|an|the)\b`)
)

func normalizeAnswer(s string) string {
	s = strings.ToLower(s)
	s = punctRe.ReplaceAllString(s, " ")
	s = artRe.ReplaceAllString(s, " ")
	s = wsRe.ReplaceAllString(s, " ")
	return strings.TrimSpace(s)
}

// exactMatch returns 1.0 if the normalized prediction equals any gold.
func exactMatch(pred string, golds []string) float64 {
	np := normalizeAnswer(pred)
	for _, g := range golds {
		if np == normalizeAnswer(g) {
			return 1
		}
	}
	return 0
}

// tokenF1 returns the max token-overlap F1 of the prediction against any gold.
func tokenF1(pred string, golds []string) float64 {
	pt := strings.Fields(normalizeAnswer(pred))
	best := 0.0
	for _, g := range golds {
		gt := strings.Fields(normalizeAnswer(g))
		if f := f1Tokens(pt, gt); f > best {
			best = f
		}
	}
	return best
}

func f1Tokens(pred, gold []string) float64 {
	if len(pred) == 0 && len(gold) == 0 {
		return 1
	}
	if len(pred) == 0 || len(gold) == 0 {
		return 0
	}
	goldCount := map[string]int{}
	for _, g := range gold {
		goldCount[g]++
	}
	common := 0
	for _, p := range pred {
		if goldCount[p] > 0 {
			goldCount[p]--
			common++
		}
	}
	if common == 0 {
		return 0
	}
	prec := float64(common) / float64(len(pred))
	rec := float64(common) / float64(len(gold))
	return 2 * prec * rec / (prec + rec)
}

// containsAnswer reports whether any gold answer appears (normalized) in text —
// used to measure whether injected context actually carries the answer.
func containsAnswer(text string, golds []string) bool {
	nt := normalizeAnswer(text)
	for _, g := range golds {
		ng := normalizeAnswer(g)
		if ng != "" && strings.Contains(nt, ng) {
			return true
		}
	}
	return false
}
