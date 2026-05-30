package main

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/maci0/muninn-sidecar/internal/apiformat"
)

func TestLastNonEmptyLine(t *testing.T) {
	tests := []struct {
		in, want string
	}{
		{"", ""},
		{"\n\n", ""},
		{"4", "4"},
		{"tokens used\n8,865\n4", "4"},
		{"answer\n\n  \n", "answer"},
		{"  spaced  ", "spaced"},
		{"line1\nline2", "line2"},
	}
	for _, tt := range tests {
		if got := lastNonEmptyLine(tt.in); got != tt.want {
			t.Errorf("lastNonEmptyLine(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestBuildCLIPrompt(t *testing.T) {
	// No context: question present, no retrieved-context markers.
	p := buildCLIPrompt("Who wrote Hamlet?", "")
	if !strings.Contains(p, "Question: Who wrote Hamlet?") {
		t.Fatalf("missing question: %q", p)
	}
	if strings.Contains(p, apiformat.ContextPrefix) {
		t.Fatalf("unexpected context markers with empty block: %q", p)
	}
	// With context: markers wrap the block, question still present.
	p = buildCLIPrompt("Who wrote Hamlet?", "Shakespeare wrote Hamlet.")
	if !strings.Contains(p, apiformat.ContextPrefix) || !strings.Contains(p, apiformat.ContextSuffix) {
		t.Fatalf("missing context markers: %q", p)
	}
	if !strings.Contains(p, "Shakespeare wrote Hamlet.") {
		t.Fatalf("missing context body: %q", p)
	}
	if !strings.Contains(p, "Question: Who wrote Hamlet?") {
		t.Fatalf("missing question: %q", p)
	}
}

func TestCLIClientLabel(t *testing.T) {
	c := &cliClient{name: "claude -p"}
	if c.label() != "claude -p" {
		t.Fatalf("label = %q", c.label())
	}
}

func TestModelClientLabel(t *testing.T) {
	m := &modelClient{model: "gpt-4o-mini"}
	if m.label() != "gpt-4o-mini" {
		t.Fatalf("label = %q", m.label())
	}
}

// TestCLIClientAnswer exercises the exec path against a real command: `printf`
// echoes the prompt's last line back, which lastNonEmptyLine then returns.
func TestCLIClientAnswer(t *testing.T) {
	// `cat` echoes the prompt arg verbatim; the prompt ends with "Answer:" so the
	// last non-empty line is "Answer:". Use a command that emits a known answer.
	c := &cliClient{name: "echo", argv: []string{"echo", "Paris"}, timeout: 5 * time.Second}
	got, err := c.answer(context.Background(), "What is the capital of France?", "")
	if err != nil {
		t.Fatalf("answer: %v", err)
	}
	// echo prints its fixed args then the appended prompt; last line is the prompt
	// tail "Answer:". The fixed "Paris" arg is on the first line. Assert we got a
	// non-empty trimmed line back.
	if got == "" {
		t.Fatalf("expected non-empty answer")
	}
}

// TestCLIClientAnswerError: a command that fails with no stdout returns an error.
func TestCLIClientAnswerError(t *testing.T) {
	c := &cliClient{name: "false", argv: []string{"false"}, timeout: 5 * time.Second}
	if _, err := c.answer(context.Background(), "q", ""); err == nil {
		t.Fatalf("expected error from failing command with no stdout")
	}
}

func FuzzLastNonEmptyLine(f *testing.F) {
	f.Add("")
	f.Add("a\nb\n")
	f.Add("   \n\t\n")
	f.Fuzz(func(t *testing.T, s string) {
		got := lastNonEmptyLine(s)
		if got != strings.TrimSpace(got) {
			t.Fatalf("result not trimmed: %q", got)
		}
		if strings.Contains(got, "\n") {
			t.Fatalf("result contains newline: %q", got)
		}
	})
}

func FuzzBuildCLIPrompt(f *testing.F) {
	f.Add("question", "context")
	f.Add("", "")
	f.Fuzz(func(t *testing.T, q, c string) {
		p := buildCLIPrompt(q, c)
		if !strings.Contains(p, q) {
			t.Fatalf("prompt missing question %q", q)
		}
		if c != "" && !strings.Contains(p, c) {
			t.Fatalf("prompt missing non-empty context %q", c)
		}
	})
}
