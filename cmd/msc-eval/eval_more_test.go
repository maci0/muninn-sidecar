package main

import (
	"encoding/json"
	"flag"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/maci0/muninn-sidecar/internal/inject"
)

func TestTrunc(t *testing.T) {
	if trunc("abcdef", 4) != "abc…" || trunc("ab", 5) != "ab" {
		t.Errorf("trunc")
	}
}

func TestGateMark(t *testing.T) {
	if gateMark(inject.EvalResult{DidInject: true, ShouldInject: true}) != "ok" {
		t.Error("ok")
	}
	if gateMark(inject.EvalResult{DidInject: false, ShouldInject: false}) != "ok" {
		t.Error("ok suppress")
	}
	if gateMark(inject.EvalResult{DidInject: true, ShouldInject: false}) != "FP" {
		t.Error("FP")
	}
	if gateMark(inject.EvalResult{DidInject: false, ShouldInject: true}) != "FN" {
		t.Error("FN")
	}
}

func TestDefaultMCPURLAndToken(t *testing.T) {
	t.Setenv("MUNINN_MCP_URL", "")
	if defaultMCPURL() != "http://127.0.0.1:8750/mcp" {
		t.Error("default")
	}
	t.Setenv("MUNINN_MCP_URL", "http://z/mcp")
	if defaultMCPURL() != "http://z/mcp" {
		t.Error("env")
	}
	if resolveToken("tok") != "tok" {
		t.Error("resolveToken")
	}
}

func TestLoadOfflineScenarios(t *testing.T) {
	// Default (embedded corpus).
	sc, err := loadOfflineScenarios("")
	if err != nil || len(sc) == 0 {
		t.Fatalf("default scenarios: err=%v n=%d", err, len(sc))
	}
	// From file.
	p := filepath.Join(t.TempDir(), "s.json")
	os.WriteFile(p, []byte(`[{"name":"x","candidates":[{"id":"a","concept":"c","content":"y","score":0.9,"relevant":true}]}]`), 0o644)
	sc2, err := loadOfflineScenarios(p)
	if err != nil || len(sc2) != 1 || sc2[0].Name != "x" {
		t.Fatalf("file scenarios: err=%v sc=%+v", err, sc2)
	}
	if _, err := loadOfflineScenarios(filepath.Join(t.TempDir(), "nope.json")); err == nil {
		t.Error("missing file should error")
	}
}

// printers + emitJSON write to stdout; redirect to /dev/null and assert no panic.
func TestPrintersSmoke(t *testing.T) {
	old := os.Stdout
	devnull, _ := os.Open(os.DevNull)
	w, _ := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	os.Stdout = w
	defer func() { os.Stdout = old; w.Close(); devnull.Close() }()

	scenarios, _ := loadOfflineScenarios("")
	results := make([]inject.EvalResult, 0, len(scenarios))
	for _, s := range scenarios {
		results = append(results, inject.RunScenario(s))
	}
	agg := inject.AggregateMetrics(results)
	printOfflineReport(results, agg)
	printSweep(inject.SweepMinScore(scenarios, []float64{0.5, 0.6}))
	printStudy(inject.RunMethodStudy(1, 60, 3))
	printLiveReport([]inject.LiveResult{{Scenario: "s", Recalled: 3, Expected: []string{"c"}, Hits: 1, HitRate: 1}})
	if err := emitJSON(map[string]any{"ok": true}); err != nil {
		t.Errorf("emitJSON: %v", err)
	}
}

func TestRunOfflineAndLive(t *testing.T) {
	old := os.Stdout
	w, _ := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	os.Stdout = w
	defer func() { os.Stdout = old; w.Close() }()

	if err := runOffline("", 0, 0, true, false, false); err != nil {
		t.Errorf("runOffline table: %v", err)
	}
	if err := runOffline("", 0, 0, false, false, true); err != nil {
		t.Errorf("runOffline json: %v", err)
	}

	// runLive with a fake MuninnDB (seed + recall), no model needed.
	muninn := newMuninnServer()
	defer muninn.Close()
	lf := filepath.Join(t.TempDir(), "live.json")
	os.WriteFile(lf, []byte(`[{"name":"a","query":"q","seed":[{"concept":"auth pattern","content":"jwt"}],"expected_concepts":["auth pattern"]}]`), 0o644)
	if err := runLive(lf, muninn.URL, "", "v", 0, 0, 0, 2e9, false); err != nil {
		t.Errorf("runLive: %v", err)
	}
	if err := runLive("", muninn.URL, "", "v", 0, 0, 0, 2e9, false); err == nil {
		t.Error("runLive without live-file should error")
	}
}

func newMuninnServer() *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var rpc struct {
			Params struct {
				Name string `json:"name"`
			} `json:"params"`
		}
		json.Unmarshal(body, &rpc)
		if rpc.Params.Name == "muninn_recall" {
			inner, _ := json.Marshal(map[string]any{"memories": []map[string]any{
				{"concept": "auth pattern", "content": "jwt", "score": 0.9, "vector_score": 0.9},
			}})
			json.NewEncoder(w).Encode(map[string]any{"result": map[string]any{"content": []map[string]any{{"type": "text", "text": string(inner)}}}})
			return
		}
		w.Write([]byte(`{"jsonrpc":"2.0","result":{"id":"ok"},"id":1}`))
	}))
}

func TestRunEval(t *testing.T) {
	old := os.Stdout
	w, _ := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	os.Stdout = w
	oldArgs, oldFS := os.Args, flag.CommandLine
	flag.CommandLine = flag.NewFlagSet("msc-eval", flag.ContinueOnError)
	os.Args = []string{"msc-eval", "-sweep"}
	defer func() { os.Stdout = old; w.Close(); os.Args, flag.CommandLine = oldArgs, oldFS }()
	if err := run(); err != nil {
		t.Errorf("run: %v", err)
	}
}
