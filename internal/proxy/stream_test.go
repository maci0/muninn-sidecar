package proxy

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestClampBytes(t *testing.T) {
	const em = "héllo" // 5 runes, 6 bytes (é is two bytes)

	cases := []struct {
		name string
		in   string
		max  int
		want string
	}{
		{"under limit", "abc", 10, "abc"},
		{"at limit", "abc", 3, "abc"},
		{"ascii cut", "abcdef", 3, "abc"},
		{"split multi-byte rune", em, 2, "h"},
		{"clean multi-byte cut", em, 5, "héll"},
		{"zero", "abc", 0, ""},
		{"negative", "abc", -1, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := clampBytes(tc.in, tc.max)
			if got != tc.want {
				t.Fatalf("clampBytes(%q, %d) = %q, want %q", tc.in, tc.max, got, tc.want)
			}
			if len(got) > tc.max && tc.max > 0 {
				t.Fatalf("clampBytes(%q, %d) returned %d bytes", tc.in, tc.max, len(got))
			}
		})
	}
}

// TestStreamCapKeepsValidUTF8 walks the text-accumulator cap end to end: deltas
// whose total far exceeds maxTextAccum must leave the accumulated text valid
// UTF-8, since a partial rune would be stored as a replacement character.
//
// The loop must actually reach the cap: a 7-rune, 21-byte delta needs
// maxTextAccum/21 iterations to fill the accumulator, and the assertions below
// require the cap to be saturated, so deleting the clamp fails the test instead
// of leaving it green.
func TestStreamCapKeepsValidUTF8(t *testing.T) {
	const delta = "日本語テキスト" // 7 runes, 21 bytes, 3 bytes per rune
	// Two more iterations than the cap strictly needs, so the delta that lands
	// on the boundary is the one clamped mid-rune.
	iters := maxTextAccum/len(delta) + 2

	sc := &streamCapture{ctx: &captureCtx{}}
	for i := 0; i < iters; i++ {
		sc.processChunk([]byte(`data: {"type":"content_block_delta","delta":{"text":"` + delta + `"}}` + "\n"))
	}
	got := sc.textAccum.String()
	if !utf8.ValidString(got) {
		t.Fatalf("accumulated %d bytes is not valid UTF-8", len(got))
	}
	if len(got) > maxTextAccum {
		t.Fatalf("accumulated %d bytes, cap is %d", len(got), maxTextAccum)
	}
	// The cap must be saturated, and the clamp may only shed the bytes of one
	// partial trailing rune (3 bytes each here), never a whole delta.
	if slack := maxTextAccum - len(got); slack > len(delta) {
		t.Fatalf("accumulated %d bytes, %d short of the %d cap: the clamp never engaged",
			len(got), slack, maxTextAccum)
	}
	if n := utf8.RuneCountInString(got); n != len(got)/len("日") {
		t.Fatalf("accumulated %d runes from %d bytes: the cap split a multi-byte sequence", n, len(got))
	}
	if n := len(sc.toolNames); n != 0 {
		t.Fatalf("collected %d tool names from text-only deltas", n)
	}
	if !strings.Contains(sc.textAccum.String(), "日本語") {
		t.Fatal("accumulated text lost its multibyte content")
	}
}
