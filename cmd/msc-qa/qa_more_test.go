package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/maci0/muninn-sidecar/internal/config"
	"github.com/maci0/muninn-sidecar/internal/mcpclient"
)

func TestSmallHelpers(t *testing.T) {
	if got := splitCSV(" a , b ,, c "); len(got) != 3 || got[0] != "a" || got[2] != "c" {
		t.Errorf("splitCSV %v", got)
	}
	if safe(1, 0) != 0 || safe(2, 4) != 0.5 {
		t.Errorf("safe")
	}
	if config.MCPURL("") != config.DefaultMCPURL {
		t.Errorf("mcp url default")
	}
	if config.Token("flagtok") != "flagtok" {
		t.Errorf("token flag precedence")
	}
}

func TestArmAgg(t *testing.T) {
	var a armAgg
	a.add(1, 1.0, true)
	a.add(0, 0.5, false)
	if a.em() != 0.5 || a.f1() != 0.75 || a.containment() != 0.5 {
		t.Errorf("armAgg em=%v f1=%v containment=%v", a.em(), a.f1(), a.containment())
	}
	a.fail()
	if a.failN() != 1 || a.n() != 2 || a.f1() != 0.75 {
		t.Errorf("failed call must be excluded: failN=%d n=%d f1=%v", a.failN(), a.n(), a.f1())
	}
	var z armAgg
	if z.em() != 0 || z.f1() != 0 || z.containment() != 0 {
		t.Errorf("empty armAgg should be 0")
	}
}

func TestPairedF1Deltas(t *testing.T) {
	var base, arm armAgg
	base.add(0, 0.2, false)
	arm.add(1, 0.9, true)
	base.add(0, 0.1, false)
	arm.fail()
	d := pairedF1Deltas(&base, &arm)
	if len(d) != 1 || !approxf(d[0], 0.7) {
		t.Errorf("pairing must skip failed calls: %v", d)
	}
}

func TestBootstrapCI(t *testing.T) {
	lo, hi := bootstrapCI([]float64{0.5, 0.5, 0.5}, 2000, 1)
	if lo != 0.5 || hi != 0.5 {
		t.Errorf("constant deltas should give a degenerate CI, got [%v, %v]", lo, hi)
	}
	lo, hi = bootstrapCI([]float64{0, 1}, 2000, 1)
	if lo < 0 || hi > 1 || lo > hi {
		t.Errorf("CI out of range: [%v, %v]", lo, hi)
	}
	lo2, hi2 := bootstrapCI([]float64{0, 1}, 2000, 1)
	if lo != lo2 || hi != hi2 {
		t.Error("bootstrap must be deterministic for a fixed seed")
	}
	if lo, hi := bootstrapCI(nil, 2000, 1); lo != 0 || hi != 0 {
		t.Errorf("empty deltas should give [0, 0], got [%v, %v]", lo, hi)
	}
}

func TestUnreliable(t *testing.T) {
	aggs := make([]armAgg, 2)
	for i := 0; i < 10; i++ {
		aggs[0].add(1, 1, false)
		aggs[1].add(1, 1, false)
	}
	if unreliable(aggs) {
		t.Error("no failures should be reliable")
	}
	aggs[1].fail()
	aggs[1].fail() // 2/12 > 10%
	if !unreliable(aggs) {
		t.Error(">10% failures in one arm should flag the row")
	}
}

func TestReproManifest(t *testing.T) {
	fs := flag.NewFlagSet("t", flag.ContinueOnError)
	fs.String("model", "m1", "")
	fs.String("token", "hunter2", "")
	got := reproManifest(fs, "abc123", 20, []string{"m1", "m2"})
	if !strings.Contains(got, `-model="m1"`) || !strings.Contains(got, "dataset-sha256=abc123") ||
		!strings.Contains(got, "questions=20") || !strings.Contains(got, `readers="m1,m2"`) {
		t.Errorf("manifest missing fields: %s", got)
	}
	if strings.Contains(got, "hunter2") || !strings.Contains(got, "<redacted>") {
		t.Errorf("manifest must redact secrets: %s", got)
	}
}

func TestWriteMDBlock(t *testing.T) {
	path := filepath.Join(t.TempDir(), "m.md")
	var a [3]armAgg
	a[0].add(1, 0.2, false)
	a[1].add(1, 0.8, true)
	a[2].add(0, 0.1, false)
	rows := []string{mdRow("modelX", 10, a), mdRow("modelY", 10, a)}
	if err := writeMDBlock(path, "m1", rows); err != nil {
		t.Fatalf("writeMDBlock: %v", err)
	}
	data, _ := os.ReadFile(path)
	if n := countSub(string(data), "model"); n < 2 {
		t.Errorf("expected 2 rows, got content: %s", data)
	}
	if !strings.Contains(string(data), "[") || strings.Contains(string(data), "unreliable") {
		t.Errorf("rows should carry CIs and no unreliable marker: %s", data)
	}
	a[1].fail() // 1/2 calls failed in one arm: >10%
	rows = append(rows, mdRow("modelZ", 10, a))
	if err := writeMDBlock(path, "m1", rows); err != nil {
		t.Fatalf("writeMDBlock: %v", err)
	}
	data, _ = os.ReadFile(path)
	if !strings.Contains(string(data), "unreliable") {
		t.Errorf("row with >10%% failed calls must be flagged: %s", data)
	}
	if err := writeMDBlock(filepath.Join(t.TempDir(), "no", "such", "dir", "m.md"), "m1", rows); err == nil {
		t.Error("writeMDBlock into a missing directory should error")
	}
}

// Rerunning the same configuration must land on the same file, not a second
// copy of the run. A different configuration keeps its own block.
func TestWriteMDBlockIsConvergent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "m.md")
	rows := []string{mdRow("modelX", 10, [3]armAgg{}), mdRow("modelY", 10, [3]armAgg{})}

	if err := writeMDBlock(path, "manifest-A", rows); err != nil {
		t.Fatalf("writeMDBlock: %v", err)
	}

	if err := writeMDBlock(path, "manifest-B", rows); err != nil {
		t.Fatalf("writeMDBlock: %v", err)
	}
	if err := writeMDBlock(path, "manifest-A", rows); err != nil {
		t.Fatalf("writeMDBlock: %v", err)
	}
	// Updating A must leave B's block intact.
	updated := append([]string(nil), rows...)
	updated = append(updated, mdRow("modelZ", 10, [3]armAgg{}))
	if err := writeMDBlock(path, "manifest-A", updated); err != nil {
		t.Fatalf("writeMDBlock: %v", err)
	}
	before, _ := os.ReadFile(path)
	if err := writeMDBlock(path, "manifest-A", updated); err != nil {
		t.Fatalf("writeMDBlock: %v", err)
	}
	second, _ := os.ReadFile(path)

	if string(before) != string(second) {
		t.Errorf("rerun changed the file\nbefore:\n%s\nafter:\n%s", before, second)
	}
	if n := countSub(string(second), "<!-- msc-qa repro: manifest-A -->"); n != 1 {
		t.Errorf("expected exactly 1 block for manifest-A, got %d:\n%s", n, second)
	}
	if n := countSub(string(second), "<!-- msc-qa repro: manifest-B -->"); n != 1 {
		t.Errorf("expected exactly 1 block for manifest-B, got %d:\n%s", n, second)
	}
	if n := countSub(string(second), "| modelX |"); n != 2 {
		t.Errorf("expected 1 modelX row per block (2 blocks), got %d:\n%s", n, second)
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
	if qs, err := loadDataset("squad", squad, 10, 1); err != nil || len(qs) != 1 || qs[0].Answers[0] != "A" {
		t.Errorf("squad loader: err=%v qs=%+v", err, qs)
	}
	hp := filepath.Join(dir, "h.json")
	os.WriteFile(hp, []byte(`[{"question":"q","answer":"yes"}]`), 0o644)
	if qs, err := loadDataset("hotpot", hp, 10, 1); err != nil || len(qs) != 1 || qs[0].Answers[0] != "yes" {
		t.Errorf("hotpot loader: err=%v qs=%+v", err, qs)
	}
	gen := filepath.Join(dir, "g.json")
	os.WriteFile(gen, []byte(`[{"question":"q","answer":"PostgreSQL"}]`), 0o644)
	if qs, err := loadDataset("generic", gen, 10, 1); err != nil || len(qs) != 1 || qs[0].Answers[0] != "PostgreSQL" {
		t.Errorf("generic loader: err=%v qs=%+v", err, qs)
	}
	if _, err := loadDataset("squad", filepath.Join(dir, "nope.json"), 1, 1); err == nil {
		t.Error("missing file should error")
	}
}

// TestSampleSeed: sampling shuffles the whole eligible pool with the seed then
// truncates, so the same seed yields the same paired sample and a different
// seed yields a different one (not just the first n in file order).
func TestSampleSeed(t *testing.T) {
	gen := filepath.Join(t.TempDir(), "g.json")
	var sb strings.Builder
	sb.WriteString("[")
	for i := 0; i < 10; i++ {
		if i > 0 {
			sb.WriteString(",")
		}
		fmt.Fprintf(&sb, `{"question":"q%d","answer":"a%d"}`, i, i)
	}
	sb.WriteString("]")
	os.WriteFile(gen, []byte(sb.String()), 0o644)

	a, err := loadDataset("generic", gen, 5, 1)
	if err != nil || len(a) != 5 {
		t.Fatalf("loadDataset: err=%v len=%d", err, len(a))
	}
	b, _ := loadDataset("generic", gen, 5, 1)
	if !reflect.DeepEqual(a, b) {
		t.Error("same seed must give the same sample")
	}
	c, _ := loadDataset("generic", gen, 5, 2)
	if reflect.DeepEqual(a, c) {
		t.Error("different seed should give a different sample")
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
		// Verify the retrieved-context block is actually forwarded to the model
		// when a context is supplied (the whole point of recall context). Without
		// this the test passes even if the context were silently dropped.
		if !strings.Contains(string(body), "France info") {
			t.Errorf("context block not forwarded to model: %s", body)
		}
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
	cands, err := recallStructured(context.Background(), cl, "v", "q", 0.6, false)
	if err != nil {
		t.Fatalf("recall: %v", err)
	}
	if got := contents(cands); got != "relevant fact" {
		t.Errorf("recall should gate at 0.6, got %q", got)
	}
}

// TestRecallErrorIsReported: a failing vault must surface as an error, not as
// an empty candidate set, or every arm is built from nothing and the run
// reports answer-coverage 0/100 as a measurement.
func TestRecallErrorIsReported(t *testing.T) {
	muninn := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer muninn.Close()
	cl := mcpclient.New(muninn.URL, "", time.Second)
	defer cl.Close()
	if _, err := recallStructured(context.Background(), cl, "v", "q", 0.6, false); err == nil {
		t.Fatal("expected an error when the recall call fails")
	}
}

// A 2xx whose body is not a JSON-RPC reply is the same class of fault as a
// failed call: read as "no memories" it becomes answer-coverage 0/100 reported
// as a real score, so the parse failure has to reach the caller.
func TestUnparseableRecallIsReported(t *testing.T) {
	muninn := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// A well-formed HTTP 200 carrying an intermediary's HTML page.
		w.Write([]byte("<html>gateway error</html>"))
	}))
	defer muninn.Close()
	cl := mcpclient.New(muninn.URL, "", time.Second)
	defer cl.Close()
	cands, err := recallStructured(context.Background(), cl, "v", "q", 0.6, false)
	if err == nil {
		t.Fatalf("an undecodable recall reply must be an error, got %d candidates", len(cands))
	}
	if len(cands) != 0 {
		t.Errorf("expected no candidates alongside the error, got %d", len(cands))
	}
}

// A reply that decodes and carries no memories is a genuine empty result, not a
// failure: the run reports 0 recall without erroring.
func TestEmptyRecallIsNotAnError(t *testing.T) {
	cands, err := parseRecallPayload([]byte(`{"result":{"content":[{"type":"text","text":"{\"memories\":[]}"}]}}`), 0.6)
	if err != nil {
		t.Fatalf("an empty memory list is a measurement, not a fault: %v", err)
	}
	if len(cands) != 0 {
		t.Errorf("want no candidates, got %d", len(cands))
	}
}

// TestRecallErrorReachesCaller: a transport failure must not be indistinguishable
// from an empty vault, or the arms are scored on empty context and the run reads
// as a completed evaluation.
func TestRecallErrorReachesCaller(t *testing.T) {
	cl := mcpclient.New("http://127.0.0.1:1/mcp", "", time.Second)
	defer cl.Close()
	cands, err := recallStructured(context.Background(), cl, "v", "q", 0.6, false)
	if err == nil {
		t.Fatalf("expected a transport error, got %d candidates", len(cands))
	}
	if len(cands) != 0 {
		t.Errorf("expected no candidates alongside the error, got %d", len(cands))
	}
}

// TestRunRejectsBadInjectFormat: the presentation enum's default branch is a
// valid mode, so a typo would silently score the bare arm under another name.
func TestRunRejectsBadInjectFormat(t *testing.T) {
	oldArgs, oldFS := os.Args, flag.CommandLine
	defer func() { os.Args, flag.CommandLine = oldArgs, oldFS }()
	flag.CommandLine = flag.NewFlagSet("msc-qa", flag.ContinueOnError)
	os.Args = []string{"msc-qa", "-inject-format", "scoredd"}
	if err := run(); err == nil {
		t.Error("expected a usage error for an unknown -inject-format")
	}
}

// contents renders recall candidates the way the prompt does, one per line.
func contents(cands []cand) string {
	out := make([]string, len(cands))
	for i, c := range cands {
		out[i] = c.Content
	}
	return strings.Join(out, "\n")
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
		_, _ = loadFlatQA(p, "generic qa", 10)
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

	md := filepath.Join(dir, "out.md")
	silenceStdout(t, func() {
		// build-only path (no model-url)
		runWith(t, "-dataset", "generic", "-squad-file", qa, "-vault", "v", "-mcp-url", muninn.URL, "-n", "1")
		// scored path
		runWith(t, "-dataset", "generic", "-squad-file", qa, "-vault", "v", "-mcp-url", muninn.URL,
			"-model-url", model.URL, "-model", "m", "-n", "1", "-min-score", "0.1", "-timeout", "5s", "-md", md)
	})
	data, _ := os.ReadFile(md)
	if !strings.Contains(string(data), "msc-qa repro:") || !strings.Contains(string(data), "dataset-sha256=") {
		t.Errorf("-md output missing repro manifest: %s", data)
	}
	if countSub(string(data), "| m |") != 1 {
		t.Errorf("-md output missing results row: %s", data)
	}
}

// TestFailedCallsExcluded: a reader whose every call fails must not deflate the
// aggregates to fake zeros; the run completes and the -md row is flagged
// unreliable because every arm lost >10% of its calls.
func TestFailedCallsExcluded(t *testing.T) {
	dir := t.TempDir()
	qa := filepath.Join(dir, "g.json")
	os.WriteFile(qa, []byte(`[{"question":"capital of France?","answer":"Paris"}]`), 0o644)

	muninn := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		inner, _ := json.Marshal(map[string]any{"memories": []map[string]any{{"content": "Paris is the capital of France.", "vector_score": 0.9}}})
		json.NewEncoder(w).Encode(map[string]any{"result": map[string]any{"content": []map[string]any{{"type": "text", "text": string(inner)}}}})
	}))
	defer muninn.Close()
	model := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer model.Close()

	md := filepath.Join(dir, "out.md")
	silenceStdout(t, func() {
		runWith(t, "-dataset", "generic", "-squad-file", qa, "-vault", "v", "-mcp-url", muninn.URL,
			"-model-url", model.URL, "-model", "m", "-n", "1", "-min-score", "0.1", "-timeout", "5s", "-md", md)
	})
	data, _ := os.ReadFile(md)
	if !strings.Contains(string(data), "unreliable") {
		t.Errorf("all-failed run must flag the row unreliable: %s", data)
	}
}

// TestDistractorBypassesGate: with the vault's only memory below the gate, the
// injected arm is empty but the distractor arm (the shifted question's ungated
// recall) must still carry that memory, so per question exactly one of the
// three model calls contains it.
func TestDistractorBypassesGate(t *testing.T) {
	dir := t.TempDir()
	qa := filepath.Join(dir, "g.json")
	os.WriteFile(qa, []byte(`[{"question":"capital of France?","answer":"Paris"},{"question":"capital of Spain?","answer":"Madrid"}]`), 0o644)

	muninn := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		inner, _ := json.Marshal(map[string]any{"memories": []map[string]any{{"content": "irrelevant cooking fact", "vector_score": 0.3}}})
		json.NewEncoder(w).Encode(map[string]any{"result": map[string]any{"content": []map[string]any{{"type": "text", "text": string(inner)}}}})
	}))
	defer muninn.Close()
	withCtx := 0
	model := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := readAll(r)
		if strings.Contains(string(body), "irrelevant cooking fact") {
			withCtx++
		}
		json.NewEncoder(w).Encode(map[string]any{"choices": []map[string]any{{"message": map[string]string{"content": "Paris"}}}})
	}))
	defer model.Close()

	silenceStdout(t, func() {
		runWith(t, "-dataset", "generic", "-squad-file", qa, "-vault", "v", "-mcp-url", muninn.URL,
			"-model-url", model.URL, "-model", "m", "-n", "2", "-min-score", "0.6", "-timeout", "5s")
	})
	if withCtx != 2 {
		t.Errorf("expected exactly 2 model calls with distractor context, got %d", withCtx)
	}
}

// TestSingleQuestionDistractorEmpty: with only one question there is no other
// question to borrow a distractor from, and the shift must not wrap the
// question's own (correct) recall into that arm. Only the injected call may
// carry the memory.
func TestSingleQuestionDistractorEmpty(t *testing.T) {
	dir := t.TempDir()
	qa := filepath.Join(dir, "g.json")
	os.WriteFile(qa, []byte(`[{"question":"capital of France?","answer":"Paris"}]`), 0o644)

	muninn := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		inner, _ := json.Marshal(map[string]any{"memories": []map[string]any{{"content": "Paris is the capital of France.", "vector_score": 0.9}}})
		json.NewEncoder(w).Encode(map[string]any{"result": map[string]any{"content": []map[string]any{{"type": "text", "text": string(inner)}}}})
	}))
	defer muninn.Close()
	withCtx := 0
	model := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := readAll(r)
		if strings.Contains(string(body), "Paris is the capital of France.") {
			withCtx++
		}
		json.NewEncoder(w).Encode(map[string]any{"choices": []map[string]any{{"message": map[string]string{"content": "Paris"}}}})
	}))
	defer model.Close()

	silenceStdout(t, func() {
		runWith(t, "-dataset", "generic", "-squad-file", qa, "-vault", "v", "-mcp-url", muninn.URL,
			"-model-url", model.URL, "-model", "m", "-n", "1", "-min-score", "0.6", "-timeout", "5s")
	})
	if withCtx != 1 {
		t.Errorf("expected only the injected call to carry the memory, got %d calls with it", withCtx)
	}
}

func TestRunRejectsNonPositiveN(t *testing.T) {
	for _, n := range []string{"0", "-3"} {
		oldArgs, oldFS := os.Args, flag.CommandLine
		flag.CommandLine = flag.NewFlagSet("msc-qa", flag.ContinueOnError)
		os.Args = []string{"msc-qa", "-n", n}
		err := run()
		os.Args, flag.CommandLine = oldArgs, oldFS
		if err == nil {
			t.Errorf("run should reject -n %s", n)
		}
	}
}

func TestRunRejectsUncappedOrUnboundedReader(t *testing.T) {
	// A non-positive -max-tokens leaves the reader's output length to the
	// provider, and a non-positive -ground-topk grades every recalled passage
	// in one judge call. Both are caps, so both fail loud at startup.
	for _, args := range [][]string{
		{"-max-tokens", "0"},
		{"-max-tokens", "-8"},
		{"-ground-topk", "0"},
		{"-ground-topk", "-2"},
	} {
		oldArgs, oldFS := os.Args, flag.CommandLine
		flag.CommandLine = flag.NewFlagSet("msc-qa", flag.ContinueOnError)
		os.Args = append([]string{"msc-qa"}, args...)
		err := run()
		os.Args, flag.CommandLine = oldArgs, oldFS
		if err == nil {
			t.Errorf("run should reject %v", args)
		}
	}
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

func TestMultiRecall(t *testing.T) {
	muninn := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		inner, _ := json.Marshal(map[string]any{"memories": []map[string]any{{"content": "fact A", "vector_score": 0.9}}})
		json.NewEncoder(w).Encode(map[string]any{"result": map[string]any{"content": []map[string]any{{"type": "text", "text": string(inner)}}}})
	}))
	defer muninn.Close()
	cl := mcpclient.New(muninn.URL, "", time.Second)
	merged, err := recallStructured(context.Background(), cl, "v", "Did Alice and Bob meet?", 0.6, true)
	if err != nil {
		t.Fatalf("multi recall: %v", err)
	}
	if got := contents(merged); got != "fact A" {
		t.Errorf("multi recall should dedup-merge to 'fact A', got %q", got)
	}
}

// TestMultiRecallDedupKeepsMaxScore: when the same memory is returned by
// several sub-queries with different scores, the merged candidate must carry
// the best score, or an in-process gate would reject a memory the per-sub-query
// gate used to admit.
func TestMultiRecallDedupKeepsMaxScore(t *testing.T) {
	muninn := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := readAll(r)
		score := 0.45 // full question scores low
		if strings.Contains(string(body), "Danny Green") {
			score = 0.85 // entity sub-query scores high
		}
		inner, _ := json.Marshal(map[string]any{"memories": []map[string]any{{"content": "fact A", "vector_score": score}}})
		json.NewEncoder(w).Encode(map[string]any{"result": map[string]any{"content": []map[string]any{{"type": "text", "text": string(inner)}}}})
	}))
	defer muninn.Close()
	cl := mcpclient.New(muninn.URL, "", time.Second)
	cands, err := recallStructured(context.Background(), cl, "v", "who coached Danny Green?", 0, true)
	if err != nil {
		t.Fatalf("multi recall: %v", err)
	}
	if len(cands) != 1 {
		t.Fatalf("expected 1 deduped candidate, got %v", cands)
	}
	if cands[0].Score != 0.85 {
		t.Errorf("dedup must keep the max score across sub-queries, got %v", cands[0].Score)
	}
}

// TestDistractorGoldWarning: when the shifted-recall distractor happens to
// contain a question's own gold answer, the run must warn instead of silently
// understating distractor harm.
func TestDistractorGoldWarning(t *testing.T) {
	dir := t.TempDir()
	qa := filepath.Join(dir, "g.json")
	os.WriteFile(qa, []byte(`[{"question":"capital of France?","answer":"Paris"},{"question":"largest city of France?","answer":"Paris"}]`), 0o644)

	muninn := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		inner, _ := json.Marshal(map[string]any{"memories": []map[string]any{{"content": "Paris is the capital and largest city of France.", "vector_score": 0.9}}})
		json.NewEncoder(w).Encode(map[string]any{"result": map[string]any{"content": []map[string]any{{"type": "text", "text": string(inner)}}}})
	}))
	defer muninn.Close()
	model := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"choices": []map[string]any{{"message": map[string]string{"content": "Paris"}}}})
	}))
	defer model.Close()

	stderr := captureStderr(t, func() {
		silenceStdout(t, func() {
			runWith(t, "-dataset", "generic", "-squad-file", qa, "-vault", "v", "-mcp-url", muninn.URL,
				"-model-url", model.URL, "-model", "m", "-n", "2", "-min-score", "0.6", "-timeout", "5s")
		})
	})
	if !strings.Contains(stderr, "distractor context contains the gold answer for 2/2") {
		t.Errorf("expected distractor-gold warning, stderr:\n%s", stderr)
	}
}

func captureStderr(t *testing.T, fn func()) string {
	t.Helper()
	old := os.Stderr
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stderr = w
	done := make(chan string)
	go func() {
		b, _ := io.ReadAll(r)
		done <- string(b)
	}()
	defer func() { os.Stderr = old }()
	fn()
	w.Close()
	return <-done
}

// A report edited on Windows (or checked out with core.autocrlf) has CRLF
// endings. The manifest marker must still match on a rerun, or every run
// appends another block and the file grows without bound.
func TestWriteMDBlockCRLF(t *testing.T) {
	path := filepath.Join(t.TempDir(), "m.md")
	rows := []string{mdRow("modelX", 10, [3]armAgg{}), mdRow("modelY", 10, [3]armAgg{})}

	first := replaceMDBlock("", mdManifestPrefix+" manifest-A -->",
		mdManifestPrefix+" manifest-A -->\n"+strings.Join(rows, ""))
	if err := os.WriteFile(path, []byte(strings.ReplaceAll(first, "\n", "\r\n")), 0o644); err != nil {
		t.Fatalf("seed CRLF file: %v", err)
	}
	for i := range 3 {
		if err := writeMDBlock(path, "manifest-A", rows); err != nil {
			t.Fatalf("writeMDBlock run %d: %v", i, err)
		}
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if n := countSub(string(data), mdManifestPrefix); n != 1 {
		t.Errorf("CRLF reruns appended blocks: %d manifest comments in %q", n, data)
	}
	if n := countSub(string(data), "modelY"); n != 1 {
		t.Errorf("CRLF reruns duplicated rows: %d in %q", n, data)
	}
	// The whole file keeps one line ending; a lone LF would be a mix.
	lines := strings.Split(strings.ReplaceAll(string(data), "\r\n", "\n"), "\n")
	for i, l := range lines[:len(lines)-1] {
		if strings.HasSuffix(l, "\r") {
			t.Errorf("line %d still carries a stray CR: %q", i, l)
		}
	}
	if !strings.Contains(string(data), "\r\n") {
		t.Errorf("CRLF file lost its endings: %q", data)
	}
}

// FuzzParseRecallPayload drives the muninn_recall reply decode: server-controlled
// JSON, parsed twice (envelope, then the text block's payload), whose float
// fields drive both the gate and the relevance number printed to the report.
// The bug a "does not crash" check misses is a candidate that gets past the gate
// carrying a score the report cannot render.
func FuzzParseRecallPayload(f *testing.F) {
	// Seeds span the shapes the real server returns: a text block, a non-text
	// block that must be skipped, memories keyed on vector_score vs score, the
	// two error envelopes, and non-finite-adjacent numbers.
	f.Add([]byte(`{"result":{"content":[{"type":"text","text":"{\"memories\":[{\"concept\":\"c\",\"content\":\"x\",\"vector_score\":0.9}]}"}]}}`), 0.6)
	f.Add([]byte(`{"result":{"content":[{"type":"text","text":"{\"memories\":[{\"concept\":\"c\",\"content\":\"x\",\"score\":0.42}]}"}]}}`), 0.0)
	f.Add([]byte(`{"result":{"content":[{"type":"image","text":"{\"memories\":[]}"}]}}`), 0.0)
	f.Add([]byte(`{"result":{"content":[{"type":"text","text":"not json"}]}}`), 0.0)
	f.Add([]byte(`{"result":{}}`), 0.0)
	f.Add([]byte(`[]`), 0.0)
	f.Add([]byte(``), 0.0)
	f.Add([]byte(`{"result":{"content":[{"type":"text","text":"{\"memories\":[{\"vector_score\":1e308},{\"vector_score\":-1e308},{\"score\":1e400}]}"}]}}`), 0.0)
	f.Fuzz(func(t *testing.T, resp []byte, minScore float64) {
		if math.IsNaN(minScore) {
			return // no server-controlled input picks the threshold; a NaN gate
			// compares false against everything and is not a parser contract
		}
		cands, err := parseRecallPayload(resp, minScore)
		if err != nil {
			// A reply that does not decode is reported, never returned as an
			// empty candidate set: a broken envelope read as "no memories" would
			// be scored as answer-coverage 0/100, a real-looking measurement of a
			// transport fault. The multi-error harness above already pins the
			// message; the fuzz body only needs the no-panic contract.
			return
		}
		for i, c := range cands {
			// The gate itself: nothing below the threshold may be returned.
			if c.Score < minScore {
				t.Fatalf("cand %d: score %v below gate %v", i, c.Score, minScore)
			}
			// A score that reaches the report must render as a number, not
			// "NaN" or "+Inf": the ungated arm in main.go prints every candidate.
			if math.IsNaN(c.Score) || math.IsInf(c.Score, 0) {
				t.Fatalf("cand %d: non-finite score %v", i, c.Score)
			}
			out := formatInjected(cands, "scored")
			if strings.Contains(out, "NaN") || strings.Contains(out, "Inf") {
				t.Fatalf("scored rendering of non-finite score: %q", out)
			}
		}
		// formatInjected is the only consumer of these; no candidates must mean
		// no report body, not an empty header.
		if len(cands) == 0 && formatInjected(cands, "scored") != "" {
			t.Fatalf("empty candidate set rendered %q", formatInjected(cands, "scored"))
		}
	})
}

// The append branch has its own line-ending hazard: a first run into a CRLF
// file with no marker yet, and one whose last line was left unterminated by an
// editor. Both must come out CRLF end to end.
func TestWriteMDBlockAppendCRLF(t *testing.T) {
	path := filepath.Join(t.TempDir(), "m.md")
	rows := []string{mdRow("modelX", 10, [3]armAgg{})}
	// An existing CRLF note with no trailing newline, so the separator has to be
	// synthesized too.
	if err := os.WriteFile(path, []byte("| existing |\r\n| note | no final break |"), 0o644); err != nil {
		t.Fatalf("seed CRLF file: %v", err)
	}
	if err := writeMDBlock(path, "manifest-A", rows); err != nil {
		t.Fatalf("writeMDBlock: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	got := string(data)
	if n := countSub(got, mdManifestPrefix); n != 1 {
		t.Errorf("append wrote %d manifest comments, want 1: %q", n, got)
	}
	if !strings.Contains(got, "manifest-A -->\r\n") {
		t.Errorf("appended block used the wrong line ending: %q", got)
	}
	if !strings.Contains(got, "| note | no final break |\r\n") {
		t.Errorf("the unterminated last line did not gain a CRLF: %q", got)
	}
	for i, l := range strings.Split(got, "\n")[:len(strings.Split(got, "\n"))-1] {
		if !strings.HasSuffix(l, "\r") {
			t.Errorf("line %d lost its CR: %q", i, l)
		}
	}
}
