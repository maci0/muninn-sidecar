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

	return inj.decayedWindowLocked(inj.turn)
}

// decayedWindowLocked returns the whole session window with each score decayed
// at turn, ordered by score descending with ties broken on ID. The tiebreak
// matters: the window is backed by a map, so without it equal-scored memories
// would come back in a different order on every call, and the injected block
// would stop being replayable. Callers must hold inj.mu.
func (inj *Injector) decayedWindowLocked(turn int) []memory {
	out := make([]memory, 0, len(inj.recentMemories))
	for _, tm := range inj.recentMemories {
		m := tm.memory
		m.Score = decayedScore(m.Score, turn-tm.lastSeen)
		out = append(out, m)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Score != out[j].Score {
			return out[i].Score > out[j].Score
		}
		return out[i].ID < out[j].ID
	})
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

	return inj.decayedWindowLocked(currentTurn)
}
