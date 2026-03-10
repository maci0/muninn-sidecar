package agents

import (
	"os"
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
	for _, k := range Registry["gemini"].DetectEnv {
		t.Setenv(k, "")
	}
	t.Setenv("GEMINI_API_KEY", "test-key")

	agent := Registry["gemini"]
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
	agent := Registry["gemini"]
	env := agent.BuildEnv("http://127.0.0.1:9999", "https://cloudcode-pa.googleapis.com")

	found := map[string]bool{}
	for _, e := range env {
		k, _, _ := strings.Cut(e, "=")
		if k == agent.EnvKey || k == mscSentinel {
			found[k] = true
		}
		for _, extra := range agent.ExtraEnvKeys {
			if k == extra {
				found[k] = true
			}
		}
	}

	if !found[agent.EnvKey] {
		t.Fatalf("primary env key %q not found", agent.EnvKey)
	}
	for _, extra := range agent.ExtraEnvKeys {
		if !found[extra] {
			t.Fatalf("extra env key %q not found", extra)
		}
	}
}

func TestBuildEnvReplacesExisting(t *testing.T) {
	// Simulate an existing env var that should be replaced.
	os.Setenv("ANTHROPIC_BASE_URL", "https://old.example.com")
	defer os.Unsetenv("ANTHROPIC_BASE_URL")

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
		if reservedNames[name] {
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
