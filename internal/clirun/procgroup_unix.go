//go:build unix

package clirun

import (
	"os/exec"
	"syscall"
)

func setProcessGroup(cmd *exec.Cmd) bool {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	return true
}

func killGroup(pid int) error {
	return syscall.Kill(-pid, syscall.SIGKILL)
}
