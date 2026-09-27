package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/maci0/muninn-sidecar/internal/config"
	"github.com/maci0/muninn-sidecar/internal/mcpclient"
)

// TestMainHelp re-execs this test binary with a sentinel env var so main() runs
// inside the coverage-instrumented process. --help / -h makes main() exit 0
// without touching the network or launching anything.
func TestMainHelp(t *testing.T) {
	if os.Getenv("MSC_RUN_MAIN") == "1" {
		os.Args = []string{"msc-bench", "-h"}
		main()
		return
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestMainHelp$")
	cmd.Env = append(os.Environ(), "MSC_RUN_MAIN=1")
	if err := cmd.Run(); err != nil {
		t.Errorf("main(-h) should exit 0, got %v", err)
	}
}

// Seeding the same corpus twice must not double it. Every seeded memory carries
// a content-addressed dedup_key, so a rerun of `-seed` presents the server with
// the same identities and stores nothing new. Duplicates are not harmless in a
// benchmark vault: they crowd recall's top-k and skew the retrieval numbers the
// tool exists to measure.
func TestSeedCorpusIsIdempotent(t *testing.T) {
	var mu sync.Mutex
	stored := map[string]bool{} // dedup_key of memories actually stored
	keyless := 0

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var rpc struct {
			Params struct {
				Name      string `json:"name"`
				Arguments struct {
					Memories []struct {
						Concept  string `json:"concept"`
						Content  string `json:"content"`
						DedupKey string `json:"dedup_key"`
					} `json:"memories"`
				} `json:"arguments"`
			} `json:"params"`
		}
		json.Unmarshal(body, &rpc)
		mu.Lock()
		if rpc.Params.Name == "muninn_remember_batch" {
			for _, m := range rpc.Params.Arguments.Memories {
				if m.DedupKey == "" {
					keyless++
					continue
				}
				// Stand-in for the server's dedup_key handling.
				stored[m.DedupKey] = true
			}
		}
		mu.Unlock()
		w.Write([]byte(`{"jsonrpc":"2.0","result":{"id":"ok"},"id":1}`))
	}))
	defer srv.Close()

	items := []item{
		{Concept: "auth#0", Content: "sessions use a signed cookie"},
		{Concept: "db#1", Content: "the store is postgres"},
	}
	c := mcpclient.New(srv.URL, "", 5*time.Second)
	for i := range 2 {
		if err := seedCorpus(t.Context(), c, "bench-rerun", items); err != nil {
			t.Fatalf("seed run %d: %v", i+1, err)
		}
	}

	mu.Lock()
	defer mu.Unlock()
	if keyless != 0 {
		t.Errorf("%d seeded memories carried no dedup_key", keyless)
	}
	if len(stored) != len(items) {
		t.Errorf("two seed runs stored %d memories, want %d", len(stored), len(items))
	}
}

// TestMainUsageExitCode pins the exit code for a bad flag value: 2, the code
// the flag package already uses for an unparseable command line and the code
// msc exits with, so a script can tell a typo from a runtime failure.
func TestMainUsageExitCode(t *testing.T) {
	if os.Getenv("MSC_RUN_MAIN") == "1" {
		os.Args = []string{"msc-bench", "-ground-topk", "0"}
		main()
		return
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestMainUsageExitCode$")
	cmd.Env = append(os.Environ(), "MSC_RUN_MAIN=1")
	cmd.Stderr = io.Discard
	var exit *exec.ExitError
	if err := cmd.Run(); !errors.As(err, &exit) || exit.ExitCode() != 2 {
		t.Errorf("main(-ground-topk 0) should exit 2, got %v", err)
	}
}

// An explicitly written `-mcp-url=` is an empty flag value, and the flag package
// does not substitute its default for it. MUNINN_MCP_URL must still be resolved,
// so an endpoint that cannot be dialed is rejected at startup by name instead
// of surfacing later as a transport error from an empty URL.
func TestEmptyMCPURLFlagFallsBackToEnv(t *testing.T) {
	if os.Getenv("MSC_RUN_MAIN") == "1" {
		os.Args = []string{"msc-bench", "-mcp-url="}
		if err := run(); err == nil || !config.IsUsageError(err) {
			t.Errorf("empty -mcp-url with a bad MUNINN_MCP_URL: %v, want a usage error", err)
		} else {
			// The parent asserts on this text, so it goes to stderr rather than
			// into the child's own (discarded) test log.
			fmt.Fprintln(os.Stderr, err)
		}
		return
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestEmptyMCPURLFlagFallsBackToEnv$")
	cmd.Env = append(os.Environ(), "MSC_RUN_MAIN=1", "MUNINN_MCP_URL=127.0.0.1:8750/mcp")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Errorf("main(-mcp-url=) with a scheme-less MUNINN_MCP_URL should exit 2, got %v: %s", err, out)
	}
	if !strings.Contains(string(out), "MuninnDB URL") {
		t.Errorf("the rejection must name the option, got: %s", out)
	}
}
