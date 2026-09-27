//go:build unix

package grounding

import (
	"os/exec"
	"syscall"
)

// isolateProcessGroup puts the judge in its own process group and replaces the
// kill-on-timeout that exec.CommandContext installs with one that signals the
// whole group. A CLI judge spawns helpers of its own; killing only the direct
// child on the grounding timeout orphans those helpers, and they outlive the
// call that spawned them.
func isolateProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
}
