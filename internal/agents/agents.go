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
	"status": true, "completion": true, "ca": true,
}

// mscSentinel is set in the child's environment by BuildEnv() so Resolve()
// can detect nested msc invocations and read the real upstream instead of
// the inner proxy's address.
const mscSentinel = "MSC_UPSTREAM"

func init() {
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
// Some agents (e.g. agy) use different API backends depending on auth mode.
// ExtraEnvKeys ensures all relevant env vars point at the proxy, while
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
	ProxyArgs      []string // flags to prepend that point the agent at the proxy; the literal "{proxy}" is replaced with the proxy URL at exec. For agents that take their base URL from a CLI flag rather than an env var (e.g. qwen).
	CapturePaths   []string // path substrings that identify LLM traffic to capture
	ExcludePaths   []string // path substrings that exclude from capture (checked first)
}

// proxyURLPlaceholder is substituted with the live proxy URL inside ProxyArgs.
const proxyURLPlaceholder = "{proxy}"

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
// they are safe if a build ever sends the full path. `/responses` is included
// because grok's CLI talks to its chat proxy via the OpenAI Responses API
// (`POST /v1/responses`), not chat-completions, under `--mitm`.
var openAIV1BaseCapturePaths = []string{"/chat/completions", "/completions", "/responses"}

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
	// codex respects OPENAI_BASE_URL only in API-key mode (OPENAI_API_KEY). In
	// ChatGPT-subscription mode (auth_mode: chatgpt in ~/.codex/auth.json) it talks
	// to the ChatGPT backend directly and ignores the env override, so the proxy is
	// bypassed and nothing is captured — documented in the README.
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
	// In its default subscription mode grok talks to cli-chat-proxy.grok.com via
	// the OpenAI Responses API (POST /v1/responses) over HTTPS (no env override),
	// so `--mitm` captures it — see openAIV1BaseCapturePaths. Verified live: the
	// turn is captured, stored, and recalled (no WebSocket involved).
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
	// qwen (Qwen Code — a Gemini-CLI fork for Qwen models). It is OpenAI-compatible
	// but takes its base URL from the --openai-base-url FLAG (and reads its endpoint
	// from ~/.qwen/settings.json), not from OPENAI_BASE_URL env, so msc injects the
	// flags via ProxyArgs instead of an env override. The base is /v1-inclusive, so
	// DefaultURL carries it (DashScope's OpenAI-compatible endpoint by default;
	// set OPENAI_BASE_URL to redirect to a local/custom upstream, e.g. ollama).
	// Verified: --auth-type openai --openai-base-url <url> routes /chat/completions
	// through the proxy. The user supplies the API key (OPENAI_API_KEY / settings).
	"qwen": {
		Command:      "qwen",
		EnvKey:       "OPENAI_BASE_URL",
		ProxyArgs:    []string{"--auth-type", "openai", "--openai-base-url", proxyURLPlaceholder},
		DetectEnv:    []string{"OPENAI_BASE_URL"},
		DefaultURL:   "https://dashscope-intl.aliyuncs.com/compatible-mode/v1",
		CapturePaths: openAIV1BaseCapturePaths,
	},
	// agy (Google Antigravity CLI) — Code Assist / Gemini family. WARNING: agy
	// authenticates via OAuth and talks to cloudcode-pa directly; in testing it
	// ignored CODE_ASSIST_ENDPOINT / GOOGLE_*_BASE_URL, so the env-override path
	// can't intercept it — use `--mitm` to capture. Registered so `msc agy`
	// launches it regardless.
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

// BuildMITMEnv constructs the child environment for TLS-MITM mode. Instead of
// overriding the agent's API base-URL env var, it points the standard proxy
// variables (HTTPS_PROXY/HTTP_PROXY/ALL_PROXY) at msc and makes the child trust
// msc's CA so the minted leaf certs verify. This catches agents that ignore a
// base-URL override (codex ChatGPT-mode, grok session auth, agy) and turns msc
// into a transparent HTTPS proxy. caCertPath is the PEM file the local CA wrote.
func (a Agent) BuildMITMEnv(proxyURL, upstream, caCertPath string) []string {
	env := os.Environ()

	// Lowercase variants are honored by curl/libcurl; uppercase by Go, Node, and
	// most runtimes. Node reads NODE_EXTRA_CA_CERTS; OpenSSL/Python read
	// SSL_CERT_FILE; requests reads REQUESTS_CA_BUNDLE; curl reads CURL_CA_BUNDLE.
	//
	// NODE_USE_ENV_PROXY=1 is critical: Node's global fetch (undici) — used by
	// the Anthropic/OpenAI SDKs that claude, qwen, and reasonix run on — ignores
	// HTTP(S)_PROXY env unless this is set (Node 24+; harmlessly ignored on older
	// Node). Verified empirically: without it those agents bypass the proxy.
	//
	// CA trust spans every runtime we support (verified with a per-runtime probe):
	// Node/Bun read NODE_EXTRA_CA_CERTS; OpenSSL/curl/Python/Go read SSL_CERT_FILE;
	// curl also CURL_CA_BUNDLE; Python-requests REQUESTS_CA_BUNDLE; Deno DENO_CERT.
	// Rust/reqwest (codex, grok) honors HTTPS_PROXY + the system store (SSL_CERT_FILE).
	replace := map[string]string{
		"HTTPS_PROXY":         proxyURL,
		"https_proxy":         proxyURL,
		"HTTP_PROXY":          proxyURL,
		"http_proxy":          proxyURL,
		"ALL_PROXY":           proxyURL,
		"all_proxy":           proxyURL,
		"NODE_USE_ENV_PROXY":  "1",
		"NODE_EXTRA_CA_CERTS": caCertPath,
		"SSL_CERT_FILE":       caCertPath,
		"REQUESTS_CA_BUNDLE":  caCertPath,
		"CURL_CA_BUNDLE":      caCertPath,
		"DENO_CERT":           caCertPath,
		mscSentinel:           upstream,
	}

	filtered := make([]string, 0, len(env)+len(replace))
	for _, e := range env {
		key, _, _ := strings.Cut(e, "=")
		if _, ok := replace[key]; ok {
			continue
		}
		filtered = append(filtered, e)
	}
	for k, v := range replace {
		filtered = append(filtered, k+"="+v)
	}
	return filtered
}

// buildArgs assembles the child argv: WaitArgs, then ProxyArgs (with the
// {proxy} placeholder substituted for the live proxy URL), then the user's args.
func (a Agent) buildArgs(proxyURL string, args []string) []string {
	out := make([]string, 0, len(a.WaitArgs)+len(a.ProxyArgs)+len(args))
	out = append(out, a.WaitArgs...)
	for _, pa := range a.ProxyArgs {
		out = append(out, strings.ReplaceAll(pa, proxyURLPlaceholder, proxyURL))
	}
	return append(out, args...)
}

// Exec resolves the agent binary via PATH, builds the modified environment,
// and runs the child process. WaitArgs (if any) are prepended to args to
// prevent the agent from running in the background. stdin/stdout/stderr are
// inherited so the user interacts with the agent normally. Returns the
// child's exit error, if any.
func (a Agent) Exec(proxyURL, upstream string, args []string) error {
	return a.runArgv(a.BuildEnv(proxyURL, upstream), a.buildArgs(proxyURL, args))
}

// ExecMITM runs the agent in TLS-MITM mode: the child trusts msc's CA (via
// caCertPath) and routes HTTPS through msc as a CONNECT proxy, rather than
// having its API base-URL env var overridden. ProxyArgs are intentionally NOT
// applied — in MITM mode the agent keeps its real upstream URL and msc
// intercepts transparently; only WaitArgs are prepended.
func (a Agent) ExecMITM(proxyURL, upstream, caCertPath string, args []string) error {
	argv := append(append([]string{}, a.WaitArgs...), args...)
	return a.runArgv(a.BuildMITMEnv(proxyURL, upstream, caCertPath), argv)
}

// runArgv looks up the agent binary and executes it with the given environment
// and already-assembled argv, inheriting stdin/stdout/stderr.
func (a Agent) runArgv(env []string, argv []string) error {
	binary, err := exec.LookPath(a.Command)
	if err != nil {
		return fmt.Errorf("agent %q not found in PATH: %w", a.Command, err)
	}
	cmd := exec.Command(binary, argv...)
	cmd.Env = env
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
