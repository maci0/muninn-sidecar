package main

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/maci0/muninn-sidecar/internal/agents"
	"github.com/maci0/muninn-sidecar/internal/config"
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
	injectBudget    int           // max tokens to inject per request (0 = default)
	minScore        float64       // injection cosine threshold (0 = default 0.6)
	recallMode      string        // MuninnDB recall mode (empty = default "semantic")
	groundCmd       string        // answer-grounding rerank via a CLI agent (e.g. "claude -p")
	groundURL       string        // answer-grounding rerank via an OpenAI-compatible URL
	groundModel     string        // grounding model name (for --ground-url)
	groundTopK      int           // candidates to ground per recall (0 = default 3)
	groundTimeout   time.Duration // in-flight grounding-call timeout (0 = default 10s); bounds how long a slow judge can stall a request
	noAutoCalibrate bool          // disable self-tuning of the injection threshold
	mitm            bool          // intercept HTTPS via a local CA + CONNECT proxy instead of a base-URL override
	mitmHosts       []string      // scope MITM to these hosts (+ upstream); empty = intercept all. "*" forces all.
	noRedact        bool          // disable secret redaction of captured content (full-fidelity capture)
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

// flagValue returns the value for a flag that requires one. Written without
// =value, the value is the following argument, so next is the index of that
// argument; written with =value, next is i unchanged.
func flagValue(args []string, i int, key, val string, hasVal bool) (string, int, error) {
	if hasVal {
		return val, i, nil
	}
	if i+1 >= len(args) {
		return "", i, fmt.Errorf("%s requires a value", key)
	}
	return args[i+1], i + 1, nil
}

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
				"-f", "--force", "--no-inject", "--no-auto-calibrate", "--log-json", "--mitm", "--no-redact":
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
		case "--mitm":
			o.mitm = true
			i++
			continue
		case "--no-redact":
			o.noRedact = true
			i++
			continue
		case "--inject-budget", "--ground-topk":
			v, ni, verr := flagValue(args, i, key, val, hasVal)
			if verr != nil {
				return nil, actionNone, verr
			}
			i = ni
			n, err := strconv.Atoi(v)
			if err != nil || n <= 0 {
				return nil, actionNone, fmt.Errorf("%s must be a positive integer", key)
			}
			if key == "--inject-budget" {
				o.injectBudget = n
			} else {
				o.groundTopK = n
			}
			i++
			continue
		case "--inject-min-score":
			v, ni, verr := flagValue(args, i, key, val, hasVal)
			if verr != nil {
				return nil, actionNone, verr
			}
			i = ni
			f, err := strconv.ParseFloat(v, 64)
			// Range test as a positive check so "NaN" is rejected too.
			if err != nil || !(f > 0 && f <= 1) {
				return nil, actionNone, fmt.Errorf("--inject-min-score must be in (0,1]")
			}
			o.minScore = f
			i++
			continue
		case "--mitm-host":
			v, ni, verr := flagValue(args, i, key, val, hasVal)
			if verr != nil {
				return nil, actionNone, verr
			}
			i = ni
			// Comma-separated and/or repeated; scoping implies --mitm.
			for _, h := range strings.Split(v, ",") {
				if h = strings.TrimSpace(h); h != "" {
					o.mitmHosts = append(o.mitmHosts, h)
				}
			}
			o.mitm = true
			i++
			continue
		case "--recall-mode":
			v, ni, verr := flagValue(args, i, key, val, hasVal)
			if verr != nil {
				return nil, actionNone, verr
			}
			i = ni
			switch v {
			case "semantic", "recent", "balanced", "deep":
				o.recallMode = v
			default:
				return nil, actionNone, fmt.Errorf("--recall-mode must be one of: semantic, recent, balanced, deep")
			}
			i++
			continue
		case "--ground-timeout":
			v, ni, verr := flagValue(args, i, key, val, hasVal)
			if verr != nil {
				return nil, actionNone, verr
			}
			i = ni
			d, err := time.ParseDuration(v)
			if err != nil || d <= 0 {
				return nil, actionNone, fmt.Errorf("--ground-timeout must be a positive duration (e.g. 10s)")
			}
			o.groundTimeout = d
			i++
			continue
		case "--vault", "--mcp-url", "--token", "--ground-cmd", "--ground-url", "--ground-model":
			v, ni, verr := flagValue(args, i, key, val, hasVal)
			if verr != nil {
				return nil, actionNone, verr
			}
			i = ni
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

// resolveConfig resolves MuninnDB connection parameters from flags, env, and
// defaults, in that order of precedence.
func resolveConfig(o *opts) (mcpURL, token, vault string) {
	return config.MCPURL(o.mcpURL), config.Token(o.token), config.Vault(o.vault)
}

func defaultMCPURL() string { return config.MCPURL("") }

func defaultToken() string { return config.Token("") }
