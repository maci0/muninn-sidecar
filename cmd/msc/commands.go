package main

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/maci0/muninn-sidecar/internal/agents"
	"github.com/maci0/muninn-sidecar/internal/config"
	"github.com/maci0/muninn-sidecar/internal/grounding"
	"github.com/maci0/muninn-sidecar/internal/inject"
	"github.com/maci0/muninn-sidecar/internal/mcpclient"
	"github.com/maci0/muninn-sidecar/internal/mitm"
	"github.com/maci0/muninn-sidecar/internal/redact"
)

// vaultStats queries MuninnDB's status tool for a vault's memory count and
// health. Best-effort: a non-nil error means the stats are simply unavailable
// (older server, missing tool) and the caller should omit them, not fail.
func vaultStats(mcpURL, token, vault string) (total int, health string, err error) {
	// One-shot client: the response is read in full, so the connection would
	// otherwise idle on a transport nothing can close for the rest of the run.
	c := mcpclient.New(mcpURL, token, 3*time.Second)
	defer c.Close()
	body, err := c.Call(context.Background(), "muninn_status", map[string]any{"vault": vault})
	if err != nil {
		return 0, "", err
	}
	var env struct {
		Result struct {
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
		} `json:"result"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		return 0, "", err
	}
	for _, ct := range env.Result.Content {
		if ct.Type != "text" {
			continue
		}
		var st struct {
			TotalMemories int    `json:"total_memories"`
			Health        string `json:"health"`
		}
		if json.Unmarshal([]byte(ct.Text), &st) == nil && st.Health != "" {
			return st.TotalMemories, st.Health, nil
		}
	}
	return 0, "", fmt.Errorf("no status content in response")
}

// cmdCA loads (or creates) the local TLS-MITM certificate authority and prints
// its certificate path and SHA-256 fingerprint, so users can trust it in tools
// msc doesn't launch itself (browsers, system store, custom HTTP clients) for
// the transparent-HTTPS-proxy use case. With -j it emits JSON incl. the PEM.
func cmdCA(o *opts) int {
	dir, err := mitmCADir()
	if err != nil {
		logerr("%v", err)
		return 1
	}
	ca, err := mitm.LoadOrCreateCA(dir)
	if err != nil {
		logerr("failed to load MITM CA: %v", err)
		return 1
	}
	certPath := filepath.Join(dir, "ca-cert.pem")
	certPEM := ca.CertPEM()

	fingerprint := "(unparseable)"
	if block, _ := pem.Decode(certPEM); block != nil {
		sum := sha256.Sum256(block.Bytes)
		parts := make([]string, len(sum))
		for i, b := range sum {
			parts[i] = fmt.Sprintf("%02X", b)
		}
		fingerprint = strings.Join(parts, ":")
	}

	if o.asJSON {
		enc := jsonEncoder(os.Stdout)
		if err := enc.Encode(map[string]string{
			"path":        certPath,
			"sha256":      fingerprint,
			"certificate": string(certPEM),
		}); err != nil {
			logerr("failed to encode JSON: %v", err)
			return 1
		}
		return 0
	}

	fmt.Printf("MITM CA certificate: %s\n", certPath)
	fmt.Printf("SHA-256:             %s\n", fingerprint)
	fmt.Println("\nmsc trusts this CA in agents it launches with --mitm automatically.")
	fmt.Println("To trust it elsewhere (browser, system store, or a custom HTTPS client):")
	for _, line := range caTrustHints(runtime.GOOS, certPath, agents.HasSystemCABundle()) {
		fmt.Println(line)
	}
	return 0
}

// caTrustHints returns the lines telling the user how to trust the CA outside
// an msc-launched agent. goos is a parameter so both shells are covered by
// tests on the one platform they run on: `export` is not a cmd or PowerShell
// command, and the cert path lives under %AppData%, which routinely contains a
// space and so needs quoting there.
// caTrustHints returns the lines telling the user how to trust the CA outside
// an msc-launched agent. goos is a parameter so both shells are covered by
// tests on the one platform they run on: `export` is not a cmd or PowerShell
// command, and the cert path lives under %AppData%, which routinely contains a
// space and so needs quoting there. hasBundle is the system-roots probe's own
// answer, not the OS name: the combined bundle is written when a PEM root
// bundle is found, and Windows and macOS are both platforms where the roots
// live in an OS store instead of a file, so naming a bundle there would point
// at a file msc never writes.
func caTrustHints(goos, certPath string, hasBundle bool) []string {
	nodeCA := "export NODE_EXTRA_CA_CERTS=" + certPath
	if goos == "windows" {
		nodeCA = `$env:NODE_EXTRA_CA_CERTS = "` + certPath + `"`
	}
	hints := []string{
		"  " + nodeCA + "   # Node (adds to the default roots)",
		"  # OpenSSL/Python/Go/curl/Deno: SSL_CERT_FILE, REQUESTS_CA_BUNDLE, CURL_CA_BUNDLE and",
		"  # DENO_CERT replace the default roots, so they need a bundle of the system roots plus",
		"  # this CA. msc builds that at " + agents.CABundlePath(certPath) + " when it launches an agent with --mitm.",
	}
	if !hasBundle {
		hints[1] = "  # No system PEM root bundle was found (Windows and macOS keep their trusted roots in an"
		hints[2] = "  # OS store, not a PEM file), so there is no system bundle for msc to prepend this CA to."
		hints[3] = "  # Set NODE_EXTRA_CA_CERTS above, or import the certificate into the system trust store."
	}
	return hints
}

// cmdList prints supported agents to stdout.
func cmdList(o *opts) int {
	names := agents.ListSorted()

	if o.asJSON {
		type agentInfo struct {
			Name         string   `json:"name"`
			EnvKey       string   `json:"env_key"`
			BaseURL      string   `json:"base_url_source"`
			ExtraEnvKeys []string `json:"extra_env_keys,omitempty"`
			DefaultURL   string   `json:"default_url"`
		}
		list := make([]agentInfo, 0, len(names))
		for _, n := range names {
			a := agents.Registry[n]
			list = append(list, agentInfo{
				Name:         n,
				EnvKey:       a.EnvKey,
				BaseURL:      a.BaseURLSource(),
				ExtraEnvKeys: a.ExtraEnvKeys,
				DefaultURL:   a.DefaultURL,
			})
		}
		enc := jsonEncoder(os.Stdout)
		if err := enc.Encode(list); err != nil {
			logerr("failed to encode JSON: %v", err)
			return 1
		}
		return 0
	}

	fmt.Println("Supported agents:")
	for _, n := range names {
		a := agents.Registry[n]
		fmt.Printf("  %-12s  %s -> %s\n", n, a.BaseURLSource(), a.DefaultURL)
	}
	return 0
}

// cmdStatus checks MuninnDB connectivity without launching an agent.
func cmdStatus(o *opts) int {
	mcpURL, token, vault := resolveConfig(o)

	// An endpoint msc could never dial is a typo, not an outage. Validating it
	// here (as the agent path does) keeps "I mistyped --mcp-url" distinguishable
	// from "MuninnDB is down" by exit code, and stops a malformed URL being
	// reported as a health check result.
	if err := config.ValidateURL("MuninnDB URL", mcpURL); err != nil {
		logerr("%v", err)
		return exitUsage
	}

	err := mcpclient.HealthCheckAt(mcpURL, token)

	// Best-effort vault stats (memory count + health) when reachable — answers
	// "is my vault populated?", the common cause of "nothing gets injected".
	var (
		haveStats bool
		memCount  int
		vaultHP   string
	)
	if err == nil {
		if total, hp, serr := vaultStats(mcpURL, token, vault); serr == nil {
			haveStats, memCount, vaultHP = true, total, hp
		}
	}

	if o.asJSON {
		out := map[string]any{
			"mcp_url": redact.URL(mcpURL),
			"vault":   vault,
		}
		if err != nil {
			out["status"] = "unreachable"
			out["error"] = err.Error()
		} else {
			out["status"] = "reachable"
		}
		if haveStats {
			out["memories"] = memCount
			out["vault_health"] = vaultHP
		}
		enc := jsonEncoder(os.Stdout)
		if encErr := enc.Encode(out); encErr != nil {
			logerr("failed to encode JSON: %v", encErr)
			return 1
		}
		if err != nil {
			return 1
		}
		return 0
	}

	if err == nil {
		fmt.Printf("MuninnDB: %s (reachable)\n", redact.URL(mcpURL))
	} else {
		fmt.Printf("MuninnDB: %s (unreachable: %v)\n", redact.URL(mcpURL), err)
	}
	fmt.Printf("Vault:    %s\n", vault)
	if haveStats {
		fmt.Printf("Memories: %d (health: %s)\n", memCount, vaultHP)
		if memCount == 0 {
			fmt.Println("          vault is empty — nothing to inject until exchanges are captured")
		}
	}

	if err != nil {
		return 1
	}
	return 0
}

func printVersion(o *opts) int {
	if o.asJSON {
		enc := jsonEncoder(os.Stdout)
		if err := enc.Encode(map[string]string{
			"version": version,
			"commit":  commit,
			"date":    date,
			"go":      runtime.Version(),
			"os":      runtime.GOOS,
			"arch":    runtime.GOARCH,
		}); err != nil {
			logerr("failed to encode JSON: %v", err)
			return 1
		}
		return 0
	}
	fmt.Printf("msc %s (%s %s) %s %s/%s\n",
		version, commit, date, runtime.Version(), runtime.GOOS, runtime.GOARCH)
	return 0
}

// cmdHelp prints the global usage, or the help for a single topic. An unknown
// topic is a usage error rather than a silent fallback to the global help, so
// `msc help statuss` does not look like it worked.
func cmdHelp(args []string) int {
	if len(args) > 1 {
		logerr("help takes at most one topic: msc help [command|agent]")
		return exitUsage
	}
	if len(args) == 0 {
		usage(os.Stdout)
		return 0
	}

	topic := args[0]
	if text, ok := commandUsage(topic); ok {
		fmt.Fprint(os.Stdout, text)
		return 0
	}
	if a, ok := agents.Registry[topic]; ok {
		envKey := a.EnvKey
		if len(a.ExtraEnvKeys) > 0 {
			envKey += " (also: " + strings.Join(a.ExtraEnvKeys, ", ") + ")"
		}
		fmt.Fprintf(os.Stdout, "msc %s - wrap the %s agent, capturing its API traffic through MuninnDB\n\n", topic, topic)
		fmt.Fprintf(os.Stdout, "  Base URL override: %s\n", envKey)
		fmt.Fprintf(os.Stdout, "  Default upstream:   %s\n", a.DefaultURL)
		fmt.Fprintf(os.Stdout, "  Launch:             msc [flags] %s [agent-args...]\n", topic)
		fmt.Fprintf(os.Stdout, "  Preview:            msc --dry-run %s\n\n", topic)
		fmt.Fprint(os.Stdout, "  Run 'msc --help' for the flags msc accepts before the agent name.\n")
		return 0
	}

	topics := helpTopics()
	if s := closestMatch(topic, topics); s != "" {
		logerr("unknown help topic: %s. Did you mean %q?", topic, s)
	} else {
		logerr("unknown help topic: %s", topic)
	}
	logf("topics: %s", strings.Join(topics, ", "))
	return exitUsage
}

// helpTopics lists every topic 'msc help' accepts, in the order it prints them.
func helpTopics() []string {
	topics := []string{"help", "list", "status", "ca", "version", "completion"}
	return append(topics, agents.ListSorted()...)
}

// commandUsage returns the help text for one of msc's own subcommands. The
// global usage names them but cannot document the flags that only apply to one
// of them, so 'msc <cmd> --help' and 'msc help <cmd>' print this instead.
func commandUsage(cmd string) (string, bool) {
	switch cmd {
	case "list":
		return `Usage: msc list [--json]

List the agents msc can wrap, with the environment variable each one reads for
its API base URL and the upstream that variable defaults to.

Flags:
  -j, --json   Emit a JSON array of {name, env_key, base_url_source,
              extra_env_keys?, default_url} instead of the table

Examples:
  msc list           Human-readable table
  msc --json list    Machine-readable output

Exit codes: 0 on success.
`, true
	case "status":
		return fmt.Sprintf(`Usage: msc status [--json]

Check whether MuninnDB is reachable and report the resolved vault, without
launching an agent. When MuninnDB answers, the vault's memory count and health
are included; an empty vault is called out, since nothing can be injected until
exchanges are captured.

Flags:
  -j, --json        Emit {mcp_url, vault, status, error?, memories?, vault_health?}
      --mcp-url URL MuninnDB MCP endpoint (default: %s)
      --token TOKEN MuninnDB bearer token (default: $MUNINN_TOKEN_FILE, else ~/.muninn/mcp.token)
      --vault NAME  Vault to report on (default: current directory name)

Examples:
  msc status                Human-readable connectivity report
  msc --json status         Machine-readable output
  msc status --vault proj   Check a specific vault

Exit codes: 0 when MuninnDB is reachable, 1 when it is not, 2 for an unusable
--mcp-url.
`, config.DefaultMCPURL), true
	case "ca":
		return `Usage: msc ca [--json]

Create (if needed) and print msc's TLS-MITM certificate authority: the path to
its certificate and its SHA-256 fingerprint. msc trusts it automatically in the
agents it launches with --mitm; run this to trust it in anything else (browser,
system store, custom HTTPS client), then follow the printed instructions.

The CA lives in <config dir>/muninn-sidecar/mitm (0700) and persists across runs.

Flags:
  -j, --json   Emit {path, sha256, certificate} instead of the human-readable report

Examples:
  msc ca                     Print the path, fingerprint, and trust instructions
  msc --json ca | jq -r .path  Resolve the cert path in a script

Exit codes: 0 on success, 1 if the CA cannot be created or read.
`, true
	case "version":
		return `Usage: msc version [--json]   (also: msc -v)

Print msc's version, the commit and build date it was built from, and the Go
version and platform it targets.

Flags:
  -j, --json   Emit {version, commit, date, go, os, arch} instead of one line

Exit codes: 0 on success.
`, true
	case "completion":
		return `Usage: msc completion <bash|zsh|fish>

Write a shell completion script to stdout.

Arguments:
  bash|zsh|fish   The shell to generate completions for

Examples:
  msc completion zsh > ~/.zsh_functions/_msc
  source <(msc completion bash)

Exit codes: 0 on success, 2 for a missing, extra, or unsupported shell argument.
`, true
	case "help":
		return `Usage: msc help [command|agent]

Print the global usage, or the help for one command or agent.

Examples:
  msc help          Global usage
  msc help status   Help for the status command
  msc help claude   How msc wraps the claude agent

Exit codes: 0 on success, 2 for an unknown topic.
`, true
	}
	return "", false
}

func usage(w io.Writer) {
	names := agents.ListSorted()

	fmt.Fprintf(w, `msc - muninn sidecar %s

Usage: msc [flags] <agent> [agent-args...]

Transparently proxy coding agent API traffic through MuninnDB.
LLM completion traffic is captured and stored as memories.

Flags must come before the agent name. Everything after it is passed
through to the agent unmodified. Use -- to separate if needed. The
commands below are the exception: their flags may follow the name
(msc list --json works as well as msc --json list).

Agents:
`, version)

	for _, n := range names {
		a := agents.Registry[n]
		fmt.Fprintf(w, "  %-12s  %s -> %s\n", n, a.BaseURLSource(), a.DefaultURL)
	}

	fmt.Fprintf(w, `
Commands (each accepts the flags below, and 'msc help <command>' for its own help):
  list           List supported agents (use --json for machine output)
  status         Check MuninnDB connectivity
  ca             Print the TLS-MITM CA cert path + fingerprint (for trusting it elsewhere)
  version        Show version information (use --json for machine output)
  completion     Generate shell completions (bash, zsh, fish)
  help [topic]   Show this help, or the help for one command or agent

Flags:
  -h, --help             Show this help (per command: msc list --help)
  -v, --version          Show version
  -d, --debug            Enable debug logging (verbose structured output)
  -q, --quiet            Suppress msc's own output
  -n, --dry-run          Show resolved config without launching
  -j, --json             Machine-readable output (for list, status, ca,
                         version, --dry-run)
  -f, --force            Launch even if MuninnDB is unreachable (captures may
                         be lost)
      --no-inject        Disable memory injection (enabled by default)
      --no-redact        Disable secret and personal-data redaction of captured
                         content (full-fidelity; trusted environments only)
      --no-auto-calibrate
                         Disable self-tuning of the injection threshold (keep
                         min-score fixed)
      --log-json         Emit logs as JSON (for log aggregation pipelines)
      --mitm             Intercept HTTPS via a local CA + CONNECT proxy instead
                         of a base-URL override (for agents that ignore
                         *_BASE_URL); the child is told to trust msc's CA
                         (NODE_EXTRA_CA_CERTS/SSL_CERT_FILE)
      --mitm-host HOST   Scope MITM to HOST (repeatable / comma-separated;
                         implies --mitm). Only the upstream + listed hosts are
                         TLS-terminated; all other hosts are blind-tunneled
                         untouched. Use "*" to force intercept-all. Default (no
                         flag): intercept all
      --inject-budget N  Max tokens to inject per request (default: 2048)
      --inject-min-score F
                         Min cosine score to inject a memory, in (0,1]
                         (default: 0.6)
      --recall-mode MODE
                         MuninnDB recall mode: semantic|recent|balanced|deep
                         (default: semantic)
      --ground-url URL   Opt-in answer-grounding rerank via an OpenAI-compatible
                         model; drops recalled passages the model says don't
                         answer the query
      --ground-cmd CMD   Answer-grounding rerank via a CLI agent (e.g.
                         "claude -p"); offline. Takes precedence over
                         --ground-url (quote a path containing spaces:
                         "C:\Program Files\...\claude.exe")
      --ground-model NAME
                         Grounding model for --ground-url (default:
                         %s)
      --ground-topk K    Candidates to ground per recall (default: %d)
      --ground-timeout D In-flight grounding-call timeout (default: %s); fails
                         open to the gate. --ground-model, --ground-topk and
                         --ground-timeout need --ground-url or --ground-cmd;
                         without one they would be silently ignored
      --vault NAME       MuninnDB vault name (default: current directory name,
                         fallback: %s)
      --mcp-url URL      MuninnDB MCP endpoint (default:
                         %s)
      --token TOKEN      MuninnDB bearer token (default: $MUNINN_TOKEN_FILE, else ~/.muninn/mcp.token)

Examples:
  msc claude                    Launch Claude Code with API capture
  msc codex                     Launch Codex with API capture
  msc grok                      Launch Grok with API capture
  msc --vault myproject claude  Capture into a specific vault
  msc --dry-run opencode        Preview config without launching
  msc --quiet aider --model x   Suppress msc output, pass args to aider
  msc --json list               Machine-readable agent list
  msc status                    Check if MuninnDB is reachable
  msc help status               Help for one command
  msc help claude               How msc wraps one agent
  msc -- claude --weird-flag    Use -- to pass flags starting with -
  msc completion zsh > ~/.zsh_functions/_msc  Save zsh completions

Exit codes:
  0        success
  1        MuninnDB unreachable, or another runtime failure
  2        usage error: unknown command or flag, bad flag value
  127      the agent binary is not in PATH
  128+N    the agent was killed by signal N (130 = SIGINT, 143 = SIGTERM)
  other    the agent's own exit code, when it exited on its own

Environment (flags take precedence):
  MUNINN_MCP_URL   MuninnDB MCP endpoint
  MUNINN_TOKEN     MuninnDB bearer token
  MUNINN_TOKEN_FILE
                   Token file read when MUNINN_TOKEN is unset (default ~/.muninn/mcp.token)
  MSC_VAULT        MuninnDB vault name
  OPENAI_API_KEY   Bearer token for --ground-url (required if that endpoint needs auth)
  MSC_WS_DEBUG     Set to log WebSocket frame types/sizes (debug aid; 0, false, off, no leave it off)
`, grounding.DefaultModel, inject.DefaultGroundTopK, grounding.DefaultTimeout,
		config.DefaultVault, config.DefaultMCPURL)
}
