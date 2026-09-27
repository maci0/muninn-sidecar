package main

import (
	"encoding/json"
	"errors"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/maci0/muninn-sidecar/internal/agents"
	"github.com/maci0/muninn-sidecar/internal/config"
	"github.com/maci0/muninn-sidecar/internal/grounding"
	"github.com/maci0/muninn-sidecar/internal/inject"
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

// TestRunStatusUnusableURL: an undialable endpoint is a typo, not an outage, so
// it takes the usage exit code like every other msc entry point instead of
// reporting a health check result that was never made.
func TestRunStatusUnusableURL(t *testing.T) {
	t.Setenv("MUNINN_TOKEN", "x")
	for _, args := range [][]string{
		{"status", "--mcp-url", "://x"},
		{"-j", "status", "--mcp-url", "://x"},
	} {
		if code := runArgs(t, args...); code != exitUsage {
			t.Errorf("%v: exit code %d, want %d", args, code, exitUsage)
		}
	}
	t.Setenv("MUNINN_MCP_URL", "://x")
	if code := runArgs(t, "status"); code != exitUsage {
		t.Errorf("status with a bad MUNINN_MCP_URL: exit code %d, want %d", code, exitUsage)
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

// The dry-run preview is only useful if it reports what the child would really
// get. printDryRun builds its env and args from the same helpers Exec/ExecMITM
// use, so pin that agreement here: a preview that drifted from the child would
// still exit 0 and print a confident, wrong answer.
func TestPrintDryRunPreviewsTheChildsOverrides(t *testing.T) {
	const upstream = "https://api.anthropic.com"
	agent := agents.Agent{
		Command:      "claude",
		EnvKey:       "ANTHROPIC_BASE_URL",
		ExtraEnvKeys: []string{"ANTHROPIC_API_URL"},
		// An agent that reads its base URL from argv, not the environment.
		ProxyArgs: []string{"--base-url", "{proxy}"},
	}

	// ArgsRouted keeps EnvKey out of the child env; asserting on the raw
	// Agent{EnvKey: ...} literal above would not exercise that branch.
	agent.ArgsRouted = true

	out := captureStdout(t, func() {
		if rc := printDryRun(&opts{}, "claude", agent, upstream, "http://127.0.0.1:9/mcp", "vault", nil, ""); rc != 0 {
			t.Errorf("printDryRun rc = %d, want 0", rc)
		}
	})

	// Every key the child would receive must appear, with the real value.
	for k, want := range agent.EnvOverrides("http://127.0.0.1:<port>", upstream) {
		if !strings.Contains(out, k+"="+want) {
			t.Errorf("preview missing child env %s=%s:\n%s", k, want, out)
		}
	}
	// {proxy} must be substituted, not printed literally.
	if strings.Contains(out, "{proxy}") {
		t.Errorf("preview left {proxy} unsubstituted:\n%s", out)
	}
	if !strings.Contains(out, "http://127.0.0.1:<port>") {
		t.Errorf("preview lost the argv override:\n%s", out)
	}
	if strings.Contains(out, agent.EnvKey+"=") {
		t.Errorf("ArgsRouted agent must not show %s in the child env:\n%s", agent.EnvKey, out)
	}
	// --force skips the health probe, so no "unreachable" claim may appear.
	if strings.Contains(out, "unreachable") {
		t.Errorf("--force preview must report the DB unchecked, not unreachable:\n%s", out)
	}
}

// The MITM branch of the preview is where the trust-store env vars come from,
// and whether they appear at all depends on the host having a system root
// bundle to combine with. Previewing the trust-replacing variables on a host
// that has no bundle would tell the operator msc writes one when it does not,
// and previewing them with the CA alone would tell them the child can still
// verify blind-tunneled hosts, which it cannot.
func TestPrintDryRunMITMTrustVars(t *testing.T) {
	const upstream = "https://api.anthropic.com"
	agent := agents.Agent{Command: "claude", EnvKey: "ANTHROPIC_BASE_URL"}
	caCertPath := filepath.Join(t.TempDir(), "ca-cert.pem")

	out := captureStdout(t, func() {
		if rc := printDryRun(&opts{mitm: true}, "claude", agent, upstream, "http://127.0.0.1:9/mcp", "vault", nil, caCertPath); rc != 0 {
			t.Errorf("printDryRun rc = %d, want 0", rc)
		}
	})

	bundlePath := ""
	if agents.HasSystemCABundle() {
		bundlePath = agents.CABundlePath(caCertPath)
	}
	want := agent.MITMOverrides("http://127.0.0.1:<port>", upstream, caCertPath, bundlePath)
	for k, v := range want {
		if !strings.Contains(out, k+"="+v) {
			t.Errorf("MITM preview missing child env %s=%s:\n%s", k, v, out)
		}
	}
	// Every trust-replacing variable moves together, so assert the whole set
	// against the same condition ExecMITM uses, not one key.
	for _, k := range []string{"SSL_CERT_FILE", "REQUESTS_CA_BUNDLE", "CURL_CA_BUNDLE"} {
		_, listed := want[k]
		if got := strings.Contains(out, k+"="); got != listed {
			t.Errorf("%s present = %t, want %t (system bundle present: %t):\n%s",
				k, got, listed, agents.HasSystemCABundle(), out)
		}
	}
}

// dry-run --json is the machine-readable half of the same contract. Assert the
// decoded fields, not the serialized text, so whitespace and field order stay
// free to change.
func TestPrintDryRunJSONShape(t *testing.T) {
	const upstream = "https://api.anthropic.com"
	agent := agents.Agent{Command: "claude", EnvKey: "ANTHROPIC_BASE_URL", ArgsRouted: true}
	o := &opts{asJSON: true, force: true, injectBudget: 900, minScore: 0.5, noAutoCalibrate: true, recallMode: "hybrid"}

	raw := captureStdout(t, func() {
		if rc := printDryRun(o, "claude", agent, upstream, "http://127.0.0.1:9/mcp", "vault", nil, ""); rc != 0 {
			t.Errorf("printDryRun rc = %d, want 0", rc)
		}
	})

	var got struct {
		Agent           string            `json:"agent"`
		Binary          string            `json:"binary"`
		Upstream        string            `json:"upstream"`
		Vault           string            `json:"vault"`
		MuninnURL       string            `json:"muninn_url"`
		MuninnStatus    string            `json:"muninn_status"`
		Env             map[string]string `json:"env"`
		ProxyArgs       []string          `json:"proxy_args"`
		Inject          bool              `json:"inject"`
		InjectBudget    int               `json:"inject_budget"`
		InjectMinScore  float64           `json:"inject_min_score"`
		InjectRecall    string            `json:"inject_recall_mode"`
		InjectCalibrate string            `json:"inject_calibration"`
	}
	if err := json.Unmarshal([]byte(raw), &got); err != nil {
		t.Fatalf("dry-run --json must emit one JSON object: %v\n%s", err, raw)
	}

	if got.Agent != "claude" || got.Upstream != upstream || got.Vault != "vault" {
		t.Errorf("identity fields = (%q, %q, %q), want (claude, %s, vault)", got.Agent, got.Upstream, got.Vault, upstream)
	}
	if got.MuninnURL != "http://127.0.0.1:9/mcp" {
		t.Errorf("muninn_url = %q", got.MuninnURL)
	}
	// --force short-circuits the health check; "unreachable" would be a false claim.
	if got.MuninnStatus != "unchecked" {
		t.Errorf("muninn_status = %q, want %q under --force", got.MuninnStatus, "unchecked")
	}
	if !got.Inject {
		t.Error("inject must default to enabled")
	}
	if got.InjectBudget != 900 || got.InjectMinScore != 0.5 || got.InjectRecall != "hybrid" {
		t.Errorf("inject knobs = (%d, %.2f, %q), want (900, 0.50, hybrid)", got.InjectBudget, got.InjectMinScore, got.InjectRecall)
	}
	if got.InjectCalibrate != "fixed" {
		t.Errorf("inject_calibration = %q, want %q with --no-auto-calibrate", got.InjectCalibrate, "fixed")
	}
	if want := agent.EnvOverrides("http://127.0.0.1:<port>", upstream); !maps.Equal(got.Env, want) {
		t.Errorf("env = %v, want %v", got.Env, want)
	}
	if len(got.ProxyArgs) != 0 {
		t.Errorf("args-routed agent without ProxyArgs must report none, got %v", got.ProxyArgs)
	}
}

// A health failure is the case --force exists to bypass, so the preview must
// still say so rather than silently reporting a reachable database.
func TestPrintDryRunJSONReportsUnreachable(t *testing.T) {
	agent := agents.Agent{Command: "claude", EnvKey: "ANTHROPIC_BASE_URL"}
	raw := captureStdout(t, func() {
		printDryRun(&opts{asJSON: true, noInject: true}, "claude", agent, "https://x", "http://127.0.0.1:1/mcp", "v", errors.New("dial refused"), "")
	})
	var got struct {
		MuninnStatus string `json:"muninn_status"`
		MuninnError  string `json:"muninn_error"`
		Inject       bool   `json:"inject"`
		InjectBudget int    `json:"inject_budget"` // omitted when injection is off
	}
	if err := json.Unmarshal([]byte(raw), &got); err != nil {
		t.Fatalf("dry-run --json must emit one JSON object: %v\n%s", err, raw)
	}
	if got.MuninnStatus != "unreachable" || got.MuninnError != "dial refused" {
		t.Errorf("unreachable DB = (%q, %q), want (unreachable, dial refused)", got.MuninnStatus, got.MuninnError)
	}
	if got.Inject {
		t.Error("--no-inject must be reported as inject=false")
	}
	if got.InjectBudget != 0 {
		t.Errorf("inject_budget must be omitted when injection is off, got %d", got.InjectBudget)
	}
}

// Text dry-run uses the same health error status does. The parenthetical is
// the state, and the error (which itself says "unreachable") is the next line.
func TestPrintDryRunTextUnreachableNamesItOnce(t *testing.T) {
	agent := agents.Agent{Command: "claude", EnvKey: "ANTHROPIC_BASE_URL"}
	err := errors.New("unreachable at http://127.0.0.1:1/mcp: dial refused")
	out := captureStdout(t, func() {
		printDryRun(&opts{}, "claude", agent, "https://x", "http://127.0.0.1:1/mcp", "v", err, "")
	})
	if strings.Contains(out, "unreachable: unreachable") {
		t.Errorf("dry-run repeats the state:\n%s", out)
	}
	if !strings.Contains(out, "(unreachable)\n") || !strings.Contains(out, "dial refused") {
		t.Errorf("dry-run must name the state and the probe error:\n%s", out)
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

// statusMCP is a MuninnDB whose health endpoint answers 200 and whose
// muninn_status reports the given memory count, so cmdStatus's reachable path
// and its best-effort vault stats can be exercised end to end.
func statusMCP(t *testing.T, total int) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		body, _ := json.Marshal(map[string]any{"vault": "v", "total_memories": total, "health": "good"})
		text, _ := json.Marshal(string(body))
		w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{"content":[{"type":"text","text":` + string(text) + `}]}}`))
	}))
	t.Cleanup(srv.Close)
	return srv
}

// `msc status` is the command an operator runs when injection is not appearing,
// so the reachable path and its vault diagnostics are the point of it. Only the
// unreachable exit code was pinned before; the populated, empty, and JSON
// outputs were exercised by nothing.
func TestCmdStatusReachable(t *testing.T) {
	srv := statusMCP(t, 47)
	t.Setenv("MUNINN_MCP_URL", srv.URL+"/mcp")
	t.Setenv("MUNINN_TOKEN", "x")

	var rc int
	out := captureStdout(t, func() { rc = cmdStatus(&opts{}) })
	if rc != 0 {
		t.Errorf("reachable status rc = %d, want 0", rc)
	}
	if !strings.Contains(out, "reachable") {
		t.Errorf("status must report the DB as reachable:\n%s", out)
	}
	if !strings.Contains(out, "47") {
		t.Errorf("status must report the memory count:\n%s", out)
	}
	// The empty-vault hint must not fire when memories exist.
	if strings.Contains(out, "vault is empty") {
		t.Errorf("populated vault must not print the empty hint:\n%s", out)
	}
}

// A reachable but empty vault is the common "nothing gets injected" cause, so
// the warning is the reason to run this command at all.
func TestCmdStatusEmptyVaultWarns(t *testing.T) {
	srv := statusMCP(t, 0)
	t.Setenv("MUNINN_MCP_URL", srv.URL+"/mcp")
	t.Setenv("MUNINN_TOKEN", "x")

	var rc int
	out := captureStdout(t, func() { rc = cmdStatus(&opts{}) })
	if rc != 0 {
		t.Errorf("empty vault is still reachable, rc = %d, want 0", rc)
	}
	if !strings.Contains(out, "vault is empty") {
		t.Errorf("zero memories must warn that nothing will be injected:\n%s", out)
	}
}

// The JSON form is what scripts read. Decode the fields rather than matching
// the printed text, so key order and indentation stay free to change.
func TestCmdStatusJSON(t *testing.T) {
	srv := statusMCP(t, 47)
	t.Setenv("MUNINN_MCP_URL", srv.URL+"/mcp")
	t.Setenv("MUNINN_TOKEN", "x")

	var rc int
	raw := captureStdout(t, func() { rc = cmdStatus(&opts{asJSON: true}) })
	if rc != 0 {
		t.Errorf("status --json rc = %d, want 0", rc)
	}
	var got struct {
		MCPURL      string `json:"mcp_url"`
		Vault       string `json:"vault"`
		Status      string `json:"status"`
		Memories    *int   `json:"memories"`
		VaultHealth string `json:"vault_health"`
	}
	if err := json.Unmarshal([]byte(raw), &got); err != nil {
		t.Fatalf("status --json must emit one JSON object: %v\n%s", err, raw)
	}
	if got.Status != "reachable" {
		t.Errorf("status = %q, want reachable", got.Status)
	}
	if got.MCPURL != srv.URL+"/mcp" {
		t.Errorf("mcp_url = %q, want %q", got.MCPURL, srv.URL+"/mcp")
	}
	if got.Memories == nil || *got.Memories != 47 {
		t.Errorf("memories = %v, want 47", got.Memories)
	}
	if got.VaultHealth != "good" {
		t.Errorf("vault_health = %q, want good", got.VaultHealth)
	}
}

// Unreachable is the failure scripts must be able to detect: nonzero exit, an
// explicit status, and no fabricated memory count.
func TestCmdStatusJSONUnreachable(t *testing.T) {
	t.Setenv("MUNINN_MCP_URL", "http://127.0.0.1:1/mcp")
	t.Setenv("MUNINN_TOKEN", "x")

	var rc int
	raw := captureStdout(t, func() { rc = cmdStatus(&opts{asJSON: true}) })
	if rc != 1 {
		t.Errorf("unreachable status --json rc = %d, want 1", rc)
	}
	var got struct {
		Status   string          `json:"status"`
		Error    string          `json:"error"`
		Memories json.RawMessage `json:"memories"`
	}
	if err := json.Unmarshal([]byte(raw), &got); err != nil {
		t.Fatalf("status --json must emit one JSON object: %v\n%s", err, raw)
	}
	if got.Status != "unreachable" {
		t.Errorf("status = %q, want unreachable", got.Status)
	}
	if got.Error == "" {
		t.Error("unreachable status must carry the probe error, not an empty string")
	}
	if got.Memories != nil {
		t.Errorf("no stats were fetched, memories must be absent, got %s", got.Memories)
	}
}

// The health error already begins with "unreachable". Folding it into the
// parenthetical prints the word twice, which is what an operator reads.
func TestCmdStatusTextUnreachableNamesItOnce(t *testing.T) {
	t.Setenv("MUNINN_MCP_URL", "http://127.0.0.1:1/mcp")
	t.Setenv("MUNINN_TOKEN", "x")

	var rc int
	out := captureStdout(t, func() { rc = cmdStatus(&opts{}) })
	if rc != 1 {
		t.Errorf("unreachable status rc = %d, want 1", rc)
	}
	if strings.Contains(out, "unreachable: unreachable") {
		t.Errorf("status repeats the state:\n%s", out)
	}
	if !strings.Contains(out, "(unreachable)\n") || !strings.Contains(out, "unreachable at ") {
		t.Errorf("status must name the state and still carry the probe error:\n%s", out)
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

// TestHelpStatesRealDefaults pins the help's stated defaults to the constants
// the code actually applies, so a default that changes without the help
// changing fails here instead of misleading an operator reading `msc --help`.
func TestHelpStatesRealDefaults(t *testing.T) {
	_, out := runArgsCapture(t, "help")
	for _, want := range []string{
		grounding.DefaultModel,
		grounding.DefaultTimeout.String(),
		strconv.Itoa(inject.DefaultGroundTopK),
		config.DefaultMCPURL,
		"MUNINN_TOKEN_FILE",
		config.DefaultVault,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("help must state the default %q, got:\n%s", want, out)
		}
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

// The README documents `msc help <topic>` and `msc <topic> --help` as the same
// request, so both forms must print the same text for a command and for an
// agent. The flag form used to fall through to the global usage for every agent.
func TestHelpTopicFormsAgree(t *testing.T) {
	for _, topic := range []string{"status", "claude", "qwen"} {
		helpCode, helpOut := runArgsCapture(t, "help", topic)
		flagCode, flagOut := runArgsCapture(t, "--help", topic)
		if helpCode != flagCode {
			t.Errorf("%s: 'help %s' exit %d, '%s --help' exit %d", topic, topic, helpCode, topic, flagCode)
		}
		if helpOut != flagOut {
			t.Errorf("%s: 'help %s' and '%s --help' disagree:\n%s\n---\n%s", topic, topic, topic, helpOut, flagOut)
		}
	}
}

// qwen is ArgsRouted: msc injects the proxy through --openai-base-url and
// deliberately does not write its EnvKey, so help must not name one. `msc list`
// already reports the flag, and the two could disagree.
func TestHelpAgentNamesTheBaseURLSource(t *testing.T) {
	_, out := runArgsCapture(t, "help", "qwen")
	if !strings.Contains(out, agents.Registry["qwen"].BaseURLSource()) {
		t.Errorf("qwen help should name %q, got:\n%s", agents.Registry["qwen"].BaseURLSource(), out)
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
	win := strings.Join(caTrustHints("windows", winPath, false), "\n")
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
	unix := strings.Join(caTrustHints("linux", unixPath, true), "\n")
	if !strings.Contains(unix, "export NODE_EXTRA_CA_CERTS="+unixPath) {
		t.Errorf("posix hints must keep the export line: %s", unix)
	}
	if !strings.Contains(unix, agents.CABundlePath(unixPath)) {
		t.Errorf("posix hints must name the combined bundle: %s", unix)
	}
}

// macOS keeps its roots in the keychain, so no combined bundle is written
// there, and the hint must not name one. The branch is keyed on the probe's
// answer rather than the OS, so this holds on any host that finds no roots.
func TestCATrustHintsOmitTheBundleWhereNoneIsWritten(t *testing.T) {
	macPath := "/Users/u/Library/Application Support/muninn-sidecar/mitm/ca-cert.pem"
	mac := strings.Join(caTrustHints("darwin", macPath, false), "\n")
	if strings.Contains(mac, "ca-bundle.pem") {
		t.Errorf("no combined bundle is written without system roots, so naming one misleads: %s", mac)
	}
	if !strings.Contains(mac, "export NODE_EXTRA_CA_CERTS="+macPath) {
		t.Errorf("the additive variable still applies: %s", mac)
	}
}
