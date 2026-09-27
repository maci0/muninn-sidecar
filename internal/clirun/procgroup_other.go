//go:build !unix

package clirun

import "os/exec"

func setProcessGroup(cmd *exec.Cmd) bool { return false }

func killGroup(pid int) error { return nil }
