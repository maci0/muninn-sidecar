package main

import "testing"

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
	cands := []string{"claude", "gemini", "codex", "status"}
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
		_ = closestMatch(s, []string{"claude", "gemini", "codex"})
	})
}
