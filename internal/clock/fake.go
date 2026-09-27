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
	f.add(&fakeTimer{deadline: f.Now().Add(d), ch: ch})
	return ch
}

// NewTicker returns a ticker that ticks once per period of scripted time.
func (f *Fake) NewTicker(d time.Duration) Ticker {
	t := &fakeTimer{
		deadline: f.Now().Add(d),
		period:   d,
		ch:       make(chan time.Time, 1),
	}
	f.add(t)
	return &fakeTicker{f: f, t: t}
}

// AfterFunc returns a timer that runs fn once the scripted clock has advanced d.
func (f *Fake) AfterFunc(d time.Duration, fn func()) Timer {
	t := &fakeTimer{deadline: f.Now().Add(d), fn: fn}
	f.add(t)
	return &fakeTimerHandle{f: f, t: t}
}

func (f *Fake) add(t *fakeTimer) {
	f.mu.Lock()
	defer f.mu.Unlock()
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
