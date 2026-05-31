package main

import (
	"context"
	"encoding/json"
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
var (
	version = "0.2.0"
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
		usage(os.Stdout)
		return 0
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

	if o.asJSON && !o.quiet {
		logf("-j/--json has no effect when running an agent (use with list, status, version, or --dry-run)")
	}

	// Resolve MuninnDB connection (flags > env > defaults).
	mcpURL, token, vault := resolveConfig(o)

	// Warn when a bearer token would be transmitted in plaintext over a
	// non-loopback HTTP connection. Localhost is exempt because the traffic
	// never leaves the machine.
	if token != "" {
		if u, err := url.Parse(mcpURL); err == nil && u.Scheme == "http" {
			h := u.Hostname()
			if h != "127.0.0.1" && h != "localhost" && h != "::1" {
				slog.Warn("bearer token will be sent over unencrypted HTTP; use HTTPS for remote MuninnDB endpoints",
					"mcp_url", mcpURL)
			}
		}
	}

	sessionStats := &stats.Stats{}
	muninn := store.New(mcpURL, token, vault, sessionStats)
	if o.noRedact {
		muninn.SetRedaction(false) // trusted env: keep full-fidelity capture
	}
	// Ensure the background worker is always stopped on exit, even for
	// early returns (dry-run, health check failure). The store's drainOnce
	// makes the second call from the normal shutdown path a no-op.
	defer muninn.Drain()

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
		grounder = grounding.New(o.groundCmd, o.groundURL, gm, os.Getenv("OPENAI_API_KEY"), gto)
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
		slog.Warn("received signal, shutting down", "signal", sig)
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
			slog.Error("agent exited with error", "err", result.err)
		}
	}
	return result.code
}

// printDryRun outputs a preview of what msc would do without launching anything.
// Called when --dry-run is set, before any proxy or agent is started.
func printDryRun(o *opts, cmd string, agent agents.Agent, upstream, mcpURL, vault string, healthErr error, caCertPath string) int {
	binary, _ := exec.LookPath(agent.Command)
	if binary == "" {
		binary = "(not found in PATH)"
	}

	if o.asJSON {
		type dryRunInfo struct {
			Agent        string            `json:"agent"`
			Binary       string            `json:"binary"`
			Upstream     string            `json:"upstream"`
			Env          map[string]string `json:"env"`
			Vault        string            `json:"vault"`
			MuninnURL    string            `json:"muninn_url"`
			MuninnStatus string            `json:"muninn_status"`
			MuninnError  string            `json:"muninn_error,omitempty"`
			Inject       bool              `json:"inject"`
			InjectBudget int               `json:"inject_budget,omitempty"`
			MITM         bool              `json:"mitm"`
			MITMCACert   string            `json:"mitm_ca_cert,omitempty"`
		}
		var envMap map[string]string
		if o.mitm {
			envMap = map[string]string{
				"HTTPS_PROXY":         "http://127.0.0.1:<port>",
				"NODE_EXTRA_CA_CERTS": caCertPath,
				"SSL_CERT_FILE":       caCertPath,
			}
		} else {
			envMap = map[string]string{agent.EnvKey: "http://127.0.0.1:<port>"}
			for _, k := range agent.ExtraEnvKeys {
				envMap[k] = "http://127.0.0.1:<port>"
			}
		}
		info := dryRunInfo{
			Agent:      cmd,
			Binary:     binary,
			Upstream:   upstream,
			Env:        envMap,
			Vault:      vault,
			MuninnURL:  mcpURL,
			Inject:     !o.noInject,
			MITM:       o.mitm,
			MITMCACert: caCertPath,
		}
		if o.force {
			info.MuninnStatus = "unchecked"
		} else if healthErr == nil {
			info.MuninnStatus = "reachable"
		} else {
			info.MuninnStatus = "unreachable"
			info.MuninnError = healthErr.Error()
		}
		if !o.noInject {
			budget := o.injectBudget
			if budget <= 0 {
				budget = 2048
			}
			info.InjectBudget = budget
		}
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(info); err != nil {
			logerr("failed to encode JSON: %v", err)
			return 1
		}
		return 0
	}

	fmt.Fprintf(os.Stdout, "Agent:    %s\n", cmd)
	fmt.Fprintf(os.Stdout, "Binary:   %s\n", binary)
	fmt.Fprintf(os.Stdout, "Upstream: %s\n", upstream)
	if o.mitm {
		scope := "all hosts"
		if len(o.mitmHosts) > 0 {
			scope = "upstream + " + strings.Join(o.mitmHosts, ", ") + " (others blind-tunneled)"
		}
		fmt.Fprintf(os.Stdout, "Mode:     TLS-MITM (transparent HTTPS proxy)\n")
		fmt.Fprintf(os.Stdout, "Intercept: %s\n", scope)
		fmt.Fprintf(os.Stdout, "Env:      HTTPS_PROXY=http://127.0.0.1:<port>\n")
		fmt.Fprintf(os.Stdout, "          NODE_EXTRA_CA_CERTS=%s\n", caCertPath)
		fmt.Fprintf(os.Stdout, "          SSL_CERT_FILE=%s\n", caCertPath)
	} else {
		fmt.Fprintf(os.Stdout, "Env:      %s=http://127.0.0.1:<port>\n", agent.EnvKey)
		for _, k := range agent.ExtraEnvKeys {
			fmt.Fprintf(os.Stdout, "          %s=http://127.0.0.1:<port>\n", k)
		}
	}
	fmt.Fprintf(os.Stdout, "Vault:    %s\n", vault)
	var muninnStatus string
	if o.force {
		muninnStatus = "(not checked)"
	} else if healthErr == nil {
		muninnStatus = "(reachable)"
	} else {
		muninnStatus = fmt.Sprintf("(unreachable: %v)", healthErr)
	}
	fmt.Fprintf(os.Stdout, "MuninnDB: %s %s\n", mcpURL, muninnStatus)
	if !o.noInject {
		budget := o.injectBudget
		if budget <= 0 {
			budget = 2048
		}
		minScore := o.minScore
		if minScore <= 0 {
			minScore = 0.6
		}
		mode := o.recallMode
		if mode == "" {
			mode = "semantic"
		}
		calib := "auto-calibrated"
		if o.noAutoCalibrate {
			calib = "fixed"
		}
		fmt.Fprintf(os.Stdout, "Inject:   enabled (budget=%d tokens, min-score=%.2f %s, recall-mode=%s)\n", budget, minScore, calib, mode)
	} else {
		fmt.Fprintln(os.Stdout, "Inject:   disabled")
	}
	return 0
}
