package main

import (
	"fmt"
	"os"
)

// logf prints a human-friendly message to stderr with the msc: prefix.
func logf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "msc: "+format+"\n", args...)
}

// logerr prints a human-friendly error to stderr with the msc: error: prefix.
func logerr(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "msc: error: "+format+"\n", args...)
}

// closestMatch returns the best match from candidates if it's within a
// reasonable edit distance (<=2), or "" if nothing is close enough. Used
// for "did you mean?" suggestions on typos.
func closestMatch(input string, candidates []string) string {
	best := ""
	bestDist := 3 // only suggest if distance <= 2
	for _, c := range candidates {
		d := levenshtein(input, c)
		if d < bestDist {
			bestDist = d
			best = c
		}
	}
	return best
}

func levenshtein(a, b string) int {
	ra, rb := []rune(a), []rune(b)
	if len(ra) == 0 {
		return len(rb)
	}
	if len(rb) == 0 {
		return len(ra)
	}
	prev := make([]int, len(rb)+1)
	curr := make([]int, len(rb)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(ra); i++ {
		curr[0] = i
		for j := 1; j <= len(rb); j++ {
			cost := 1
			if ra[i-1] == rb[j-1] {
				cost = 0
			}
			curr[j] = min(curr[j-1]+1, min(prev[j]+1, prev[j-1]+cost))
		}
		prev, curr = curr, prev
	}
	return prev[len(rb)]
}
