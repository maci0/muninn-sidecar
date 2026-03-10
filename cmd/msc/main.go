package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"os/signal"
	"runtime"
	"strings"
	"syscall"
	"time"

	"github.com/maci0/muninn-sidecar/internal/agents"
	"github.com/maci0/muninn-sidecar/internal/inject"
	"github.com/maci0/muninn-sidecar/internal/proxy"
	"github.com/maci0/muninn-sidecar/internal/stats"
	"github.com/maci0/muninn-sidecar/internal/store"
)

// Build-time variables, set via -ldflags:
//
//	go build -ldflags "-X main.version=1.0.0 -X main.commit=abc1234 -X main.date=2026-03-10"
var (
	version = "0.1.0"
	commit  = "dev"
	date    = "unknown"
)

// opts holds the parsed command-line options. Flags are parsed before the
// first positional argument (the agent name); everything after the agent
// name is passed through to the child process unmodified.
type opts struct {
	vault    string
	mcpURL   string
	token    string
	debug    bool
	quiet    bool
	dryRun   bool
	asJSON   bool
	force    bool
	noInject bool
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

func main() {
	os.Exit(run())
}

// run is the real entry point; returns the exit code.
func run() int {
	if len(os.Args) < 2 {
		logerr("missing command. Run 'msc --help' for usage.")
		return exitUsage
	}

	// Parse global flags, stopping at the first positional argument.
	o := &opts{}
	remaining, action, err := parseFlags(os.Args[1:], o)
	if err != nil {
		logerr("%v", err)
		return exitUsage
	}

	switch action {
	case actionHelp:
		usage(os.Stdout)
		return 0
	case actionVersion:
		printVersion(o)
		return 0
	}

	if len(remaining) == 0 {
		logerr("missing command. Run 'msc --help' for usage.")
		return exitUsage
	}

	cmd := remaining[0]
	agentArgs := remaining[1:]

	switch cmd {
	case "help":
		usage(os.Stdout)
		return 0
	case "version":
		printVersion(o)
		return 0
	case "list":
		return cmdList(o)
	case "status":
		return cmdStatus(o)
	case "completion":
	if len(agentArgs) == 0 {
		logerr("usage: msc completion <bash|zsh|fish>")
		logerr("  e.g. source <(msc completion zsh)")
		return exitUsage
	}
	return cmdCompletion(agentArgs[0])
	}

	// Look up agent.
	agent, ok := agents.Registry[cmd]
	if !ok {
		names := agents.ListSorted()
		allNames := append(names, "list", "status", "version", "help", "completion")
		if suggestion := closestMatch(cmd, allNames); suggestion != "" {
			logerr("unknown command: %s. Did you mean %q?", cmd, suggestion)
		} else {
			logerr("unknown command: %s", cmd)
		}
		logf("known agents: %s", strings.Join(names, ", "))
		return exitUsage
	}

	// Configure logging. Default to WARN so normal usage is clean; the
	// user-friendly msc: prefixed messages cover the INFO case. --debug
	// enables DEBUG for full structured logging.
	level := slog.LevelWarn
	if o.debug {
		level = slog.LevelDebug
	}
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level})))

	// Resolve MuninnDB connection (flags > env > defaults).
	mcpURL, token, vault := resolveConfig(o)

	sessionStats := &stats.Stats{}
	muninn := store.New(mcpURL, token, vault, sessionStats)

	// Health check: verify MuninnDB is reachable before launching the agent.
	// The whole point of msc is to capture traffic — silently dropping captures
	// defeats the purpose. --force skips this check. healthErr is reused
	// below in --dry-run output.
	var healthErr error
	if !o.force {
		healthErr = muninn.HealthCheck()
		if healthErr != nil && !o.dryRun {
			logerr("MuninnDB at %s is unreachable: %v", mcpURL, healthErr)
			logf("captures will be lost. Use --force to launch anyway.")
			return 1
		}
	}

	// Resolve upstream.
	upstream := agent.Resolve()
	slog.Debug("resolved upstream", "agent", cmd, "upstream", upstream)

	// --dry-run: show what would happen without launching anything.
	if o.dryRun {
		binary, _ := exec.LookPath(agent.Command)
		if binary == "" {
			binary = "(not found in PATH)"
		}
		fmt.Fprintf(os.Stdout, "Agent:    %s\n", cmd)
		fmt.Fprintf(os.Stdout, "Binary:   %s\n", binary)
		fmt.Fprintf(os.Stdout, "Upstream: %s\n", upstream)
		fmt.Fprintf(os.Stdout, "Env:      %s=http://127.0.0.1:<port>\n", agent.EnvKey)
		for _, k := range agent.ExtraEnvKeys {
			fmt.Fprintf(os.Stdout, "          %s=http://127.0.0.1:<port>\n", k)
		}
		fmt.Fprintf(os.Stdout, "Vault:    %s\n", vault)
		fmt.Fprintf(os.Stdout, "MuninnDB: %s", mcpURL)
		if healthErr == nil {
			fmt.Fprintln(os.Stdout, " (reachable)")
		} else {
			fmt.Fprintln(os.Stdout, " (unreachable)")
		}
		if !o.noInject {
			fmt.Fprintf(os.Stdout, "Inject:   enabled (vault=%s)\n", vault)
		} else {
			fmt.Fprintln(os.Stdout, "Inject:   disabled")
		}
		return 0
	}

	// Create injector unless --no-inject is set.
	var injector *inject.Injector
	if !o.noInject {
		injector = inject.New(inject.Config{
			MCPURL: mcpURL,
			Token:  token,
			Vault:  vault,
			Stats:  sessionStats,
		})
	}

	// Start proxy on random port.
	p, err := proxy.New(proxy.Config{
		ListenAddr:   "127.0.0.1:0",
		Upstream:     upstream,
		AgentName:    cmd,
		Store:        muninn,
		CapturePaths: agent.CapturePaths,
		ExcludePaths: agent.ExcludePaths,
		Injector:     injector,
	})
	if err != nil {
		logerr("failed to create proxy: %v", err)
		return 1
	}

	addr, err := p.Start()
	if err != nil {
		logerr("failed to start proxy: %v", err)
		return 1
	}

	proxyURL := fmt.Sprintf("http://%s", addr)
	if !o.quiet {
		logf("proxying %s traffic via %s -> %s", cmd, proxyURL, upstream)
		logf("dumping to muninn vault=%q", vault)
	}

	// Trap signals for graceful shutdown: stop the proxy, drain pending
	// captures so nothing is lost, then exit with the child's code.
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	// Launch the agent in a goroutine so we can select on both the agent
	// exiting and a signal arriving.
	type exitResult struct {
		err  error
		code int
	}
	doneCh := make(chan exitResult, 1)
	go func() {
		err := agent.Exec(proxyURL, upstream, agentArgs)
		code := 0
		if err != nil {
			code = 1
			var exitErr *exec.ExitError
			if errors.As(err, &exitErr) {
				code = exitErr.ExitCode()
			}
		}
		doneCh <- exitResult{err: err, code: code}
	}()

	var result exitResult
	select {
	case result = <-doneCh:
		// Agent exited on its own.
	case sig := <-sigCh:
		slog.Debug("received signal", "signal", sig)
		// Wait briefly for the agent to also receive the signal and exit.
		select {
		case result = <-doneCh:
		case <-time.After(3 * time.Second):
			result = exitResult{code: 130} // conventional SIGINT exit code
		}
	}

	// Graceful shutdown: stop proxy, flush pending captures.
	shutCtx, shutCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer shutCancel()
	p.Shutdown(shutCtx)
	muninn.Drain()

	if !o.quiet {
		if summary := sessionStats.Summary(); summary != "" {
			for _, line := range strings.Split(summary, "\n") {
				logf("%s", line)
			}
		}
	}

	// Surface agent errors so the user knows why msc exited.
	if result.err != nil {
		var pathErr *exec.Error
		if errors.As(result.err, &pathErr) {
			logerr("%s not found in PATH", cmd)
		} else {
			slog.Debug("agent exited with error", "err", result.err)
		}
	}
	return result.code
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

			// If it's not an internal command, OR we already saw a non-flag,
			// we stop parsing flags ONLY IF we aren't currently processing flags for an internal command.
			if !isInternalCmd {
				break
			} else {
				remaining = append(remaining, arg)
				i++
				continue
			}
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

		switch key {
		case "-h", "--help":
			return nil, actionHelp, nil
		case "-v", "--version":
			return nil, actionVersion, nil
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
		case "--vault", "--mcp-url", "--token":
			v := val
			if !hasVal {
				i++
				if i >= len(args) {
					return nil, actionNone, fmt.Errorf("%s requires a value", key)
				}
				v = args[i]
			}
			switch key {
			case "--vault":
				o.vault = v
			case "--mcp-url":
				o.mcpURL = v
			case "--token":
				o.token = v
			}
			i++
			continue
		default:
			return nil, actionNone, fmt.Errorf("unknown flag: %s\nrun 'msc --help' for usage", arg)
		}
	}

	if i < len(args) {
		remaining = append(remaining, args[i:]...)
	}

	return remaining, actionNone, nil
}

// resolveConfig resolves MuninnDB connection parameters from flags, env, and defaults.
func resolveConfig(o *opts) (mcpURL, token, vault string) {
	mcpURL = o.mcpURL
	if mcpURL == "" {
		mcpURL = store.DefaultMCPURL()
	}
	token = o.token
	if token == "" {
		token = store.DefaultToken()
	}
	vault = o.vault
	if vault == "" {
		if v := os.Getenv("MSC_VAULT"); v != "" {
			vault = v
		} else {
			// Default to the name of the current working directory.
			cwd, err := os.Getwd()
			if err == nil {
				// Get the last element of the path.
				parts := strings.Split(strings.TrimRight(cwd, "/\\"), "/")
				if len(parts) > 0 && parts[len(parts)-1] != "" {
					vault = parts[len(parts)-1]
				}
			}
			if vault == "" {
				vault = "sidecar"
			}
		}
	}
	return
}

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
		enc.Encode(list)
		return 0
	}

	fmt.Println("Supported agents:")
	for _, n := range names {
		a := agents.Registry[n]
		fmt.Printf("  %-12s  %s -> %s\n", n, a.EnvKey, a.DefaultURL)
	}
	return 0
}

// cmdStatus checks MuninnDB connectivity without launching an agent.
func cmdStatus(o *opts) int {
	mcpURL, token, vault := resolveConfig(o)

	s := store.New(mcpURL, token, vault, nil)
	err := s.HealthCheck()
	s.Drain()

	if o.asJSON {
		status := "reachable"
		if err != nil {
			status = "unreachable"
		}
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		enc.Encode(map[string]string{
			"mcp_url": mcpURL,
			"vault":   vault,
			"status":  status,
		})
		if err != nil {
			return 1
		}
		return 0
	}

	fmt.Printf("MuninnDB: %s", mcpURL)
	if err == nil {
		fmt.Println(" (reachable)")
	} else {
		fmt.Println(" (unreachable)")
	}
	fmt.Printf("Vault:    %s\n", vault)

	if err != nil {
		return 1
	}
	return 0
}

func cmdCompletion(shell string) int {
	names := agents.ListSorted()
	agentList := strings.Join(names, " ")

	switch shell {
	case "bash":
		fmt.Printf(`_msc() {
    local cur="${COMP_WORDS[COMP_CWORD]}"
    local prev="${COMP_WORDS[COMP_CWORD-1]}"

    if [[ "$cur" == -* ]]; then
        COMPREPLY=($(compgen -W "-h --help -v --version -d --debug -q --quiet -n --dry-run -j --json -f --force --no-inject --vault --mcp-url --token" -- "$cur"))
        return
    fi

    # Complete agent names and subcommands for the first positional arg.
    local commands="%s list status version help completion"
    COMPREPLY=($(compgen -W "$commands" -- "$cur"))
}
complete -F _msc msc
`, agentList)

	case "zsh":
		fmt.Printf(`#compdef msc

_msc() {
    local -a agents=(%s)
    local -a commands=(list status version help completion)
    local -a flags=(
        {-h,--help}'[Show help]'
        {-v,--version}'[Show version]'
        {-d,--debug}'[Enable debug logging]'
        {-q,--quiet}'[Suppress msc output]'
        {-n,--dry-run}'[Show what would happen]'
        {-j,--json}'[Output as JSON]'
        {-f,--force}'[Skip health check]'
        '--no-inject[Disable memory injection]'
        '--vault[MuninnDB vault name]:vault:'
        '--mcp-url[MuninnDB MCP endpoint]:url:'
        '--token[MuninnDB bearer token]:token:'
    )

    _arguments -s \
        $flags \
        '1:command:(${agents} ${commands})' \
        '*::arg:->args'
}

_msc "$@"
`, agentList)

	case "fish":
		fmt.Printf(`complete -c msc -f
complete -c msc -l help -s h -d "Show help"
complete -c msc -l version -s v -d "Show version"
complete -c msc -l debug -s d -d "Enable debug logging (structured slog output)"
complete -c msc -l quiet -s q -d "Suppress msc output"
complete -c msc -l dry-run -s n -d "Show what would happen"
complete -c msc -l json -s j -d "Output as JSON"
complete -c msc -l force -s f -d "Skip health check"
complete -c msc -l no-inject -d "Disable memory injection"
complete -c msc -l vault -r -d "MuninnDB vault name"
complete -c msc -l mcp-url -r -d "MuninnDB MCP endpoint"
complete -c msc -l token -r -d "MuninnDB bearer token"
`)
		for _, n := range names {
			fmt.Printf("complete -c msc -a %s -d \"Proxy %s API traffic\"\n", n, n)
		}
		fmt.Println(`complete -c msc -a list -d "List supported agents"`)
		fmt.Println(`complete -c msc -a status -d "Check MuninnDB connectivity"`)
		fmt.Println(`complete -c msc -a version -d "Show version"`)
		fmt.Println(`complete -c msc -a completion -d "Generate shell completions"`)

	default:
		logerr("unsupported shell: %s (use bash, zsh, or fish)", shell)
		return exitUsage
	}
	return 0
}

func printVersion(o *opts) {
	if o.asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		enc.Encode(map[string]string{
			"version": version,
			"commit":  commit,
			"date":    date,
			"go":      runtime.Version(),
			"os":      runtime.GOOS,
			"arch":    runtime.GOARCH,
		})
		return
	}
	fmt.Printf("msc %s (%s %s) %s %s/%s\n",
		version, commit, date, runtime.Version(), runtime.GOOS, runtime.GOARCH)
}

// closestMatch returns the best match from candidates if it's within a
// reasonable edit distance (<=2), or "" if nothing is close enough. Used
// for "did you mean?" suggestions on typos.
func closestMatch(input string, candidates []string) string {
	best := ""
	bestDist := 3 // only suggest if distance <= 2
	for _, c := range candidates {
		d := levenshtein(input, c)
		if d < bestDist {
			bestDist = d
			best = c
		}
	}
	return best
}

func levenshtein(a, b string) int {
	if len(a) == 0 {
		return len(b)
	}
	if len(b) == 0 {
		return len(a)
	}
	prev := make([]int, len(b)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(a); i++ {
		curr := make([]int, len(b)+1)
		curr[0] = i
		for j := 1; j <= len(b); j++ {
			cost := 1
			if a[i-1] == b[j-1] {
				cost = 0
			}
			curr[j] = min(curr[j-1]+1, min(prev[j]+1, prev[j-1]+cost))
		}
		prev = curr
	}
	return prev[len(b)]
}

// logf prints a human-friendly message to stderr with the msc: prefix.
func logf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "msc: "+format+"\n", args...)
}

// logerr prints a human-friendly error to stderr with the msc: error: prefix.
func logerr(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "msc: error: "+format+"\n", args...)
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
		fmt.Fprintf(w, "  %-12s  %s -> %s\n", n, a.EnvKey, a.DefaultURL)
	}

	fmt.Fprintf(w, `
Commands:
  list           List supported agents (use --json for machine output)
  status         Check MuninnDB connectivity
  version        Show version information
  completion     Generate shell completions (bash, zsh, fish)

Flags:
  -h, --help         Show this help
  -v, --version      Show version
  -d, --debug        Enable debug logging (structured slog output)
  -q, --quiet        Suppress msc's own output
  -n, --dry-run      Show resolved config without launching
  -j, --json         Machine-readable output (for list, status)
  -f, --force        Launch even if MuninnDB is unreachable (captures lost)
      --no-inject    Disable memory injection (enabled by default)
      --vault NAME   MuninnDB vault name (default: current directory name, fallback: sidecar)
      --mcp-url URL  MuninnDB MCP endpoint (default: http://127.0.0.1:8750/mcp)
      --token TOKEN  MuninnDB bearer token (default: ~/.muninn/mcp.token)

Examples:
  msc claude                    Launch Claude Code with API capture
  msc gemini                    Launch Gemini CLI with API capture
  msc codex                     Launch Codex with API capture
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
