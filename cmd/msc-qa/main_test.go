package main

import (
	"errors"
	"io"
	"os"
	"os/exec"
	"testing"
)

// TestMainHelp re-execs this test binary with a sentinel env var so main() runs
// inside the coverage-instrumented process. --help / -h makes main() exit 0
// without touching the network or launching anything.
func TestMainHelp(t *testing.T) {
	if os.Getenv("MSC_RUN_MAIN") == "1" {
		os.Args = []string{"msc-qa", "-h"}
		main()
		return
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestMainHelp$")
	cmd.Env = append(os.Environ(), "MSC_RUN_MAIN=1")
	if err := cmd.Run(); err != nil {
		t.Errorf("main(-h) should exit 0, got %v", err)
	}
}

// TestMainUsageExitCode pins the exit code for a bad flag value: 2, the code
// the flag package already uses for an unparseable command line and the code
// msc exits with, so a script can tell a typo from a runtime failure.
func TestMainUsageExitCode(t *testing.T) {
	if os.Getenv("MSC_RUN_MAIN") == "1" {
		os.Args = []string{"msc-qa", "-n", "0"}
		main()
		return
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestMainUsageExitCode$")
	cmd.Env = append(os.Environ(), "MSC_RUN_MAIN=1")
	cmd.Stderr = io.Discard
	var exit *exec.ExitError
	if err := cmd.Run(); !errors.As(err, &exit) || exit.ExitCode() != 2 {
		t.Errorf("main(-n 0) should exit 2, got %v", err)
	}
}
