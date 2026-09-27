package config

import (
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
