package main

import (
	"strings"
	"testing"
)

func TestFormatInjected(t *testing.T) {
	cands := []cand{
		{Concept: "auth#0", Content: "Auth uses Postgres.", Score: 0.82},
		{Concept: "cache#1", Content: "Cache is Redis.", Score: 0.71},
	}

	bare := formatInjected(cands, "bare")
	if strings.Contains(bare, "auth#0") || strings.Contains(bare, "relevance") {
		t.Errorf("bare must contain no concept/relevance: %q", bare)
	}
	if !strings.Contains(bare, "Auth uses Postgres.") || !strings.Contains(bare, "Cache is Redis.") {
		t.Errorf("bare missing content: %q", bare)
	}

	labeled := formatInjected(cands, "labeled")
	if !strings.Contains(labeled, "[auth#0]") || strings.Contains(labeled, "relevance") {
		t.Errorf("labeled must have concept but no relevance: %q", labeled)
	}

	scored := formatInjected(cands, "scored")
	if !strings.Contains(scored, "[auth#0] (relevance: 0.82)") {
		t.Errorf("scored must reproduce the live format: %q", scored)
	}
	if !strings.Contains(scored, "Auth uses Postgres.") {
		t.Errorf("scored missing content: %q", scored)
	}

	if formatInjected(nil, "scored") != "" {
		t.Error("empty candidates → empty string")
	}
}

func TestAnswerInstruction(t *testing.T) {
	answerHint = ""
	if !strings.Contains(answerInstruction(), "shortest exact span") {
		t.Errorf("default should be extractive: %q", answerInstruction())
	}
	answerHint = "SUPPORTS, REFUTES"
	defer func() { answerHint = "" }()
	got := answerInstruction()
	if !strings.Contains(got, "SUPPORTS, REFUTES") || !strings.Contains(got, "exactly one of") {
		t.Errorf("hinted instruction should constrain to the label set: %q", got)
	}
}
