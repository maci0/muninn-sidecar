package grounding

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestParseMaskNumbered(t *testing.T) {
	mask := ParseMask("1: yes\n2: no\n3: yes", 3)
	want := []bool{true, false, true}
	for i := range want {
		if mask[i] != want[i] {
			t.Fatalf("ParseMask numbered = %v, want %v", mask, want)
		}
	}
}

func TestParseMaskVariants(t *testing.T) {
	// Mixed formatting, extra prose, and a missing verdict (defaults true).
	mask := ParseMask("Here are my judgments:\n1) no\n2 - relevant\n3: no", 4)
	want := []bool{false, true, false, true} // #4 unmentioned → fail-open true
	for i := range want {
		if mask[i] != want[i] {
			t.Fatalf("ParseMask variants = %v, want %v", mask, want)
		}
	}
}

func TestParseMaskSingleBare(t *testing.T) {
	if m := ParseMask("no", 1); m[0] {
		t.Error("bare 'no' for n=1 should be false")
	}
	if m := ParseMask("the answer is present, yes", 1); !m[0] {
		t.Error("bare 'yes' for n=1 should be true")
	}
	if m := ParseMask("", 1); !m[0] {
		t.Error("empty reply → fail-open true")
	}
}

func TestParseMaskOutOfRangeIgnored(t *testing.T) {
	// Verdicts referencing nonexistent passages must not panic or misindex.
	mask := ParseMask("5: no\n1: no", 2)
	if mask[0] || !mask[1] {
		t.Fatalf("out-of-range verdict mishandled: %v", mask)
	}
}

func TestPrompt(t *testing.T) {
	p := Prompt("where is Town Moor?", []string{"Town Moor is in Newcastle.", "Unrelated."})
	if !strings.Contains(p, "where is Town Moor?") || !strings.Contains(p, "[1]") || !strings.Contains(p, "[2]") {
		t.Fatalf("prompt missing query/numbering: %q", p)
	}
	if !strings.Contains(strings.ToLower(p), "yes") {
		t.Fatalf("prompt should request yes/no verdicts: %q", p)
	}
}

// stub keeps passages containing "ANSWER".
type stub struct{ calls int }

func (s *stub) Label() string { return "stub" }
func (s *stub) Relevant(_ context.Context, _ string, passages []string) []bool {
	s.calls++
	out := make([]bool, len(passages))
	for i, p := range passages {
		out[i] = strings.Contains(p, "ANSWER")
	}
	return out
}

func TestFilter(t *testing.T) {
	cands := []string{"has ANSWER", "no answer here", "also nope"}
	g := &stub{}
	got := Filter(context.Background(), g, "q", cands, 5)
	if len(got) != 1 || got[0] != "has ANSWER" {
		t.Fatalf("expected only answer-bearing kept, got %v", got)
	}
	if g.calls != 1 {
		t.Errorf("expected ONE listwise call, got %d", g.calls)
	}
	// topK caps judged candidates AND drops the untouched tail.
	g2 := &stub{}
	if got := Filter(context.Background(), g2, "q", cands, 2); len(got) != 1 || g2.calls != 1 {
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
	reply := "1: no\n2: yes"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte(`{"choices":[{"message":{"content":` + jsonString(reply) + `}}]}`))
	}))
	defer srv.Close()
	g := New("", srv.URL, "test-model", "", 5*time.Second)
	mask := g.Relevant(context.Background(), "q", []string{"p1", "p2"})
	if len(mask) != 2 || mask[0] || !mask[1] {
		t.Fatalf("expected [false true], got %v", mask)
	}
	// Unreachable endpoint fails open (all true).
	bad := New("", "http://127.0.0.1:0", "m", "", 200*time.Millisecond)
	if m := bad.Relevant(context.Background(), "q", []string{"p"}); !m[0] {
		t.Error("unreachable grounder must fail open (true)")
	}
}

func TestCLIGrounder(t *testing.T) {
	// `printf` emits fixed verdict lines, ignoring the appended prompt arg.
	g := &cliGrounder{name: "printf", argv: []string{"printf", "1: no\n2: yes\n"}, timeout: 5 * time.Second}
	mask := g.Relevant(context.Background(), "q", []string{"a", "b"})
	if len(mask) != 2 || mask[0] || !mask[1] {
		t.Fatalf("expected [false true], got %v", mask)
	}
	// A failing command with no output fails open.
	gf := &cliGrounder{name: "false", argv: []string{"false"}, timeout: 5 * time.Second}
	if m := gf.Relevant(context.Background(), "q", []string{"p"}); !m[0] {
		t.Error("failing command must fail open (true)")
	}
}

// jsonString quotes s as a JSON string literal for embedding in a test payload.
func jsonString(s string) string {
	s = strings.ReplaceAll(s, "\n", `\n`)
	return `"` + s + `"`
}

func FuzzPrompt(f *testing.F) {
	f.Add("where?", "Newcastle.")
	f.Add("", "")
	f.Fuzz(func(t *testing.T, q, p string) {
		out := Prompt(q, []string{p})
		if !strings.Contains(out, q) {
			t.Fatalf("prompt must contain the query verbatim")
		}
		// Passages are flattened (newlines → spaces) before embedding.
		if norm := strings.ReplaceAll(p, "\n", " "); !strings.Contains(out, norm) {
			t.Fatalf("prompt must contain the newline-normalized passage")
		}
	})
}

func FuzzParseMask(f *testing.F) {
	f.Add("1: yes\n2: no", 2)
	f.Add("garbage", 3)
	f.Add("", 1)
	f.Fuzz(func(t *testing.T, s string, n int) {
		if n < 0 || n > 1000 {
			t.Skip()
		}
		mask := ParseMask(s, n)
		if len(mask) != n {
			t.Fatalf("ParseMask(%q,%d) len=%d", s, n, len(mask))
		}
	})
}
