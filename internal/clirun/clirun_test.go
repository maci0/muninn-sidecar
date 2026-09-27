package clirun

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

// A judge that prints without bound must not be able to grow the caller's heap
// without limit, and the payload it prints last is what every caller parses, so
// the cap has to keep the tail.
func TestRunKeepsTailWithinLimit(t *testing.T) {
	if _, err := os.Stat("/bin/sh"); err != nil {
		t.Skip("no /bin/sh")
	}
	// 5 MiB of filler (past the 4 MiB cap) then the answer, on one line.
	argv := []string{"/bin/sh", "-c",
		"head -c 5242880 /dev/zero | tr '\\0' 'a'; printf '\\nTHE-ANSWER\\n'"}
	out, err := Run(context.Background(), argv, "", 30*time.Second)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(out) != maxOutput {
		t.Errorf("captured %d bytes, want the %d-byte cap", len(out), maxOutput)
	}
	if !strings.HasSuffix(out, "\nTHE-ANSWER\n") {
		t.Errorf("output does not end in the answer the child printed last: %q", last(out, 40))
	}
}

// A CLI agent spawns helpers of its own. On timeout the whole process group has
// to go down: killing only the direct child orphans the grandchild, which then
// outlives the call that spawned it.
func TestRunKillsProcessGroupOnTimeout(t *testing.T) {
	if _, err := os.Stat("/bin/sh"); err != nil {
		t.Skip("no /bin/sh")
	}
	// The grandchild appends a line to a file, then sleeps far past the timeout.
	// If it survives the kill it appends a second line.
	marker := filepath.Join(t.TempDir(), "grandchild")
	script := "( sleep 2; echo alive >> " + marker + " ) & sleep 60"
	if _, err := Run(context.Background(), []string{"/bin/sh", "-c", script}, "", 200*time.Millisecond); err == nil {
		t.Fatal("Run returned no error for a child killed by the timeout")
	}
	time.Sleep(3 * time.Second)
	if b, err := os.ReadFile(marker); err == nil {
		t.Errorf("grandchild survived the group kill and wrote %q", b)
	}
}

// A child that overruns the cap mid-character must not leave the retained tail
// starting inside a multi-byte rune: every caller parses the capture, and a head
// that decodes to a replacement character corrupts a JSON verdict line and shows
// up as mojibake in the parsed text.
func TestRunKeepsTailOnRuneBoundary(t *testing.T) {
	if _, err := os.Stat("/bin/sh"); err != nil {
		t.Skip("no /bin/sh")
	}
	// 3-byte CJK filler past the 4 MiB cap, then the verdict. 4 MiB is not a
	// multiple of 3, so a byte-wise tail necessarily starts mid-character.
	argv := []string{"/bin/sh", "-c",
		"head -c 5242880 /dev/zero | tr '\\0' '\\xe4'; printf 'VERDICT-1\\n'"}
	out, err := Run(context.Background(), argv, "", 30*time.Second)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !utf8.ValidString(out) {
		t.Errorf("captured tail is not valid UTF-8, head = %q", first(out, 8))
	}
	if !strings.HasSuffix(out, "VERDICT-1\n") {
		t.Errorf("output does not end in the verdict the child printed last: %q", last(out, 40))
	}
}

// A child that exits non-zero but still printed the answer must yield the
// output, not just the error: these agents exit non-zero on warnings.
func TestRunReturnsOutputOnNonZeroExit(t *testing.T) {
	if _, err := os.Stat("/bin/sh"); err != nil {
		t.Skip("no /bin/sh")
	}
	out, err := Run(context.Background(), []string{"/bin/sh", "-c", "echo answer; exit 3"}, "", 10*time.Second)
	if err == nil {
		t.Error("err = nil, want the child's non-zero exit")
	}
	if out != "answer\n" {
		t.Errorf("out = %q, want %q", out, "answer\n")
	}
}

// The prompt may be large (a query plus recalled passages), so it goes on the
// child's stdin and must arrive intact.
func TestRunDeliversStdin(t *testing.T) {
	if _, err := os.Stat("/bin/sh"); err != nil {
		t.Skip("no /bin/sh")
	}
	prompt := "line one\nline two " + strings.Repeat("x", 100_000)
	out, err := Run(context.Background(), []string{"/bin/sh", "-c", "cat"}, prompt, 10*time.Second)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if out != prompt {
		t.Errorf("child read %d bytes, want the %d-byte prompt", len(out), len(prompt))
	}
}

// A user-supplied command line has to survive a binary installed under a path
// with a space in it, which is the norm on Windows (C:\Program Files\...) and
// common elsewhere. Whitespace splits fields; quoted whitespace does not.
func TestSplitCommand(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want []string
	}{
		{"plain", "claude -p", []string{"claude", "-p"}},
		{"extra spaces", "  claude   -p  ", []string{"claude", "-p"}},
		{"windows program files", `"C:\Program Files\Git\bin\bash.exe" -c "echo hi"`,
			[]string{`C:\Program Files\Git\bin\bash.exe`, "-c", "echo hi"}},
		{"an unquoted path with a space still splits", `C:\Program Files\bash.exe`,
			[]string{`C:\Program`, `Files\bash.exe`}},
		{"double quotes hold a path with spaces", `"/opt/my agent/bin/agent" --flag`,
			[]string{"/opt/my agent/bin/agent", "--flag"}},
		{"single quotes hold a space", `agent -p 'what is x'`,
			[]string{"agent", "-p", "what is x"}},
		{"empty quoted field is kept", `agent "" x`, []string{"agent", "", "x"}},
		{"unterminated quote takes the rest", `agent "oops`, []string{"agent", "oops"}},
		{"empty", "   ", nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := SplitCommand(tt.in)
			if !slices.Equal(got, tt.want) {
				t.Errorf("SplitCommand(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

// A command named with a quoted path must run, end to end: the split is only
// correct if exec gets the path whole.
func TestSplitCommandRunsQuotedPath(t *testing.T) {
	if _, err := os.Stat("/bin/sh"); err != nil {
		t.Skip("no /bin/sh")
	}
	dir := t.TempDir()
	bin := filepath.Join(dir, "my agent")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\necho ran\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	argv := SplitCommand(`"` + bin + `" -x`)
	out, err := Run(context.Background(), argv, "", 10*time.Second)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if out != "ran\n" {
		t.Errorf("out = %q, want %q", out, "ran\n")
	}
}

func last(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[len(s)-n:]
}

func first(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

// IsolateProcessGroup must leave a command that never started cancellable
// without panicking, which is the state exec.CommandContext kills from.
func TestIsolateProcessGroupWithoutStart(t *testing.T) {
	cmd := exec.Command("true")
	IsolateProcessGroup(cmd)
	if cmd.Cancel == nil {
		t.Skip("process groups unavailable on this platform")
	}
	if err := cmd.Cancel(); err != nil {
		t.Errorf("Cancel on an unstarted command: %v", err)
	}
}
