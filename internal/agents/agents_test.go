package agents

import (
	"os/exec"
	"strings"
	"testing"
)

func TestResolveDefaultURL(t *testing.T) {
	// Clear any env vars that might interfere.
	for _, a := range Registry {
		for _, k := range a.DetectEnv {
			t.Setenv(k, "")
		}
	}
	t.Setenv(mscSentinel, "")

	agent := Registry["claude"]
	got := agent.Resolve()
	if got != "https://api.anthropic.com" {
		t.Fatalf("expected default URL, got %q", got)
	}
}

func TestResolveDetectEnv(t *testing.T) {
	t.Setenv(mscSentinel, "")
	t.Setenv("ANTHROPIC_BASE_URL", "https://custom.example.com/")

	agent := Registry["claude"]
	got := agent.Resolve()
	// Trailing slash should be stripped.
	if got != "https://custom.example.com" {
		t.Fatalf("expected custom URL without trailing slash, got %q", got)
	}
}

func TestResolveMSCSentinelTakesPriority(t *testing.T) {
	t.Setenv(mscSentinel, "https://sentinel.example.com")
	t.Setenv("ANTHROPIC_BASE_URL", "https://custom.example.com")

	agent := Registry["claude"]
	got := agent.Resolve()
	if got != "https://sentinel.example.com" {
		t.Fatalf("expected sentinel URL to take priority, got %q", got)
	}
}

func TestResolveAltDefault(t *testing.T) {
	t.Setenv(mscSentinel, "")
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
		if strings.HasPrefix(e, mscSentinel+"=") {
			if e != mscSentinel+"=https://api.anthropic.com" {
				t.Fatalf("expected upstream in MSC_UPSTREAM, got %q", e)
			}
			foundSentinel = true
		}
	}

	if !foundEnvKey {
		t.Fatal("ANTHROPIC_BASE_URL not found in env")
	}
	if !foundSentinel {
		t.Fatal("MSC_UPSTREAM not found in env")
	}
}

func TestBuildEnvExtraKeys(t *testing.T) {
	agent := Registry["agy"]
	const proxyURL = "http://127.0.0.1:9999"
	env := agent.BuildEnv(proxyURL, "https://cloudcode-pa.googleapis.com")

	values := map[string]string{}
	for _, e := range env {
		k, v, _ := strings.Cut(e, "=")
		if k == agent.EnvKey || k == mscSentinel {
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
		proxyURL = "http://127.0.0.1:9999"
		upstream = "https://api.anthropic.com"
		caPath   = "/tmp/msc/ca-cert.pem"
	)
	agent := Registry["claude"]
	env := agent.BuildMITMEnv(proxyURL, upstream, caPath)

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
	// CA-trust vars across runtimes (Node/Bun, OpenSSL/curl, Python, Deno) must
	// point at the CA cert.
	for _, k := range []string{"NODE_EXTRA_CA_CERTS", "SSL_CERT_FILE", "REQUESTS_CA_BUNDLE", "CURL_CA_BUNDLE", "DENO_CERT"} {
		if got[k] != caPath {
			t.Errorf("%s = %q, want %q", k, got[k], caPath)
		}
	}
	// Node's undici fetch ignores proxy env without this; required for claude/qwen/reasonix.
	if got["NODE_USE_ENV_PROXY"] != "1" {
		t.Errorf("NODE_USE_ENV_PROXY = %q, want 1", got["NODE_USE_ENV_PROXY"])
	}
	if got[mscSentinel] != upstream {
		t.Errorf("%s = %q, want %q", mscSentinel, got[mscSentinel], upstream)
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
	env := agent.BuildMITMEnv("http://127.0.0.1:9999", "https://api.anthropic.com", "/new/ca.pem")

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
	a := Agent{Command: "msc-nonexistent-binary-xyz-123", EnvKey: "FOO_URL", DefaultURL: "https://x"}
	if err := a.ExecMITM("http://127.0.0.1:1", "https://x", "/tmp/ca.pem", nil); err == nil {
		t.Error("expected error for missing binary")
	}
}

func FuzzBuildMITMEnv(f *testing.F) {
	f.Add("http://127.0.0.1:9", "https://up", "/ca.pem")
	f.Fuzz(func(t *testing.T, proxyURL, upstream, caPath string) {
		a := Agent{Command: "claude", EnvKey: "ANTHROPIC_BASE_URL"}
		env := a.BuildMITMEnv(proxyURL, upstream, caPath)
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
		for _, k := range []string{"HTTPS_PROXY", "https_proxy", "NODE_EXTRA_CA_CERTS", mscSentinel} {
			if seen[k] != 1 {
				t.Fatalf("key %q appears %d times, want 1", k, seen[k])
			}
		}
	})
}
