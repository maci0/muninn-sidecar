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

// TestStreamCapKeepsValidUTF8 walks the text-accumulator cap end to end: a delta
// large enough to overshoot maxTextAccum must leave the accumulated text valid
// UTF-8, since a partial rune would be stored as a replacement character.
func TestStreamCapKeepsValidUTF8(t *testing.T) {
	sc := &streamCapture{ctx: &captureCtx{}}
	for i := 0; i < 64; i++ {
		sc.processChunk([]byte("data: {\"type\":\"content_block_delta\",\"delta\":{\"text\":\"日本語テキスト\"}}\n"))
	}
	if !utf8.ValidString(sc.textAccum.String()) {
		t.Fatal("accumulated text is not valid UTF-8")
	}
	if got := sc.textAccum.Len(); got > maxTextAccum {
		t.Fatalf("accumulated %d bytes, cap is %d", got, maxTextAccum)
	}
	if n := len(sc.toolNames); n != 0 {
		t.Fatalf("collected %d tool names from text-only deltas", n)
	}
	if !strings.Contains(sc.textAccum.String(), "日本語") {
		t.Fatal("accumulated text lost its multibyte content")
	}
}
