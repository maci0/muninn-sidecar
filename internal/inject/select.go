// This file contains the selection policy: which of the candidate memories
// recalled this turn are actually worth injecting. That is the fitness filter
// (memory.go), the absolute cosine threshold (which doubles as the *when* to
// inject decision), near-duplicate removal, and resolution of contradictions
// between kept memories.
package inject

import (
	"log/slog"
	"slices"
	"strings"
	"time"
)

// dupTokenOverlap is the Jaccard similarity (over lowercased word sets) above
// which two memories are considered near-duplicates. A content-overlap pair
// keeps the higher-scored memory (a same-concept pair is decided by supersedes
// instead), so redundant memories don't waste the budget or dilute the injected
// context. 0.8 catches re-phrasings and supersets without collapsing genuinely
// distinct memories that merely share vocabulary.
const dupTokenOverlap = 0.8

// selectForInjection is the full inject decision for a turn. It expects input
// pre-sorted by effective score (descending), as mergeMemories returns, and
// applies two filters plus a fitness check:
//
//  0. Fitness: a memory MuninnDB marks dead or untrusted is dropped whatever it
//     scored (injectable, in memory.go).
//
//  1. Absolute threshold (minScore): keep only memories whose effective score is
//     at least minScore. Because this drops every candidate when none is
//     confident enough, it decides *when* to inject (an empty result suppresses
//     the turn) and *what* to inject in one step. The empirical method study
//     (eval_study.go) found this single-threshold rule matches a separate
//     relative cutoff + gate while being simpler and wasting less budget.
//
//  2. Near-duplicate removal: a memory is dropped if it duplicates an
//     already-kept memory — either by identical normalized concept or by high
//     word-set overlap of content. This keeps the injected block from spending
//     budget on redundant restatements of the same fact.
//
//     For same-concept duplicates (one concept = one fact), the *fresher* memory
//     wins rather than the higher-cosine one: an updated fact ("migrated to
//     Postgres") supersedes the stale restatement it duplicates ("we use MySQL"),
//     even if the stale one scored marginally higher. This is the anti-staleness
//     behavior a long-lived vault needs; recall ranks by similarity, not by
//     which statement is currently true. Cross-concept content-overlap dups keep
//     the higher-cosine one (they may be genuinely distinct facts).
//
// Survivors are then passed through resolveConflicts. minScore <= 0 disables
// the threshold (every recalled memory that passes the fitness check is
// eligible).
func selectForInjection(merged []memory, minScore float64) []memory {
	if len(merged) == 0 {
		return merged
	}

	kept := make([]memory, 0, len(merged))
	keptTokens := make([][]string, 0, len(merged))
	conceptIdx := make(map[string]int, len(merged))

	for _, m := range merged {
		if minScore > 0 && m.Score < minScore {
			break // sorted descending — nothing after this clears the threshold
		}

		if !injectable(m) {
			slog.Debug("inject: skipped unfit memory", "id", m.ID, "state", m.State, "trust", m.Trust)
			continue
		}

		concept := normalizeConcept(m.Concept)
		if concept != "" {
			if i, ok := conceptIdx[concept]; ok {
				// Same fact already kept: keep the current version. MuninnDB's
				// stale annotation is authoritative (a non-stale memory supersedes a
				// stale duplicate); created_at breaks ties when staleness matches.
				if supersedes(m, kept[i]) {
					slog.Debug("inject: replaced stale same-concept memory with current", "id", m.ID, "old_id", kept[i].ID, "old_stale", kept[i].Annotations.Stale, "new_stale", m.Annotations.Stale, "old_created", kept[i].CreatedAt, "new_created", m.CreatedAt)
					kept[i] = m
					keptTokens[i] = wordSet(m.Content)
				} else {
					slog.Debug("inject: skipped duplicate-concept memory (not current)", "id", m.ID)
				}
				continue
			}
		}

		tokens := wordSet(m.Content)
		if isNearDuplicate(tokens, keptTokens) {
			slog.Debug("inject: skipped near-duplicate memory", "id", m.ID)
			continue
		}

		kept = append(kept, m)
		keptTokens = append(keptTokens, tokens)
		if concept != "" {
			conceptIdx[concept] = len(kept) - 1
		}
	}

	return resolveConflicts(kept)
}

// resolveConflicts drops the superseded side of any contradiction among the kept
// memories, using MuninnDB's conflicts_with annotation. Injecting both sides of a
// contradiction ("deploys to AWS" + "never AWS, only GCP") feeds the agent
// mutually-exclusive facts; keep only the side that supersedes() (non-stale, then
// newer). Conflicts are checked both directions (the edge may be annotated on
// either memory). O(n²) over the already-small kept set.
func resolveConflicts(kept []memory) []memory {
	if len(kept) < 2 {
		return kept
	}
	drop := make([]bool, len(kept))
	for i := range kept {
		for j := i + 1; j < len(kept); j++ {
			if drop[i] || drop[j] || !conflicts(kept[i], kept[j]) {
				continue
			}
			// Keep the superseding side; drop the other.
			if supersedes(kept[j], kept[i]) {
				drop[i] = true
			} else {
				drop[j] = true
			}
			slog.Debug("inject: dropped contradicted memory", "kept_a", kept[i].ID, "kept_b", kept[j].ID, "dropped_i", drop[i])
		}
	}
	out := kept[:0]
	for i, m := range kept {
		if !drop[i] {
			out = append(out, m)
		}
	}
	return out
}

// conflicts reports whether a and b are flagged as contradicting each other
// (the conflicts_with edge may be annotated on either side).
func conflicts(a, b memory) bool {
	return slices.Contains(a.Annotations.ConflictsWith, b.ID) ||
		slices.Contains(b.Annotations.ConflictsWith, a.ID)
}

// supersedes reports whether candidate should replace the kept same-concept
// memory: a non-stale memory supersedes a stale one (MuninnDB's annotation is
// authoritative); when staleness matches, the one with the later created_at wins.
// Equal on both → keep the incumbent.
func supersedes(candidate, kept memory) bool {
	if candidate.Annotations.Stale != kept.Annotations.Stale {
		return !candidate.Annotations.Stale // candidate wins iff it is the fresh (non-stale) one
	}
	ct, cok := createdAtInstant(candidate.CreatedAt)
	kt, kok := createdAtInstant(kept.CreatedAt)
	if cok && kok {
		if !ct.Equal(kt) {
			return ct.After(kt)
		}
		return false
	}
	return candidate.CreatedAt > kept.CreatedAt
}

// createdAtInstant parses an RFC3339 created_at into an instant. Lexical
// comparison only agrees with chronological order for the exact shape
// "…THH:MM:SSZ": a fractional second ("…:02.5Z") sorts *before* the whole
// second it belongs to, and "+02:00" offsets sort by local time, not instant.
// A value that does not parse as RFC3339 (missing, malformed, or a date-only
// field from an older server) reports false and falls back to lexical order.
func createdAtInstant(s string) (time.Time, bool) {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return time.Time{}, false
	}
	return t, true
}

// normalizeConcept lowercases and trims a concept for duplicate detection so
// "Auth Pattern" and "auth pattern " collapse to the same key.
func normalizeConcept(c string) string {
	return strings.ToLower(strings.TrimSpace(c))
}

// wordSet returns the set of distinct lowercased whitespace-delimited words in
// s, used to estimate content overlap without embeddings.
func wordSet(s string) []string {
	fields := strings.Fields(strings.ToLower(s))
	seen := make(map[string]struct{}, len(fields))
	out := make([]string, 0, len(fields))
	for _, f := range fields {
		if _, ok := seen[f]; ok {
			continue
		}
		seen[f] = struct{}{}
		out = append(out, f)
	}
	return out
}

// isNearDuplicate reports whether tokens overlaps any previously-kept token set
// with Jaccard similarity at or above dupTokenOverlap.
func isNearDuplicate(tokens []string, kept [][]string) bool {
	if len(tokens) == 0 {
		return false
	}
	for _, k := range kept {
		if jaccard(tokens, k) >= dupTokenOverlap {
			return true
		}
	}
	return false
}

// jaccard returns the Jaccard similarity (|A∩B| / |A∪B|) of two word sets.
// Both inputs are expected to contain no duplicates (see wordSet).
func jaccard(a, b []string) float64 {
	if len(a) == 0 || len(b) == 0 {
		return 0
	}
	set := make(map[string]struct{}, len(a))
	for _, w := range a {
		set[w] = struct{}{}
	}
	inter := 0
	for _, w := range b {
		if _, ok := set[w]; ok {
			inter++
		}
	}
	union := len(a) + len(b) - inter
	if union == 0 {
		return 0
	}
	return float64(inter) / float64(union)
}
