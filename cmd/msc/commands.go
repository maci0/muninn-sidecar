package main

import (
	"crypto/sha256"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/maci0/muninn-sidecar/internal/agents"
	"github.com/maci0/muninn-sidecar/internal/mcpclient"
	"github.com/maci0/muninn-sidecar/internal/mitm"
)

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
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
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
	fmt.Printf("  export NODE_EXTRA_CA_CERTS=%s   # Node\n", certPath)
	fmt.Printf("  export SSL_CERT_FILE=%s         # OpenSSL/Python/Go/curl\n", certPath)
	return 0
}

// cmdList prints supported agents to stdout.
func cmdList(o *opts) int {
	names := agents.ListSorted()

	if o.asJSON {
		type agentInfo struct {
			Name         string   `json:"name"`
			EnvKey       string   `json:"env_key"`
			ExtraEnvKeys []string `json:"extra_env_keys,omitempty"`
			DefaultURL   string   `json:"default_url"`
		}
		list := make([]agentInfo, 0, len(names))
		for _, n := range names {
			a := agents.Registry[n]
			list = append(list, agentInfo{
				Name:         n,
				EnvKey:       a.EnvKey,
				ExtraEnvKeys: a.ExtraEnvKeys,
				DefaultURL:   a.DefaultURL,
			})
		}
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(list); err != nil {
			logerr("failed to encode JSON: %v", err)
			return 1
		}
		return 0
	}

	fmt.Println("Supported agents:")
	for _, n := range names {
		a := agents.Registry[n]
		envKey := a.EnvKey
		if len(a.ExtraEnvKeys) > 0 {
			envKey += " (also: " + strings.Join(a.ExtraEnvKeys, ", ") + ")"
		}
		fmt.Printf("  %-12s  %s -> %s\n", n, envKey, a.DefaultURL)
	}
	return 0
}

// cmdStatus checks MuninnDB connectivity without launching an agent.
func cmdStatus(o *opts) int {
	mcpURL, token, vault := resolveConfig(o)

	err := mcpclient.HealthCheckAt(mcpURL, token)

	if o.asJSON {
		out := map[string]string{
			"mcp_url": mcpURL,
			"vault":   vault,
		}
		if err != nil {
			out["status"] = "unreachable"
			out["error"] = err.Error()
		} else {
			out["status"] = "reachable"
		}
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
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
		fmt.Printf("MuninnDB: %s (reachable)\n", mcpURL)
	} else {
		fmt.Printf("MuninnDB: %s (unreachable: %v)\n", mcpURL, err)
	}
	fmt.Printf("Vault:    %s\n", vault)

	if err != nil {
		return 1
	}
	return 0
}

func printVersion(o *opts) int {
	if o.asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
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

func usage(w io.Writer) {
	names := agents.ListSorted()

	fmt.Fprintf(w, `msc - muninn sidecar %s

Usage: msc [flags] <agent> [agent-args...]

Transparently proxy coding agent API traffic through MuninnDB.
LLM completion traffic is captured and stored as memories.

Flags must come before the agent name. Everything after it is passed
through to the agent unmodified. Use -- to separate if needed.

Agents:
`, version)

	for _, n := range names {
		a := agents.Registry[n]
		envKey := a.EnvKey
		if len(a.ExtraEnvKeys) > 0 {
			envKey += " (also: " + strings.Join(a.ExtraEnvKeys, ", ") + ")"
		}
		fmt.Fprintf(w, "  %-12s  %s -> %s\n", n, envKey, a.DefaultURL)
	}

	fmt.Fprintf(w, `
Commands:
  list           List supported agents (use --json for machine output)
  status         Check MuninnDB connectivity
  ca             Print the TLS-MITM CA cert path + fingerprint (for trusting it elsewhere)
  version        Show version information (use --json for machine output)
  completion     Generate shell completions (bash, zsh, fish)

Flags:
  -h, --help             Show this help
  -v, --version          Show version
  -d, --debug            Enable debug logging (verbose structured output)
  -q, --quiet            Suppress msc's own output
  -n, --dry-run          Show resolved config without launching
  -j, --json             Machine-readable output (for list, status, version, --dry-run)
  -f, --force            Launch even if MuninnDB is unreachable (captures may be lost)
      --no-inject        Disable memory injection (enabled by default)
      --inject-budget N  Max tokens to inject per request (default: 2048)
      --inject-min-score F  Min cosine score to inject a memory, 0-1 (default: 0.6)
      --recall-mode MODE    MuninnDB recall mode: semantic|recent|balanced|deep (default: semantic)
      --no-auto-calibrate   Disable self-tuning of the injection threshold (keep min-score fixed)
      --mitm             Intercept HTTPS via a local CA + CONNECT proxy instead of a
                         base-URL override (for agents that ignore *_BASE_URL); the
                         child is told to trust msc's CA (NODE_EXTRA_CA_CERTS/SSL_CERT_FILE)
      --mitm-host HOST   Scope MITM to HOST (repeatable / comma-separated; implies --mitm).
                         Only the upstream + listed hosts are TLS-terminated; all other
                         hosts are blind-tunneled untouched. Default (no flag): intercept all
      --log-json         Emit logs as JSON (for log aggregation pipelines)
      --vault NAME       MuninnDB vault name (default: current directory name, fallback: sidecar)
      --mcp-url URL      MuninnDB MCP endpoint (default: http://127.0.0.1:8750/mcp)
      --token TOKEN      MuninnDB bearer token (default: ~/.muninn/mcp.token)

Examples:
  msc claude                    Launch Claude Code with API capture
  msc codex                     Launch Codex with API capture
  msc grok                      Launch Grok with API capture
  msc --vault myproject claude  Capture into a specific vault
  msc --dry-run opencode        Preview config without launching
  msc --quiet aider --model x   Suppress msc output, pass args to aider
  msc --json list               Machine-readable agent list
  msc status                    Check if MuninnDB is reachable
  msc -- claude --weird-flag    Use -- to pass flags starting with -
  msc completion zsh > ~/.zsh_functions/_msc  Save zsh completions

Environment (flags take precedence):
  MUNINN_MCP_URL   MuninnDB MCP endpoint
  MUNINN_TOKEN     MuninnDB bearer token
  MSC_VAULT        MuninnDB vault name
`)
}
