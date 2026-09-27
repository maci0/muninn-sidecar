package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/maci0/muninn-sidecar/internal/agents"
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
		if rc := printVersion(&opts{}); rc != 0 {
			t.Errorf("printVersion plain rc = %d, want 0", rc)
		}
		if rc := printVersion(&opts{asJSON: true}); rc != 0 {
			t.Errorf("printVersion json rc = %d, want 0", rc)
		}
	})
}

// captureStdout returns everything fn writes to stdout, so a test can assert
// on the help text a command actually prints.
func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
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

// runArgsCapture runs run() with stdout piped, so a test can read the help text
// a command prints. Only for commands whose entire output is the help text
// under test; runArgs silences stdout and would swallow it.
func runArgsCapture(t *testing.T, args ...string) (int, string) {
	t.Helper()
	oldArgs := os.Args
	os.Args = append([]string{"msc"}, args...)
	defer func() { os.Args = oldArgs }()
	var code int
	out := captureStdout(t, func() { code = run() })
	return code, out
}

// globalUsagePrefix starts the top-level usage, which no per-command help
// repeats. Its absence is how a test tells the two apart.
const globalUsagePrefix = "msc - muninn sidecar"

func TestCommandHelp(t *testing.T) {
	for _, cmd := range []string{"list", "status", "ca", "version", "completion", "help"} {
		for _, args := range [][]string{{cmd, "--help"}, {"help", cmd}} {
			code, out := runArgsCapture(t, args...)
			if code != 0 {
				t.Errorf("msc %v: exit %d, want 0", args, code)
			}
			if !strings.HasPrefix(out, "Usage: msc "+cmd) {
				t.Errorf("msc %v printed the wrong help, got:\n%s", args, firstLine(out))
			}
			if strings.HasPrefix(out, globalUsagePrefix) {
				t.Errorf("msc %v fell back to the global usage", args)
			}
		}
	}
	// The global usage is still what a bare --help and a bare 'help' print.
	for _, args := range [][]string{{"--help"}, {"help"}} {
		code, out := runArgsCapture(t, args...)
		if code != 0 {
			t.Errorf("msc %v: exit %d, want 0", args, code)
		}
		if !strings.HasPrefix(out, globalUsagePrefix) {
			t.Errorf("msc %v should print the global usage, got:\n%s", args, firstLine(out))
		}
	}
}

func TestHelpUnknownTopic(t *testing.T) {
	// A misspelled topic must fail rather than silently print the global
	// help and exit 0, which reads as "that worked".
	if code := runArgs(t, "help", "statuss"); code != exitUsage {
		t.Errorf("unknown help topic: exit %d, want %d", code, exitUsage)
	}
	if code := runArgs(t, "help", "list", "status"); code != exitUsage {
		t.Errorf("two help topics: exit %d, want %d", code, exitUsage)
	}
}

func TestHelpAgent(t *testing.T) {
	code, out := runArgsCapture(t, "help", "claude")
	if code != 0 {
		t.Errorf("help for an agent: exit %d, want 0", code)
	}
	a := agents.Registry["claude"]
	if !strings.Contains(out, a.EnvKey) || !strings.Contains(out, a.DefaultURL) {
		t.Errorf("agent help should name %s and %s, got:\n%s", a.EnvKey, a.DefaultURL, out)
	}
}

// TestRunAgentNotFound pins the exit code for an agent binary that is not
// installed: 127, the shell's "command not found", so a script can tell a
// mistyped agent name apart from an agent that ran and failed.
func TestRunAgentNotFound(t *testing.T) {
	// An empty PATH makes every agent's binary unresolvable.
	t.Setenv("PATH", t.TempDir())
	if got := runArgs(t, "--force", "claude"); got != exitNotFound {
		t.Errorf("missing agent binary: exit %d, want %d", got, exitNotFound)
	}
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

// `msc ca` prints trust instructions to paste into a shell. On Windows that
// shell is cmd or PowerShell, where `export` does not exist, and the cert path
// under %AppData% usually contains a space, so the value has to be quoted.
func TestCATrustHintsPerPlatform(t *testing.T) {
	winPath := `C:\Users\a b\AppData\Roaming\muninn-sidecar\mitm\ca-cert.pem`
	win := strings.Join(caTrustHints("windows", winPath), "\n")
	if strings.Contains(win, "export ") {
		t.Errorf("windows hints must not print a POSIX export line: %s", win)
	}
	if !strings.Contains(win, `$env:NODE_EXTRA_CA_CERTS = "`+winPath+`"`) {
		t.Errorf("windows hints must set the variable with the path quoted: %s", win)
	}
	if strings.Contains(win, "ca-bundle.pem") {
		t.Errorf("windows has no system PEM bundle to combine, so naming one misleads: %s", win)
	}

	unixPath := "/home/u/.config/muninn-sidecar/mitm/ca-cert.pem"
	unix := strings.Join(caTrustHints("darwin", unixPath), "\n")
	if !strings.Contains(unix, "export NODE_EXTRA_CA_CERTS="+unixPath) {
		t.Errorf("posix hints must keep the export line: %s", unix)
	}
	if !strings.Contains(unix, agents.CABundlePath(unixPath)) {
		t.Errorf("posix hints must name the combined bundle: %s", unix)
	}
}
