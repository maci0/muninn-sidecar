package tailbuf

import "testing"

// A judge or agent that prints without bound must not be able to grow the
// caller's heap without limit; the verdicts it prints last are what the parser
// needs, so the capped buffer has to keep the tail.
func TestBufferKeepsTailWithinLimit(t *testing.T) {
	tb := New(8)
	tb.Write([]byte("aaaa"))
	tb.Write([]byte("bbbbbb"))
	if tb.Len() != 8 {
		t.Fatalf("len=%d, want 8", tb.Len())
	}
	if got := tb.String(); got != "aabbbbbb" {
		t.Errorf("buffer=%q, want the newest 8 bytes %q", got, "aabbbbbb")
	}
	// A single write larger than the limit still yields exactly the limit.
	tb2 := New(4)
	tb2.Write([]byte("123456789"))
	if got := tb2.String(); got != "6789" {
		t.Errorf("buffer=%q, want %q", got, "6789")
	}
}
