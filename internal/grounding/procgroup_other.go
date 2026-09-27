//go:build !unix

package grounding

import "os/exec"

// isolateProcessGroup is a no-op where process groups are not available: the
// judge keeps the default kill-the-direct-child behavior on timeout.
func isolateProcessGroup(cmd *exec.Cmd) {}
