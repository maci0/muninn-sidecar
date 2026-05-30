package main

import (
	"strings"
	"testing"
	"time"
)

func TestParseSubqueries(t *testing.T) {
	out := "Who directed the film?\n2. What year was the director born?\n- where is the studio"
	subs := parseSubqueries("orig question", out, 4)
	if subs[0] != "orig question" {
		t.Fatalf("original must come first, got %q", subs[0])
	}
	if len(subs) != 4 {
		t.Fatalf("expected 4 subs (orig + 3), got %d: %v", len(subs), subs)
	}
	// List prefixes ("2.", "-") must be stripped.
	for _, s := range subs[1:] {
		if strings.HasPrefix(s, "-") || strings.HasPrefix(s, "2.") {
			t.Errorf("prefix not stripped: %q", s)
		}
	}
}

func TestParseSubqueriesDedupAndCap(t *testing.T) {
	out := "orig\nORIG\nsame fact\nsame fact\nextra"
	subs := parseSubqueries("orig", out, 3)
	if len(subs) != 3 {
		t.Fatalf("cap not honored: %v", subs)
	}
	// "orig"/"ORIG" dedup against the original (case-insensitive).
	seen := map[string]int{}
	for _, s := range subs {
		seen[strings.ToLower(s)]++
	}
	if seen["orig"] != 1 {
		t.Errorf("original duplicated: %v", subs)
	}
}

func TestParseSubqueriesEmpty(t *testing.T) {
	// No usable lines → just the original.
	subs := parseSubqueries("only this", "\n  \n.\n", 4)
	if len(subs) != 1 || subs[0] != "only this" {
		t.Fatalf("expected only the original, got %v", subs)
	}
}

func TestItoa(t *testing.T) {
	for _, c := range []struct {
		n    int
		want string
	}{{0, "0"}, {-3, "0"}, {7, "7"}, {42, "42"}, {1000, "1000"}} {
		if got := itoa(c.n); got != c.want {
			t.Errorf("itoa(%d)=%q want %q", c.n, got, c.want)
		}
	}
}

func TestRewritePrompt(t *testing.T) {
	p := rewritePrompt("who won?", 3)
	if !strings.Contains(p, "who won?") || !strings.Contains(p, "3") {
		t.Fatalf("prompt missing query/limit: %q", p)
	}
}

func TestBuildRewriter(t *testing.T) {
	if buildRewriter("", "", "m", "", time.Second) != nil {
		t.Error("no backend → nil")
	}
	if r := buildRewriter("claude -p", "", "m", "", time.Second); r == nil || !strings.HasPrefix(r.label(), "cli:") {
		t.Errorf("cmd → cli, got %v", r)
	}
	if r := buildRewriter("", "http://x/v1", "m", "", time.Second); r == nil || !strings.HasPrefix(r.label(), "http:") {
		t.Errorf("url → http, got %v", r)
	}
	if r := buildRewriter("grok -p", "http://x/v1", "m", "", time.Second); !strings.HasPrefix(r.label(), "cli:") {
		t.Error("cmd should win over url")
	}
}

func FuzzParseSubqueries(f *testing.F) {
	f.Add("orig", "a\nb\nc", 4)
	f.Add("q", "", 1)
	f.Fuzz(func(t *testing.T, orig, out string, max int) {
		if max < 1 || max > 100 {
			t.Skip()
		}
		subs := parseSubqueries(orig, out, max)
		if len(subs) < 1 || subs[0] != orig {
			t.Fatalf("must always return the original first: %v", subs)
		}
		if len(subs) > max {
			t.Fatalf("exceeded cap %d: %d", max, len(subs))
		}
	})
}
