//go:build unix

package main

import (
	"os/exec"
	"syscall"
	"testing"
)

func TestChildProcs(t *testing.T) {
	requireProc(t)
	cmd := exec.Command("sleep", "30")
	if err := cmd.Start(); err != nil {
		t.Fatalf("start child: %v", err)
	}
	defer func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	}()
	found := false
	for _, c := range childProcs() {
		if c.pid == cmd.Process.Pid {
			found = true
			// Spawned without Setpgid, the child shares our process group.
			if want := syscall.Getpgrp(); c.pgrp != want {
				t.Errorf("child pgrp = %d, want %d", c.pgrp, want)
			}
		}
	}
	if !found {
		t.Errorf("childProcs() missing spawned child %d", cmd.Process.Pid)
	}
}
