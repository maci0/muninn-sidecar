package main

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestParseYesNo(t *testing.T) {
	cases := []struct {
		in   string
		want bool
	}{
		{"yes", true},
		{"no", true == false},
		{"No.", false},
		{"YES", true},
		{"The passage answers the question. yes", true},
		{"After reading, the answer is not present. no", false},
		{"true", true},
		{"false", false},
		{"irrelevant", false},
		{"", true},        // ambiguous → fail-open
		{"maybe???", true}, // ambiguous → fail-open
	}
	for _, c := range cases {
		if got := parseYesNo(c.in); got != c.want {
			t.Errorf("parseYesNo(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}

func TestSortByVecDesc(t *testing.T) {
	mems := []recalledMemory{
		{Concept: "a", VectorScore: 0.3},
		{Concept: "b", VectorScore: 0.9},
		{Concept: "c", VectorScore: 0.6},
	}
	sortByVecDesc(mems)
	if mems[0].Concept != "b" || mems[1].Concept != "c" || mems[2].Concept != "a" {
		t.Fatalf("not sorted desc: %+v", mems)
	}
}

// stubGrounder accepts a passage iff it contains the substring "ANSWER".
type stubGrounder struct{ calls int }

func (s *stubGrounder) label() string { return "stub" }
func (s *stubGrounder) grounded(_ context.Context, _, passage string) bool {
	s.calls++
	return strings.Contains(passage, "ANSWER")
}

func TestApplyGrounding(t *testing.T) {
	results := []probeResult{
		{ // present: one answer-bearing candidate among siblings → kept
			probe:    probe{Query: "q1", Gold: "g#0", Present: true},
			Recalled: []recalledMemory{{Concept: "g#0", Content: "has the ANSWER", VectorScore: 0.8}, {Concept: "g#2", Content: "sibling", VectorScore: 0.7}},
		},
		{ // hard negative: no candidate answers → all dropped → suppressed
			probe:    probe{Query: "q2", Gold: "", Present: false},
			Recalled: []recalledMemory{{Concept: "h#1", Content: "same topic, no answer", VectorScore: 0.75}, {Concept: "h#3", Content: "also no", VectorScore: 0.7}},
		},
	}
	g := &stubGrounder{}
	calls := applyGrounding(context.Background(), g, results, 5)
	if calls != 4 {
		t.Errorf("expected 4 grounding calls, got %d", calls)
	}
	if len(results[0].Recalled) != 1 || results[0].Recalled[0].Concept != "g#0" {
		t.Errorf("present probe should keep only the answer-bearing candidate: %+v", results[0].Recalled)
	}
	if len(results[1].Recalled) != 0 {
		t.Errorf("hard negative should be fully filtered, got %+v", results[1].Recalled)
	}
}

func TestApplyGroundingTopK(t *testing.T) {
	results := []probeResult{{
		probe: probe{Query: "q", Present: true},
		Recalled: []recalledMemory{
			{Concept: "a", Content: "x", VectorScore: 0.9},
			{Concept: "b", Content: "x", VectorScore: 0.8},
			{Concept: "c", Content: "x", VectorScore: 0.7},
		},
	}}
	g := &stubGrounder{}
	calls := applyGrounding(context.Background(), g, results, 2)
	if calls != 2 {
		t.Errorf("top-2 should ground 2 candidates, got %d", calls)
	}
}

func TestBuildGrounder(t *testing.T) {
	if buildGrounder("", "", "m", "", time.Second) != nil {
		t.Error("no backend → nil")
	}
	if g := buildGrounder("claude -p", "", "m", "", time.Second); g == nil || !strings.HasPrefix(g.label(), "cli:") {
		t.Errorf("cmd → cli grounder, got %v", g)
	}
	if g := buildGrounder("", "http://x/v1", "m", "", time.Second); g == nil || !strings.HasPrefix(g.label(), "http:") {
		t.Errorf("url → http grounder, got %v", g)
	}
	// CLI takes precedence when both set.
	if g := buildGrounder("grok -p", "http://x/v1", "m", "", time.Second); !strings.HasPrefix(g.label(), "cli:") {
		t.Error("cmd should take precedence over url")
	}
}

func TestSplitPresentAbsentAndCopy(t *testing.T) {
	results := []probeResult{
		{probe: probe{Present: true}, Recalled: []recalledMemory{{Concept: "a"}}},
		{probe: probe{Present: false}, Recalled: []recalledMemory{{Concept: "b"}}},
	}
	p, a := splitPresentAbsent(results)
	if len(p) != 1 || len(a) != 1 {
		t.Fatalf("split: present=%d absent=%d", len(p), len(a))
	}
	cp := deepCopyResults(results)
	cp[0].Recalled[0].Concept = "MUT"
	if results[0].Recalled[0].Concept != "a" {
		t.Error("deepCopyResults must not alias the original Recalled slice")
	}
}

func TestGroundPrompt(t *testing.T) {
	p := groundPrompt("where is Town Moor?", "Town Moor is in Newcastle.")
	if !strings.Contains(p, "where is Town Moor?") || !strings.Contains(p, "Town Moor is in Newcastle.") {
		t.Fatalf("prompt missing query/passage: %q", p)
	}
	if !strings.Contains(strings.ToLower(p), "yes or no") {
		t.Fatalf("prompt should request yes/no: %q", p)
	}
}

func FuzzParseYesNo(f *testing.F) {
	f.Add("yes")
	f.Add("no")
	f.Add("the answer is not here, no")
	f.Fuzz(func(t *testing.T, s string) {
		_ = parseYesNo(s) // must not panic for any input
	})
}
