package inject

import (
	"fmt"
	"testing"
)

// The session window is a Go map, so its iteration order changes on every run.
// Memories with equal effective scores would otherwise be emitted, filtered, and
// budget-truncated in a different order each time, making the injected block
// unreplayable. Sorting ties on ID pins the order.
func TestWindowOrderIsDeterministicForEqualScores(t *testing.T) {
	ids := []string{"m5", "m2", "m9", "m1", "m7", "m3", "m8", "m4", "m6"}
	sorted := []string{"m1", "m2", "m3", "m4", "m5", "m6", "m7", "m8", "m9"}
	idsOf := func(mems []memory) []string {
		out := make([]string, 0, len(mems))
		for _, m := range mems {
			out = append(out, m.ID)
		}
		return out
	}

	var want []string
	for run := range 50 {
		inj := New(Config{MCPURL: "http://unused"})
		recalled := make([]memory, 0, len(ids))
		for _, id := range ids {
			recalled = append(recalled, memory{ID: id, Concept: id, Content: "shared fact", Score: 0.8})
		}
		// Rotate the insertion order per run; all scores are equal, so only the
		// ID tiebreak can produce a stable order.
		k := run % len(ids)
		rotated := append(append([]memory(nil), recalled[k:]...), recalled[:k]...)

		got := idsOf(inj.mergeMemories(rotated))
		if run == 0 {
			want = got
			continue
		}
		if fmt.Sprint(got) != fmt.Sprint(want) {
			t.Fatalf("run %d window order %v, want %v", run, got, want)
		}
	}
	if fmt.Sprint(want) != fmt.Sprint(sorted) {
		t.Errorf("equal-score window should order by ID ascending, got %v", want)
	}
}

func TestSnapshotWindowOrderIsDeterministicForEqualScores(t *testing.T) {
	inj := New(Config{MCPURL: "http://unused"})
	inj.turn = 2
	// Both memories were last seen at the same turn with the same score, so
	// their decayed scores tie and only the ID tiebreak orders them.
	inj.recentMemories["b"] = trackedMemory{memory: memory{ID: "b", Content: "x", Score: 0.8}, lastSeen: 0}
	inj.recentMemories["a"] = trackedMemory{memory: memory{ID: "a", Content: "y", Score: 0.8}, lastSeen: 0}

	for i := range 50 {
		got := inj.snapshotWindow()
		if len(got) != 2 || got[0].ID != "a" || got[1].ID != "b" {
			t.Fatalf("run %d: got [%s %s], want [a b]", i, got[0].ID, got[1].ID)
		}
	}
}
