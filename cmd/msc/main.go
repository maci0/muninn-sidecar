// Command msc is the muninn sidecar: a transparent reverse proxy that gives any
// stateless AI coding agent long-term memory backed by MuninnDB. It overrides the
// agent's API base URL to route traffic through a local proxy that forwards all
// requests unchanged while providing two zero-config features:
//
//   - Auto-memorization: LLM completion responses are captured and stored as
//     semantic memories in MuninnDB.
//   - Auto-injection: before forwarding a request, relevant past memories are
//     recalled from the conversation and injected into the system prompt.
//
// Invoked as "msc <agent> [args...]" it launches a wrapped agent; it also exposes
// list, status, ca, version, and completion subcommands defined in commands.go.
// The --dry-run preview lives in dryrun.go, option parsing in flags.go, and the
// subcommands in commands.go.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/maci0/muninn-sidecar/internal/agents"
	"github.com/maci0/muninn-sidecar/internal/config"
	"github.com/maci0/muninn-sidecar/internal/grounding"
	"github.com/maci0/muninn-sidecar/internal/inject"
	"github.com/maci0/muninn-sidecar/internal/mitm"
	"github.com/maci0/muninn-sidecar/internal/proxy"
	"github.com/maci0/muninn-sidecar/internal/stats"
	"github.com/maci0/muninn-sidecar/internal/store"
)

// Build-time variables, set via -ldflags:
//
//	go build -ldflags "-X main.version=1.0.0 -X main.commit=abc1234 -X main.date=2026-03-10T12:34:56Z"
//
// The defaults describe a build that carries no stamp: claiming a released
// version here would make a `go build` or `go install` of a tree past the tag
// report the last release's number for a binary that does not contain it. A
// release build gets the real values from `make build` (which passes
// `git describe` through -X) or from the -ldflags line above.
var (
	version = "dev"
	commit  = "dev"
	date    = "unknown"
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
	// All flags are parsed before acting on special actions (--help, --version)
	// so flag order doesn't matter (e.g. -v -j and -j -v both work).
	o := &opts{}
	remaining, action, err := parseFlags(os.Args[1:], o)
	if err != nil {
		logerr("%v", err)
		logf("Run 'msc --help' for usage.")
		return exitUsage
	}

	switch action {
	case actionHelp:
		// 'msc list --help' is a request for that command's help, not the
		// global one: the global usage cannot document flags that apply to a
		// single command.
		if len(remaining) > 0 {
			if text, ok := commandUsage(remaining[0]); ok {
				fmt.Fprint(os.Stdout, text)
				return 0
			}
		}
		usage(os.Stdout)
		return 0
	case actionVersion:
		return printVersion(o)
	}

	if len(remaining) == 0 {
		logerr("missing command. Run 'msc --help' for usage.")
		return exitUsage
	}

	// Configure logging. Default to WARN so normal usage is clean; the
	// user-friendly msc: prefixed messages cover the INFO case. --debug
	// enables DEBUG for full structured logging.
	level := slog.LevelWarn
	if o.debug {
		level = slog.LevelDebug
	}
	handlerOpts := &slog.HandlerOptions{Level: level}
	var handler slog.Handler
	if o.logJSON {
		handler = slog.NewJSONHandler(os.Stderr, handlerOpts)
	} else {
		handler = slog.NewTextHandler(os.Stderr, handlerOpts)
	}
	slog.SetDefault(slog.New(handler))

	cmd := remaining[0]
	agentArgs := remaining[1:]

	switch cmd {
	case "help":
		return cmdHelp(agentArgs)
	case "version":
		if len(agentArgs) > 0 {
			logerr("version does not accept arguments")
			return exitUsage
		}
		return printVersion(o)
	case "list":
		if len(agentArgs) > 0 {
			logerr("list does not accept arguments")
			return exitUsage
		}
		return cmdList(o)
	case "status":
		if len(agentArgs) > 0 {
			logerr("status does not accept arguments")
			return exitUsage
		}
		return cmdStatus(o)
	case "ca":
		if len(agentArgs) > 0 {
			logerr("ca does not accept arguments")
			return exitUsage
		}
		return cmdCA(o)
	case "completion":
		if len(agentArgs) == 0 {
			logerr("missing shell argument: msc completion <bash|zsh|fish>")
			shell := "zsh"
			if s := os.Getenv("SHELL"); s != "" {
				switch {
				case strings.HasSuffix(s, "bash"):
					shell = "bash"
				case strings.HasSuffix(s, "fish"):
					shell = "fish"
				}
			}
			logf("example: source <(msc completion %s)", shell)
			return exitUsage
		}
		if len(agentArgs) > 1 {
			logerr("completion takes exactly one argument: msc completion <bash|zsh|fish>")
			return exitUsage
		}
		return cmdCompletion(agentArgs[0])
	}

	agent, ok := agents.Registry[cmd]
	if !ok {
		names := agents.ListSorted()
		allNames := make([]string, 0, len(names)+5)
		allNames = append(allNames, names...)
		allNames = append(allNames, "list", "status", "ca", "version", "help", "completion")
		if suggestion := closestMatch(cmd, allNames); suggestion != "" {
			logerr("unknown command: %s. Did you mean %q?", cmd, suggestion)
		} else {
			logerr("unknown command: %s", cmd)
		}
		logf("agents: %s", strings.Join(names, ", "))
		logf("commands: list, status, ca, version, completion, help")
		return exitUsage
	}

	if o.asJSON && !o.quiet && !o.dryRun {
		logf("-j/--json has no effect when running an agent (use with list, status, ca, version, or --dry-run)")
	}

	// Resolve MuninnDB connection (flags > env > defaults).
	mcpURL, token, vault := resolveConfig(o)

	// Reject an undialable endpoint before anything else happens, so a typo in
	// --mcp-url or MUNINN_MCP_URL names itself instead of surfacing as a
	// transport error from the health check (or worse, silent capture loss).
	if err := config.ValidateURL("MuninnDB URL", mcpURL); err != nil {
		logerr("%v", err)
		return exitUsage
	}

	// Warn when a bearer token would be transmitted in plaintext over a
	// non-loopback HTTP connection. Localhost is exempt because the traffic
	// never leaves the machine.
	if token != "" {
		if w := config.ArgSecretWarning("--token", o.token, "MUNINN_TOKEN"); w != "" {
			slog.Warn(w)
		}
		if u, err := url.Parse(mcpURL); err == nil && u.Scheme == "http" && !config.IsLoopbackHost(u.Hostname()) {
			slog.Warn("bearer token will be sent over unencrypted HTTP; use HTTPS for remote MuninnDB endpoints",
				"mcp_url", mcpURL)
		}
	}

	// One line naming the effective configuration, so a captured session log
	// answers "which endpoint, vault, and gate did this run use?" without a
	// separate --dry-run. The token is reported as set/unset, never by value.
	// The injection numbers come from the same helpers the dry-run preview uses,
	// so the two cannot report different values.
	slog.Debug("resolved config",
		"mcp_url", mcpURL,
		"vault", vault,
		"token", token != "",
		"inject", !o.noInject,
		"inject_budget", dryRunBudget(o),
		"inject_min_score", dryRunMinScore(o),
		"recall_mode", dryRunRecallMode(o),
		"auto_calibrate", !o.noAutoCalibrate,
		"redact", !o.noRedact,
		"mitm", o.mitm)

	sessionStats := &stats.Stats{}
	muninn := store.New(mcpURL, token, vault, sessionStats)
	if o.noRedact {
		slog.Warn("redaction disabled (--no-redact): API keys, tokens, and personal data in captured conversations will be stored in MuninnDB unscrubbed; use only in trusted environments")
		muninn.SetRedaction(false) // trusted env: keep full-fidelity capture
	}
	// Ensure the background worker is always stopped on exit, even for
	// early returns (dry-run, health check failure). The store's drainOnce
	// makes the second call from the normal shutdown path a no-op. Close
	// releases its MCP connection pool; the injector created below gets its
	// own Close on the same path.
	defer muninn.Drain()
	defer muninn.Close()

	// Health check: verify MuninnDB is reachable before launching the agent.
	// The whole point of msc is to capture traffic — silently dropping captures
	// defeats the purpose. --force skips this check. healthErr is reused
	// below in --dry-run output.
	var healthErr error
	if !o.force {
		healthErr = muninn.HealthCheck()
		if healthErr != nil && !o.dryRun {
			logerr("MuninnDB at %s is unreachable: %v", mcpURL, healthErr)
			logf("Captures will be lost. Use --force to launch anyway.")
			return 1
		}
	}

	upstream := agent.Resolve()
	slog.Debug("resolved upstream", "agent", cmd, "upstream", upstream)

	// TLS-MITM mode: load/create the local CA so the proxy can intercept HTTPS
	// CONNECT tunnels and the child can be told to trust it. Built before the
	// dry-run so the preview can report the CA path.
	var (
		ca         *mitm.CA
		caCertPath string
	)
	if o.mitm {
		dir, err := mitmCADir()
		if err != nil {
			logerr("%v", err)
			return 1
		}
		ca, err = mitm.LoadOrCreateCA(dir)
		if err != nil {
			logerr("failed to load MITM CA: %v", err)
			return 1
		}
		caCertPath = filepath.Join(dir, "ca-cert.pem")
	}

	// --dry-run: show what would happen without launching anything.
	if o.dryRun {
		return printDryRun(o, cmd, agent, upstream, mcpURL, vault, healthErr, caCertPath)
	}

	// Optional answer-grounding rerank (opt-in precision step, docs §B4): a fast
	// local judge (--ground-url) is viable in-flight for harm-prone vaults; a
	// frontier CLI (--ground-cmd) is best offline. The grounder caps its own
	// per-call latency, so it gets a generous timeout independent of the MCP one.
	var grounder grounding.Grounder
	if o.groundCmd != "" || o.groundURL != "" {
		gm := o.groundModel
		if gm == "" {
			gm = "qwen2.5:7b-instruct"
		}
		// Bound the in-flight grounding call so a slow/hung judge fails open fast
		// (degrading to the cosine gate) instead of stalling the user's request.
		gto := o.groundTimeout
		if gto <= 0 {
			gto = 10 * time.Second
		}
		groundKey := os.Getenv("OPENAI_API_KEY")
		// Warn before OPENAI_API_KEY is sent to a grounding endpoint that is not
		// OpenAI itself: over plaintext HTTP, or to any third-party host, the key
		// leaves the machine in a form the user may not have intended. The CLI
		// backend (--ground-cmd) takes precedence and sends no key.
		if groundKey != "" && o.groundCmd == "" && o.groundURL != "" {
			if u, err := url.Parse(o.groundURL); err == nil {
				switch {
				case u.Scheme == "http" && !config.IsLoopbackHost(u.Hostname()):
					slog.Warn("OPENAI_API_KEY will be sent over unencrypted HTTP to the grounding endpoint; use HTTPS",
						"ground_url", o.groundURL)
				case !config.IsOpenAIHost(u.Hostname()):
					slog.Warn("OPENAI_API_KEY will be sent to a grounding endpoint that is not api.openai.com; set the key for that endpoint explicitly to confirm",
						"ground_url", o.groundURL)
				}
			}
		}
		grounder = grounding.New(o.groundCmd, o.groundURL, gm, groundKey, gto)
	}

	// Create injector unless --no-inject is set.
	var injector *inject.Injector
	if !o.noInject {
		injector = inject.New(inject.Config{
			MCPURL:        mcpURL,
			Token:         token,
			Vault:         vault,
			Budget:        o.injectBudget,
			MinScore:      o.minScore,
			RecallMode:    o.recallMode,
			AutoCalibrate: !o.noAutoCalibrate, // self-tune the gate by default
			Grounder:      grounder,
			GroundTopK:    o.groundTopK,
			Stats:         sessionStats,
		})
		// Registered before the proxy exists so every return past this point —
		// a failed Start, a health-check failure, the normal shutdown — releases
		// the injector's connection pool.
		defer injector.Close()
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
		CA:           ca,          // nil unless --mitm; enables CONNECT/TLS interception
		MITMHosts:    o.mitmHosts, // empty = intercept all CONNECT hosts; non-empty scopes (+ upstream)
		Stats:        sessionStats,
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
		if o.mitm {
			logf("MITM-proxying %s HTTPS via %s (CA: %s)", cmd, proxyURL, caCertPath)
		} else {
			logf("proxying %s traffic via %s -> %s", cmd, proxyURL, upstream)
		}
		logf("storing in vault %q", vault)
		if o.force {
			logf("warning: MuninnDB check skipped (--force); captures may be lost if unreachable")
		}
	}

	// Trap signals for graceful shutdown: stop the proxy, drain pending
	// captures so nothing is lost, then exit with the child's code.
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	// Stop delivering once the shutdown path is entered: the runtime's signal
	// handler and the channel's single slot would otherwise outlive the select
	// below and keep a handler installed for the rest of the process.
	defer signal.Stop(sigCh)

	// Launch the agent in a goroutine so we can select on both the agent
	// exiting and a signal arriving.
	type exitResult struct {
		err  error
		code int
	}
	doneCh := make(chan exitResult, 1)
	go func() {
		var err error
		if o.mitm {
			err = agent.ExecMITM(proxyURL, upstream, caCertPath, agentArgs)
		} else {
			err = agent.Exec(proxyURL, upstream, agentArgs)
		}
		doneCh <- exitResult{err: err, code: exitCodeFromErr(err)}
	}()

	var result exitResult
	select {
	case result = <-doneCh:
		// Agent exited on its own.
	case sig := <-sigCh:
		slog.Warn("received signal, shutting down", "signal", sig)
		// Forward the signal to the agent: a signal sent to msc's PID alone
		// (kill, docker stop) never reaches the child. signalChildren skips
		// children the kernel already signalled via the terminal's foreground
		// process group (Ctrl+C), so the agent sees the signal exactly once.
		signalChildren(sig)
		// Wait briefly for the agent to exit.
		select {
		case result = <-doneCh:
		case <-time.After(3 * time.Second):
			// The agent ignored the signal; kill it so it is not left
			// running against a proxy that is about to shut down.
			signalChildren(syscall.SIGKILL)
			code := 130 // fallback: conventional SIGINT exit code
			if s, ok := sig.(syscall.Signal); ok {
				code = 128 + int(s) // shell convention, e.g. 143 for SIGTERM
			}
			result = exitResult{code: code}
		}
	}

	// Graceful shutdown: stop proxy, flush pending captures.
	shutCtx, shutCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer shutCancel()
	// A failed Shutdown means requests were still in flight when the deadline
	// hit, so their captures never arrive. Report it instead of letting the
	// session summary read as a clean exit.
	if err := p.Shutdown(shutCtx); err != nil {
		slog.Warn("proxy shutdown did not complete; in-flight requests were cut off", "err", err)
	}
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
			// 127 is the shell's "command not found", so a script can tell a
			// mistyped agent name apart from an agent that ran and failed.
			result.code = exitNotFound
		} else {
			slog.Error("agent exited with error", "err", result.err)
		}
	}
	return result.code
}

// exitCodeFromErr maps the error from running the agent to msc's exit code:
// 0 on success, the child's own code when it exited, 128+N when signal N
// killed it (the shell convention; ExitCode() reports -1 there, which
// os.Exit would surface as an opaque 255), and 1 for any other failure.
func exitCodeFromErr(err error) int {
	if err == nil {
		return 0
	}
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		return 1
	}
	if ws, ok := exitErr.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
		return 128 + int(ws.Signal())
	}
	return exitErr.ExitCode()
}
