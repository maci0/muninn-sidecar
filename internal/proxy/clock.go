package proxy

import "time"

// Clock is the proxy's only source of wall-clock time. Every timestamp and
// duration written to a CapturedExchange comes from here, so a test or
// simulator can substitute a controllable clock and get byte-identical
// captures across runs. Production uses SystemClock.
type Clock interface {
	Now() time.Time
	Since(time.Time) time.Duration
}

// SystemClock reads the host wall clock and monotonic elapsed time.
type SystemClock struct{}

// Now returns the current wall-clock time.
func (SystemClock) Now() time.Time { return time.Now() }

// Since returns the time elapsed since t.
func (SystemClock) Since(t time.Time) time.Duration { return time.Since(t) }

// clockOrSystem resolves c, falling back to the system clock when c is nil
// (a Proxy or streamCapture built directly rather than through New).
func clockOrSystem(c Clock) Clock {
	if c == nil {
		return SystemClock{}
	}
	return c
}
