package proxy

import (
	"context"
	"log/slog"
	"net/http"
	"time"

	"github.com/maci0/muninn-sidecar/internal/reqid"
)

// requestRecorder wraps the response writer to remember the status code the
// turn settled on, and whether the response was already on the wire when it
// ended. A panic handler needs the second to know if it can still answer, and
// the access line needs the first.
//
// The wrapper is transparent: Unwrap lets http.ResponseController (which the
// stdlib reverse proxy uses to flush and to hijack an upgraded connection)
// reach the real writer through it, so recording a status cannot cost the
// pipeline SSE flushing or WebSocket upgrades.
type requestRecorder struct {
	http.ResponseWriter
	status    int
	committed bool
}

func newRequestRecorder(w http.ResponseWriter) *requestRecorder {
	return &requestRecorder{ResponseWriter: w}
}

func (r *requestRecorder) Unwrap() http.ResponseWriter { return r.ResponseWriter }

func (r *requestRecorder) WriteHeader(code int) {
	r.status = code
	r.committed = true
	r.ResponseWriter.WriteHeader(code)
}

// Write records the implicit 200 of a handler that writes a body without
// calling WriteHeader, which is how the stdlib reverse proxy serves a
// response it copied straight from upstream.
func (r *requestRecorder) Write(p []byte) (int, error) {
	r.committed = true
	if r.status == 0 {
		r.status = http.StatusOK
	}
	return r.ResponseWriter.Write(p)
}

// statusCode is the status the turn ended with, defaulting to 200 for a
// handler that returned without writing anything at all (the reverse proxy
// answers the status endpoint, and a cancelled turn writes no response).
func (r *requestRecorder) statusCode() int {
	if r.status == 0 {
		return http.StatusOK
	}
	return r.status
}

// logTurn is the per-turn access line: one line saying whether the turn
// succeeded, how long it took, and under which correlation ID every other line
// about it can be found. It is the join key between the log stream and the
// store counters on the status endpoint, and the only place a successful turn's
// duration is recorded.
//
// Info, not Debug, and not once per byte: the unit here is an LLM turn, which
// the agent spaces seconds to minutes apart, so one line per turn costs an
// operator nothing to read and a few hundred bytes to store. Debug stays the
// per-stage trace (injection, capture, upstream rewrite) for the turn being
// diagnosed.
//
// A panicked turn already logged an error carrying the same correlation ID;
// this line repeats the outcome so a filter on either one finds the other.
//
// 5xx and panics escalate to Error because a captured path that fails already
// logged the cause (the upstream-error warning), and an uncaptured one that
// fails is otherwise silent. 4xx stays Info: the provider-side ones that
// matter are already warned, and the rest are the agent's own bad requests,
// which an operator cannot act on.
func (p *Proxy) logTurn(ctx context.Context, method, path string, status int, elapsed time.Duration, panicked bool) {
	level := slog.LevelInfo
	if panicked || status >= http.StatusInternalServerError {
		level = slog.LevelError
	}
	slog.Log(ctx, level, "turn", reqid.Field, requestID(ctx), "method", method,
		"path", path, "status", status, "duration_ms", elapsed.Milliseconds(),
		"agent", p.agentName)
}

// logTunnel is the outcome line for a connection msc hijacks and forwards
// itself: a CONNECT tunnel (plain or intercepted) or a spliced protocol
// upgrade. Neither is a request, so neither gets a "turn" line, and the
// requests decrypted inside a tunnel log their own turns. This is therefore the
// only line saying that the connection itself opened, that the agent could use
// it, how long it lived, and which stage refused it.
//
// Every stage inside those paths logged at Debug, which is off by default, so a
// tunnel the agent could not use (a CA it does not trust, a host it cannot
// reach) left nothing at the level an operator runs at: the session summary
// showed a rising request count and no turn line to account for it.
//
// Info once the connection was established and lived out its life, Error when
// it never got that far. reason carries the stage that gave up, so the Error
// needs no second lookup through the Debug lines.
func (p *Proxy) logTunnel(ctx context.Context, kind, target string, elapsed time.Duration, reason string) {
	level := slog.LevelInfo
	attrs := []any{reqid.Field, requestID(ctx), "kind", kind, "target", target,
		"duration_ms", elapsed.Milliseconds(), "agent", p.agentName}
	if reason != "" {
		level = slog.LevelError
		attrs = append(attrs, "reason", reason)
	}
	slog.Log(ctx, level, "tunnel", attrs...)
}
