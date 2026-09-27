package proxy

import (
	"net/http"
	"time"
)

// writeIdleTimeout bounds the gap between two writes of one response before the
// agent's connection is cut. An SSE turn streams a delta every few seconds for
// as long as the model thinks, so the meaningful failure is a stream that stops
// entirely, not a stream that runs long: the agent's read must fail rather than
// hang forever when the upstream goes quiet mid-response. Generous, because a
// reasoning model between two deltas is legitimately slow.
//
// This is a per-response idle bound, and it replaces http.Server.WriteTimeout,
// which Go arms once when the request headers are read and never extends: an
// absolute cap that truncates any response, however healthy, that outlives it
// (see Proxy's server configuration).
const writeIdleTimeout = 5 * time.Minute

// idleDeadlineWriter pushes the connection's write deadline forward on every
// write and every flush, so a response that keeps producing bytes is never cut
// off for taking a long time, while one that stalls is.
//
// http.ResponseController reaches the underlying connection through Unwrap, so
// the wrapper stays transparent to the stdlib reverse proxy's flushing and to
// anything else that type-asserts the writer. SetWriteDeadline is best-effort:
// a writer with no deadline support (a recorder, a wrapper that does not
// implement it) simply forwards the response unbounded.
type idleDeadlineWriter struct {
	http.ResponseWriter
	rc   *http.ResponseController
	idle time.Duration
}

// newIdleDeadlineWriter wraps w so each write and flush extends the write
// deadline to idle from now.
func newIdleDeadlineWriter(w http.ResponseWriter, idle time.Duration) *idleDeadlineWriter {
	return &idleDeadlineWriter{ResponseWriter: w, rc: http.NewResponseController(w), idle: idle}
}

// Unwrap exposes the wrapped writer, which http.ResponseController uses to reach
// the connection's deadline and flush support.
func (w *idleDeadlineWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// WriteHeader arms the deadline as the response starts, before any body byte
// exists. Without it a response that sends headers and then stalls upstream
// would hold the agent's read open with no deadline at all: the one case the
// bound exists for.
func (w *idleDeadlineWriter) WriteHeader(code int) {
	w.extend()
	w.ResponseWriter.WriteHeader(code)
}

// Write forwards the response bytes, extending the deadline first.
func (w *idleDeadlineWriter) Write(p []byte) (int, error) {
	w.extend()
	return w.ResponseWriter.Write(p)
}

// FlushError implements the flush interface http.ResponseController prefers over
// http.Flusher, so a flush extends the deadline too. Without it the reverse
// proxy's per-write flush (FlushInterval -1) would reach the connection through
// Unwrap and leave the deadline where the last Write put it.
func (w *idleDeadlineWriter) FlushError() error {
	w.extend()
	return w.rc.Flush()
}

// extend arms the write deadline one idle window ahead of now.
func (w *idleDeadlineWriter) extend() {
	_ = w.rc.SetWriteDeadline(time.Now().Add(w.idle))
}
