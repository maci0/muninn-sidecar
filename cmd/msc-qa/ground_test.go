package main

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestParseYesNoQA(t *testing.T) {
	cases := []struct {
		in   string
		want bool
	}{
		{"yes", true},
		{"No.", false},
		{"The answer is not present. no", false},
		{"after review, yes", true},
		{"irrelevant", false},
		{"", true},      // ambiguous → fail-open
		{"hmm?!", true}, // ambiguous → fail-open
	}
	for _, c := range cases {
		if got := parseYesNo(c.in); got != c.want {
			t.Errorf("parseYesNo(%q)=%v want %v", c.in, got, c.want)
		}
	}
}

// stubGrounder accepts a passage iff it contains "ANSWER".
type stubGrounder struct{ calls int }

func (s *stubGrounder) label() string { return "stub" }
func (s *stubGrounder) grounded(_ context.Context, _, passage string) bool {
	s.calls++
	return strings.Contains(passage, "ANSWER")
}

func TestGroundFilter(t *testing.T) {
	cands := []string{"has the ANSWER here", "same topic no answer", "also irrelevant"}
	g := &stubGrounder{}
	got := groundFilter(context.Background(), g, "q", cands, 5)
	if len(got) != 1 || got[0] != "has the ANSWER here" {
		t.Fatalf("expected only the answer-bearing passage, got %v", got)
	}
	if g.calls != 3 {
		t.Errorf("expected 3 judge calls, got %d", g.calls)
	}
	// topK bounds the calls.
	g2 := &stubGrounder{}
	groundFilter(context.Background(), g2, "q", cands, 2)
	if g2.calls != 2 {
		t.Errorf("topK=2 should make 2 calls, got %d", g2.calls)
	}
	// nil grounder is a pass-through.
	if got := groundFilter(context.Background(), nil, "q", cands, 5); len(got) != 3 {
		t.Errorf("nil grounder must pass through all candidates, got %d", len(got))
	}
	// empty candidates → empty.
	if got := groundFilter(context.Background(), g, "q", nil, 5); len(got) != 0 {
		t.Errorf("empty candidates → empty, got %v", got)
	}
}

func TestBuildGrounderQA(t *testing.T) {
	if buildGrounder("", "", "m", "", time.Second) != nil {
		t.Error("no backend → nil")
	}
	if g := buildGrounder("claude -p", "", "m", "", time.Second); g == nil || !strings.HasPrefix(g.label(), "cli:") {
		t.Errorf("cmd → cli, got %v", g)
	}
	if g := buildGrounder("", "http://x/v1", "m", "", time.Second); g == nil || !strings.HasPrefix(g.label(), "http:") {
		t.Errorf("url → http, got %v", g)
	}
	if g := buildGrounder("grok -p", "http://x/v1", "m", "", time.Second); !strings.HasPrefix(g.label(), "cli:") {
		t.Error("cmd should win over url")
	}
}

func TestGroundPromptQA(t *testing.T) {
	p := groundPrompt("where?", "Newcastle.")
	if !strings.Contains(p, "where?") || !strings.Contains(p, "Newcastle.") || !strings.Contains(strings.ToLower(p), "yes or no") {
		t.Fatalf("bad prompt: %q", p)
	}
}

func FuzzParseYesNoQA(f *testing.F) {
	f.Add("yes")
	f.Add("no, not here")
	f.Fuzz(func(t *testing.T, s string) { _ = parseYesNo(s) })
}
