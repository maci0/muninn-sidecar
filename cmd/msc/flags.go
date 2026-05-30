package main

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/maci0/muninn-sidecar/internal/agents"
)

// opts holds the parsed command-line options. Flags are parsed before the
// first positional argument (the agent name); everything after the agent
// name is passed through to the child process unmodified.
type opts struct {
	vault           string
	mcpURL          string
	token           string
	debug           bool
	quiet           bool
	dryRun          bool
	asJSON          bool
	logJSON         bool
	force           bool
	noInject        bool
	injectBudget    int     // max tokens to inject per request (0 = default)
	minScore        float64 // injection cosine threshold (0 = default 0.6)
	recallMode      string  // MuninnDB recall mode (empty = default "semantic")
	groundCmd       string  // answer-grounding rerank via a CLI agent (e.g. "claude -p")
	groundURL       string  // answer-grounding rerank via an OpenAI-compatible URL
	groundModel     string  // grounding model name (for --ground-url)
	groundTopK      int     // candidates to ground per recall (0 = default 3)
	noAutoCalibrate bool    // disable self-tuning of the injection threshold
}

// parseAction signals a special action from parseFlags instead of os.Exit.
type parseAction int

const (
	actionNone parseAction = iota
	actionHelp
	actionVersion
)

const (
	exitUsage = 2 // usage/config errors
)

// parseFlags extracts msc's global flags from args and returns the remaining
// positional arguments. Parsing stops at the first non-flag argument (unless
// it is an internal command like 'list' or 'status', in which case flag parsing
// continues). This mimics the behavior of env(1) and similar wrapper tools.
//
// Returns a parseAction if a special flag (--help, --version) was encountered,
// or an error for invalid input. This avoids os.Exit inside the parser,
// keeping run() as the single exit point.
func parseFlags(args []string, o *opts) (remaining []string, action parseAction, err error) {
	i := 0
	isInternalCmd := false

	for i < len(args) {
		arg := args[i]

		if !strings.HasPrefix(arg, "-") {
			// If it's an internal command, record it and continue parsing flags.
			if len(remaining) == 0 && agents.ReservedCommands[arg] {
				isInternalCmd = true
				remaining = append(remaining, arg)
				i++
				continue
			}

			// Positional argument: stop here so the rest goes to the agent unchanged.
			// For internal commands (list, status, etc.) flags are allowed after the
			// command name (e.g. "msc list --json"), so we fall through instead of breaking.
			if !isInternalCmd {
				break
			}
			remaining = append(remaining, arg)
			i++
			continue
		}

		if arg == "--" {
			i++
			break
		}

		// Handle --flag=value syntax.
		key := arg
		var val string
		hasVal := false
		if k, v, ok := strings.Cut(arg, "="); ok {
			key = k
			val = v
			hasVal = true
		}

		// Boolean flags must not accept =value syntax. Reject early so
		// that e.g. --no-inject=false doesn't silently enable no-inject.
		if hasVal {
			switch key {
			case "-h", "--help", "-v", "--version", "-d", "--debug",
				"-q", "--quiet", "-n", "--dry-run", "-j", "--json",
				"-f", "--force", "--no-inject", "--no-auto-calibrate", "--log-json":
				return nil, actionNone, fmt.Errorf("%s does not accept a value", key)
			}
		}

		switch key {
		case "-h", "--help":
			action = actionHelp
			i++
			continue
		case "-v", "--version":
			action = actionVersion
			i++
			continue
		case "-d", "--debug":
			o.debug = true
			i++
			continue
		case "-q", "--quiet":
			o.quiet = true
			i++
			continue
		case "-n", "--dry-run":
			o.dryRun = true
			i++
			continue
		case "-j", "--json":
			o.asJSON = true
			i++
			continue
		case "-f", "--force":
			o.force = true
			i++
			continue
		case "--no-inject":
			o.noInject = true
			i++
			continue
		case "--no-auto-calibrate":
			o.noAutoCalibrate = true
			i++
			continue
		case "--log-json":
			o.logJSON = true
			i++
			continue
		case "--inject-budget":
			v := val
			if !hasVal {
				i++
				if i >= len(args) {
					return nil, actionNone, fmt.Errorf("%s requires a value", key)
				}
				v = args[i]
			}
			n, err := strconv.Atoi(v)
			if err != nil || n <= 0 {
				return nil, actionNone, fmt.Errorf("--inject-budget must be a positive integer")
			}
			o.injectBudget = n
			i++
			continue
		case "--inject-min-score":
			v := val
			if !hasVal {
				i++
				if i >= len(args) {
					return nil, actionNone, fmt.Errorf("%s requires a value", key)
				}
				v = args[i]
			}
			f, err := strconv.ParseFloat(v, 64)
			if err != nil || f <= 0 || f > 1 {
				return nil, actionNone, fmt.Errorf("--inject-min-score must be in (0,1]")
			}
			o.minScore = f
			i++
			continue
		case "--recall-mode":
			v := val
			if !hasVal {
				i++
				if i >= len(args) {
					return nil, actionNone, fmt.Errorf("%s requires a value", key)
				}
				v = args[i]
			}
			switch v {
			case "semantic", "recent", "balanced", "deep":
				o.recallMode = v
			default:
				return nil, actionNone, fmt.Errorf("--recall-mode must be one of: semantic, recent, balanced, deep")
			}
			i++
			continue
		case "--ground-topk":
			v := val
			if !hasVal {
				i++
				if i >= len(args) {
					return nil, actionNone, fmt.Errorf("%s requires a value", key)
				}
				v = args[i]
			}
			n, err := strconv.Atoi(v)
			if err != nil || n <= 0 {
				return nil, actionNone, fmt.Errorf("--ground-topk must be a positive integer")
			}
			o.groundTopK = n
			i++
			continue
		case "--vault", "--mcp-url", "--token", "--ground-cmd", "--ground-url", "--ground-model":
			v := val
			if !hasVal {
				i++
				if i >= len(args) {
					return nil, actionNone, fmt.Errorf("%s requires a value", key)
				}
				v = args[i]
			}
			if v == "" {
				return nil, actionNone, fmt.Errorf("%s requires a non-empty value", key)
			}
			switch key {
			case "--vault":
				o.vault = v
			case "--mcp-url":
				o.mcpURL = v
			case "--token":
				o.token = v
			case "--ground-cmd":
				o.groundCmd = v
			case "--ground-url":
				o.groundURL = v
			case "--ground-model":
				o.groundModel = v
			}
			i++
			continue
		default:
			return nil, actionNone, fmt.Errorf("unknown flag: %s", key)
		}
	}

	if i < len(args) {
		remaining = append(remaining, args[i:]...)
	}

	return remaining, action, nil
}

// resolveConfig resolves MuninnDB connection parameters from flags, env, and defaults.
func resolveConfig(o *opts) (mcpURL, token, vault string) {
	mcpURL = o.mcpURL
	if mcpURL == "" {
		mcpURL = defaultMCPURL()
	}
	token = o.token
	if token == "" {
		token = defaultToken()
	}
	vault = o.vault
	if vault == "" {
		if v := os.Getenv("MSC_VAULT"); v != "" {
			vault = v
		} else {
			// Default to the name of the current working directory.
			cwd, err := os.Getwd()
			if err == nil {
				if base := filepath.Base(cwd); base != "." && base != "/" {
					vault = base
				}
			}
			if vault == "" {
				vault = "sidecar"
			}
		}
	}
	return
}

// defaultMCPURL returns the MuninnDB MCP endpoint from MUNINN_MCP_URL,
// falling back to the standard local address.
func defaultMCPURL() string {
	if u := os.Getenv("MUNINN_MCP_URL"); u != "" {
		return u
	}
	return "http://127.0.0.1:8750/mcp"
}

// defaultToken reads the MuninnDB bearer token from MUNINN_TOKEN or the
// well-known file at ~/.muninn/mcp.token (the same file MuninnDB writes
// on first start).
func defaultToken() string {
	if t := os.Getenv("MUNINN_TOKEN"); t != "" {
		return t
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	tokenPath := filepath.Join(home, ".muninn", "mcp.token")
	data, err := os.ReadFile(tokenPath)
	if err != nil {
		return ""
	}
	// Warn if the token file is readable by group or other users.
	if info, err := os.Stat(tokenPath); err == nil {
		if info.Mode().Perm()&0o077 != 0 {
			slog.Warn("token file has overly permissive permissions",
				"path", tokenPath, "fix", "chmod 600 "+tokenPath, "mode", info.Mode().Perm())
		}
	}
	return strings.TrimSpace(string(data))
}
