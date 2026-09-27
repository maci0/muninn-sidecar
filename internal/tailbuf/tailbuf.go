// Package tailbuf provides a fixed-size writer that keeps the tail of a
// stream, for capturing the output of a model call whose length nothing bounds.
//
// Every model call site here (the grounding judge, the bench rewriter, the QA
// answerer) shells out to or reads from a process or endpoint that may print
// far more than the few tokens the caller needs. A plain bytes.Buffer grows
// with it, so one chatty run costs the caller its heap. This keeps the last
// limit bytes instead: model and agent CLIs print their reasoning and banners
// before the answer, so the answer is in the tail, and a truncated capture
// still parses.
package tailbuf

import "unicode/utf8"

// Buffer accumulates the tail of a stream that may outgrow any fixed budget.
// Writes past the limit drop the oldest bytes. It is not safe for concurrent
// use; exec.Cmd writes to it from a single goroutine.
type Buffer struct {
	buf   []byte
	limit int
}

// New returns a Buffer keeping at most limit bytes. A negative limit is
// clamped to 0 (keep nothing) rather than slicing backwards on the first
// Write, which would panic.
func New(limit int) *Buffer {
	if limit < 0 {
		limit = 0
	}
	return &Buffer{limit: limit}
}

func (b *Buffer) Write(p []byte) (int, error) {
	b.buf = append(b.buf, p...)
	if len(b.buf) > b.limit {
		tail := b.buf[len(b.buf)-b.limit:]
		// Writes are arbitrary byte runs, so the kept tail can begin part-way
		// through a multi-byte character. Advance to the first complete rune so
		// String reports decodable text: a JSON parser reading the capture would
		// otherwise see invalid UTF-8 at the head.
		for len(tail) > 0 {
			r, size := utf8.DecodeRune(tail)
			if r == utf8.RuneError && size <= 1 {
				tail = tail[1:] // stray invalid byte
				continue
			}
			if !utf8.FullRune(tail) {
				tail = tail[1:] // truncated sequence
				continue
			}
			break
		}
		b.buf = append(b.buf[:0], tail...)
	}
	return len(p), nil
}

func (b *Buffer) String() string { return string(b.buf) }

func (b *Buffer) Len() int { return len(b.buf) }
