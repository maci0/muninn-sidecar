package report

import "testing"

func TestTrunc(t *testing.T) {
	if Trunc("abcdef", 4) != "abc…" || Trunc("ab", 5) != "ab" {
		t.Errorf("Trunc")
	}
	// A report label is external text: a byte budget cuts a multi-byte
	// character in half and the column ends in a replacement character.
	if got := Trunc("日本語モデル", 4); got != "日本語…" {
		t.Errorf("Trunc(CJK) = %q, want %q", got, "日本語…")
	}
	if got := Trunc("a😀b", 2); got != "a…" {
		t.Errorf("Trunc(emoji) = %q, want %q", got, "a…")
	}
	if got := Trunc("ab", 1); got != "…" {
		t.Errorf("Trunc(n=1) = %q, want %q", got, "…")
	}
}
