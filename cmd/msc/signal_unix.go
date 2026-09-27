//go:build unix

package main

import (
	"os"
	"syscall"
)

// signalPID sends sig to the process with the given pid. The pid comes from a
// /proc walk, so the process may have exited between the walk and the signal.
func signalPID(pid int, sig syscall.Signal) error {
	return syscall.Kill(pid, sig)
}

// signalPIDGroup sends sig to every process in the group pgrp. A coding agent
// spawns helpers of its own, and signalling only the agent leaves those helpers
// running once msc exits.
func signalPIDGroup(pgrp int, sig syscall.Signal) error {
	return syscall.Kill(-pgrp, sig)
}

// selfPgrp returns msc's own process group, which must never be group-signalled:
// it also holds the shell and whatever else shares the terminal.
func selfPgrp() int { return syscall.Getpgrp() }

// signalAgent sends sig to the running agent process.
func signalAgent(p *os.Process, sig syscall.Signal) error {
	return p.Signal(sig)
}
