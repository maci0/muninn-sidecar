package proxy

import (
	"context"
	"time"
)

// ctxKey is the context key for capture metadata. Using an unexported struct
// type avoids collisions with other packages that use context values.
type ctxKey struct{}

// captureCtx carries request metadata through the reverse proxy pipeline:
// ServeHTTP → rewrite → upstream → captureResponse. The request body is
// buffered in ServeHTTP before the reverse proxy forwards it, since the
// body stream can only be read once.
type captureCtx struct {
	start          time.Time // request arrival time (for duration calculation)
	method         string    // HTTP method (GET, POST, etc.)
	path           string    // original request path
	reqBody        []byte    // buffered request body
	agent          string    // agent name for MuninnDB tagging
	filterPatterns []string  // tool name patterns to strip from stored bodies
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
