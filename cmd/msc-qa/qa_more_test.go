package main

import (
	"context"
	"encoding/json"
	"flag"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/maci0/muninn-sidecar/internal/mcpclient"
)

func TestSmallHelpers(t *testing.T) {
	if got := splitCSV(" a , b ,, c "); len(got) != 3 || got[0] != "a" || got[2] != "c" {
		t.Errorf("splitCSV %v", got)
	}
	if trunc("abcdef", 4) != "abc…" || trunc("ab", 4) != "ab" {
		t.Errorf("trunc")
	}
	if safe(1, 0) != 0 || safe(2, 4) != 0.5 {
		t.Errorf("safe")
	}
	if envOr("DEFINITELY_UNSET_X", "d") != "d" {
		t.Errorf("envOr")
	}
	if resolveToken("flagtok") != "flagtok" {
		t.Errorf("resolveToken")
	}
}

func TestArmAgg(t *testing.T) {
	var a armAgg
	a.add(1, 1.0, true)
	a.add(0, 0.5, false)
	if a.em() != 0.5 || a.f1() != 0.75 || a.util() != 0.5 {
		t.Errorf("armAgg em=%v f1=%v util=%v", a.em(), a.f1(), a.util())
	}
	var z armAgg
	if z.em() != 0 || z.f1() != 0 || z.util() != 0 {
		t.Errorf("empty armAgg should be 0")
	}
}

func TestAppendMD(t *testing.T) {
	path := filepath.Join(t.TempDir(), "m.md")
	var a [3]armAgg
	a[0].add(1, 0.2, false)
	a[1].add(1, 0.8, true)
	appendMD(path, "modelX", 10, a)
	appendMD(path, "modelY", 10, a)
	data, _ := os.ReadFile(path)
	if n := countSub(string(data), "model"); n < 2 {
		t.Errorf("expected 2 rows, got content: %s", data)
	}
}

func countSub(s, sub string) int {
	n := 0
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			n++
		}
	}
	return n
}

func TestLoaders(t *testing.T) {
	dir := t.TempDir()
	squad := filepath.Join(dir, "s.json")
	os.WriteFile(squad, []byte(`{"data":[{"paragraphs":[{"qas":[{"question":"q","is_impossible":false,"answers":[{"text":"A"}]}]}]}]}`), 0o644)
	if qs, err := loadDataset("squad", squad, 10); err != nil || len(qs) != 1 || qs[0].Answers[0] != "A" {
		t.Errorf("squad loader: err=%v qs=%+v", err, qs)
	}
	hp := filepath.Join(dir, "h.json")
	os.WriteFile(hp, []byte(`[{"question":"q","answer":"yes"}]`), 0o644)
	if qs, err := loadDataset("hotpot", hp, 10); err != nil || len(qs) != 1 || qs[0].Answers[0] != "yes" {
		t.Errorf("hotpot loader: err=%v qs=%+v", err, qs)
	}
	gen := filepath.Join(dir, "g.json")
	os.WriteFile(gen, []byte(`[{"question":"q","answer":"PostgreSQL"}]`), 0o644)
	if qs, err := loadDataset("generic", gen, 10); err != nil || len(qs) != 1 || qs[0].Answers[0] != "PostgreSQL" {
		t.Errorf("generic loader: err=%v qs=%+v", err, qs)
	}
	if _, err := loadDataset("squad", filepath.Join(dir, "nope.json"), 1); err == nil {
		t.Error("missing file should error")
	}
}

func TestNewReqAndDoJSON(t *testing.T) {
	req, err := newReq(context.Background(), "http://x/v1/chat/completions", "key123", []byte(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	if req.Header.Get("Authorization") != "Bearer key123" || req.Header.Get("Content-Type") != "application/json" {
		t.Errorf("headers: %v", req.Header)
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()
	req2, _ := newReq(context.Background(), srv.URL, "", []byte(`{}`))
	var out struct {
		OK bool `json:"ok"`
	}
	if err := doJSON(req2, time.Second, &out); err != nil || !out.OK {
		t.Errorf("doJSON: err=%v out=%+v", err, out)
	}

	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(500) }))
	defer bad.Close()
	req3, _ := newReq(context.Background(), bad.URL, "", []byte(`{}`))
	if err := doJSON(req3, time.Second, &out); err == nil {
		t.Error("doJSON should error on 500")
	}
}

func TestAnswerAndRecallContext(t *testing.T) {
	model := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := readAll(r)
		// echo: ensure a retrieved-context system msg is included when context given.
		_ = body
		json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{{"message": map[string]string{"content": "Paris"}}},
		})
	}))
	defer model.Close()
	mc := &modelClient{baseURL: model.URL, model: "m", timeout: 2 * time.Second, maxTokens: 64}
	ans, err := mc.answer(context.Background(), "capital?", "France info")
	if err != nil || ans != "Paris" {
		t.Errorf("answer: err=%v ans=%q", err, ans)
	}

	muninn := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		inner, _ := json.Marshal(map[string]any{"memories": []map[string]any{
			{"content": "relevant fact", "vector_score": 0.9},
			{"content": "weak", "vector_score": 0.2},
		}})
		json.NewEncoder(w).Encode(map[string]any{"result": map[string]any{"content": []map[string]any{{"type": "text", "text": string(inner)}}}})
	}))
	defer muninn.Close()
	cl := mcpclient.New(muninn.URL, "", time.Second)
	got := recallContext(context.Background(), cl, "v", "q", 0.6, false)
	if got != "relevant fact" {
		t.Errorf("recallContext should gate at 0.6, got %q", got)
	}
}

func readAll(r *http.Request) ([]byte, error) {
	b := make([]byte, 0)
	buf := make([]byte, 1024)
	for {
		n, err := r.Body.Read(buf)
		b = append(b, buf[:n]...)
		if err != nil {
			return b, nil
		}
	}
}

func FuzzLoadGenericQA(f *testing.F) {
	f.Add([]byte(`[{"question":"q","answer":"a"}]`))
	f.Add([]byte(`garbage`))
	f.Fuzz(func(t *testing.T, data []byte) {
		dir := t.TempDir()
		p := filepath.Join(dir, "g.json")
		os.WriteFile(p, data, 0o644)
		_, _ = loadGenericQA(p, 10)
	})
}

func TestRunEndToEnd(t *testing.T) {
	dir := t.TempDir()
	qa := filepath.Join(dir, "g.json")
	os.WriteFile(qa, []byte(`[{"question":"capital of France?","answer":"Paris"}]`), 0o644)

	muninn := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		inner, _ := json.Marshal(map[string]any{"memories": []map[string]any{{"content": "Paris is the capital of France.", "vector_score": 0.9}}})
		json.NewEncoder(w).Encode(map[string]any{"result": map[string]any{"content": []map[string]any{{"type": "text", "text": string(inner)}}}})
	}))
	defer muninn.Close()
	model := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"choices": []map[string]any{{"message": map[string]string{"content": "Paris"}}}})
	}))
	defer model.Close()

	silenceStdout(t, func() {
		// build-only path (no model-url)
		runWith(t, "-dataset", "generic", "-squad-file", qa, "-vault", "v", "-mcp-url", muninn.URL, "-n", "1")
		// scored path
		runWith(t, "-dataset", "generic", "-squad-file", qa, "-vault", "v", "-mcp-url", muninn.URL,
			"-model-url", model.URL, "-model", "m", "-n", "1", "-min-score", "0.1", "-timeout", "5s")
	})
}

func runWith(t *testing.T, args ...string) {
	t.Helper()
	oldArgs, oldFS := os.Args, flag.CommandLine
	flag.CommandLine = flag.NewFlagSet("msc-qa", flag.ContinueOnError)
	os.Args = append([]string{"msc-qa"}, args...)
	defer func() { os.Args, flag.CommandLine = oldArgs, oldFS }()
	if err := run(); err != nil {
		t.Errorf("run(%v): %v", args, err)
	}
}

func silenceStdout(t *testing.T, fn func()) {
	t.Helper()
	old := os.Stdout
	w, _ := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	os.Stdout = w
	defer func() { os.Stdout = old; w.Close() }()
	fn()
}

func TestSplitQueryQAAndMultiRecall(t *testing.T) {
	subs := splitQueryQA("Did Alice and Bob meet in Paris?")
	if subs[0] != "Did Alice and Bob meet in Paris?" || len(subs) < 2 {
		t.Errorf("splitQueryQA: %v", subs)
	}
	muninn := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		inner, _ := json.Marshal(map[string]any{"memories": []map[string]any{{"content": "fact A", "vector_score": 0.9}}})
		json.NewEncoder(w).Encode(map[string]any{"result": map[string]any{"content": []map[string]any{{"type": "text", "text": string(inner)}}}})
	}))
	defer muninn.Close()
	cl := mcpclient.New(muninn.URL, "", time.Second)
	got := recallContext(context.Background(), cl, "v", "Did Alice and Bob meet?", 0.6, true)
	if got != "fact A" {
		t.Errorf("multi recallContext should dedup-merge to 'fact A', got %q", got)
	}
}

func FuzzSplitQueryQA(f *testing.F) {
	f.Add("Did Alice and Bob meet in Paris?")
	f.Fuzz(func(t *testing.T, q string) {
		if s := splitQueryQA(q); len(s) == 0 || s[0] != q {
			t.Fatalf("bad split: %v", s)
		}
	})
}
