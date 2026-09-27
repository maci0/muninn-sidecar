package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestMITMCADir(t *testing.T) {
	// Pin a config home so the test is deterministic and writes nowhere global.
	tmp := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", tmp)

	dir, err := mitmCADir()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(dir, tmp) {
		t.Errorf("CA dir %q not under config home %q", dir, tmp)
	}
	if filepath.Base(dir) != "mitm" {
		t.Errorf("expected dir to end in .../mitm, got %q", dir)
	}
	fi, err := os.Stat(dir)
	if err != nil {
		t.Fatalf("CA dir not created: %v", err)
	}
	if !fi.IsDir() {
		t.Error("CA dir is not a directory")
	}
	if perm := fi.Mode().Perm(); perm != 0o700 {
		t.Errorf("CA dir perms = %v, want 0700", perm)
	}
	// Idempotent: a second call returns the same path without error.
	dir2, err := mitmCADir()
	if err != nil || dir2 != dir {
		t.Errorf("second call: dir=%q err=%v, want %q", dir2, err, dir)
	}
}

// The failure path is the untested half of mitmCADir, and it decides where a
// TLS-MITM private key would be written. With no config dir and no home there
// is no safe location, so the call must fail loudly rather than pick one.
func TestMITMCADirWithoutConfigDirOrHome(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", "")
	t.Setenv("HOME", "")
	dir, err := mitmCADir()
	if err == nil {
		t.Fatalf("mitmCADir must fail with neither a config dir nor a home, got %q", dir)
	}
	if dir != "" {
		t.Errorf("failed lookup must not return a path, got %q", dir)
	}
	// The message has to say the CA directory could not be located, so the
	// operator knows it is an environment problem and not a bad flag.
	if !strings.Contains(err.Error(), "config directory") {
		t.Errorf("error must name the config-directory lookup, got %v", err)
	}
}

// requireProc skips the test on platforms without a /proc filesystem, where
// childProcs returns nothing and signal forwarding falls back to the agent
// process handle.
func requireProc(t *testing.T) {
	t.Helper()
	if _, err := os.Stat("/proc/self/stat"); err != nil {
		t.Skipf("no /proc on this platform: %v", err)
	}
}

func TestTermForegroundPgrp(t *testing.T) {
	requireProc(t)
	// With a controlling terminal tpgid is a positive pgrp; without one the
	// kernel reports -1. Zero would mean a parse bug.
	if got := termForegroundPgrp(); got == 0 {
		t.Errorf("termForegroundPgrp() = 0, want positive pgrp or -1")
	}
}

func TestShouldForward(t *testing.T) {
	cases := []struct {
		name         string
		sig          syscall.Signal
		pgrp, fgPgrp int
		want         bool
	}{
		{"Ctrl+C already hit foreground child", syscall.SIGINT, 100, 100, false},
		{"SIGINT to child outside foreground group", syscall.SIGINT, 100, 200, true},
		{"SIGINT with no controlling terminal", syscall.SIGINT, 100, -1, true},
		{"SIGTERM always forwarded", syscall.SIGTERM, 100, 100, true},
		{"SIGKILL always forwarded", syscall.SIGKILL, 100, 100, true},
	}
	for _, c := range cases {
		if got := shouldForward(c.sig, c.pgrp, c.fgPgrp); got != c.want {
			t.Errorf("%s: shouldForward(%v, %d, %d) = %v, want %v",
				c.name, c.sig, c.pgrp, c.fgPgrp, got, c.want)
		}
	}
}

// TestSignalChildren verifies the shutdown fix: a signal delivered only to
// msc's PID (kill, docker stop) is forwarded to the launched agent instead of
// leaving it orphaned against a dead proxy.
func TestSignalChildren(t *testing.T) {
	requireProc(t)
	cmd := exec.Command("sleep", "30")
	if err := cmd.Start(); err != nil {
		t.Fatalf("start child: %v", err)
	}
	signalChildren(syscall.SIGTERM)
	waitCh := make(chan error, 1)
	go func() { waitCh <- cmd.Wait() }()
	select {
	case err := <-waitCh:
		if err == nil {
			t.Error("expected non-nil error from a SIGTERM-terminated child")
		}
	case <-time.After(5 * time.Second):
		_ = cmd.Process.Kill()
		<-waitCh
		t.Fatal("child did not exit after forwarded SIGTERM")
	}
}

func TestLevenshtein(t *testing.T) {
	cases := []struct {
		a, b string
		want int
	}{
		{"", "", 0},
		{"abc", "abc", 0},
		{"abc", "abd", 1},
		{"kitten", "sitting", 3},
		{"", "abc", 3},
		{"abc", "", 3},
	}
	for _, c := range cases {
		if got := levenshtein(c.a, c.b); got != c.want {
			t.Errorf("levenshtein(%q,%q)=%d want %d", c.a, c.b, got, c.want)
		}
	}
}

func TestClosestMatch(t *testing.T) {
	cands := []string{"claude", "qwen", "codex", "status"}
	if got := closestMatch("clade", cands); got != "claude" {
		t.Errorf("typo: got %q", got)
	}
	if got := closestMatch("statuss", cands); got != "status" {
		t.Errorf("typo: got %q", got)
	}
	// Too far from anything → no suggestion.
	if got := closestMatch("xyzzyplugh", cands); got != "" {
		t.Errorf("far input should give no suggestion, got %q", got)
	}
	if got := closestMatch("anything", nil); got != "" {
		t.Errorf("empty candidates should give no suggestion, got %q", got)
	}
}

func FuzzLevenshtein(f *testing.F) {
	f.Add("kitten", "sitting")
	f.Add("", "x")
	f.Fuzz(func(t *testing.T, a, b string) {
		d := levenshtein(a, b)
		if d < 0 {
			t.Fatalf("negative distance %d", d)
		}
		// Symmetry.
		if levenshtein(b, a) != d {
			t.Fatalf("asymmetric: %q,%q", a, b)
		}
	})
}

func FuzzClosestMatch(f *testing.F) {
	f.Add("clade")
	f.Fuzz(func(t *testing.T, s string) {
		_ = closestMatch(s, []string{"claude", "qwen", "codex"})
	})
}
