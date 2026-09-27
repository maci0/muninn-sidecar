// Package clock provides the project's single source of wall-clock time and
// timers. Anything whose behavior depends on *when* it runs reads time through a
// Clock, so a test or a simulator can drive a run on scripted time instead of
// the host's, and replay it step for step. Production passes SystemClock.
package clock

import "time"

// Ticker delivers ticks on a fixed period until stopped.
type Ticker interface {
	// C is the tick channel. It carries one tick per period, like time.Ticker.
	C() <-chan time.Time
	Stop()
}

// Timer fires its function once, unless stopped first.
type Timer interface {
	Stop()
}

// Clock is the time source: reads, elapsed time, and the three timer kinds the
// sidecar's background work depends on. Time-based behavior belongs behind this
// interface so a run's timing is a function of the script, not of the machine.
type Clock interface {
	Now() time.Time
	Since(time.Time) time.Duration
	// After returns a channel that receives once d has elapsed.
	After(time.Duration) <-chan time.Time
	NewTicker(time.Duration) Ticker
	AfterFunc(time.Duration, func()) Timer
}

// SystemClock reads the host wall clock and monotonic elapsed time.
type SystemClock struct{}

func (SystemClock) Now() time.Time { return time.Now() }

func (SystemClock) Since(t time.Time) time.Duration { return time.Since(t) }

func (SystemClock) After(d time.Duration) <-chan time.Time { return time.After(d) }

func (SystemClock) NewTicker(d time.Duration) Ticker { return &systemTicker{t: time.NewTicker(d)} }

func (SystemClock) AfterFunc(d time.Duration, f func()) Timer {
	return &systemTimer{t: time.AfterFunc(d, f)}
}

type systemTimer struct{ t *time.Timer }

func (s *systemTimer) Stop() { s.t.Stop() }

type systemTicker struct{ t *time.Ticker }

func (s *systemTicker) C() <-chan time.Time { return s.t.C }

func (s *systemTicker) Stop() { s.t.Stop() }
