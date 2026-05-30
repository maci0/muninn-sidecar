package main

import (
	"context"
	"strings"
	"testing"
)

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

// stubGrounder accepts a passage iff it contains "ANSWER".
type stubGrounder struct{ calls int }

func (s *stubGrounder) Label() string { return "stub" }
func (s *stubGrounder) Grounded(_ context.Context, _, passage string) bool {
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
	if calls := applyGrounding(context.Background(), g, results, 2); calls != 2 {
		t.Errorf("top-2 should ground 2 candidates, got %d", calls)
	}
}
