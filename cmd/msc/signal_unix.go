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

// signalAgent sends sig to the running agent process.
func signalAgent(p *os.Process, sig syscall.Signal) error {
	return p.Signal(sig)
}
