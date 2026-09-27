package main

import (
	"bytes"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	"github.com/maci0/muninn-sidecar/internal/agents"
)

// mitmCADir returns the directory holding msc's TLS-MITM certificate authority,
// creating it (0700) if needed. The CA persists across runs so a child only has
// to trust it once. Uses the user config dir (<config>/muninn-sidecar/mitm),
// falling back to ~/.config/muninn-sidecar/mitm when it can't be resolved.
func mitmCADir() (string, error) {
	base, err := os.UserConfigDir()
	if err != nil || base == "" {
		home, herr := os.UserHomeDir()
		if herr != nil {
			return "", fmt.Errorf("cannot locate a config directory for the MITM CA: %w", herr)
		}
		base = filepath.Join(home, ".config")
	}
	dir := filepath.Join(base, "muninn-sidecar", "mitm")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("creating MITM CA dir: %w", err)
	}
	return dir, nil
}

// statFields reads /proc/<pid>/stat and returns the fields after the comm
// field, or nil on error. The stat format is "pid (comm) state ppid pgrp
// session tty_nr tpgid ..."; comm may contain spaces or parens, so parsing
// starts from the last ')'. In the returned slice, index 0 is the state,
// 1 the ppid, 2 the pgrp, and 5 the tpgid.
func statFields(pid int) []string {
	stat, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "stat"))
	if err != nil {
		return nil
	}
	i := bytes.LastIndexByte(stat, ')')
	if i < 0 {
		return nil
	}
	return strings.Fields(string(stat[i+1:]))
}

// childProc identifies a direct child process and its process group.
type childProc struct {
	pid  int
	pgrp int
}

// childProcs returns this process's direct children by walking /proc. The
// agents package owns child process construction and does not expose the
// handle, so signal forwarding discovers the agent via the kernel. Returns
// nil on platforms without /proc (forwarding is then a no-op and only
// terminal-generated signals reach the child).
func childProcs() []childProc {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil
	}
	self := os.Getpid()
	var children []childProc
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue // not a process directory
		}
		fields := statFields(pid)
		if len(fields) < 3 {
			continue // process exited mid-walk or malformed stat
		}
		ppid, perr := strconv.Atoi(fields[1])
		pgrp, gerr := strconv.Atoi(fields[2])
		if perr == nil && gerr == nil && ppid == self {
			children = append(children, childProc{pid: pid, pgrp: pgrp})
		}
	}
	return children
}

// termForegroundPgrp returns the foreground process group of this process's
// controlling terminal (tpgid), or -1 when there is no controlling terminal
// (the kernel's own sentinel) or no /proc.
func termForegroundPgrp() int {
	fields := statFields(os.Getpid())
	if len(fields) < 6 {
		return -1
	}
	tpgid, err := strconv.Atoi(fields[5])
	if err != nil {
		return -1
	}
	return tpgid
}

// shouldForward reports whether sig must be forwarded to a child in process
// group pgrp, given fgPgrp, the foreground process group of the controlling
// terminal. A terminal-generated SIGINT (Ctrl+C) is already delivered by the
// kernel to every process in the foreground group; forwarding it again would
// double-signal the agent, which "press Ctrl+C again to force-quit" CLIs
// treat as a hard exit. All other cases forward: SIGTERM has no terminal
// keybinding (kill and docker stop signal msc's PID alone), and a child
// outside the foreground group cannot have seen a terminal SIGINT. The rare
// kill -INT aimed at a foreground msc is indistinguishable from Ctrl+C and
// is left to the SIGKILL fallback in run().
func shouldForward(sig syscall.Signal, pgrp, fgPgrp int) bool {
	return sig != syscall.SIGINT || pgrp != fgPgrp
}

// signalChildren forwards sig to the launched agent. A signal sent to msc's
// PID alone (kill, docker stop) never reaches the child, so it has to be
// passed on explicitly. Two paths, in order of fidelity:
//
//   - /proc enumeration, which also yields the child's process group and so
//     can skip children the kernel already signalled through the terminal's
//     foreground process group (Ctrl+C);
//   - the process handle the agents package published, for platforms with no
//     /proc (macOS, Windows, or a restricted /proc).
//
// Errors other than "process already gone" are logged, not fatal: shutdown
// must proceed regardless.
func signalChildren(sig os.Signal) {
	s, ok := sig.(syscall.Signal)
	if !ok {
		return
	}
	children := childProcs()
	if len(children) == 0 {
		signalChildHandle(s)
		return
	}
	fgPgrp := termForegroundPgrp()
	for _, c := range children {
		if !shouldForward(s, c.pgrp, fgPgrp) {
			continue
		}
		if err := signalPID(c.pid, s); err != nil && !errors.Is(err, os.ErrProcessDone) && !errors.Is(err, syscall.ESRCH) {
			slog.Warn("failed to signal child", "pid", c.pid, "err", err)
		}
	}
}

// signalChildHandle signals the agent through the handle Exec/ExecMITM
// published, for the case where the process table is not walkable. A
// terminal-generated SIGINT is already delivered by the kernel to the whole
// foreground group the agent shares, and without /proc there is no way to tell
// that apart from `kill -INT msc`, so SIGINT is left to the kernel; every other
// signal is sent exactly once, since the terminal only ever generates SIGINT.
func signalChildHandle(sig syscall.Signal) {
	if sig == syscall.SIGINT {
		return
	}
	p := agents.Child()
	if p == nil {
		return
	}
	if err := signalAgent(p, sig); err != nil && !errors.Is(err, os.ErrProcessDone) {
		slog.Warn("failed to signal agent", "pid", p.Pid, "err", err)
	}
}

// logf prints a human-friendly message to stderr with the msc: prefix.
func logf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "msc: "+format+"\n", args...)
}

// logerr prints a human-friendly error to stderr with the msc: error: prefix.
func logerr(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "msc: error: "+format+"\n", args...)
}

// containsStr reports whether s appears in list.
func containsStr(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

// closestMatch returns the best match from candidates if it's within a
// reasonable edit distance (<=2), or "" if nothing is close enough. Used
// for "did you mean?" suggestions on typos.
func closestMatch(input string, candidates []string) string {
	best := ""
	bestDist := 3 // only suggest if distance <= 2
	for _, c := range candidates {
		d := levenshtein(input, c)
		if d < bestDist {
			bestDist = d
			best = c
		}
	}
	return best
}

func levenshtein(a, b string) int {
	ra, rb := []rune(a), []rune(b)
	if len(ra) == 0 {
		return len(rb)
	}
	if len(rb) == 0 {
		return len(ra)
	}
	prev := make([]int, len(rb)+1)
	curr := make([]int, len(rb)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(ra); i++ {
		curr[0] = i
		for j := 1; j <= len(rb); j++ {
			cost := 1
			if ra[i-1] == rb[j-1] {
				cost = 0
			}
			curr[j] = min(curr[j-1]+1, min(prev[j]+1, prev[j-1]+cost))
		}
		prev, curr = curr, prev
	}
	return prev[len(rb)]
}
