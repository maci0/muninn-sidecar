package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
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
