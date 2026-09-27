// Package report holds the column formatting the msc report binaries share.
package report

// ellipsis marks a value Trunc shortened to fit its report column.
const ellipsis = "…"

// Trunc clips s to at most n characters, replacing the last one with an
// ellipsis. The budget is characters, counted as runes: a byte count would cut
// a multi-byte character in half, leaving a replacement character at the end of
// a non-ASCII label.
func Trunc(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	if n <= 1 {
		return ellipsis
	}
	return string(r[:n-1]) + ellipsis
}
