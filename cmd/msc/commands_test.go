package main

import (
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
)

// silence redirects stdout+stderr to /dev/null for the duration of fn, so the
// many printer functions can be exercised without polluting test output.
func silence(t *testing.T, fn func()) {
	t.Helper()
	oldOut, oldErr := os.Stdout, os.Stderr
	w, _ := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	os.Stdout, os.Stderr = w, w
	defer func() { os.Stdout, os.Stderr = oldOut, oldErr; w.Close() }()
	fn()
}

func runArgs(t *testing.T, args ...string) int {
	t.Helper()
	old := os.Args
	os.Args = append([]string{"msc"}, args...)
	defer func() { os.Args = old }()
	var code int
	silence(t, func() { code = run() })
	return code
}

func TestRunSimpleCommands(t *testing.T) {
	if runArgs(t, "--help") != 0 {
		t.Error("--help should exit 0")
	}
	if runArgs(t, "version") != 0 {
		t.Error("version should exit 0")
	}
	if runArgs(t, "-v", "-j") != 0 {
		t.Error("version json should exit 0")
	}
	if runArgs(t, "list") != 0 {
		t.Error("list should exit 0")
	}
	if runArgs(t, "list", "-j") != 0 {
		t.Error("list json should exit 0")
	}
	if runArgs(t, "completion", "bash") != 0 || runArgs(t, "completion", "zsh") != 0 || runArgs(t, "completion", "fish") != 0 {
		t.Error("completion should exit 0")
	}
	if runArgs(t, "completion", "tcsh") == 0 {
		t.Error("unsupported shell should be nonzero")
	}
	if runArgs(t) != exitUsage {
		t.Error("no args should be usage error")
	}
	if runArgs(t, "claud") == 0 {
		t.Error("unknown command should be nonzero")
	}
	if runArgs(t, "completion") == 0 {
		t.Error("completion w/o shell should be nonzero")
	}
}

func TestRunStatusUnreachable(t *testing.T) {
	t.Setenv("MUNINN_MCP_URL", "http://127.0.0.1:1/mcp")
	t.Setenv("MUNINN_TOKEN", "x")
	if runArgs(t, "status") == 0 {
		t.Error("status against unreachable MuninnDB should be nonzero")
	}
}

func TestRunDryRun(t *testing.T) {
	t.Setenv("MUNINN_MCP_URL", "http://127.0.0.1:1/mcp")
	t.Setenv("MUNINN_TOKEN", "x")
	// --dry-run --force: resolve + print config, never launch the agent.
	if code := runArgs(t, "--dry-run", "--force", "claude"); code != 0 {
		t.Errorf("dry-run should exit 0, got %d", code)
	}
	if code := runArgs(t, "--dry-run", "--force", "--no-auto-calibrate", "--inject-min-score", "0.5", "claude"); code != 0 {
		t.Errorf("dry-run with knobs should exit 0, got %d", code)
	}
}

func TestRunCA(t *testing.T) {
	// Pin a throwaway config home so the CA is created under it.
	tmp := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", tmp)

	if code := runArgs(t, "ca"); code != 0 {
		t.Errorf("ca should exit 0, got %d", code)
	}
	if code := runArgs(t, "-j", "ca"); code != 0 {
		t.Errorf("ca --json should exit 0, got %d", code)
	}
	// The command must have created the CA cert under the pinned config home.
	if _, err := os.Stat(tmp + "/muninn-sidecar/mitm/ca-cert.pem"); err != nil {
		t.Errorf("ca command did not create the CA cert: %v", err)
	}
	// ca takes no arguments.
	if runArgs(t, "ca", "extra") == 0 {
		t.Error("ca with an argument should be a usage error")
	}
}

func TestVaultStats(t *testing.T) {
	// Fake MuninnDB returning a muninn_status result in the MCP content envelope.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{"content":[{"type":"text","text":"{\"vault\":\"v\",\"total_memories\":47,\"health\":\"good\"}"}]}}`))
	}))
	defer srv.Close()

	total, health, err := vaultStats(srv.URL, "", "v")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if total != 47 || health != "good" {
		t.Errorf("vaultStats = (%d, %q), want (47, good)", total, health)
	}

	// Unreachable endpoint -> error (caller omits stats, doesn't fail).
	if _, _, err := vaultStats("http://127.0.0.1:1/mcp", "", "v"); err == nil {
		t.Error("expected error against unreachable endpoint")
	}
}

func TestUsageAndVersionWriters(t *testing.T) {
	silence(t, func() {
		usage(os.Stdout)
		_ = printVersion(&opts{})
		_ = printVersion(&opts{asJSON: true})
	})
}
