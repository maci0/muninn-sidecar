package proxy

import (
	"time"

	"github.com/maci0/muninn-sidecar/internal/clock"
)

// Clock is the clock the capture path reads; it is the project's single clock
// (internal/clock), so a simulator can drive request timestamps and durations
// from one script and replay a request sequence byte-for-byte
// (TestCaptureIsReplayableFromClock).
type Clock = clock.Clock

// SystemClock reads the host wall clock and is the default.
type SystemClock = clock.SystemClock

// clockOrSystem resolves c, falling back to the system clock when c is nil
// (a Proxy or streamCapture built directly rather than through New).
func clockOrSystem(c Clock) Clock {
	if c == nil {
		return SystemClock{}
	}
	return c
}

// socketDeadlineBase is the time base for every deadline armed on a live
// network connection: the tunnel handshake, the spliced WebSocket upgrade, the
// blind-tunnel idle window, and the per-response write idle bound.
//
// These four are the project's one timing decision an injected clock cannot
// script. A net.Conn deadline is not compared by this process: the runtime's
// netpoller holds it and fires against wall time, so a scripted timestamp
// (clock.Fake starts at 2024-01-01) is already years in the past when it
// arrives and the connection is torn down at once. Reading them from the
// injected clock would not make a run replayable, it would break every run
// that injects one, which is why they are named here instead of calling
// time.Now() at four sites.
//
// Scripting them needs a transport the simulator owns, in-memory conns whose
// deadlines are checked against clock.Fake rather than by the netpoller. Until
// that exists, this function is the documented seam: a simulated run controls
// the recorded timeline (capture timestamps, durations, cache ages, flush
// cycles, retry backoff) and the real sockets stay on the host clock.
func socketDeadlineBase() time.Time { return time.Now() }
