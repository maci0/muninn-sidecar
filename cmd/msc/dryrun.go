package main

import (
	"fmt"
	"os"
	"os/exec"
	"slices"
	"sort"
	"strings"

	"github.com/maci0/muninn-sidecar/internal/agents"
	"github.com/maci0/muninn-sidecar/internal/inject"
	"github.com/maci0/muninn-sidecar/internal/redact"
)

// printDryRun outputs a preview of what msc would do without launching anything.
// Called when --dry-run is set, before any proxy or agent is started.
func printDryRun(o *opts, cmd string, agent agents.Agent, upstream, mcpURL, vault string, healthErr error, caCertPath string) int {
	binary, _ := exec.LookPath(agent.Command)
	if binary == "" {
		binary = "(not found in PATH)"
	}

	// The preview must list the same overrides the child really gets, so build
	// them from the same helpers Exec/ExecMITM use.
	envMap := agent.EnvOverrides("http://127.0.0.1:<port>", upstream)
	var args []string
	if o.mitm {
		// A combined system-roots+CA bundle only exists where msc can find a
		// system bundle to combine with. Where none exists (Windows and macOS
		// keep their roots in an OS store, not a PEM file), ExecMITM passes an
		// empty bundle path and the trust-store-replacing variables stay unset,
		// so the preview must not list them.
		bundlePath := ""
		if agents.HasSystemCABundle() {
			bundlePath = agents.CABundlePath(caCertPath)
		}
		envMap = agent.MITMOverrides("http://127.0.0.1:<port>", upstream, caCertPath, bundlePath)
	} else {
		for _, pa := range agent.ProxyArgs {
			args = append(args, strings.ReplaceAll(pa, "{proxy}", "http://127.0.0.1:<port>"))
		}
	}
	keys := make([]string, 0, len(envMap))
	for k := range envMap {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	if o.asJSON {
		type dryRunInfo struct {
			Agent           string            `json:"agent"`
			Binary          string            `json:"binary"`
			Upstream        string            `json:"upstream"`
			Env             map[string]string `json:"env"`
			ProxyArgs       []string          `json:"proxy_args,omitempty"`
			Vault           string            `json:"vault"`
			MuninnURL       string            `json:"muninn_url"`
			MuninnStatus    string            `json:"muninn_status"`
			MuninnError     string            `json:"muninn_error,omitempty"`
			Inject          bool              `json:"inject"`
			InjectBudget    int               `json:"inject_budget,omitempty"`
			InjectMinScore  float64           `json:"inject_min_score,omitempty"`
			InjectRecall    string            `json:"inject_recall_mode,omitempty"`
			InjectCalibrate string            `json:"inject_calibration,omitempty"`
			MITM            bool              `json:"mitm"`
			MITMHosts       []string          `json:"mitm_hosts,omitempty"` // scope (+ upstream); empty when intercepting all, ["*"] when --mitm-host "*" forces it
			MITMCACert      string            `json:"mitm_ca_cert,omitempty"`
		}
		info := dryRunInfo{
			Agent:      cmd,
			Binary:     binary,
			Upstream:   redact.URL(upstream),
			Env:        envMap,
			ProxyArgs:  args,
			Vault:      vault,
			MuninnURL:  redact.URL(mcpURL),
			Inject:     !o.noInject,
			MITM:       o.mitm,
			MITMHosts:  o.mitmHosts,
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
			info.InjectBudget = dryRunBudget(o)
			info.InjectMinScore = dryRunMinScore(o)
			info.InjectRecall = dryRunRecallMode(o)
			if o.noAutoCalibrate {
				info.InjectCalibrate = "fixed"
			} else {
				info.InjectCalibrate = "auto-calibrated"
			}
		}
		enc := jsonEncoder(os.Stdout)
		if err := enc.Encode(info); err != nil {
			logerr("failed to encode JSON: %v", err)
			return 1
		}
		return 0
	}

	fmt.Fprintf(os.Stdout, "Agent:    %s\n", cmd)
	fmt.Fprintf(os.Stdout, "Binary:   %s\n", binary)
	fmt.Fprintf(os.Stdout, "Upstream: %s\n", redact.URL(upstream))
	if o.mitm {
		scope := "all hosts"
		// "*" forces intercept-all in the proxy, so scoped wording would be wrong.
		if len(o.mitmHosts) > 0 && !slices.Contains(o.mitmHosts, "*") {
			scope = "upstream + " + strings.Join(o.mitmHosts, ", ") + " (others blind-tunneled)"
		}
		fmt.Fprintf(os.Stdout, "Mode:     TLS-MITM (transparent HTTPS proxy)\n")
		fmt.Fprintf(os.Stdout, "Intercept: %s\n", scope)
	} else if len(args) > 0 {
		// An agent that ignores the env override is intercepted via argv.
		fmt.Fprintf(os.Stdout, "Args:     %s\n", strings.Join(args, " "))
	}
	for i, k := range keys {
		if i == 0 {
			fmt.Fprintf(os.Stdout, "Env:      %s=%s\n", k, envMap[k])
			continue
		}
		fmt.Fprintf(os.Stdout, "          %s=%s\n", k, envMap[k])
	}
	fmt.Fprintf(os.Stdout, "Vault:    %s\n", vault)
	var muninnStatus string
	if o.force {
		muninnStatus = "(not checked)"
	} else if healthErr == nil {
		muninnStatus = "(reachable)"
	} else {
		muninnStatus = "(unreachable)"
	}
	fmt.Fprintf(os.Stdout, "MuninnDB: %s %s\n", redact.URL(mcpURL), muninnStatus)
	if healthErr != nil && !o.force {
		fmt.Fprintf(os.Stdout, "          %v\n", healthErr)
	}
	if !o.noInject {
		calib := "auto-calibrated"
		if o.noAutoCalibrate {
			calib = "fixed"
		}
		fmt.Fprintf(os.Stdout, "Inject:   enabled (budget=%d tokens, min-score=%.2f %s, recall-mode=%s)\n",
			dryRunBudget(o), dryRunMinScore(o), calib, dryRunRecallMode(o))
	} else {
		fmt.Fprintln(os.Stdout, "Inject:   disabled")
	}
	return 0
}

// dry-run echoes the injection settings the way inject.New resolves them, so
// the preview and the running proxy cannot disagree.
func dryRunBudget(o *opts) int {
	if o.injectBudget <= 0 {
		return inject.DefaultBudget
	}
	return o.injectBudget
}

func dryRunMinScore(o *opts) float64 {
	if !(o.minScore > 0 && o.minScore <= 1) {
		return inject.DefaultMinScore
	}
	return o.minScore
}

func dryRunRecallMode(o *opts) string {
	if o.recallMode == "" {
		return inject.DefaultRecallMode
	}
	return o.recallMode
}
