package clock

import (
	"sync"
	"time"
)

// Fake is a manually advanced Clock: time moves only when Advance is called, and
// every timer registered in between fires on the script's schedule. It is the
// substitute a test or a simulator substitutes for SystemClock, so a run that
// depends on flush cycles, expiry windows, and retry backoff can be replayed
// step for step and reaches the same state every time.
//
// Like the standard library's tickers, a tick is dropped when the previous one
// has not been consumed yet, and Advance does not wait for the goroutine that
// receives it: a script that needs the effects of a tick to have landed must
// observe them (poll, or wait on a signal the driven code sets).
type Fake struct {
	mu     sync.Mutex
	now    time.Time
	timers []*fakeTimer
}

type fakeTimer struct {
	deadline time.Time
	period   time.Duration // > 0 for a ticker, 0 for a one-shot
	ch       chan time.Time
	fn       func()
	stopped  bool
}

// NewFake returns a Fake positioned at 2024-01-01T00:00:00Z.
func NewFake() *Fake {
	return &Fake{now: time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)}
}

// Now returns the scripted time.
func (f *Fake) Now() time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.now
}

// Since returns the scripted elapsed time since t.
func (f *Fake) Since(t time.Time) time.Duration { return f.Now().Sub(t) }

// Advance moves the scripted clock forward and fires every timer that comes
// due, in deadline order, repeating for tickers until the new time is passed.
func (f *Fake) Advance(d time.Duration) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.now = f.now.Add(d)
	for {
		var next *fakeTimer
		for _, t := range f.timers {
			if t.stopped || t.deadline.After(f.now) {
				continue
			}
			if next == nil || t.deadline.Before(next.deadline) {
				next = t
			}
		}
		if next == nil {
			return
		}
		if next.period > 0 {
			next.deadline = next.deadline.Add(next.period)
		} else {
			next.stopped = true
			f.dropLocked(next) // a one-shot never fires again; keeping it grows the scan below
		}
		if next.ch != nil {
			select {
			case next.ch <- f.now:
			default: // previous tick not consumed yet
			}
		}
		if next.fn != nil {
			next.fn()
		}
	}
}

// After returns a channel that receives once the scripted clock has advanced d.
func (f *Fake) After(d time.Duration) <-chan time.Time {
	ch := make(chan time.Time, 1)
	f.arm(&fakeTimer{ch: ch}, d)
	return ch
}

// PendingOneShots reports how many one-shot timers (After, AfterFunc) are armed
// and have not fired. A script that advances time to release one must wait for
// this first: a timer armed after the advance is due relative to the advanced
// time, so the advance that should have released it is spent and the timer
// never fires.
func (f *Fake) PendingOneShots() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, t := range f.timers {
		if t.period == 0 && !t.stopped {
			n++
		}
	}
	return n
}

// NewTicker returns a ticker that ticks once per period of scripted time.
func (f *Fake) NewTicker(d time.Duration) Ticker {
	t := &fakeTimer{
		period: d,
		ch:     make(chan time.Time, 1),
	}
	f.arm(t, d)
	return &fakeTicker{f: f, t: t}
}

// AfterFunc returns a timer that runs fn once the scripted clock has advanced d.
func (f *Fake) AfterFunc(d time.Duration, fn func()) Timer {
	t := &fakeTimer{fn: fn}
	f.arm(t, d)
	return &fakeTimerHandle{f: f, t: t}
}

// arm sets t's deadline from the scripted time and registers it in one critical
// section. A caller arms from its own goroutine (the store's worker arms a
// retry backoff) while the script advances from another, and splitting the two
// steps across two lock acquisitions left a window between them: an Advance
// landing there scanned a list the timer was not yet in, so the timer was
// registered with a deadline already past and missed the advance meant to
// release it. Armed atomically, a timer is either wholly visible to the scan
// that follows or wholly after it, with its deadline measured from the advanced
// time.
func (f *Fake) arm(t *fakeTimer, d time.Duration) {
	f.mu.Lock()
	defer f.mu.Unlock()
	t.deadline = f.now.Add(d)
	f.timers = append(f.timers, t)
}

func (f *Fake) stop(t *fakeTimer) {
	f.mu.Lock()
	defer f.mu.Unlock()
	t.stopped = true
	f.dropLocked(t)
}

// dropLocked removes a timer that will never fire again from the pending set, so
// a test that starts and stops timers does not leave every retired one behind
// for Advance to scan on each tick. Callers hold f.mu.
func (f *Fake) dropLocked(t *fakeTimer) {
	for i, cur := range f.timers {
		if cur == t {
			f.timers = append(f.timers[:i], f.timers[i+1:]...)
			return
		}
	}
}

type fakeTicker struct {
	f *Fake
	t *fakeTimer
}

func (s *fakeTicker) C() <-chan time.Time { return s.t.ch }

func (s *fakeTicker) Stop() { s.f.stop(s.t) }

type fakeTimerHandle struct {
	f *Fake
	t *fakeTimer
}

func (s *fakeTimerHandle) Stop() { s.f.stop(s.t) }
