package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"

	"github.com/maci0/muninn-sidecar/internal/config"
	"testing"
)

// TestMainUsageExitCode pins the exit code for a bad flag value: 2, the code
// the flag package already uses for an unparseable command line and the code
// msc exits with, so a script can tell a typo from a runtime failure.
func TestMainUsageExitCode(t *testing.T) {
	if os.Getenv("MSC_RUN_MAIN") == "1" {
		os.Args = []string{"msc-eval", "-min-score", "5"}
		main()
		return
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestMainUsageExitCode$")
	cmd.Env = append(os.Environ(), "MSC_RUN_MAIN=1")
	cmd.Stderr = io.Discard
	var exit *exec.ExitError
	if err := cmd.Run(); !errors.As(err, &exit) || exit.ExitCode() != 2 {
		t.Errorf("main(-min-score 5) should exit 2, got %v", err)
	}
}

// An explicitly written `-mcp-url=` is an empty flag value, and the flag package
// does not substitute its default for it. MUNINN_MCP_URL must still be resolved,
// so an endpoint that cannot be dialed is rejected at startup by name instead
// of surfacing later as a transport error from an empty URL.
func TestEmptyMCPURLFlagFallsBackToEnv(t *testing.T) {
	if os.Getenv("MSC_RUN_MAIN") == "1" {
		os.Args = []string{"msc-eval", "-live", "-mcp-url="}
		if err := run(); err == nil || !config.IsUsageError(err) {
			t.Errorf("empty -mcp-url with a bad MUNINN_MCP_URL: %v, want a usage error", err)
		} else {
			// The parent asserts on this text, so it goes to stderr rather than
			// into the child's own (discarded) test log.
			fmt.Fprintln(os.Stderr, err)
		}
		return
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestEmptyMCPURLFlagFallsBackToEnv$")
	cmd.Env = append(os.Environ(), "MSC_RUN_MAIN=1", "MUNINN_MCP_URL=127.0.0.1:8750/mcp")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Errorf("main(-mcp-url=) with a scheme-less MUNINN_MCP_URL should exit 2, got %v: %s", err, out)
	}
	if !strings.Contains(string(out), "MuninnDB URL") {
		t.Errorf("the rejection must name the option, got: %s", out)
	}
}
