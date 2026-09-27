package config

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestEnvOr(t *testing.T) {
	t.Setenv("MSC_TEST_ENV", "set")
	if got := EnvOr("MSC_TEST_ENV", "d"); got != "set" {
		t.Errorf("set: %q", got)
	}
	// An empty value is "not configured", so the default applies.
	t.Setenv("MSC_TEST_ENV", "")
	if got := EnvOr("MSC_TEST_ENV", "d"); got != "d" {
		t.Errorf("empty: %q", got)
	}
	if got := EnvOr("MSC_TEST_UNSET", "d"); got != "d" {
		t.Errorf("unset: %q", got)
	}
}

func TestEnvBool(t *testing.T) {
	for _, off := range []string{"", "0", "false", "FALSE", "off", "No"} {
		t.Setenv("MSC_TEST_BOOL", off)
		if EnvBool("MSC_TEST_BOOL") {
			t.Errorf("EnvBool with %q = true, want false", off)
		}
	}
	for _, on := range []string{"1", "true", "on", "yes", "debug"} {
		t.Setenv("MSC_TEST_BOOL", on)
		if !EnvBool("MSC_TEST_BOOL") {
			t.Errorf("EnvBool with %q = false, want true", on)
		}
	}
	if EnvBool("MSC_TEST_BOOL_UNSET") {
		t.Error("unset variable must be off")
	}
}

func TestOneOf(t *testing.T) {
	if err := OneOf("-mode", "deep", "", "semantic", "recent", "balanced", "deep"); err != nil {
		t.Errorf("listed value: %v", err)
	}
	if err := OneOf("-mode", "", "", "semantic", "recent", "balanced", "deep"); err != nil {
		t.Errorf("empty value when allowed: %v", err)
	}
	err := OneOf("-mode", "Deep", "", "semantic", "recent", "balanced", "deep")
	if err == nil {
		t.Fatal("value outside the set must be rejected")
	}
	for _, want := range []string{"-mode", `"Deep"`, "semantic, recent, balanced, deep", "empty value"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q must mention %q", err, want)
		}
	}
	if err := OneOf("-corpus", "squad", "homogeneous", "squad"); err != nil {
		t.Errorf("listed value: %v", err)
	}
	if err := OneOf("-corpus", "Squad", "homogeneous", "squad"); err == nil || strings.Contains(err.Error(), "empty") {
		t.Errorf("set without an empty value must not mention it: %v", err)
	}
}

func TestArgSecretWarning(t *testing.T) {
	if got := ArgSecretWarning("--token", "", "MUNINN_TOKEN"); got != "" {
		t.Errorf("no flag value must be silent, got %q", got)
	}
	got := ArgSecretWarning("--token", "s3cr3t-value", "MUNINN_TOKEN")
	for _, want := range []string{"--token", "MUNINN_TOKEN", "process list"} {
		if !strings.Contains(got, want) {
			t.Errorf("warning %q must mention %q", got, want)
		}
	}
	if strings.Contains(got, "s3cr3t-value") {
		t.Errorf("warning must not repeat the secret: %q", got)
	}
}

func TestMCPURLPrecedence(t *testing.T) {
	t.Setenv("MUNINN_MCP_URL", "http://from-env/mcp")
	if got := MCPURL("http://from-flag/mcp"); got != "http://from-flag/mcp" {
		t.Errorf("flag must win: %q", got)
	}
	if got := MCPURL(""); got != "http://from-env/mcp" {
		t.Errorf("env must be used when the flag is empty: %q", got)
	}
	t.Setenv("MUNINN_MCP_URL", "")
	if got := MCPURL(""); got != DefaultMCPURL {
		t.Errorf("default: %q", got)
	}
}

func TestValidateURL(t *testing.T) {
	valid := []string{
		"",
		"http://127.0.0.1:8750/mcp",
		"https://muninn.example.com/mcp",
		"https://muninn.example.com:8443/mcp",
	}
	for _, raw := range valid {
		if err := ValidateURL("MuninnDB URL", raw); err != nil {
			t.Errorf("ValidateURL(%q) = %v, want nil", raw, err)
		}
	}
	invalid := []string{
		"127.0.0.1:8750/mcp",   // no scheme
		"htp://127.0.0.1:8750", // typo'd scheme
		"ftp://host/mcp",       // not a transport msc speaks
		"http://",              // no host
		"://host/mcp",          // unparseable
		"http://[::1/mcp",      // malformed
	}
	for _, raw := range invalid {
		err := ValidateURL("MuninnDB URL", raw)
		if err == nil {
			t.Errorf("ValidateURL(%q) = nil, want an error", raw)
		}
		if !strings.Contains(err.Error(), "MuninnDB URL") {
			t.Errorf("ValidateURL(%q) error must name the option, got %v", raw, err)
		}
	}
}

func TestTokenPrecedence(t *testing.T) {
	t.Setenv("MUNINN_TOKEN", "envtok")
	if got := Token("flagtok"); got != "flagtok" {
		t.Errorf("flag must win: %q", got)
	}
	if got := Token(""); got != "envtok" {
		t.Errorf("env: %q", got)
	}
	t.Setenv("MUNINN_TOKEN", "")
	// With no env token, the ~/.muninn/mcp.token file is consulted. It is
	// absent under an empty HOME, so the result is empty.
	t.Setenv("HOME", t.TempDir())
	if got := Token(""); got != "" {
		t.Errorf("no token configured, got %q", got)
	}
}

func TestTokenReadsFile(t *testing.T) {
	t.Setenv("MUNINN_TOKEN", "")
	home := t.TempDir()
	t.Setenv("HOME", home)
	path := filepath.Join(home, ".muninn", "mcp.token")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("filetok\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := Token(""); got != "filetok" {
		t.Errorf("file token %q (whitespace must be trimmed)", got)
	}
}

// A token file that is present but unreadable is a fault, not "no token
// configured": silently dropping it launched msc with no Authorization header,
// so every MCP call failed 401 and the cause surfaced as a generic delivery
// error far from the file. The read still returns no token (a server needing no
// auth must not be blocked by a broken file), but the reason is named.
func TestTokenUnreadableFileIsReported(t *testing.T) {
	t.Setenv("MUNINN_TOKEN", "")
	dir := t.TempDir()
	// A directory where a file is expected: the read fails with something
	// other than "does not exist", on every platform and without depending on
	// running as a user that cannot read a 0000 file.
	t.Setenv("MUNINN_TOKEN_FILE", dir)

	var logs strings.Builder
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelWarn})))
	defer slog.SetDefault(prev)

	if got := Token(""); got != "" {
		t.Errorf("an unreadable token file must yield no token, got %q", got)
	}
	out := logs.String()
	if !strings.Contains(out, dir) {
		t.Errorf("unreadable token file was silent; want the path in the log, got:\n%s", out)
	}
}

// A missing file is a valid deployment (a server that needs no auth) and must
// stay quiet: warning on every run without a token would train operators to
// ignore the line that names a real read failure.
func TestTokenMissingFileIsQuiet(t *testing.T) {
	t.Setenv("MUNINN_TOKEN", "")
	t.Setenv("MUNINN_TOKEN_FILE", filepath.Join(t.TempDir(), "absent"))

	var logs strings.Builder
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelWarn})))
	defer slog.SetDefault(prev)

	if got := Token(""); got != "" {
		t.Errorf("no token configured, got %q", got)
	}
	if logs.Len() != 0 {
		t.Errorf("a missing token file must be silent, got:\n%s", logs.String())
	}
}

func TestTokenFileEnvOverride(t *testing.T) {
	t.Setenv("MUNINN_TOKEN", "")
	home := t.TempDir()
	t.Setenv("HOME", home)
	// A token in the default location, and one where MUNINN_TOKEN_FILE points.
	// The override must win, so a deployment (and test-live.sh) can move the
	// file without the binaries reading a different one than the test harness.
	if err := os.MkdirAll(filepath.Join(home, ".muninn"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, ".muninn", "mcp.token"), []byte("deftok\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	elsewhere := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(elsewhere, []byte("envfiletok\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("MUNINN_TOKEN_FILE", elsewhere)
	if got := TokenFilePath(); got != elsewhere {
		t.Errorf("TokenFilePath() = %q, want %q", got, elsewhere)
	}
	if got := Token(""); got != "envfiletok" {
		t.Errorf("token from MUNINN_TOKEN_FILE = %q", got)
	}
	// An empty value means "not configured", so the default applies.
	t.Setenv("MUNINN_TOKEN_FILE", "")
	if got := TokenFilePath(); got != filepath.Join(home, ".muninn", "mcp.token") {
		t.Errorf("default path = %q", got)
	}
	if got := Token(""); got != "deftok" {
		t.Errorf("token from the default file = %q", got)
	}
}

func TestVaultPrecedence(t *testing.T) {
	t.Setenv("MSC_VAULT", "envvault")
	if got := Vault("flagvault"); got != "flagvault" {
		t.Errorf("flag must win: %q", got)
	}
	if got := Vault(""); got != "envvault" {
		t.Errorf("env: %q", got)
	}
	t.Setenv("MSC_VAULT", "")
	dir := t.TempDir()
	t.Chdir(dir)
	base := filepath.Base(dir)
	if got := Vault(""); got != base && got != DefaultVault {
		t.Errorf("cwd basename or default: %q", got)
	}
}

func TestHostClassification(t *testing.T) {
	for _, h := range []string{"127.0.0.1", "localhost", "::1"} {
		if !IsLoopbackHost(h) {
			t.Errorf("IsLoopbackHost(%q) = false", h)
		}
	}
	for _, h := range []string{"muninn.example.com", "10.0.0.5", "", "localhost.evil.com"} {
		if IsLoopbackHost(h) {
			t.Errorf("IsLoopbackHost(%q) = true", h)
		}
	}
	for _, h := range []string{"api.openai.com", "API.OpenAI.com", "eu.openai.com"} {
		if !IsOpenAIHost(h) {
			t.Errorf("IsOpenAIHost(%q) = false", h)
		}
	}
	for _, h := range []string{"openai.com.evil.com", "localhost:11434", "", "notopenai.com"} {
		if IsOpenAIHost(h) {
			t.Errorf("IsOpenAIHost(%q) = true", h)
		}
	}
}

// TestPlaintextRemoteHost pins the condition callers warn on before forwarding
// a credential: an http:// endpoint that is not loopback. Loopback traffic
// never leaves the machine, so it is not a plaintext send.
func TestPlaintextRemoteHost(t *testing.T) {
	plaintext := []string{
		"http://api.openai.com/v1",
		"http://muninn.example.com:8750/mcp",
		"http://10.0.0.5/mcp",
	}
	for _, raw := range plaintext {
		if !PlaintextRemoteHost(raw) {
			t.Errorf("PlaintextRemoteHost(%q) = false, want true", raw)
		}
	}
	safe := []string{
		"https://api.openai.com/v1",
		"http://127.0.0.1:8750/mcp",
		"http://localhost:11434/v1",
		"http://[::1]:8750/mcp",
		"://bad url",
		"",
	}
	for _, raw := range safe {
		if PlaintextRemoteHost(raw) {
			t.Errorf("PlaintextRemoteHost(%q) = true, want false", raw)
		}
	}
}

// A root directory has no name to derive a vault from, and on Windows the root
// is "C:\" rather than "/", so both shapes must fall back to DefaultVault
// instead of naming the vault "\" or "C:".
func TestVaultDirName(t *testing.T) {
	named := map[string]string{
		"/home/u/project":      "project",
		"/home/u/project/":     "project",
		"/project":             "project",
		`C:\Users\u\project`:   "project",
		`C:\Users\u\project\`:  "project",
		`\\host\share\project`: "project",
		`/home/u/my project`:   "my project",
		"/home/u/.hidden":      ".hidden",
		"/home/u/UPPER":        "UPPER",
	}
	for dir, want := range named {
		if got := dirName(dir); got != want {
			t.Errorf("dirName(%q) = %q, want %q", dir, got, want)
		}
	}
	roots := []string{"/", `C:\`, "C:/", "", ".", "..", "C:"}
	for _, dir := range roots {
		if got := dirName(dir); got != "" {
			t.Errorf("dirName(%q) = %q, want no name", dir, got)
		}
	}
}

func TestUsageErrorClassification(t *testing.T) {
	// Every rejection of a command-line value must be a UsageError, so the CLI
	// exits 2 for a typo the way the flag package does, and 1 for a run-time
	// failure.
	rejects := map[string]error{
		"OneOf":       OneOf("-corpus", "bogus", "homogeneous", "diverse"),
		"ValidateURL": ValidateURL("MuninnDB URL", "not a url"),
		"Usagef":      Usagef("invalid -n 0: must be positive"),
		"no scheme":   ValidateURL("--mcp-url", "127.0.0.1:8750"),
	}
	for name, err := range rejects {
		if err == nil {
			t.Errorf("%s: want an error", name)
			continue
		}
		if !IsUsageError(err) {
			t.Errorf("%s: %v is not a usage error", name, err)
		}
	}
	// Values that are not rejections, and errors from elsewhere.
	if IsUsageError(OneOf("-corpus", "squad", "homogeneous", "squad")) {
		t.Error("an allowed value reported a usage error")
	}
	if IsUsageError(ValidateURL("--mcp-url", "")) {
		t.Error("an unset URL reported a usage error")
	}
	if IsUsageError(os.ErrNotExist) {
		t.Error("a runtime error was classified as a usage error")
	}
	// A usage error wrapped by a caller is still one.
	wrapped := fmt.Errorf("loading settings: %w", Usagef("invalid -n 0: must be positive"))
	if !IsUsageError(wrapped) {
		t.Error("a wrapped usage error lost its type")
	}
}
