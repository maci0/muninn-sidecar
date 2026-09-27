// This file contains the memory model: the record recalled from MuninnDB,
// the relevance normalization that decides which number the gate reads, and the
// decay arithmetic that ages a memory across turns in the session window.
package inject

import "math"

// charPerToken is the bytes-to-token approximation used throughout the inject
// package. 4 bytes ≈ 1 token is a standard heuristic for English prose; it is
// intentionally imprecise since injection stays well within model context limits.
// The unit is bytes, not characters (tokenizers charge by encoded length, so a
// CJK or emoji memory costs more than its character count suggests).
const charPerToken = 4

// memory represents a recalled memory from MuninnDB.
//
// MuninnDB returns two relevance numbers: Score is a composite that folds in
// recency and graph traversal (it can exceed 1.0 and, critically, does not
// separate relevant from irrelevant — a benchmark against a real instance found
// it injects on irrelevant queries just as readily as relevant ones). VectorScore
// is the raw embedding cosine similarity, which separates cleanly. The injector
// gates and ranks on VectorScore (falling back to Score only when the field is
// absent); see normalizeRelevance and cmd/msc-bench.
type memory struct {
	ID          string  `json:"id"`
	Concept     string  `json:"concept"`
	Content     string  `json:"content"`
	Score       float64 `json:"score"`
	VectorScore float64 `json:"vector_score"`
	// CreatedAt is the memory's creation timestamp (RFC3339 UTC, e.g.
	// "2026-05-30T14:32:02Z"). Compared as an instant, not lexically, to prefer
	// the fresher of two near-duplicate memories so an updated fact wins over
	// the stale one it supersedes (anti-staleness; see supersedes and
	// createdAtInstant, which explains why lexical order is not enough).
	CreatedAt string `json:"created_at"`
	// State is the MuninnDB lifecycle state (planning|active|paused|blocked|
	// completed|cancelled|archived, or "" if unset). Trust is the reliability
	// level (verified|inferred|external|untrusted). Explicitly-dead or untrusted
	// memories are excluded from injection (see injectable).
	State string `json:"state"`
	Trust string `json:"trust"`
	// Annotations.Stale is MuninnDB's staleness verdict (present when recall is
	// called with annotate:true) — a fact that has aged past its verification
	// window. Used as the authoritative tiebreak in same-concept dedup: a fresh
	// memory supersedes a stale duplicate even if created_at comparison is
	// ambiguous. (Staleness is age-based, not wrongness, so a *lone* stale memory
	// is still injected — it may be the only answer; only duplicates are pruned.)
	Annotations struct {
		Stale bool `json:"stale"`
		// ConflictsWith lists IDs of memories MuninnDB has detected this one
		// contradicts. When both sides of a contradiction are recalled, injecting
		// both feeds the agent mutually-exclusive "facts"; selectForInjection keeps
		// only the superseding side (see resolveConflicts).
		ConflictsWith []string `json:"conflicts_with"`
	} `json:"annotations"`
}

// injectable reports whether a recalled memory is fit to inject. It excludes
// memories MuninnDB has marked as explicitly dead — `archived` (retired) or
// `cancelled` (abandoned) — and `untrusted` ones (flagged unreliable). Surfacing
// any of these as current context misleads the agent. `completed` is kept: a
// finished task's decisions/facts remain relevant. Empty/unrecognized values are
// kept, so vaults that don't populate these fields see no change.
func injectable(m memory) bool {
	switch m.State {
	case "archived", "cancelled":
		return false
	}
	return m.Trust != "untrusted"
}

// normalizeRelevance rewrites each memory's Score to the gating/ranking signal:
// the embedding cosine (VectorScore) when present, else the original composite
// Score as a fallback. After this, the whole downstream pipeline (decay,
// ordering, threshold, the displayed relevance) operates on cosine similarity.
// Returns whether any memory carried a cosine — when none do, the gate is
// operating on the recency/graph-inflated composite, which is far less reliable.
//
// A score the response cannot back up is dropped to 0 rather than propagated:
// NaN fails every comparison below (the gate's `Score < minScore` is false, the
// sort's `>` is false), so such a memory would clear the gate unranked and
// decay to NaN; +Inf decays to +Inf and FormatFloat writes it into the injected
// block as a 309-digit relevance. Either way the number stops being a relevance.
// JSON cannot spell NaN or ±Inf, but it can spell 1e308, and a caller that
// computed the score itself (the offline evaluators) can pass either.
func normalizeRelevance(mems []memory) (anyVector bool) {
	for i := range mems {
		if v := mems[i].VectorScore; v > 0 && isUsableCosine(v) {
			mems[i].Score = v
			anyVector = true
		}
		if math.IsNaN(mems[i].Score) || math.IsInf(mems[i].Score, 0) {
			mems[i].Score = 0
		}
	}
	return anyVector
}

// isUsableCosine reports whether v is a cosine this package can rank on: a
// finite number in [-1, 1]. The composite Score is not range-checked — it
// folds in recency and graph traversal, so it legitimately exceeds 1.0 — but a
// cosine outside the unit interval is a broken response, not a similarity.
func isUsableCosine(v float64) bool {
	return !math.IsNaN(v) && !math.IsInf(v, 0) && v >= -1 && v <= 1
}

// decayFactor is multiplied against a memory's score for each turn it is
// not re-recalled. This creates a gradual fade-out rather than abrupt removal.
const decayFactor = 0.7

// decayFloor is the minimum effective score before a memory is evicted from
// the session window. With decayFactor=0.7 and a starting score of 0.85,
// a memory survives 4 turns without being re-recalled and is evicted on the
// fifth, when its decayed score drops below 0.2.
const decayFloor = 0.2

// trackedMemory wraps a recalled memory with session-level tracking state.
type trackedMemory struct {
	memory
	lastSeen int // turn number when last recalled/refreshed
}

// decayedScore returns the effective score of a memory that was last seen
// `age` turns ago, applying the per-turn decay factor.
func decayedScore(score float64, age int) float64 {
	return score * math.Pow(decayFactor, float64(age))
}
