package main

import (
	"context"
	"encoding/json"
	"flag"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/maci0/muninn-sidecar/internal/mcpclient"
)

func fakeMuninn() *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var rpc struct {
			Params struct {
				Name string `json:"name"`
			} `json:"params"`
		}
		json.Unmarshal(body, &rpc)
		switch rpc.Params.Name {
		case "muninn_recall":
			inner, _ := json.Marshal(map[string]any{"memories": []map[string]any{
				{"concept": "a#0", "content": "ctx", "score": 0.9, "vector_score": 0.8},
			}})
			json.NewEncoder(w).Encode(map[string]any{"result": map[string]any{"content": []map[string]any{{"type": "text", "text": string(inner)}}}})
		default:
			w.Write([]byte(`{"jsonrpc":"2.0","result":{"id":"ok"},"id":1}`))
		}
	}))
}

func TestSeedCorpusAndRunProbes(t *testing.T) {
	srv := fakeMuninn()
	defer srv.Close()
	c := mcpclient.New(srv.URL, "", 2*time.Second)

	items := []item{{Concept: "a#0", Content: "ctx"}, {Concept: "b#0", Content: "other"}}
	if err := seedCorpus(context.Background(), c, "v", items); err != nil {
		t.Fatalf("seedCorpus: %v", err)
	}

	probes := []probe{
		{Query: "q", Gold: "a#0", Present: true},
		{Query: "z", Gold: "", Present: false},
	}
	results, err := runProbes(context.Background(), c, "v", probes, 5, probeOpts{mode: "semantic"})
	if err != nil {
		t.Fatalf("runProbes: %v", err)
	}
	if len(results) != 2 {
		t.Fatalf("expected 2 results, got %d", len(results))
	}
	if results[0].RankByVec != 0 {
		t.Errorf("present gold should rank 0, got %d", results[0].RankByVec)
	}
}

func TestPrintersSmoke(t *testing.T) {
	old := os.Stdout
	w, _ := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	os.Stdout = w
	defer func() { os.Stdout = old; w.Close() }()

	results := []probeResult{
		{probe: probe{Gold: "g", Present: true}, Recalled: []recalledMemory{{Concept: "g", VectorScore: 0.8}}, RankByVec: 0, RankRerank: 0, RankArtVec: 0},
		{probe: probe{Present: false}, RankByVec: -1, RankRerank: -1, RankArtVec: -1},
	}
	rep := analyze(results)
	printReport(rep, results)
	printGate("vector", rep.GateByVec)
	printGate("score", rep.GateByScore)
}

func TestRunBench(t *testing.T) {
	srv := fakeMuninn()
	defer srv.Close()
	old := os.Stdout
	w, _ := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	os.Stdout = w
	oldArgs, oldFS := os.Args, flag.CommandLine
	flag.CommandLine = flag.NewFlagSet("msc-bench", flag.ContinueOnError)
	os.Args = []string{"msc-bench", "-seed", "-probe", "-corpus", "agentmem", "-vault", "v",
		"-mcp-url", srv.URL, "-n", "6", "-present", "6", "-absent", "2", "-mode", "semantic"}
	defer func() { os.Stdout = old; w.Close(); os.Args, flag.CommandLine = oldArgs, oldFS }()
	if err := run(); err != nil {
		t.Errorf("run: %v", err)
	}
}

// TestRunBenchRewriteKey: -rewrite-key must reach the rewrite backend as a
// bearer token; a keyless call against an authenticated endpoint would fail
// open and silently measure the baseline.
func TestRunBenchRewriteKey(t *testing.T) {
	srv := fakeMuninn()
	defer srv.Close()
	var gotAuth string
	rw := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.Write([]byte(`{"choices":[{"message":{"content":"sub one"}}]}`))
	}))
	defer rw.Close()
	old := os.Stdout
	w, _ := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	os.Stdout = w
	oldArgs, oldFS := os.Args, flag.CommandLine
	flag.CommandLine = flag.NewFlagSet("msc-bench", flag.ContinueOnError)
	os.Args = []string{"msc-bench", "-probe", "-corpus", "agentmem", "-vault", "v",
		"-mcp-url", srv.URL, "-n", "4", "-absent", "1", "-mode", "semantic",
		"-rewrite-url", rw.URL, "-rewrite-key", "sk-test"}
	defer func() { os.Stdout = old; w.Close(); os.Args, flag.CommandLine = oldArgs, oldFS }()
	if err := run(); err != nil {
		t.Fatalf("run: %v", err)
	}
	rw.Close() // waits for handlers, so reading gotAuth is race-free
	if gotAuth != "Bearer sk-test" {
		t.Errorf("rewrite backend got Authorization %q, want %q", gotAuth, "Bearer sk-test")
	}
}

func TestRecallMerged(t *testing.T) {
	srv := fakeMuninn()
	defer srv.Close()
	c := mcpclient.New(srv.URL, "", 2*time.Second)
	// single
	ms, err := recallMerged(context.Background(), c, "v", "q", 5, "semantic", false)
	if err != nil || len(ms) == 0 {
		t.Fatalf("single recallMerged: err=%v n=%d", err, len(ms))
	}
	// multi (splits "Scott Derrickson" etc.; fake server returns same set → deduped)
	ms2, err := recallMerged(context.Background(), c, "v", "Was Scott Derrickson here?", 5, "semantic", true)
	if err != nil || len(ms2) == 0 {
		t.Fatalf("multi recallMerged: err=%v n=%d", err, len(ms2))
	}
}
