package agents

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestResolveDefaultURL(t *testing.T) {
	// Clear any env vars that might interfere.
	for _, a := range Registry {
		for _, k := range a.DetectEnv {
			t.Setenv(k, "")
		}
		t.Setenv(a.sentinelKey(), "")
	}

	agent := Registry["claude"]
	got := agent.Resolve()
	if got != "https://api.anthropic.com" {
		t.Fatalf("expected default URL, got %q", got)
	}
}

func TestResolveDetectEnv(t *testing.T) {
	agent := Registry["claude"]
	t.Setenv(agent.sentinelKey(), "")
	t.Setenv("ANTHROPIC_BASE_URL", "https://custom.example.com/")

	got := agent.Resolve()
	// Trailing slash should be stripped.
	if got != "https://custom.example.com" {
		t.Fatalf("expected custom URL without trailing slash, got %q", got)
	}
}

func TestResolveMSCSentinelTakesPriority(t *testing.T) {
	agent := Registry["claude"]
	t.Setenv(agent.sentinelKey(), "https://sentinel.example.com")
	t.Setenv("ANTHROPIC_BASE_URL", "https://custom.example.com")

	got := agent.Resolve()
	if got != "https://sentinel.example.com" {
		t.Fatalf("expected sentinel URL to take priority, got %q", got)
	}
}

func TestResolveSentinelScopedPerAgent(t *testing.T) {
	// A sentinel set by a parent msc running claude must NOT redirect a nested
	// msc for a different agent: codex would forward OpenAI traffic to Anthropic.
	t.Setenv(Registry["claude"].sentinelKey(), "https://api.anthropic.com")
	for _, k := range Registry["codex"].DetectEnv {
		t.Setenv(k, "")
	}
	t.Setenv(Registry["codex"].sentinelKey(), "")

	if got := Registry["codex"].Resolve(); got != openAIDefaultURL {
		t.Fatalf("codex must ignore claude's sentinel, got %q", got)
	}
	// Same-agent nesting still honors its own sentinel.
	if got := Registry["claude"].Resolve(); got != "https://api.anthropic.com" {
		t.Fatalf("claude must honor its own sentinel, got %q", got)
	}
}

// clearOpenAIFamilyEnv unsets every DetectEnv var and sentinel of the agents
// that share the OpenAI base-URL env vars, so cross-agent sentinel tests are
// hermetic.
func clearOpenAIFamilyEnv(t *testing.T) {
	t.Helper()
	for _, name := range []string{"codex", "opencode", "aider", "qwen"} {
		a := Registry[name]
		for _, k := range a.DetectEnv {
			t.Setenv(k, "")
		}
		t.Setenv(a.sentinelKey(), "")
	}
}

func TestResolveSharedEnvVarUsesSiblingSentinel(t *testing.T) {
	// aider nested under `msc codex`: the parent set OPENAI_BASE_URL to its own
	// proxy address and MSC_UPSTREAM_CODEX to the real upstream. Agents sharing
	// that env var must adopt codex's sentinel instead of chaining onto the
	// parent proxy (which would inject and capture every exchange twice).
	clearOpenAIFamilyEnv(t)
	t.Setenv("OPENAI_BASE_URL", "http://127.0.0.1:41000") // parent msc proxy
	t.Setenv(Registry["codex"].sentinelKey(), "https://api.openai.com")

	for _, name := range []string{"aider", "opencode"} {
		if got := Registry[name].Resolve(); got != "https://api.openai.com" {
			t.Errorf("%s under msc codex must use codex's sentinel, got %q", name, got)
		}
	}
}

func TestResolveUnpoisonedEnvVarBeatsSiblingSentinel(t *testing.T) {
	// OPENAI_API_BASE is not overridden by `msc codex` (it only sets its EnvKey,
	// OPENAI_BASE_URL), so a user-configured value there must still win for
	// aider, whose DetectEnv checks it first.
	clearOpenAIFamilyEnv(t)
	t.Setenv("OPENAI_BASE_URL", "http://127.0.0.1:41000") // parent msc proxy
	t.Setenv(Registry["codex"].sentinelKey(), "https://api.openai.com")
	t.Setenv("OPENAI_API_BASE", "https://user.example.com")

	if got := Registry["aider"].Resolve(); got != "https://user.example.com" {
		t.Fatalf("aider must honor untouched OPENAI_API_BASE, got %q", got)
	}
}

func TestResolveUnrelatedSentinelDoesNotHijackEnvVar(t *testing.T) {
	// claude's sentinel does not override OPENAI_BASE_URL, so codex must keep
	// the user-configured value even when MSC_UPSTREAM_CLAUDE is present.
	clearOpenAIFamilyEnv(t)
	t.Setenv(Registry["claude"].sentinelKey(), "https://api.anthropic.com")
	t.Setenv("OPENAI_BASE_URL", "https://user.example.com")

	if got := Registry["codex"].Resolve(); got != "https://user.example.com" {
		t.Fatalf("codex must ignore claude's sentinel for OPENAI_BASE_URL, got %q", got)
	}
}

func TestResolveOwnSentinelBeatsSiblingSentinel(t *testing.T) {
	// Same-agent nesting: the agent's own sentinel stays authoritative even when
	// a sibling sharing the env var also left one behind.
	clearOpenAIFamilyEnv(t)
	t.Setenv("OPENAI_BASE_URL", "http://127.0.0.1:41000")
	t.Setenv(Registry["codex"].sentinelKey(), "https://codex.example.com")
	t.Setenv(Registry["aider"].sentinelKey(), "https://aider.example.com")

	if got := Registry["aider"].Resolve(); got != "https://aider.example.com" {
		t.Fatalf("aider must prefer its own sentinel, got %q", got)
	}
}

func TestResolveAltDefault(t *testing.T) {
	t.Setenv(Registry["agy"].sentinelKey(), "")
	for _, k := range Registry["agy"].DetectEnv {
		t.Setenv(k, "")
	}
	t.Setenv("GEMINI_API_KEY", "test-key")

	agent := Registry["agy"]
	got := agent.Resolve()
	if got != "https://generativelanguage.googleapis.com" {
		t.Fatalf("expected alt default URL for API key auth, got %q", got)
	}
}

func TestBuildEnvSetsProxyAndSentinel(t *testing.T) {
	agent := Registry["claude"]
	env := agent.BuildEnv("http://127.0.0.1:9999", "https://api.anthropic.com")

	var foundEnvKey, foundSentinel bool
	for _, e := range env {
		if strings.HasPrefix(e, "ANTHROPIC_BASE_URL=") {
			if e != "ANTHROPIC_BASE_URL=http://127.0.0.1:9999" {
				t.Fatalf("expected proxy URL in ANTHROPIC_BASE_URL, got %q", e)
			}
			foundEnvKey = true
		}
		if strings.HasPrefix(e, agent.sentinelKey()+"=") {
			if e != agent.sentinelKey()+"=https://api.anthropic.com" {
				t.Fatalf("expected upstream in %s, got %q", agent.sentinelKey(), e)
			}
			foundSentinel = true
		}
	}

	if !foundEnvKey {
		t.Fatal("ANTHROPIC_BASE_URL not found in env")
	}
	if !foundSentinel {
		t.Fatalf("%s not found in env", agent.sentinelKey())
	}
	if agent.sentinelKey() != "MSC_UPSTREAM_CLAUDE" {
		t.Fatalf("sentinel key = %q, want MSC_UPSTREAM_CLAUDE", agent.sentinelKey())
	}
}

func TestBuildEnvExtraKeys(t *testing.T) {
	agent := Registry["agy"]
	const proxyURL = "http://127.0.0.1:9999"
	env := agent.BuildEnv(proxyURL, "https://cloudcode-pa.googleapis.com")

	values := map[string]string{}
	for _, e := range env {
		k, v, _ := strings.Cut(e, "=")
		if k == agent.EnvKey || k == agent.sentinelKey() {
			values[k] = v
		}
		for _, extra := range agent.ExtraEnvKeys {
			if k == extra {
				values[k] = v
			}
		}
	}

	if _, ok := values[agent.EnvKey]; !ok {
		t.Fatalf("primary env key %q not found", agent.EnvKey)
	}
	if values[agent.EnvKey] != proxyURL {
		t.Errorf("primary env key %q = %q, want %q", agent.EnvKey, values[agent.EnvKey], proxyURL)
	}
	for _, extra := range agent.ExtraEnvKeys {
		if _, ok := values[extra]; !ok {
			t.Fatalf("extra env key %q not found", extra)
		}
		if values[extra] != proxyURL {
			t.Errorf("extra env key %q = %q, want %q", extra, values[extra], proxyURL)
		}
	}
}

func TestBuildEnvReplacesExisting(t *testing.T) {
	// Simulate an existing env var that should be replaced.
	t.Setenv("ANTHROPIC_BASE_URL", "https://old.example.com")

	agent := Registry["claude"]
	env := agent.BuildEnv("http://127.0.0.1:9999", "https://api.anthropic.com")

	count := 0
	for _, e := range env {
		if strings.HasPrefix(e, "ANTHROPIC_BASE_URL=") {
			count++
		}
	}

	if count != 1 {
		t.Fatalf("expected exactly 1 ANTHROPIC_BASE_URL entry, got %d", count)
	}
}

func TestListSorted(t *testing.T) {
	names := ListSorted()
	if len(names) == 0 {
		t.Fatal("expected at least one agent")
	}

	for i := 1; i < len(names); i++ {
		if names[i] < names[i-1] {
			t.Fatalf("names not sorted: %q comes after %q", names[i], names[i-1])
		}
	}
}

func TestRegistryNoReservedNames(t *testing.T) {
	for name := range Registry {
		if ReservedCommands[name] {
			t.Fatalf("agent name %q is a reserved command name", name)
		}
	}
}

func TestAllAgentsHaveRequiredFields(t *testing.T) {
	for name, a := range Registry {
		if a.Command == "" {
			t.Errorf("agent %q: missing Command", name)
		}
		if a.EnvKey == "" {
			t.Errorf("agent %q: missing EnvKey", name)
		}
		if a.DefaultURL == "" {
			t.Errorf("agent %q: missing DefaultURL", name)
		}
		if len(a.CapturePaths) == 0 {
			t.Errorf("agent %q: missing CapturePaths", name)
		}
	}
}

// captures reports whether the agent's CapturePaths match a request path the way
// the proxy does — a case-insensitive substring of the request path.
func captures(a Agent, reqPath string) bool {
	lower := strings.ToLower(reqPath)
	for _, sub := range a.CapturePaths {
		if strings.Contains(lower, strings.ToLower(sub)) {
			return true
		}
	}
	return false
}

func TestGrokCapturesResponsesEndpoint(t *testing.T) {
	// grok's CLI talks to its chat proxy via the OpenAI Responses API
	// (POST /v1/responses) under --mitm — that endpoint must be captured.
	// Regression for the path that was previously missed (capture=false).
	grok := Registry["grok"]
	if !captures(grok, "/v1/responses") {
		t.Errorf("grok must capture /v1/responses; CapturePaths=%v", grok.CapturePaths)
	}
	// Still captures the chat-completions path used in API-key mode.
	if !captures(grok, "/v1/chat/completions") {
		t.Errorf("grok must still capture /v1/chat/completions; CapturePaths=%v", grok.CapturePaths)
	}
	// A non-LLM control path is not captured.
	if captures(grok, "/v1/models") {
		t.Errorf("grok must not capture /v1/models; CapturePaths=%v", grok.CapturePaths)
	}
}

func TestOpencodeCapturesBothFormats(t *testing.T) {
	// opencode's "zen" backend routes some models via the OpenAI Chat Completions
	// API and others via the Anthropic Messages API; both must be captured.
	oc := Registry["opencode"]
	for _, p := range []string{"/zen/v1/chat/completions", "/zen/v1/messages", "/v1/responses"} {
		if !captures(oc, p) {
			t.Errorf("opencode must capture %s; CapturePaths=%v", p, oc.CapturePaths)
		}
	}
	if captures(oc, "/api.json") {
		t.Errorf("opencode must not capture /api.json; CapturePaths=%v", oc.CapturePaths)
	}
}

func TestQwenCapturesOpenAIAndGemini(t *testing.T) {
	// qwen (Qwen Code, a Gemini-CLI fork) speaks OpenAI-compatible by default and
	// the Gemini API in Google auth mode; both must be captured.
	qwen := Registry["qwen"]
	paths := []string{
		"/v1/chat/completions",                             // DashScope OpenAI mode
		"/v1beta/models/qwen3-coder:streamGenerateContent", // Gemini-CLI streaming
		"/v1beta/models/qwen3:generateContent",             // Gemini-CLI non-streaming
	}
	for _, p := range paths {
		if !captures(qwen, p) {
			t.Errorf("qwen must capture %s; CapturePaths=%v", p, qwen.CapturePaths)
		}
	}
}

func TestGeminiCountTokensNotCaptured(t *testing.T) {
	// Gemini-CLI-family clients send the full "contents" array to :countTokens
	// before each :generateContent; capturing it would store a user-only
	// duplicate memory per turn that escapes dedup (its concept hashes
	// differently from the paired generateContent capture). Mirrors claude's
	// /count_tokens exclusion.
	for _, name := range []string{"qwen", "agy"} {
		a := Registry[name]
		if captures(a, "/v1beta/models/qwen3:countTokens") {
			t.Errorf("%s must not capture :countTokens; CapturePaths=%v", name, a.CapturePaths)
		}
		if !captures(a, "/v1beta/models/qwen3:generateContent") {
			t.Errorf("%s must still capture :generateContent; CapturePaths=%v", name, a.CapturePaths)
		}
	}
}

func TestReasonixRemoved(t *testing.T) {
	if _, ok := Registry["reasonix"]; ok {
		t.Error("reasonix must not be in the registry")
	}
}

func TestBuildArgsProxySubstitution(t *testing.T) {
	// qwen takes its base URL from a flag, so ProxyArgs must be injected with the
	// live proxy URL substituted for {proxy}, before the user's args.
	a := Agent{ProxyArgs: []string{"--auth-type", "openai", "--openai-base-url", proxyURLPlaceholder}}
	got := a.buildArgs("http://127.0.0.1:7777", []string{"-p", "hi"})
	want := []string{"--auth-type", "openai", "--openai-base-url", "http://127.0.0.1:7777", "-p", "hi"}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("arg %d: got %q want %q", i, got[i], want[i])
		}
	}
	if q := Registry["qwen"]; len(q.ProxyArgs) == 0 {
		t.Error("qwen should define ProxyArgs so its base URL is injected")
	}
}

func FuzzBuildArgs(f *testing.F) {
	f.Add("http://127.0.0.1:9", "-p", "hi")
	f.Fuzz(func(t *testing.T, proxyURL, arg1, arg2 string) {
		a := Agent{
			WaitArgs:  []string{"--wait"},
			ProxyArgs: []string{"--openai-base-url", proxyURLPlaceholder},
		}
		got := a.buildArgs(proxyURL, []string{arg1, arg2})
		// Shape: WaitArgs + ProxyArgs + user args, with the placeholder substituted.
		if len(got) != 1+2+2 {
			t.Fatalf("arg count = %d", len(got))
		}
		if got[0] != "--wait" || got[1] != "--openai-base-url" {
			t.Fatalf("prefix args wrong: %v", got[:2])
		}
		if got[2] != proxyURL { // placeholder replaced with the proxy URL
			t.Fatalf("proxy URL not substituted into ProxyArgs: %q", got[2])
		}
		if got[3] != arg1 || got[4] != arg2 {
			t.Fatalf("user args not appended verbatim: %v", got[3:])
		}
	})
}

func TestExecMissingBinary(t *testing.T) {
	a := Agent{Command: "msc-nonexistent-binary-xyz-123", EnvKey: "FOO_URL", DefaultURL: "https://x"}
	if err := a.Exec("http://127.0.0.1:1", "https://x", nil); err == nil {
		t.Error("expected error for missing binary")
	}
}

func TestExecRunsTrue(t *testing.T) {
	if _, err := exec.LookPath("true"); err != nil {
		t.Skip("/bin/true not available")
	}
	a := Agent{Command: "true", EnvKey: "FOO_URL", DefaultURL: "https://x"}
	if err := a.Exec("http://127.0.0.1:9", "https://x", nil); err != nil {
		t.Errorf("Exec true should succeed, got %v", err)
	}
}

func TestBuildMITMEnv(t *testing.T) {
	const (
		proxyURL   = "http://127.0.0.1:9999"
		upstream   = "https://api.anthropic.com"
		caPath     = "/tmp/msc/ca-cert.pem"
		bundlePath = "/tmp/msc/ca-bundle.pem"
	)
	agent := Registry["claude"]
	env := agent.BuildMITMEnv(proxyURL, upstream, caPath, bundlePath)

	got := map[string]string{}
	for _, e := range env {
		k, v, _ := strings.Cut(e, "=")
		got[k] = v
	}

	// Both cases for HTTP and HTTPS proxy vars must point at msc.
	for _, k := range []string{"HTTP_PROXY", "http_proxy", "HTTPS_PROXY", "https_proxy", "ALL_PROXY", "all_proxy"} {
		if got[k] != proxyURL {
			t.Errorf("%s = %q, want %q", k, got[k], proxyURL)
		}
	}
	// Additive CA-trust vars (Node/Bun, Deno) get the CA cert alone.
	for _, k := range []string{"NODE_EXTRA_CA_CERTS", "DENO_CERT"} {
		if got[k] != caPath {
			t.Errorf("%s = %q, want %q", k, got[k], caPath)
		}
	}
	// Replacing CA-trust vars (OpenSSL/curl, Python-requests) get the combined
	// bundle so blind-tunneled hosts still verify under --mitm-host scoping.
	for _, k := range []string{"SSL_CERT_FILE", "REQUESTS_CA_BUNDLE", "CURL_CA_BUNDLE"} {
		if got[k] != bundlePath {
			t.Errorf("%s = %q, want %q", k, got[k], bundlePath)
		}
	}
	// Node's undici fetch ignores proxy env without this; required for claude/qwen.
	if got["NODE_USE_ENV_PROXY"] != "1" {
		t.Errorf("NODE_USE_ENV_PROXY = %q, want 1", got["NODE_USE_ENV_PROXY"])
	}
	if got[agent.sentinelKey()] != upstream {
		t.Errorf("%s = %q, want %q", agent.sentinelKey(), got[agent.sentinelKey()], upstream)
	}
	// MITM mode must NOT override the agent's base-URL env var (that's the point:
	// interception is transparent for agents that ignore it).
	if _, ok := got[agent.EnvKey]; ok {
		t.Errorf("MITM env should not set the base-URL key %q", agent.EnvKey)
	}
}

func TestBuildMITMEnvReplacesExisting(t *testing.T) {
	t.Setenv("HTTPS_PROXY", "http://old.example.com")
	t.Setenv("NODE_EXTRA_CA_CERTS", "/old/ca.pem")

	agent := Registry["claude"]
	env := agent.BuildMITMEnv("http://127.0.0.1:9999", "https://api.anthropic.com", "/new/ca.pem", "/new/ca-bundle.pem")

	var httpsCount, caCount int
	for _, e := range env {
		if strings.HasPrefix(e, "HTTPS_PROXY=") {
			httpsCount++
		}
		if strings.HasPrefix(e, "NODE_EXTRA_CA_CERTS=") {
			caCount++
		}
	}
	if httpsCount != 1 {
		t.Errorf("expected HTTPS_PROXY to appear once, got %d", httpsCount)
	}
	if caCount != 1 {
		t.Errorf("expected NODE_EXTRA_CA_CERTS to appear once, got %d", caCount)
	}
}

func TestExecMITMMissingBinary(t *testing.T) {
	dir := t.TempDir()
	caPath := filepath.Join(dir, "ca-cert.pem")
	if err := os.WriteFile(caPath, []byte("FAKE MSC CA\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	a := Agent{Command: "msc-nonexistent-binary-xyz-123", EnvKey: "FOO_URL", DefaultURL: "https://x"}
	if err := a.ExecMITM("http://127.0.0.1:1", "https://x", caPath, nil); err == nil {
		t.Error("expected error for missing binary")
	}
}

func TestWriteCombinedCABundle(t *testing.T) {
	dir := t.TempDir()
	rootsPath := filepath.Join(dir, "roots.pem")
	caPath := filepath.Join(dir, "ca-cert.pem")
	const roots = "FAKE SYSTEM ROOTS" // no trailing newline: separator must be inserted
	const ca = "FAKE MSC CA\n"
	if err := os.WriteFile(rootsPath, []byte(roots), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(caPath, []byte(ca), 0o600); err != nil {
		t.Fatal(err)
	}
	// systemRootsPEM honors an SSL_CERT_FILE set by the user before probing the
	// well-known locations; use it to make the test hermetic.
	t.Setenv("SSL_CERT_FILE", rootsPath)

	bundlePath, err := writeCombinedCABundle(caPath)
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(dir, "ca-bundle.pem"); bundlePath != want {
		t.Fatalf("bundle path = %q, want %q", bundlePath, want)
	}
	got, err := os.ReadFile(bundlePath)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != roots+"\n"+ca {
		t.Fatalf("bundle content = %q, want system roots followed by CA", got)
	}
}

func FuzzBuildMITMEnv(f *testing.F) {
	f.Add("http://127.0.0.1:9", "https://up", "/ca.pem", "/ca-bundle.pem")
	f.Fuzz(func(t *testing.T, proxyURL, upstream, caPath, bundlePath string) {
		a := Agent{Command: "claude", EnvKey: "ANTHROPIC_BASE_URL"}
		env := a.BuildMITMEnv(proxyURL, upstream, caPath, bundlePath)
		// Must never panic; every entry is a well-formed key=value pair, and no
		// proxy/CA key is duplicated.
		seen := map[string]int{}
		for _, e := range env {
			if !strings.Contains(e, "=") {
				t.Fatalf("malformed env entry: %q", e)
			}
			k, _, _ := strings.Cut(e, "=")
			seen[k]++
		}
		for _, k := range []string{"HTTPS_PROXY", "https_proxy", "NODE_EXTRA_CA_CERTS", a.sentinelKey()} {
			if seen[k] != 1 {
				t.Fatalf("key %q appears %d times, want 1", k, seen[k])
			}
		}
	})
}

// TestResolveQwenAdoptsOpenAIFamilySentinel pins the direction that stays
// shared: qwen reads OPENAI_BASE_URL to pick a custom upstream (the flag
// msc injects only overrides that choice for the child), so when a parent msc
// codex has replaced the var with its proxy address, qwen must take codex's
// sentinel rather than chain onto the parent proxy.
func TestResolveQwenAdoptsOpenAIFamilySentinel(t *testing.T) {
	clearOpenAIFamilyEnv(t)
	t.Setenv("OPENAI_BASE_URL", "http://127.0.0.1:41000") // parent msc proxy
	t.Setenv(Registry["codex"].sentinelKey(), "https://api.openai.com")

	if got := Registry["qwen"].Resolve(); got != "https://api.openai.com" {
		t.Errorf("qwen under msc codex must use codex's sentinel, got %q", got)
	}
}

// TestEnvOverridesLeavesArgsRoutedEnvKeyAlone is the other direction: msc qwen
// must not poison OPENAI_BASE_URL for a nested msc codex/opencode/aider, which
// does read it and would otherwise resolve DashScope as its upstream.
func TestEnvOverridesLeavesArgsRoutedEnvKeyAlone(t *testing.T) {
	overrides := Registry["qwen"].EnvOverrides("http://127.0.0.1:41000", Registry["qwen"].DefaultURL)
	if _, ok := overrides["OPENAI_BASE_URL"]; ok {
		t.Error("qwen must not set OPENAI_BASE_URL; it does not read that var")
	}
	if got := overrides[Registry["qwen"].sentinelKey()]; got != Registry["qwen"].DefaultURL {
		t.Errorf("qwen sentinel = %q, want its own upstream", got)
	}

	// A sibling that does read the var still gets it.
	codex := Registry["codex"].EnvOverrides("http://127.0.0.1:41000", "https://api.openai.com")
	if got := codex["OPENAI_BASE_URL"]; got != "http://127.0.0.1:41000" {
		t.Errorf("codex OPENAI_BASE_URL = %q, want the proxy URL", got)
	}
}

// TestBaseURLSourceReportsTheFlagForArgsRouted keeps `msc list` and `--help`
// honest: qwen's base URL comes from the injected flag, not the env var that
// happens to share its name.
func TestBaseURLSourceReportsTheFlagForArgsRouted(t *testing.T) {
	if got := Registry["qwen"].BaseURLSource(); got != "--openai-base-url flag" {
		t.Errorf("qwen base URL source = %q", got)
	}
	if got := Registry["codex"].BaseURLSource(); got != "OPENAI_BASE_URL" {
		t.Errorf("codex base URL source = %q", got)
	}
	if got := Registry["agy"].BaseURLSource(); !strings.HasPrefix(got, "CODE_ASSIST_ENDPOINT (also: GOOGLE_GEMINI_BASE_URL") {
		t.Errorf("agy base URL source = %q", got)
	}
}

// With no system PEM bundle there is nothing to prepend msc's CA to, so no
// bundle is written and the env vars that REPLACE the child's trust store stay
// unset. Pointing them at msc's CA alone would break TLS to every other host
// the agent reaches; this is the path Windows takes.
func TestNoSystemCABundleLeavesTrustStoreAlone(t *testing.T) {
	dir := t.TempDir()
	caPath := filepath.Join(dir, "ca-cert.pem")
	if err := os.WriteFile(caPath, []byte("FAKE MSC CA\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// The system-roots probe is a separate step so this covers the no-bundle
	// branch on any host, installed bundles and all.
	bundlePath, err := writeCABundle(caPath, nil)
	if err != nil {
		t.Fatal(err)
	}
	if bundlePath != "" {
		t.Errorf("no system roots should yield no bundle path, got %q", bundlePath)
	}
	if _, err := os.Stat(CABundlePath(caPath)); !os.IsNotExist(err) {
		t.Errorf("no bundle file should be written, stat err = %v", err)
	}

	a := Agent{Command: "claude", EnvKey: "ANTHROPIC_BASE_URL"}
	overrides := a.MITMOverrides("http://127.0.0.1:9", "https://up", caPath, bundlePath)
	for _, k := range []string{"SSL_CERT_FILE", "REQUESTS_CA_BUNDLE", "CURL_CA_BUNDLE"} {
		if _, ok := overrides[k]; ok {
			t.Errorf("%s replaces the trust store and must be unset with no combined bundle", k)
		}
	}
	// The additive variables still carry the CA, so the child trusts msc.
	if overrides["NODE_EXTRA_CA_CERTS"] != caPath || overrides["DENO_CERT"] != caPath {
		t.Errorf("additive CA variables must still point at the CA: %v", overrides)
	}
}
