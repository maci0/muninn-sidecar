package main

import (
	"errors"
	"os"
	"os/exec"
	"syscall"
	"testing"
)

// TestMainHelp re-execs this test binary with a sentinel env var so main() runs
// inside the coverage-instrumented process. --help / -h makes main() exit 0
// without touching the network or launching anything.
func TestMainHelp(t *testing.T) {
	if os.Getenv("MSC_RUN_MAIN") == "1" {
		os.Args = []string{"msc", "--help"}
		main()
		return
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestMainHelp$")
	cmd.Env = append(os.Environ(), "MSC_RUN_MAIN=1")
	if err := cmd.Run(); err != nil {
		t.Errorf("main(-h) should exit 0, got %v", err)
	}
}

func TestExitCodeFromErr(t *testing.T) {
	if got := exitCodeFromErr(nil); got != 0 {
		t.Errorf("nil error: got %d, want 0", got)
	}
	if got := exitCodeFromErr(errors.New("boom")); got != 1 {
		t.Errorf("non-exec error: got %d, want 1", got)
	}

	// A child that exits with its own code keeps that code.
	err := exec.Command("sh", "-c", "exit 7").Run()
	if err == nil {
		t.Fatal("expected exit 7 to fail")
	}
	if got := exitCodeFromErr(err); got != 7 {
		t.Errorf("exit 7: got %d, want 7", got)
	}

	// A signal-terminated child maps to 128+N instead of ExitCode()'s -1,
	// which os.Exit would turn into 255.
	cmd := exec.Command("sleep", "30")
	if err := cmd.Start(); err != nil {
		t.Fatalf("start child: %v", err)
	}
	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatalf("signal child: %v", err)
	}
	err = cmd.Wait()
	if err == nil {
		t.Fatal("expected error from SIGTERM-terminated child")
	}
	if got, want := exitCodeFromErr(err), 128+int(syscall.SIGTERM); got != want {
		t.Errorf("SIGTERM death: got %d, want %d", got, want)
	}
}
