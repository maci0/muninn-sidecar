//go:build !unix

package main

import (
	"fmt"
	"os"
	"syscall"
)

// signalPID is unreachable without a /proc walk; present so the shared
// signalling code compiles.
func signalPID(pid int, sig syscall.Signal) error {
	return fmt.Errorf("cannot signal pid %d: no process table on this platform", pid)
}

// signalPIDGroup has no portable equivalent here; callers fall back to
// signalPID, which is all this platform can do anyway.
func signalPIDGroup(pgrp int, sig syscall.Signal) error {
	return fmt.Errorf("cannot signal process group %d on this platform", pgrp)
}

// selfPgrp is always 0 where process groups do not apply, so the caller never
// group-signals.
func selfPgrp() int { return 0 }

// signalAgent stops the running agent. Windows has no way to deliver a POSIX
// signal to another process, so the only delivery msc can offer is termination.
func signalAgent(p *os.Process, sig syscall.Signal) error {
	return p.Kill()
}
