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

// mscSentinel is set in the child's environment by BuildEnv() so Resolve()
// can detect nested msc invocations and read the real upstream instead of
// the inner proxy's address.
const mscSentinel = "MSC_UPSTREAM"

func init() {
	if os.Getenv("MSC_EXPERIMENTAL_ANTIGRAVITY") == "1" {
		Registry["antigravity"] = Agent{
			Command:        "antigravity",
			EnvKey:         "CODE_ASSIST_ENDPOINT",
			ExtraEnvKeys:   []string{"GOOGLE_GEMINI_BASE_URL", "GOOGLE_GENAI_BASE_URL"},
			DetectEnv:      []string{"CODE_ASSIST_ENDPOINT", "GOOGLE_GEMINI_BASE_URL", "GOOGLE_GENAI_BASE_URL"},
			DefaultURL:     "https://cloudcode-pa.googleapis.com",
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
	AltDefaultCond string   // if this env var is set and no DetectEnv vars are set, use AltDefaultURL instead
	AltDefaultURL  string   // alternative upstream for a different auth mode
	WaitArgs       []string // flags to prepend when executing the agent to prevent it from backgrounding
	CapturePaths   []string // path substrings that identify LLM traffic to capture
	ExcludePaths   []string // path substrings that exclude from capture (checked first)
}

// openAIDefaultURL and openAICapturePaths are shared by all OpenAI-compatible
// agents (codex, opencode, aider). Centralised here so a single edit covers
// all agents if the paths or upstream URL ever change.
const openAIDefaultURL = "https://api.openai.com"

var openAICapturePaths = []string{"/v1/chat/completions", "/v1/completions", "/responses"}

// openAIV1BaseCapturePaths is for OpenAI-compatible agents whose base-URL env is
// expected to already include the `/v1` segment (grok, reasonix): the client
// appends `/chat/completions` to it, so the path the proxy sees has no `/v1`
// prefix. The upstream's own `/v1` is restored by the DefaultURL path
// (singleJoiningSlash in the proxy). These substrings also match `/v1/...`, so
// they are safe if a build ever sends the full path.
var openAIV1BaseCapturePaths = []string{"/chat/completions", "/completions"}

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
		ExtraEnvKeys: []string{"GOOGLE_GEMINI_BASE_URL", "GOOGLE_GENAI_BASE_URL"},
		DetectEnv:    []string{"CODE_ASSIST_ENDPOINT", "GOOGLE_GEMINI_BASE_URL", "GOOGLE_GENAI_BASE_URL"},
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
		DefaultURL:   openAIDefaultURL,
		CapturePaths: openAICapturePaths,
	},
	"opencode": {
		Command:      "opencode",
		EnvKey:       "OPENAI_BASE_URL",
		DetectEnv:    []string{"OPENAI_BASE_URL", "OPENAI_API_BASE"},
		DefaultURL:   openAIDefaultURL,
		CapturePaths: openAICapturePaths,
	},
	"aider": {
		Command:      "aider",
		EnvKey:       "OPENAI_API_BASE",
		DetectEnv:    []string{"OPENAI_API_BASE", "OPENAI_BASE_URL"},
		DefaultURL:   openAIDefaultURL,
		CapturePaths: openAICapturePaths,
	},
	// grok (xAI CLI) — set GROK_MODELS_BASE_URL to a custom OpenAI-compatible
	// endpoint, which switches grok to API-key (Bearer) auth and routes inference
	// through it (verified: GET /v1/models, POST /v1/chat/completions). The user
	// must have an xAI API key configured; grok ignores OAuth/session auth in this
	// mode. The base is expected to include `/v1`, so DefaultURL carries it.
	"grok": {
		Command:      "grok",
		EnvKey:       "GROK_MODELS_BASE_URL",
		DetectEnv:    []string{"GROK_MODELS_BASE_URL"},
		DefaultURL:   "https://api.x.ai/v1",
		CapturePaths: openAIV1BaseCapturePaths,
	},
	// reasonix (DeepSeek-native agent) — OpenAI-compatible, overridable via
	// DEEPSEEK_BASE_URL (verified: POST /v1/chat/completions). OPENAI_BASE_URL is
	// also pointed at the proxy for its OpenAI-provider code path.
	"reasonix": {
		Command:      "reasonix",
		EnvKey:       "DEEPSEEK_BASE_URL",
		ExtraEnvKeys: []string{"OPENAI_BASE_URL"},
		DetectEnv:    []string{"DEEPSEEK_BASE_URL", "OPENAI_BASE_URL"},
		DefaultURL:   "https://api.deepseek.com/v1",
		CapturePaths: openAIV1BaseCapturePaths,
	},
	// agy (Google Antigravity CLI) — same Code Assist / Gemini family as the
	// gated "antigravity" entry. WARNING: agy is a Google-internal binary that
	// authenticates via OAuth and talks to cloudcode-pa directly; in testing it
	// ignored CODE_ASSIST_ENDPOINT / GOOGLE_*_BASE_URL, so the proxy cannot
	// currently intercept it (capture/injection will not fire). Registered so
	// `msc agy` launches it, but transparent capture is not yet supported.
	"agy": {
		Command:        "agy",
		EnvKey:         "CODE_ASSIST_ENDPOINT",
		ExtraEnvKeys:   []string{"GOOGLE_GEMINI_BASE_URL", "GOOGLE_GENAI_BASE_URL"},
		DetectEnv:      []string{"CODE_ASSIST_ENDPOINT", "GOOGLE_GEMINI_BASE_URL", "GOOGLE_GENAI_BASE_URL"},
		DefaultURL:     "https://cloudcode-pa.googleapis.com",
		AltDefaultCond: "GEMINI_API_KEY",
		AltDefaultURL:  "https://generativelanguage.googleapis.com",
		CapturePaths:   []string{"GenerateContent", "CountTokens", "LanguageServerService"},
	},
}

// Resolve discovers the real upstream URL by checking the user's environment
// in DetectEnv order. If AltDefaultCond is set and its env var is present,
// AltDefaultURL is used instead of DefaultURL. Falls back to DefaultURL if
// nothing is set. The trailing slash is stripped to prevent double-slash
// issues in the reverse proxy.
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
		return strings.TrimRight(a.AltDefaultURL, "/")
	}
	return strings.TrimRight(a.DefaultURL, "/")
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
// and runs the child process. WaitArgs (if any) are prepended to args to
// prevent the agent from running in the background. stdin/stdout/stderr are
// inherited so the user interacts with the agent normally. Returns the
// child's exit error, if any.
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
