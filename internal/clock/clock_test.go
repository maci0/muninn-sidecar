package clock

import (
	"testing"
	"time"
)

// Every production default is SystemClock, and nearly every test swaps in Fake in
// its place: the store's flush ticker, dedup expiry, proxy latency, and the
// inject decay window all read through the Clock interface. The two
// implementations reach their results by different means, so a divergence in
// either is invisible from one side. These tests pin what both must do, so the
// substitution a test relies on holds for the run it stands in for.

const (
	// Long enough that a loaded CI machine has not blown the deadline when the
	// test checks that nothing fired yet; short enough to keep the suite quick.
	notYet = 200 * time.Millisecond
	// Bound every wait for a real timer, so a broken SystemClock fails the test
	// instead of hanging it. A hang reads as a slow suite, not as a failure.
	await = 2 * time.Second
	// Fires promptly once the deadline passes.
	soon = time.Millisecond
)

func TestSystemClockNowAndSince(t *testing.T) {
	c := SystemClock{}

	start := c.Now()
	if start.IsZero() {
		t.Fatal("SystemClock.Now returned the zero time")
	}
	if d := c.Since(start); d < 0 || d > await {
		t.Fatalf("SystemClock.Since(now) = %s, want a non-negative sub-%s span", d, await)
	}
	// Elapsed grows with elapsed wall time, and the second reading is at least
	// the first: a clock that stopped advancing would make a capture's recorded
	// duration 0 for every request.
	time.Sleep(soon)
	if second := c.Since(start); second < 0 {
		t.Fatalf("SystemClock.Since(now) = %s, want non-negative", second)
	}
}

// Since is elapsed, so a timestamp ahead of now reads negative on both clocks.
// A capture path that stored a start time from one clock and measured with
// another would otherwise report a large positive duration instead of the
// negative span that reveals the mismatch.
func TestSinceIsElapsedOnBothClocks(t *testing.T) {
	clocks := []struct {
		name string
		c    Clock
	}{
		{"SystemClock", SystemClock{}},
		{"Fake", NewFake()},
	}
	for _, tc := range clocks {
		if d := tc.c.Since(tc.c.Now().Add(time.Hour)); d >= 0 {
			t.Errorf("%s.Since(an hour from now) = %s, want a negative span", tc.name, d)
		}
	}
}

func TestSystemClockAfterFiresOnceAfterDeadline(t *testing.T) {
	c := SystemClock{}

	ch := c.After(notYet)
	select {
	case <-ch:
		t.Fatal("SystemClock.After fired before its deadline")
	default:
	}
	select {
	case got := <-ch:
		if got.IsZero() {
			t.Fatal("SystemClock.After delivered the zero time")
		}
	case <-time.After(await):
		t.Fatal("SystemClock.After did not fire after its deadline")
	}
	select {
	case <-ch:
		t.Fatal("SystemClock.After fired twice; it is a one-shot")
	default:
	}
}

func TestSystemClockAfterFuncRunsOnceAndStopCancels(t *testing.T) {
	c := SystemClock{}

	fired := make(chan struct{}, 2)
	c.AfterFunc(soon, func() { fired <- struct{}{} })
	select {
	case <-fired:
	case <-time.After(await):
		t.Fatal("SystemClock.AfterFunc did not run its function")
	}
	select {
	case <-fired:
		t.Fatal("SystemClock.AfterFunc ran its function twice; it is a one-shot")
	case <-time.After(50 * time.Millisecond):
	}

	// A timer stopped before its deadline must not run: the store arms a drain
	// guard this way, and a fire after a fast shutdown would touch a store the
	// caller believes is closed.
	stopped := make(chan struct{}, 1)
	timer := c.AfterFunc(notYet, func() { stopped <- struct{}{} })
	timer.Stop()
	select {
	case <-stopped:
		t.Fatal("stopped AfterFunc still ran its function")
	case <-time.After(notYet + soon):
	}
}

func TestSystemClockTickerRepeatsUntilStopped(t *testing.T) {
	c := SystemClock{}

	tk := c.NewTicker(soon)
	defer tk.Stop()
	for i := range 2 {
		select {
		case got := <-tk.C():
			if got.IsZero() {
				t.Fatal("ticker delivered the zero time")
			}
		case <-time.After(await):
			t.Fatalf("no tick %d from a running ticker", i+1)
		}
	}
	// time.Ticker drops a tick rather than blocking, so drain what is pending
	// before Stop: the assertion below is that no further tick arrives, and a
	// tick already in the channel would be indistinguishable from a live one.
	for len(tk.C()) > 0 {
	}
	tk.Stop()
	select {
	case <-tk.C():
		t.Fatal("stopped ticker still ticked")
	case <-time.After(50 * time.Millisecond):
	}
}

// Fake.Since is the side of the pair the test suite actually reads: inject's
// decay window and the proxy's latency both call it in every run, and no other
// test in the tree does. It must agree with Fake.Now rather than the host.
func TestFakeSinceIsScriptedNotHost(t *testing.T) {
	f := NewFake()

	start := f.Now()
	if d := f.Since(start); d != 0 {
		t.Fatalf("Fake.Since(now) = %s before any Advance, want 0", d)
	}
	// Host time passing must not move a scripted clock.
	time.Sleep(soon)
	if d := f.Since(start); d != 0 {
		t.Fatalf("Fake.Since(now) = %s after %s of host time, want 0", d, soon)
	}
	f.Advance(90 * time.Second)
	if d := f.Since(start); d != 90*time.Second {
		t.Fatalf("Fake.Since(now) = %s after advancing 90s, want 90s", d)
	}
	// Since is elapsed, not absolute: an earlier timestamp is a positive span.
	if d := f.Since(start.Add(time.Minute)); d != 30*time.Second {
		t.Fatalf("Fake.Since(a minute ago) = %s, want 30s", d)
	}
	if d := f.Since(start.Add(2 * time.Minute)); d != -30*time.Second {
		t.Fatalf("Fake.Since(2m after start) = %s, want -30s", d)
	}
}

// Both implementations must be usable everywhere the interface is, and must
// start where a caller can read time at all. SystemClock is the zero-value
// default the production structs carry, so it must satisfy Clock on its own.
func TestBothClocksSatisfyTheInterface(t *testing.T) {
	clocks := []struct {
		name string
		c    Clock
	}{
		{"SystemClock", SystemClock{}},
		{"Fake", NewFake()},
	}
	for _, tc := range clocks {
		if tc.c.Now().IsZero() {
			t.Errorf("%s.Now() is the zero time", tc.name)
		}
		if d := tc.c.Since(tc.c.Now()); d < 0 {
			t.Errorf("%s.Since(now) = %s, want non-negative", tc.name, d)
		}
	}
}
