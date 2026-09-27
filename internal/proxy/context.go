package proxy

import (
	"context"
	"strconv"
	"sync/atomic"
	"time"
)

// ctxKey is the context key for capture metadata. Using an unexported struct
// type avoids collisions with other packages that use context values.
type ctxKey struct{}

// reqIDKey is the context key for the per-request correlation ID.
type reqIDKey struct{}

// reqSeq mints correlation IDs. A process-wide counter is enough: msc is one
// process per session, and an ID only has to be unique within a log.
var reqSeq atomic.Uint64

// nextRequestID mints a short, process-unique correlation ID. Every request
// gets one, and every log line the request path emits carries it, so the
// inject failure, the upstream error, and the store drop belonging to one
// agent turn can be told apart from the same lines of a concurrent turn.
func nextRequestID() string {
	return "req-" + strconv.FormatUint(reqSeq.Add(1), 36)
}

// withRequestID attaches a correlation ID to a request context.
func withRequestID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, reqIDKey{}, id)
}

// requestID returns the correlation ID of ctx, or "" when unset.
func requestID(ctx context.Context) string {
	id, _ := ctx.Value(reqIDKey{}).(string)
	return id
}

// captureCtx carries request metadata through the reverse proxy pipeline:
// ServeHTTP → instrument → rewrite → upstream → captureResponse. The request
// body is buffered in instrument before the reverse proxy forwards it, since the
// body stream can only be read once; the MITM path buffers there too.
type captureCtx struct {
	id      string    // correlation ID, also carried on the request context
	start   time.Time // request arrival time (for duration calculation)
	method  string    // HTTP method (GET, POST, etc.)
	path    string    // original request path
	reqBody []byte    // buffered request body, as sent by the agent
	agent   string    // agent name for MuninnDB tagging
}

// withCapture attaches capture metadata to a request context.
func withCapture(ctx context.Context, c *captureCtx) context.Context {
	return context.WithValue(ctx, ctxKey{}, c)
}

// captureFromContext retrieves capture metadata, or nil if absent.
func captureFromContext(ctx context.Context) *captureCtx {
	c, _ := ctx.Value(ctxKey{}).(*captureCtx)
	return c
}
