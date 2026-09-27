// This file contains the session memory window: how freshly recalled
// memories merge with memories carried over from earlier turns, and how the
// window is read without aging when a request continues the same intent.
package inject

import (
	"log/slog"
	"sort"
)

// snapshotWindow returns the current session window decayed at the current turn
// WITHOUT advancing the turn counter or evicting — used on continuations, where
// no new intent has arrived so memories should neither age nor be re-recalled.
func (inj *Injector) snapshotWindow() []memory {
	inj.mu.Lock()
	defer inj.mu.Unlock()

	cur := inj.turn
	out := make([]memory, 0, len(inj.recentMemories))
	for _, tm := range inj.recentMemories {
		m := tm.memory
		m.Score = decayedScore(m.Score, cur-tm.lastSeen)
		out = append(out, m)
	}
	if len(out) > 1 {
		sort.Slice(out, func(i, j int) bool { return out[i].Score > out[j].Score })
	}
	return out
}

// mergeMemories merges freshly recalled memories into the session window.
// Previously seen memories that are recalled again have their score refreshed.
// Memories not re-recalled decay by decayFactor per turn and are evicted when
// they drop below decayFloor. Returns the merged set sorted by effective score.
func (inj *Injector) mergeMemories(recalled []memory) []memory {
	inj.mu.Lock()
	defer inj.mu.Unlock()

	inj.turn++
	currentTurn := inj.turn

	// Refresh or add recalled memories.
	recalledIDs := make(map[string]bool, len(recalled))
	for _, m := range recalled {
		recalledIDs[m.ID] = true
		inj.recentMemories[m.ID] = trackedMemory{
			memory:   m,
			lastSeen: currentTurn,
		}
	}

	// Decay and evict stale memories.
	for id, tm := range inj.recentMemories {
		if recalledIDs[id] {
			continue // just refreshed
		}
		turnsAgo := currentTurn - tm.lastSeen
		if decayedScore(tm.Score, turnsAgo) < decayFloor {
			delete(inj.recentMemories, id)
			slog.Debug("inject: evicted stale memory", "id", id, "age", turnsAgo)
		}
	}

	// Build sorted output from the merged window.
	merged := make([]memory, 0, len(inj.recentMemories))
	for _, tm := range inj.recentMemories {
		turnsAgo := currentTurn - tm.lastSeen
		m := tm.memory
		m.Score = decayedScore(m.Score, turnsAgo)
		merged = append(merged, m)
	}
	if len(merged) > 1 {
		sort.Slice(merged, func(i, j int) bool {
			return merged[i].Score > merged[j].Score
		})
	}

	return merged
}
