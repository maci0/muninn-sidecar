// Package agents defines the supported coding agents and their API
// interception configuration.
package agents

import (
	"fmt"
	"os"
	"os/exec"
	"sort"
	"strings"
)

// ReservedCommands are command names that cannot be used as agent names.
// init() validates this at startup to prevent silent shadowing.
var ReservedCommands = map[string]bool{
	"help": true, "list": true, "version": true,
	"status": true, "completion": true,
}

// mscSentinel is set in the child's environment so Resolve() can detect
// nested msc invocations and avoid reading back the proxy URL as upstream.
const mscSentinel = "MSC_UPSTREAM"

func init() {
	if os.Getenv("MSC_EXPERIMENTAL_ANTIGRAVITY") == "1" {
		Registry["antigravity"] = Agent{
			Command:      "antigravity",
			EnvKey:       "CODE_ASSIST_ENDPOINT",
			ExtraEnvKeys: []string{"GOOGLE_GEMINI_BASE_URL", "GOOGLE_GENAI_BASE_URL"},
			DetectEnv:    []string{"CODE_ASSIST_ENDPOINT", "GOOGLE_GEMINI_BASE_URL", "GOOGLE_GENAI_BASE_URL"},
			DefaultURL:   "https://cloudcode-pa.googleapis.com",
			AltDefaultCond: "GEMINI_API_KEY",
			AltDefaultURL:  "https://generativelanguage.googleapis.com",
			WaitArgs:       []string{"--wait"},
			CapturePaths:   []string{"GenerateContent", "CountTokens", "LanguageServerService"},
		}
	}
	for name := range Registry {
		if ReservedCommands[name] {
			panic(fmt.Sprintf("agent name %q collides with reserved command", name))
		}
	}
}

// Agent describes a coding agent and how to intercept its API traffic. Each
// agent communicates with a different LLM provider (Anthropic, Google, OpenAI)
// and uses a different env var to configure the API base URL. The sidecar
// overrides that env var to point at the local reverse proxy, which forwards
// traffic to the real upstream while capturing every exchange for MuninnDB.
//
// Some agents (e.g. Gemini CLI) use different API backends depending on auth
// mode. ExtraEnvKeys ensures all relevant env vars point at the proxy, while
// AltDefaultCond/AltDefaultURL select the correct upstream automatically.
type Agent struct {
	Command        string   // binary to exec (resolved via PATH)
	EnvKey         string   // primary env var to override with the proxy URL
	ExtraEnvKeys   []string // additional env vars to also set to the proxy URL
	DetectEnv      []string // env vars to check (in order) for the real upstream
	DefaultURL     string   // fallback upstream when none of DetectEnv are set
	AltDefaultCond string   // if this env var is set, use AltDefaultURL instead
	AltDefaultURL  string   // alternative upstream for a different auth mode
	WaitArgs       []string // flags to append when executing the agent to prevent it from backgrounding
	CapturePaths   []string // path substrings that identify LLM traffic to capture
	ExcludePaths   []string // path substrings that exclude from capture (checked first)
}

// Registry maps short names to their agent definitions. Add new agents here.
// The map key is the canonical name used in logs, tags, and CLI arguments.
var Registry = map[string]Agent{
	"claude": {
		Command:      "claude",
		EnvKey:       "ANTHROPIC_BASE_URL",
		DetectEnv:    []string{"ANTHROPIC_BASE_URL"},
		DefaultURL:   "https://api.anthropic.com",
		CapturePaths: []string{"/v1/messages"},
		ExcludePaths: []string{"/count_tokens"},
	},
	"gemini": {
		Command:      "gemini",
		EnvKey:       "CODE_ASSIST_ENDPOINT",
		ExtraEnvKeys: []string{"GOOGLE_GEMINI_BASE_URL"},
		DetectEnv:    []string{"CODE_ASSIST_ENDPOINT", "GOOGLE_GEMINI_BASE_URL"},
		DefaultURL:   "https://cloudcode-pa.googleapis.com",
		// API key auth uses the standard Gemini API instead of Code Assist.
		AltDefaultCond: "GEMINI_API_KEY",
		AltDefaultURL:  "https://generativelanguage.googleapis.com",
		CapturePaths:   []string{"GenerateContent", "CountTokens"},
	},
	"codex": {
		Command:      "codex",
		EnvKey:       "OPENAI_BASE_URL",
		DetectEnv:    []string{"OPENAI_BASE_URL", "OPENAI_API_BASE"},
		DefaultURL:   "https://api.openai.com",
		CapturePaths: []string{"/v1/chat/completions", "/v1/completions", "/responses"},
	},
	"opencode": {
		Command:      "opencode",
		EnvKey:       "OPENAI_BASE_URL",
		DetectEnv:    []string{"OPENAI_BASE_URL", "OPENAI_API_BASE"},
		DefaultURL:   "https://api.openai.com",
		CapturePaths: []string{"/v1/chat/completions", "/v1/completions", "/responses"},
	},
	"aider": {
		Command:      "aider",
		EnvKey:       "OPENAI_API_BASE",
		DetectEnv:    []string{"OPENAI_API_BASE", "OPENAI_BASE_URL"},
		DefaultURL:   "https://api.openai.com",
		CapturePaths: []string{"/v1/chat/completions", "/v1/completions", "/responses"},
	},
}

// Resolve discovers the real upstream URL by checking the user's environment
// in DetectEnv order. Falls back to DefaultURL if nothing is set. The trailing
// slash is stripped to prevent double-slash issues in the reverse proxy.
//
// If MSC_UPSTREAM is set (by a parent msc process), it takes priority over
// DetectEnv to prevent an infinite proxy loop where a nested msc reads back
// the inner proxy's address as the upstream.
func (a Agent) Resolve() string {
	if v := os.Getenv(mscSentinel); v != "" {
		return strings.TrimRight(v, "/")
	}
	for _, k := range a.DetectEnv {
		if v := os.Getenv(k); v != "" {
			return strings.TrimRight(v, "/")
		}
	}
	if a.AltDefaultCond != "" && os.Getenv(a.AltDefaultCond) != "" {
		return a.AltDefaultURL
	}
	return a.DefaultURL
}

// BuildEnv constructs the child process environment. It copies the current env,
// replaces EnvKey with the proxy URL, and sets MSC_UPSTREAM to the resolved
// upstream so nested msc invocations can detect the real origin and avoid
// infinite forwarding loops. The proxy listens on plain HTTP so TLS is not
// involved in the agent→proxy hop.
func (a Agent) BuildEnv(proxyURL, upstream string) []string {
	env := os.Environ()

	// Keys to replace in the inherited environment. ExtraEnvKeys are also
	// set to the proxy URL so the agent routes through us regardless of
	// which internal code path it takes (e.g. Gemini OAuth vs API key).
	replace := map[string]string{
		a.EnvKey:    proxyURL,
		mscSentinel: upstream,
	}
	for _, k := range a.ExtraEnvKeys {
		replace[k] = proxyURL
	}

	filtered := make([]string, 0, len(env)+len(replace))
	for _, e := range env {
		key, _, _ := strings.Cut(e, "=")
		if _, ok := replace[key]; ok {
			continue // will be re-added below
		}
		filtered = append(filtered, e)
	}
	for k, v := range replace {
		filtered = append(filtered, k+"="+v)
	}

	return filtered
}

// Exec resolves the agent binary via PATH, builds the modified environment,
// and runs the child process. stdin/stdout/stderr are inherited so the user
// interacts with the agent normally. Returns the child's exit error, if any.
func (a Agent) Exec(proxyURL, upstream string, args []string) error {
	binary, err := exec.LookPath(a.Command)
	if err != nil {
		return fmt.Errorf("agent %q not found in PATH: %w", a.Command, err)
	}

	execArgs := make([]string, 0, len(a.WaitArgs)+len(args))
	execArgs = append(execArgs, a.WaitArgs...)
	execArgs = append(execArgs, args...)

	cmd := exec.Command(binary, execArgs...)
	cmd.Env = a.BuildEnv(proxyURL, upstream)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	return cmd.Run()
}

// ListSorted returns all registered agent names in sorted order.
func ListSorted() []string {
	names := make([]string, 0, len(Registry))
	for k := range Registry {
		names = append(names, k)
	}
	sort.Strings(names)
	return names
}
