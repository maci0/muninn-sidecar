package reqid

import (
	"context"
	"testing"
)

func TestFromRoundTripsTheMintedID(t *testing.T) {
	id := Next()
	if id == "" {
		t.Fatal("Next() returned an empty ID")
	}
	if got := From(With(context.Background(), id)); got != id {
		t.Errorf("From() = %q, want %q", got, id)
	}
}

func TestNextIsUnique(t *testing.T) {
	// Every request gets its own ID, so two turns of one session never share
	// one; a collision would merge their log lines under a single key.
	seen := make(map[string]bool, 1000)
	for range 1000 {
		if id := Next(); seen[id] {
			t.Fatalf("Next() repeated %q", id)
		} else {
			seen[id] = true
		}
	}
}

func TestFromUnsetContext(t *testing.T) {
	// A background path with no request behind it must read as empty rather
	// than invent an ID, so an operator can tell it apart from a real turn.
	if got := From(context.Background()); got != "" {
		t.Errorf("From() = %q on a bare context, want \"\"", got)
	}
}
