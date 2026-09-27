package agents

import (
	"fmt"
	"os"
	"os/exec"
	"sync"
)

// childMu guards child, the process most recently started by Exec/ExecMITM.
var (
	childMu sync.Mutex
	child   *os.Process
)

// Child returns the agent process Exec or ExecMITM most recently started, or
// nil when none is running. It lets a caller signal the agent directly, which
// is the only option on platforms with no /proc to enumerate processes through.
func Child() *os.Process {
	childMu.Lock()
	defer childMu.Unlock()
	return child
}

func setChild(p *os.Process) {
	childMu.Lock()
	child = p
	childMu.Unlock()
}

// runArgv looks up the agent binary and executes it with the given environment
// and already-assembled argv, inheriting stdin/stdout/stderr. The running
// child is published via Child() until it is reaped.
func (a Agent) runArgv(env []string, argv []string) error {
	binary, err := exec.LookPath(a.Command)
	if err != nil {
		return fmt.Errorf("agent %q not found in PATH: %w", a.Command, err)
	}
	cmd := exec.Command(binary, argv...)
	cmd.Env = env
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start agent %q: %w", a.Command, err)
	}
	setChild(cmd.Process)
	defer setChild(nil)
	if err := cmd.Wait(); err != nil {
		return fmt.Errorf("agent %q exited with an error: %w", a.Command, err)
	}
	return nil
}
