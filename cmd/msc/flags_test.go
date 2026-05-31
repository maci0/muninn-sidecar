package main

import (
	"strings"
	"testing"
	"time"
)

func TestParseFlags(t *testing.T) {
	t.Run("agent passthrough stops parsing", func(t *testing.T) {
		o := &opts{}
		rem, act, err := parseFlags([]string{"-d", "claude", "--weird", "x"}, o)
		if err != nil || act != actionNone {
			t.Fatalf("err=%v act=%v", err, act)
		}
		if !o.debug {
			t.Error("--debug should be set")
		}
		if len(rem) != 3 || rem[0] != "claude" || rem[1] != "--weird" {
			t.Errorf("agent args should pass through unparsed: %v", rem)
		}
	})

	t.Run("value flags", func(t *testing.T) {
		o := &opts{}
		_, _, err := parseFlags([]string{"--vault", "v", "--inject-budget", "1024", "--inject-min-score", "0.55", "--recall-mode", "deep", "--no-auto-calibrate", "claude"}, o)
		if err != nil {
			t.Fatal(err)
		}
		if o.vault != "v" || o.injectBudget != 1024 || o.minScore != 0.55 || o.recallMode != "deep" || !o.noAutoCalibrate {
			t.Errorf("flags not parsed: %+v", o)
		}
	})

	t.Run("grounding flags", func(t *testing.T) {
		o := &opts{}
		_, _, err := parseFlags([]string{"--ground-url", "http://x/v1", "--ground-model", "m", "--ground-topk", "4", "--ground-timeout", "8s", "claude"}, o)
		if err != nil {
			t.Fatal(err)
		}
		if o.groundURL != "http://x/v1" || o.groundModel != "m" || o.groundTopK != 4 || o.groundTimeout != 8*time.Second {
			t.Errorf("grounding flags not parsed: %+v", o)
		}
	})

	t.Run("mitm flag", func(t *testing.T) {
		o := &opts{}
		rem, _, err := parseFlags([]string{"--mitm", "claude"}, o)
		if err != nil || !o.mitm {
			t.Fatalf("--mitm not parsed: err=%v mitm=%v", err, o.mitm)
		}
		if len(rem) != 1 || rem[0] != "claude" {
			t.Errorf("agent should follow --mitm: %v", rem)
		}
	})

	t.Run("mitm rejects =value", func(t *testing.T) {
		if _, _, err := parseFlags([]string{"--mitm=true", "claude"}, &opts{}); err == nil {
			t.Error("expected error for --mitm=true")
		}
	})

	t.Run("=value syntax", func(t *testing.T) {
		o := &opts{}
		if _, _, err := parseFlags([]string{"--vault=x", "claude"}, o); err != nil || o.vault != "x" {
			t.Errorf("=value: err=%v vault=%q", err, o.vault)
		}
	})

	t.Run("bool rejects =value", func(t *testing.T) {
		if _, _, err := parseFlags([]string{"--no-inject=false", "claude"}, &opts{}); err == nil {
			t.Error("expected error for --no-inject=false")
		}
	})

	t.Run("invalid values rejected", func(t *testing.T) {
		for _, args := range [][]string{
			{"--inject-budget", "0"},
			{"--inject-budget", "abc"},
			{"--inject-min-score", "9"},
			{"--inject-min-score", "x"},
			{"--recall-mode", "bogus"},
			{"--ground-topk", "0"},
			{"--ground-topk", "abc"},
			{"--ground-timeout", "nope"},
			{"--ground-timeout", "-5s"},
			{"--vault", ""},
			{"--unknown-flag"},
		} {
			if _, _, err := parseFlags(args, &opts{}); err == nil {
				t.Errorf("expected error for %v", args)
			}
		}
	})

	t.Run("missing value", func(t *testing.T) {
		if _, _, err := parseFlags([]string{"--vault"}, &opts{}); err == nil {
			t.Error("expected error for trailing --vault")
		}
	})

	t.Run("help/version actions", func(t *testing.T) {
		if _, a, _ := parseFlags([]string{"--help"}, &opts{}); a != actionHelp {
			t.Error("--help")
		}
		if _, a, _ := parseFlags([]string{"-v"}, &opts{}); a != actionVersion {
			t.Error("-v")
		}
	})

	t.Run("internal command keeps parsing flags after", func(t *testing.T) {
		o := &opts{}
		rem, _, err := parseFlags([]string{"status", "--json"}, o)
		if err != nil || !o.asJSON || len(rem) == 0 || rem[0] != "status" {
			t.Errorf("status --json: err=%v json=%v rem=%v", err, o.asJSON, rem)
		}
	})

	t.Run("double dash terminates", func(t *testing.T) {
		o := &opts{}
		rem, _, err := parseFlags([]string{"claude", "--", "-x"}, o)
		if err != nil {
			t.Fatal(err)
		}
		if rem[0] != "claude" {
			t.Errorf("rem=%v", rem)
		}
	})
}

func TestResolveConfig(t *testing.T) {
	o := &opts{mcpURL: "http://h/mcp", token: "tok", vault: "myvault"}
	u, tk, v := resolveConfig(o)
	if u != "http://h/mcp" || tk != "tok" || v != "myvault" {
		t.Errorf("explicit values not used: %q %q %q", u, tk, v)
	}
	// Defaults when empty (vault falls back to cwd basename or sidecar).
	u2, _, v2 := resolveConfig(&opts{})
	if u2 == "" || v2 == "" {
		t.Errorf("defaults empty: url=%q vault=%q", u2, v2)
	}
}

func TestDefaultMCPURL(t *testing.T) {
	t.Setenv("MUNINN_MCP_URL", "")
	if got := defaultMCPURL(); got != "http://127.0.0.1:8750/mcp" {
		t.Errorf("default mcp url %q", got)
	}
	t.Setenv("MUNINN_MCP_URL", "http://custom/mcp")
	if got := defaultMCPURL(); got != "http://custom/mcp" {
		t.Errorf("env mcp url %q", got)
	}
}

func TestDefaultToken(t *testing.T) {
	t.Setenv("MUNINN_TOKEN", "envtok")
	if got := defaultToken(); got != "envtok" {
		t.Errorf("env token %q", got)
	}
}

// FuzzParseFlags: parsing arbitrary arg lists must never panic.
func FuzzParseFlags(f *testing.F) {
	f.Add("--vault v --debug claude")
	f.Add("--inject-min-score 0.5 --recall-mode deep gemini")
	f.Add("--no-inject=bad")
	f.Add("status --json")
	f.Fuzz(func(t *testing.T, s string) {
		_, _, _ = parseFlags(strings.Fields(s), &opts{})
	})
}
