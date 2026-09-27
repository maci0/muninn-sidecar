//go:build unix

package main

import (
	"syscall"
	"testing"
	"time"

	"github.com/maci0/muninn-sidecar/internal/agents"
)

// TestSignalChildHandle pins the fallback used where /proc is unavailable
// (macOS, Windows, a restricted /proc): a signal aimed at msc's PID alone must
// still reach the agent through the handle the agents package publishes,
// instead of leaving it orphaned against a dead proxy.
func TestSignalChildHandle(t *testing.T) {
	a := agents.Agent{Command: "sleep", EnvKey: "FOO_URL", DefaultURL: "https://x"}
	errCh := make(chan error, 1)
	go func() { errCh <- a.Exec("http://127.0.0.1:1", "https://x", []string{"30"}) }()

	var pid int
	for range 100 {
		if p := agents.Child(); p != nil {
			pid = p.Pid
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if pid == 0 {
		t.Fatal("agents.Child() never published the running agent")
	}

	signalChildHandle(syscall.SIGTERM)
	select {
	case err := <-errCh:
		if err == nil {
			t.Error("expected non-nil error from a SIGTERM-terminated agent")
		}
	case <-time.After(5 * time.Second):
		t.Error("agent did not exit after forwarded SIGTERM")
	}
	if p := agents.Child(); p != nil {
		t.Errorf("agents.Child() still holds pid %d after the agent was reaped", p.Pid)
	}
}

// TestSignalChildHandleSkipsSIGINT pins that a terminal-generated Ctrl+C is
// left to the kernel: the agent shares msc's process group, so the kernel
// already delivered it and a second SIGINT would read as "force quit".
func TestSignalChildHandleSkipsSIGINT(t *testing.T) {
	a := agents.Agent{Command: "sleep", EnvKey: "FOO_URL", DefaultURL: "https://x"}
	errCh := make(chan error, 1)
	go func() { errCh <- a.Exec("http://127.0.0.1:1", "https://x", []string{"30"}) }()

	for range 100 {
		if agents.Child() != nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if agents.Child() == nil {
		t.Fatal("agents.Child() never published the running agent")
	}

	signalChildHandle(syscall.SIGINT)
	select {
	case err := <-errCh:
		t.Fatalf("agent exited on a signal the kernel already delivered: %v", err)
	case <-time.After(200 * time.Millisecond):
	}
	signalChildHandle(syscall.SIGKILL)
	<-errCh
}
