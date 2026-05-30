package grounding

import (
	"context"
	"net/http"
	"net/http/httptest"
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
		{"No.", false},
		{"YES", true},
		{"the passage does not contain it. no", false},
		{"after review, yes", true},
		{"true", true},
		{"false", false},
		{"irrelevant", false},
		{"", true},      // ambiguous → fail-open
		{"hmm???", true}, // ambiguous → fail-open
	}
	for _, c := range cases {
		if got := ParseYesNo(c.in); got != c.want {
			t.Errorf("ParseYesNo(%q)=%v want %v", c.in, got, c.want)
		}
	}
}

func TestPrompt(t *testing.T) {
	p := Prompt("where is Town Moor?", "Town Moor is in Newcastle.")
	if !strings.Contains(p, "where is Town Moor?") || !strings.Contains(p, "Town Moor is in Newcastle.") {
		t.Fatalf("prompt missing query/passage: %q", p)
	}
	if !strings.Contains(strings.ToLower(p), "yes or no") {
		t.Fatalf("prompt should request yes/no: %q", p)
	}
}

type stub struct {
	calls int
	accept func(passage string) bool
}

func (s *stub) Label() string { return "stub" }
func (s *stub) Grounded(_ context.Context, _, passage string) bool {
	s.calls++
	return s.accept(passage)
}

func TestFilter(t *testing.T) {
	cands := []string{"has ANSWER", "no answer here", "also nope"}
	g := &stub{accept: func(p string) bool { return strings.Contains(p, "ANSWER") }}
	got := Filter(context.Background(), g, "q", cands, 5)
	if len(got) != 1 || got[0] != "has ANSWER" {
		t.Fatalf("expected only answer-bearing kept, got %v", got)
	}
	if g.calls != 3 {
		t.Errorf("expected 3 calls, got %d", g.calls)
	}
	// topK caps judged candidates AND drops the untouched tail.
	g2 := &stub{accept: func(string) bool { return true }}
	if got := Filter(context.Background(), g2, "q", cands, 2); len(got) != 2 || g2.calls != 2 {
		t.Errorf("topK=2: kept=%d calls=%d", len(got), g2.calls)
	}
	// nil grounder / empty → pass-through.
	if got := Filter(context.Background(), nil, "q", cands, 5); len(got) != 3 {
		t.Errorf("nil grounder passthrough: %d", len(got))
	}
	if got := Filter(context.Background(), g, "q", nil, 5); len(got) != 0 {
		t.Errorf("empty input: %d", len(got))
	}
}

func TestNew(t *testing.T) {
	if New("", "", "m", "", time.Second) != nil {
		t.Error("no backend → nil")
	}
	if g := New("claude -p", "", "m", "", time.Second); g == nil || !strings.HasPrefix(g.Label(), "cli:") {
		t.Errorf("cmd → cli, got %v", g)
	}
	if g := New("", "http://x/v1", "m", "", time.Second); g == nil || !strings.HasPrefix(g.Label(), "http:") {
		t.Errorf("url → http, got %v", g)
	}
	if g := New("grok -p", "http://x/v1", "m", "", time.Second); !strings.HasPrefix(g.Label(), "cli:") {
		t.Error("cmd should win over url")
	}
}

func TestHTTPGrounder(t *testing.T) {
	reply := "no"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte(`{"choices":[{"message":{"content":"` + reply + `"}}]}`))
	}))
	defer srv.Close()
	g := New("", srv.URL, "test-model", "", 5*time.Second)
	if g.Grounded(context.Background(), "q", "p") {
		t.Error("reply 'no' should yield false")
	}
	reply = "yes"
	if !g.Grounded(context.Background(), "q", "p") {
		t.Error("reply 'yes' should yield true")
	}
	// Unreachable endpoint fails open (keep).
	bad := New("", "http://127.0.0.1:0", "m", "", 200*time.Millisecond)
	if !bad.Grounded(context.Background(), "q", "p") {
		t.Error("unreachable grounder must fail open (return true)")
	}
}

func TestCLIGrounder(t *testing.T) {
	// `sh -c 'echo no'` ignores the appended prompt arg and prints a fixed verdict.
	g := &cliGrounder{name: "sh", argv: []string{"sh", "-c", "echo no"}, timeout: 5 * time.Second}
	if g.Grounded(context.Background(), "q", "p") {
		t.Error("echo no → false")
	}
	gy := &cliGrounder{name: "sh", argv: []string{"sh", "-c", "echo yes"}, timeout: 5 * time.Second}
	if !gy.Grounded(context.Background(), "q", "p") {
		t.Error("echo yes → true")
	}
	// A failing command with no output fails open.
	gf := &cliGrounder{name: "false", argv: []string{"false"}, timeout: 5 * time.Second}
	if !gf.Grounded(context.Background(), "q", "p") {
		t.Error("failing command must fail open (return true)")
	}
}

func FuzzParseYesNo(f *testing.F) {
	f.Add("yes")
	f.Add("no")
	f.Add("the answer is not present, no")
	f.Fuzz(func(t *testing.T, s string) { _ = ParseYesNo(s) })
}
