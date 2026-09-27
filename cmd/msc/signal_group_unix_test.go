//go:build unix

package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// An agent spawns helpers of its own, and msc exits right after signalling it.
// Signalling only the agent leaves those helpers running against a proxy that is
// gone, so the forwarded signal has to reach the whole process group.
func TestSignalChildrenReachesGrandchildren(t *testing.T) {
	requireProc(t)

	// The shell records its background child's pid, then blocks. The sleep lands
	// in the shell's (new) process group, so only a group signal reaches it.
	dir := t.TempDir()
	pidFile := filepath.Join(dir, "grandchild.pid")
	sh := exec.Command("sh", "-c", "sleep 30 & echo $! > "+pidFile+"; wait")
	sh.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := sh.Start(); err != nil {
		t.Skipf("cannot start helper shell: %v", err)
	}
	defer func() {
		_ = syscall.Kill(-sh.Process.Pid, syscall.SIGKILL)
		_ = sh.Wait()
	}()

	grandchild := waitForPIDFile(t, pidFile)

	signalChildren(syscall.SIGKILL)
	_ = sh.Wait()

	if !waitForExit(t, grandchild) {
		t.Fatalf("grandchild %d survived the forwarded signal", grandchild)
	}
}

// waitForPIDFile polls until the helper shell has written its background child's
// pid, and returns it.
func waitForPIDFile(t *testing.T, path string) int {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		// The shell's `echo $! > file` truncates before it writes, so a read can
		// land in between and see an empty or half-written file. Parse what is
		// there and keep polling instead of indexing into a possibly empty
		// buffer.
		if b, err := os.ReadFile(path); err == nil {
			if pid, err := strconv.Atoi(strings.TrimSpace(string(b))); err == nil {
				return pid
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("helper never reported its background child pid")
	return 0
}

// waitForExit reports whether pid is gone from the process table within 5s.
func waitForExit(t *testing.T, pid int) bool {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(filepath.Join("/proc", strconv.Itoa(pid))); os.IsNotExist(err) {
			return true
		}
		time.Sleep(10 * time.Millisecond)
	}
	return false
}
