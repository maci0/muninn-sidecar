package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestHTTPRewriter(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte(`{"choices":[{"message":{"content":"who directed it\nwhat year was the director born"}}]}`))
	}))
	defer srv.Close()
	r := buildRewriter("", srv.URL, "m", "", 5*time.Second)
	if r == nil {
		t.Fatal("expected http rewriter")
	}
	subs := r.Rewrite(context.Background(), "orig question", 4)
	if len(subs) < 2 || subs[0] != "orig question" {
		t.Fatalf("expected original + sub-queries, got %v", subs)
	}
	if !strings.HasPrefix(r.label(), "http:") {
		t.Errorf("label = %q", r.label())
	}
	// Unreachable endpoint fails safe → just the original.
	bad := buildRewriter("", "http://127.0.0.1:0", "m", "", 200*time.Millisecond)
	if got := bad.Rewrite(context.Background(), "only this", 4); len(got) != 1 || got[0] != "only this" {
		t.Fatalf("unreachable rewriter must fail safe to [query], got %v", got)
	}
}

func TestCLIRewriter(t *testing.T) {
	// `printf` emits fixed sub-query lines, ignoring the appended prompt arg.
	r := &cliRewriter{name: "printf", argv: []string{"printf", "sub one\nsub two\n"}, timeout: 5 * time.Second}
	subs := r.Rewrite(context.Background(), "orig", 4)
	if subs[0] != "orig" || len(subs) != 3 {
		t.Fatalf("expected [orig, sub one, sub two], got %v", subs)
	}
	if !strings.HasPrefix(r.label(), "cli:") {
		t.Errorf("label = %q", r.label())
	}
	// A failing command fails safe to the original.
	rf := &cliRewriter{name: "false", argv: []string{"false"}, timeout: 5 * time.Second}
	if got := rf.Rewrite(context.Background(), "q", 4); len(got) != 1 || got[0] != "q" {
		t.Fatalf("failing rewriter must fail safe, got %v", got)
	}
}

func FuzzRewritePrompt(f *testing.F) {
	f.Add("who won?", 3)
	f.Fuzz(func(t *testing.T, q string, max int) {
		p := rewritePrompt(q, max)
		if !strings.Contains(p, q) {
			t.Fatalf("prompt must contain the query")
		}
	})
}

func FuzzItoa(f *testing.F) {
	f.Add(0)
	f.Add(42)
	f.Add(-5)
	f.Fuzz(func(t *testing.T, n int) {
		got := itoa(n)
		want := "0"
		if n > 0 {
			want = strconv.Itoa(n)
		}
		if got != want {
			t.Fatalf("itoa(%d) = %q, want %q", n, got, want)
		}
	})
}

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
