// Package clirun runs a child agent CLI (claude -p, codex exec, grok -p, …)
// with the two bounds a caller would otherwise have to re-derive every time:
// output that cannot grow the parent's heap without limit, and a timeout that
// takes the child's whole process group down with it.
//
// A judge, rewriter, or answer agent is a black box on the other end of a
// prompt. It prints reasoning traces and banners of unpredictable size, and it
// spawns helpers of its own. So the parent's side must not be unbounded on
// either count: cap the captured output, and signal the process group rather
// than the direct child, or a timeout orphans grandchildren that outlive the
// call.
package clirun

import (
	"context"
	"os/exec"
	"strings"
	"time"
)

// maxOutput caps the captured output. Replies are a few lines (verdicts,
// subqueries, one answer span), so a real output is tiny; the cap stops a
// runaway or misbehaving agent from growing the parent's heap without bound.
// Writes past the limit drop the oldest bytes: these agents print their
// reasoning first and the payload last, so keeping the tail preserves what
// callers parse, where an uncapped buffer would instead exhaust memory.
const maxOutput = 4 << 20 // 4 MiB

// Run executes argv and returns its standard output, or an error if the child
// could not be run. argv[0] is the binary; a non-empty stdin is delivered to
// the child on its standard input. Standard error is discarded, as it was at
// every call site before this package existed.
//
// Output is returned even alongside a non-nil error when the child produced
// any: these agents routinely exit non-zero on warnings and still print a
// usable answer, and a caller that only saw the error would discard it. A
// truncated output keeps the tail, so the last line — the answer, the verdicts,
// the subqueries — survives a child that outran the cap.
func Run(ctx context.Context, argv []string, stdin string, timeout time.Duration) (string, error) {
	if len(argv) == 0 {
		return "", nil
	}
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cmd := exec.CommandContext(cctx, argv[0], argv[1:]...)
	IsolateProcessGroup(cmd)
	if stdin != "" {
		cmd.Stdin = strings.NewReader(stdin)
	}
	out := &tailBuffer{limit: maxOutput}
	cmd.Stdout = out
	err := cmd.Run()
	if out.Len() == 0 {
		return "", err
	}
	return out.String(), err
}

// SplitCommand turns a user-supplied command line ("claude -p", or an agent
// installed under a path with a space in it) into argv for Run. Whitespace
// separates fields unless it sits inside single or double quotes, which are
// removed; that is the only quoting a user needs to name a binary, and it is
// what Windows paths require, since C:\Program Files\... has to be written
// "C:\Program Files\..." to survive as one argv[0].
//
// Backslash is not an escape character: on Windows it is a path separator, so
// honoring shell-style backslash escapes would corrupt the very paths the
// quoting exists to protect. An unterminated quote takes the rest of the
// string as the field it opened.
func SplitCommand(cmd string) []string {
	var (
		argv    []string
		field   strings.Builder
		quote   byte
		started bool
	)
	flush := func() {
		if started {
			argv = append(argv, field.String())
			field.Reset()
			started = false
		}
	}
	for i := 0; i < len(cmd); i++ {
		c := cmd[i]
		switch {
		case quote != 0:
			if c == quote {
				quote = 0
			} else {
				field.WriteByte(c)
			}
		case c == '\'' || c == '"':
			quote = c
			started = true
		case c == ' ' || c == '\t' || c == '\n' || c == '\r':
			flush()
		default:
			field.WriteByte(c)
			started = true
		}
	}
	flush()
	return argv
}

// IsolateProcessGroup puts the child in its own process group and replaces the
// kill-on-timeout that exec.CommandContext installs with one that signals the
// whole group. A CLI agent spawns helpers of its own; killing only the direct
// child on timeout orphans those helpers, and they outlive the call that
// spawned them. No-op where process groups are not available: the child keeps
// the default kill-the-direct-child behavior on timeout.
func IsolateProcessGroup(cmd *exec.Cmd) {
	if !setProcessGroup(cmd) {
		return
	}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		return killGroup(cmd.Process.Pid)
	}
}

// tailBuffer accumulates the tail of a stream that may outgrow any fixed
// budget. Writes past the limit drop the oldest bytes. It is not safe for
// concurrent use; exec.Cmd writes to it from a single goroutine.
type tailBuffer struct {
	buf   []byte
	limit int
}

func (t *tailBuffer) Write(p []byte) (int, error) {
	t.buf = append(t.buf, p...)
	if len(t.buf) > t.limit {
		t.buf = append(t.buf[:0], t.buf[len(t.buf)-t.limit:]...)
	}
	return len(p), nil
}

func (t *tailBuffer) String() string { return string(t.buf) }

func (t *tailBuffer) Len() int { return len(t.buf) }
