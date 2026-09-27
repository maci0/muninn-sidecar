package proxy

import "github.com/maci0/muninn-sidecar/internal/clock"

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
