package main

import (
	"context"
	"encoding/json"
	"flag"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
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

// TestRunProbesAllFail: with every probe failing, the run measured nothing, so
// it must report the failure instead of an all-zero report and a success exit.
func TestRunProbesAllFail(t *testing.T) {
	c := mcpclient.New("http://127.0.0.1:1/mcp", "", 2*time.Second)
	defer c.Close()
	probes := []probe{{Query: "q", Gold: "a#0", Present: true}, {Query: "z", Present: false}}
	results, err := runProbes(context.Background(), c, "v", probes, 5, probeOpts{})
	if err == nil {
		t.Fatalf("expected an error when every probe fails, got %d results", len(results))
	}
	if results != nil {
		t.Errorf("expected no results alongside the error, got %d", len(results))
	}
}

// TestRunRejectsBadEnums: a typo'd enum would otherwise fall through to the
// default branch of the switch that consumes it and measure the baseline under
// another configuration's name.
func TestRunRejectsBadEnums(t *testing.T) {
	for _, args := range [][]string{
		{"msc-bench", "-probe", "-chunk", "sentnce"},
		{"msc-bench", "-probe", "-query-transform", "emphsis"},
		{"msc-bench", "-probe", "-rerank", "lexicalx"},
	} {
		oldArgs, oldFS := os.Args, flag.CommandLine
		flag.CommandLine = flag.NewFlagSet("msc-bench", flag.ContinueOnError)
		os.Args = args
		err := run()
		os.Args, flag.CommandLine = oldArgs, oldFS
		if err == nil {
			t.Errorf("%v: expected a usage error, got nil", args)
		}
	}
}

// captureStdout returns everything fn writes to os.Stdout, so a printer test can
// assert on the report a reader would see rather than only that it did not panic.
// The pipe is drained in a goroutine: it holds far less than a full report, so a
// synchronous read would deadlock.
func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	old := os.Stdout
	os.Stdout = w
	done := make(chan string, 1)
	go func() {
		b, _ := io.ReadAll(r)
		done <- string(b)
	}()
	fn()
	os.Stdout = old
	w.Close()
	out := <-done
	r.Close()
	return out
}

// TestPrintReport: the report is this tool's only output, so the numbers it
// prints are the result. A printer that emitted an empty body, or a probe that
// stopped counting toward the totals, would still have "not panicked".
func TestPrintReport(t *testing.T) {
	results := []probeResult{
		{probe: probe{Gold: "g", Present: true}, Recalled: []recalledMemory{{Concept: "g", VectorScore: 0.8}}, RankByVec: 0, RankRerank: 0, RankArtVec: 0},
		{probe: probe{Present: false}, RankByVec: -1, RankRerank: -1, RankArtVec: -1},
	}
	rep := analyze(results)
	out := captureStdout(t, func() { printReport(rep, results) })
	for _, want := range []string{
		"present probes: 1   absent probes: 1",
		"BEST gate on vector",
		"AUTO-CALIBRATED gate:",
		"ranked by vector     R@1=1.00 R@3=1.00 R@5=1.00 MRR=1.000",
		"article-level (vec)  R@1=1.00 R@3=1.00 R@5=1.00 MRR=1.000",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("report missing %q:\n%s", want, out)
		}
	}
}

// TestPrintGate: one row per swept threshold, each carrying that threshold's
// metrics. An empty sweep prints the header and no rows, so a gate table that
// silently lost its thresholds is visible.
func TestPrintGate(t *testing.T) {
	pts := []gatePoint{{Threshold: 0.3, GateAcc: 1, GateF1: 1, InjectWhenS: 1, SuppressOK: 1, WhatCorrect: 1}}
	out := captureStdout(t, func() { printGate("vector", pts) })
	if !strings.Contains(out, "thresh") || !strings.Contains(out, "0.300") {
		t.Errorf("gate table missing header or row:\n%s", out)
	}
	empty := captureStdout(t, func() { printGate("vector", nil) })
	if strings.Contains(empty, "0.300") {
		t.Errorf("empty sweep printed a threshold row:\n%s", empty)
	}
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
