// Package reqid mints and carries the per-request correlation ID that ties one
// agent turn's log lines together.
//
// The ID is minted at the proxy's ingress and read back on every stage that
// logs about that request: enrichment, upstream call, capture, and the store's
// background worker, which sees the exchange only as a value handed to it
// through a channel. Without it in the context, a store warning cannot be tied
// to the turn that produced it, because a session interleaves many turns
// through one worker goroutine.
package reqid

import (
	"context"
	"strconv"
	"sync/atomic"
)

type ctxKey struct{}

// seq mints correlation IDs. A process-wide counter is enough: msc is one
// process per session, and an ID only has to be unique within a log.
var seq atomic.Uint64

// Field is the structured-logging key every line carrying a correlation ID
// uses. One name across all packages, so a single grep in the log stream
// returns the whole turn.
const Field = "request_id"

// Next mints a short, process-unique correlation ID.
func Next() string {
	return "req-" + strconv.FormatUint(seq.Add(1), 36)
}

// With attaches a correlation ID to a context.
func With(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, ctxKey{}, id)
}

// From returns the correlation ID carried by ctx, or "" when unset. The empty
// result is deliberately a legal log value: a package that logs on a path
// reachable both with and without an ID (a CLI, a background job) should say
// nothing rather than guess.
func From(ctx context.Context) string {
	id, _ := ctx.Value(ctxKey{}).(string)
	return id
}
