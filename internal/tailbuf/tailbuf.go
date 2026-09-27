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

// Buffer accumulates the tail of a stream that may outgrow any fixed budget.
// Writes past the limit drop the oldest bytes. It is not safe for concurrent
// use; exec.Cmd writes to it from a single goroutine.
type Buffer struct {
	buf   []byte
	limit int
}

// New returns a Buffer keeping at most limit bytes.
func New(limit int) *Buffer { return &Buffer{limit: limit} }

func (b *Buffer) Write(p []byte) (int, error) {
	b.buf = append(b.buf, p...)
	if len(b.buf) > b.limit {
		b.buf = append(b.buf[:0], b.buf[len(b.buf)-b.limit:]...)
	}
	return len(p), nil
}

func (b *Buffer) String() string { return string(b.buf) }

func (b *Buffer) Len() int { return len(b.buf) }
