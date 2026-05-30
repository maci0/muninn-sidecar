package inject

import (
	"context"
	"strings"
	"testing"
)

// stubGrounder accepts a passage iff it contains "ANSWER".
type stubGrounder struct{ calls int }

func (s *stubGrounder) Label() string { return "stub" }
func (s *stubGrounder) Grounded(_ context.Context, _, passage string) bool {
	s.calls++
	return strings.Contains(passage, "ANSWER")
}

func TestGroundMemories(t *testing.T) {
	g := &stubGrounder{}
	inj := New(Config{MCPURL: "http://unused", Grounder: g, GroundTopK: 5})
	mems := []memory{
		{ID: "1", Content: "has the ANSWER", Score: 0.9},
		{ID: "2", Content: "same topic, no answer", Score: 0.8},
		{ID: "3", Content: "ANSWER also here", Score: 0.7},
	}
	got := inj.groundMemories(context.Background(), "q", mems)
	if len(got) != 2 || got[0].ID != "1" || got[1].ID != "3" {
		t.Fatalf("expected the two ANSWER-bearing memories kept in order, got %+v", got)
	}
	if g.calls != 3 {
		t.Errorf("expected 3 judge calls, got %d", g.calls)
	}
}

func TestGroundMemoriesTopK(t *testing.T) {
	g := &stubGrounder{}
	// GroundTopK defaults to 3 when a grounder is set; only the top-3 are judged,
	// the lower-ranked tail is kept untouched.
	inj := New(Config{MCPURL: "http://unused", Grounder: g})
	if inj.groundTopK != 3 {
		t.Fatalf("expected default groundTopK=3, got %d", inj.groundTopK)
	}
	mems := []memory{
		{ID: "1", Content: "no", Score: 0.9},
		{ID: "2", Content: "no", Score: 0.8},
		{ID: "3", Content: "no", Score: 0.7},
		{ID: "4", Content: "untouched tail", Score: 0.6},
	}
	got := inj.groundMemories(context.Background(), "q", mems)
	if g.calls != 3 {
		t.Errorf("only the top-3 should be judged, got %d calls", g.calls)
	}
	// Top-3 all rejected (no "ANSWER"), tail kept untouched.
	if len(got) != 1 || got[0].ID != "4" {
		t.Fatalf("expected only the untouched tail kept, got %+v", got)
	}
}

func TestGroundMemoriesNilGrounderUnused(t *testing.T) {
	// Without a grounder, groundTopK stays 0 and the helper is never wired in.
	inj := New(Config{MCPURL: "http://unused"})
	if inj.grounder != nil || inj.groundTopK != 0 {
		t.Fatalf("no grounder configured: grounder=%v topK=%d", inj.grounder, inj.groundTopK)
	}
}
