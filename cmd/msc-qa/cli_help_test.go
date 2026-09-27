package main

import (
	"bytes"
	"os"
	"os/exec"
	"strings"
	"testing"
)

// TestMainHelp re-execs this test binary with a sentinel env var so main() runs
// inside the coverage-instrumented process. --help / -h prints the help text to
// stdout and exits 0 without touching the network or launching anything.
func TestMainHelp(t *testing.T) {
	if os.Getenv("MSC_RUN_MAIN") == "1" {
		os.Args = []string{"msc-qa", "-h"}
		main()
		return
	}
	stdout, stderr, code := runMain(t, "TestMainHelp", "-h")
	if code != 0 {
		t.Errorf("main(-h) should exit 0, got %d (stderr: %s)", code, stderr)
	}
	if !strings.Contains(stdout, "Usage: msc-qa [flags]") {
		t.Errorf("help should go to stdout, got: %q", stdout)
	}
	if stderr != "" {
		t.Errorf("stderr should stay empty for -h, got: %s", stderr)
	}
}

// TestMainUsageError pins the other half of the contract: a bad flag is a usage
// error (exit 2) reported on stderr, with stdout left clean for scripts.
func TestMainUsageError(t *testing.T) {
	for _, arg := range []string{"-not-a-flag", "unexpected-positional"} {
		stdout, stderr, code := runMain(t, "TestMainUsageError", arg)
		if code != 2 {
			t.Errorf("main(%s) should exit 2, got %d", arg, code)
		}
		if stdout != "" {
			t.Errorf("main(%s) should write nothing to stdout, got: %q", arg, stdout)
		}
		if !strings.Contains(stderr, "Run 'msc-qa -h' for usage.") {
			t.Errorf("main(%s) should point at -h on stderr, got: %s", arg, stderr)
		}
	}
}

// runMain re-execs the test binary with a sentinel env var so main() runs
// inside the coverage-instrumented process, and returns its streams and exit code.
func runMain(t *testing.T, test, arg string) (stdout, stderr string, code int) {
	t.Helper()
	if os.Getenv("MSC_RUN_MAIN") == "1" {
		os.Args = []string{"msc-qa", arg}
		main()
		return "", "", 0 // unreachable: main() exits
	}
	cmd := exec.Command(os.Args[0], "-test.run=^"+test+"$")
	cmd.Env = append(os.Environ(), "MSC_RUN_MAIN=1")
	var out, errBuf bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errBuf
	runErr := cmd.Run()
	if ee, ok := runErr.(*exec.ExitError); ok {
		code = ee.ExitCode()
	} else if runErr != nil {
		t.Fatalf("re-exec: %v", runErr)
	}
	return out.String(), errBuf.String(), code
}
