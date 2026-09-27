// Package agents defines the supported coding agents and their API
// interception configuration.
//
// The registry (the Agent type and its table) and the environment each agent is
// launched with live in agents.go. The CA-bundle filesystem work that backs
// TLS-MITM trust is in cabundle.go, and tracking the running child process is
// in child.go.
package agents

import (
	"fmt"
	"maps"
	"os"
	"slices"
	"strings"
)

// ReservedCommands are command names that cannot be used as agent names.
// init() validates this at startup to prevent silent shadowing.
var ReservedCommands = map[string]bool{
	"help": true, "list": true, "version": true,
	"status": true, "completion": true, "ca": true,
}

// mscSentinelPrefix prefixes the per-agent env var set in the child's
// environment by BuildEnv() so Resolve() can detect nested msc invocations of
// the same agent and read the real upstream instead of the inner proxy's
// address. The sentinel is scoped per agent because the upstream it carries is
// only valid for that agent: a nested msc for a different agent (e.g. running
// `msc codex` inside an `msc claude` session) must resolve its own upstream.
// Agents that share a base-URL env var (codex/opencode/aider all read
// OPENAI_BASE_URL / OPENAI_API_BASE) are handled by sentinelForEnv: the parent
// poisoned that var with its proxy address, so its sentinel carries the real
// upstream for the whole family.
const mscSentinelPrefix = "MSC_UPSTREAM_"

// sentinelKey returns the agent-scoped upstream sentinel env var name, e.g.
// MSC_UPSTREAM_CLAUDE.
func (a Agent) sentinelKey() string {
	return mscSentinelPrefix + strings.ToUpper(strings.ReplaceAll(a.Command, "-", "_"))
}

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
	EnvKey         string   // primary env var to override with the proxy URL. For an ArgsRouted agent it is a detection hint only and is never written to the child env.
	ExtraEnvKeys   []string // additional env vars to also set to the proxy URL
	ArgsRouted     bool     // the agent reads its base URL from ProxyArgs, not from EnvKey, so EnvKey must not be poisoned in the child env
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

// geminiCapturePaths match the Gemini / Code Assist API surface (used by agy and
// by qwen's Gemini-CLI heritage). Matching is case-insensitive substring, so
// "generatecontent" also catches `:streamGenerateContent`. `:countTokens` is
// deliberately absent: Gemini-CLI-family clients send the full "contents" array
// to it before each `:generateContent`, so capturing it would store a
// user-only duplicate of every turn (mirrors claude's /count_tokens exclusion).
var geminiCapturePaths = []string{"generateContent"}

// openAIV1BaseCapturePaths is for OpenAI-compatible agents whose base-URL env is
// expected to already include the `/v1` segment (grok, qwen): the client
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
	// opencode is provider-agnostic: its default "zen" backend routes some models
	// via the OpenAI Chat Completions API (/zen/v1/chat/completions) and others
	// via the Anthropic Messages API (/zen/v1/messages), depending on the selected
	// model. Capture both formats (verified live: chat/completions captured; the
	// Anthropic path was otherwise missed). The proxy detects the format from the
	// body, so the extra path is safe for OpenAI providers too.
	"opencode": {
		Command:      "opencode",
		EnvKey:       "OPENAI_BASE_URL",
		DetectEnv:    []string{"OPENAI_BASE_URL", "OPENAI_API_BASE"},
		DefaultURL:   openAIDefaultURL,
		CapturePaths: append(append([]string(nil), openAICapturePaths...), "/v1/messages"),
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
	// qwen (Qwen Code — a Gemini-CLI fork for Qwen models). It is OpenAI-compatible
	// but takes its base URL from the --openai-base-url FLAG (and reads its endpoint
	// from ~/.qwen/settings.json), not from OPENAI_BASE_URL env, so msc injects the
	// flags via ProxyArgs instead of an env override. The base is /v1-inclusive, so
	// DefaultURL carries it (DashScope's OpenAI-compatible endpoint by default;
	// set OPENAI_BASE_URL to redirect to a local/custom upstream, e.g. ollama).
	// Verified: --auth-type openai --openai-base-url <url> routes /chat/completions
	// through the proxy. The user supplies the API key (OPENAI_API_KEY / settings).
	// As a Gemini-CLI fork it also speaks the Gemini API (`:generateContent`) in
	// its Google auth mode, so capture both formats.
	"qwen": {
		Command:      "qwen",
		EnvKey:       "OPENAI_BASE_URL",
		ProxyArgs:    []string{"--auth-type", "openai", "--openai-base-url", proxyURLPlaceholder},
		DetectEnv:    []string{"OPENAI_BASE_URL"},
		DefaultURL:   "https://dashscope-intl.aliyuncs.com/compatible-mode/v1",
		ArgsRouted:   true,
		CapturePaths: append(append([]string(nil), openAIV1BaseCapturePaths...), geminiCapturePaths...),
	},
	// agy (Google Antigravity CLI) — Code Assist / Gemini family. WARNING: agy
	// authenticates via OAuth and talks to cloudcode-pa directly; in testing it
	// ignored CODE_ASSIST_ENDPOINT / GOOGLE_*_BASE_URL, so the env-override path
	// can't intercept it. `--mitm` DOES intercept agy's HTTPS (verified live:
	// register/features/oauth/userinfo all decrypt cleanly), but its inference
	// runs over gRPC/protobuf (`application/grpc` on cloudcode-pa) — not the JSON
	// that msc's extractors read — so turns are not yet captured in usable form;
	// full support needs protobuf decoding. These CapturePaths match the gRPC
	// method names so the traffic is at least classified. Registered so `msc agy`
	// launches it regardless.
	"agy": {
		Command:        "agy",
		EnvKey:         "CODE_ASSIST_ENDPOINT",
		ExtraEnvKeys:   []string{"GOOGLE_GEMINI_BASE_URL", "GOOGLE_GENAI_BASE_URL"},
		DetectEnv:      []string{"CODE_ASSIST_ENDPOINT", "GOOGLE_GEMINI_BASE_URL", "GOOGLE_GENAI_BASE_URL"},
		DefaultURL:     "https://cloudcode-pa.googleapis.com",
		AltDefaultCond: "GEMINI_API_KEY",
		AltDefaultURL:  "https://generativelanguage.googleapis.com",
		CapturePaths:   append(append([]string(nil), geminiCapturePaths...), "LanguageServerService"),
	},
}

// Resolve discovers the real upstream URL by checking the user's environment
// in DetectEnv order. If AltDefaultCond is set and its env var is present,
// AltDefaultURL is used instead of DefaultURL. Falls back to DefaultURL if
// nothing is set. The trailing slash is stripped to prevent double-slash
// issues in the reverse proxy.
//
// If the agent's MSC_UPSTREAM_<AGENT> sentinel is set (by a parent msc process
// running the same agent), it takes priority over DetectEnv to prevent an
// infinite proxy loop where a nested msc reads back the inner proxy's address
// as the upstream. When a DetectEnv var was overridden by a parent msc running
// a DIFFERENT agent (detected via that agent's sentinel, see sentinelForEnv),
// the var holds the parent proxy's address; the sentinel value is used instead
// so the two proxies do not chain (which would inject and capture every
// exchange twice). Sentinels of agents unrelated to the var are ignored.
func (a Agent) Resolve() string {
	if v := os.Getenv(a.sentinelKey()); v != "" {
		return strings.TrimRight(v, "/")
	}
	for _, k := range a.DetectEnv {
		if v := os.Getenv(k); v != "" {
			if s := sentinelForEnv(k); s != "" {
				v = s
			}
			return strings.TrimRight(v, "/")
		}
	}
	if a.AltDefaultCond != "" && os.Getenv(a.AltDefaultCond) != "" {
		return strings.TrimRight(a.AltDefaultURL, "/")
	}
	return strings.TrimRight(a.DefaultURL, "/")
}

// sentinelForEnv returns the real upstream behind env var k when a parent msc
// has poisoned it with a proxy address. BuildEnv sets an agent's EnvKey (and
// ExtraEnvKeys) to the proxy URL together with that agent's sentinel, so a
// present sentinel for an agent that overrides k proves k no longer holds a
// user-configured upstream. Registry order is randomized, so agents are
// scanned in sorted-name order for determinism; nested parents that override
// the same key resolve to the same upstream, so any match is equivalent.
// Returns "" when k is trustworthy.
func sentinelForEnv(k string) string {
	for _, name := range ListSorted() {
		agent := Registry[name]
		if agent.EnvKey != k && !slices.Contains(agent.ExtraEnvKeys, k) {
			continue
		}
		if v := os.Getenv(agent.sentinelKey()); v != "" {
			return v
		}
	}
	return ""
}

// BaseURLSource names where the agent reads its base URL from, as shown by
// `msc list` and `--help`: the env var msc overrides, or the flag it injects.
// For an ArgsRouted agent the flag is derived from ProxyArgs (the arguments
// whose value is the proxy URL) so it cannot drift from what the agent is
// actually launched with.
func (a Agent) BaseURLSource() string {
	if a.ArgsRouted {
		var flags []string
		for i, arg := range a.ProxyArgs {
			if i+1 < len(a.ProxyArgs) && a.ProxyArgs[i+1] == proxyURLPlaceholder {
				flags = append(flags, arg)
			}
		}
		if len(flags) == 0 {
			return a.EnvKey
		}
		return strings.Join(flags, " ") + " flag"
	}
	if len(a.ExtraEnvKeys) > 0 {
		return a.EnvKey + " (also: " + strings.Join(a.ExtraEnvKeys, ", ") + ")"
	}
	return a.EnvKey
}

// EnvOverrides is the child-environment override set for the plain
// base-URL-override path, shared by BuildEnv and `msc --dry-run` so the preview
// matches what the child actually receives.
func (a Agent) EnvOverrides(proxyURL, upstream string) map[string]string {
	replace := map[string]string{
		a.sentinelKey(): upstream,
	}
	// An ArgsRouted agent takes its base URL from ProxyArgs, so the proxy
	// address never has to travel in an env var. Writing its EnvKey anyway
	// points a var msc does not need at the proxy and, worse, poisons it for
	// a nested msc of a sibling agent that does read it.
	if !a.ArgsRouted {
		replace[a.EnvKey] = proxyURL
	}
	for _, k := range a.ExtraEnvKeys {
		replace[k] = proxyURL
	}
	return replace
}

// applyOverrides returns env with every key in replace dropped and re-added
// with the override's value, so an inherited setting never wins over ours.
func applyOverrides(env []string, replace map[string]string) []string {
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

func (a Agent) BuildEnv(proxyURL, upstream string) []string {
	env := os.Environ()

	// Keys to replace in the inherited environment. ExtraEnvKeys are also
	// set to the proxy URL so the agent routes through us regardless of
	// which internal code path it takes (e.g. Gemini OAuth vs API key).
	replace := a.EnvOverrides(proxyURL, upstream)

	return applyOverrides(env, replace)
}

// MITMOverrides is the child-environment override set for TLS-MITM mode, shared
// by BuildMITMEnv (which applies it) and `msc --dry-run` (which previews it, so
// the two cannot drift). An empty caBundlePath means no combined system-roots
// bundle exists (Windows keeps its roots in the OS certificate store), so the
// variables that would replace the child's trust store are left out entirely
// rather than set to msc's CA alone.
func (a Agent) MITMOverrides(proxyURL, upstream, caCertPath, caBundlePath string) map[string]string {
	overrides := map[string]string{
		"HTTPS_PROXY":         proxyURL,
		"https_proxy":         proxyURL,
		"HTTP_PROXY":          proxyURL,
		"http_proxy":          proxyURL,
		"ALL_PROXY":           proxyURL,
		"all_proxy":           proxyURL,
		"NODE_USE_ENV_PROXY":  "1",
		"NODE_EXTRA_CA_CERTS": caCertPath,
		"DENO_CERT":           caCertPath,
		a.sentinelKey():       upstream,
	}
	if caBundlePath != "" {
		overrides["SSL_CERT_FILE"] = caBundlePath
		overrides["REQUESTS_CA_BUNDLE"] = caBundlePath
		overrides["CURL_CA_BUNDLE"] = caBundlePath
	}
	return overrides
}

// BuildMITMEnv constructs the child environment for TLS-MITM mode. Instead of
// overriding the agent's API base-URL env var, it points the standard proxy
// variables (HTTPS_PROXY/HTTP_PROXY/ALL_PROXY) at msc and makes the child trust
// msc's CA so the minted leaf certs verify. This catches agents that ignore a
// base-URL override (codex ChatGPT-mode, grok session auth, agy) and turns msc
// into a transparent HTTPS proxy. caCertPath is the PEM file the local CA
// wrote; caBundlePath is the combined system-roots+CA bundle (see
// writeCombinedCABundle).
func (a Agent) BuildMITMEnv(proxyURL, upstream, caCertPath, caBundlePath string) []string {
	env := os.Environ()

	// Lowercase variants are honored by curl/libcurl; uppercase by Go, Node, and
	// most runtimes. Node reads NODE_EXTRA_CA_CERTS; OpenSSL/Python read
	// SSL_CERT_FILE; requests reads REQUESTS_CA_BUNDLE; curl reads CURL_CA_BUNDLE.
	//
	// NODE_USE_ENV_PROXY=1 is critical: Node's global fetch (undici) — used by
	// the Anthropic/OpenAI SDKs that claude and qwen run on — ignores
	// HTTP(S)_PROXY env unless this is set (Node 24+; harmlessly ignored on older
	// Node). Verified empirically: without it those agents bypass the proxy.
	//
	// CA trust spans every runtime we support (verified with a per-runtime probe):
	// Node/Bun read NODE_EXTRA_CA_CERTS; OpenSSL/curl/Python/Go read SSL_CERT_FILE;
	// curl also CURL_CA_BUNDLE; Python-requests REQUESTS_CA_BUNDLE; Deno DENO_CERT.
	// Rust/reqwest (codex, grok) honors HTTPS_PROXY + the system store (SSL_CERT_FILE).
	//
	// NODE_EXTRA_CA_CERTS and DENO_CERT are additive, so the CA alone suffices.
	// SSL_CERT_FILE, REQUESTS_CA_BUNDLE, and CURL_CA_BUNDLE REPLACE the default
	// root store, so they get the combined bundle: with --mitm-host scoping,
	// out-of-scope hosts are blind-tunneled and present real Web-PKI certs the
	// child must still be able to verify. An empty caBundlePath means no system
	// bundle was found to combine with, so they stay unset rather than replacing
	// the child's roots with msc's CA alone.
	replace := a.MITMOverrides(proxyURL, upstream, caCertPath, caBundlePath)

	return applyOverrides(env, replace)
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
	caBundlePath, err := writeCombinedCABundle(caCertPath)
	if err != nil {
		return err
	}
	argv := append(append([]string{}, a.WaitArgs...), args...)
	return a.runArgv(a.BuildMITMEnv(proxyURL, upstream, caCertPath, caBundlePath), argv)
}

// ListSorted returns all registered agent names in sorted order.
func ListSorted() []string {
	return slices.Sorted(maps.Keys(Registry))
}
