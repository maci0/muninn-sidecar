package clock

import (
	"sync"
	"testing"
	"time"
)

func TestFakeAdvanceFiresTimersInDeadlineOrder(t *testing.T) {
	f := NewFake()
	start := f.Now()

	var order []string
	var mu sync.Mutex
	f.AfterFunc(4*time.Second, func() { mu.Lock(); order = append(order, "afterfunc"); mu.Unlock() })
	f.After(1 * time.Second)
	f.AfterFunc(2*time.Second, func() { mu.Lock(); order = append(order, "second"); mu.Unlock() })

	f.Advance(3 * time.Second)
	mu.Lock()
	got := append([]string(nil), order...)
	mu.Unlock()
	if want := []string{"second"}; len(got) != len(want) || got[0] != want[0] {
		t.Fatalf("after 3s fired %v, want %v", got, want)
	}

	f.Advance(1 * time.Second)
	mu.Lock()
	got = append([]string(nil), order...)
	mu.Unlock()
	if len(got) != 2 || got[1] != "afterfunc" {
		t.Fatalf("after 4s fired %v, want [second afterfunc]", got)
	}
	if elapsed := f.Now().Sub(start); elapsed != 4*time.Second {
		t.Fatalf("clock advanced %s, want 4s", elapsed)
	}
}

func TestFakeAfterDeliversOnAdvance(t *testing.T) {
	f := NewFake()
	ch := f.After(time.Second)
	f.Advance(500 * time.Millisecond)
	select {
	case got := <-ch:
		t.Fatalf("timer fired early at %s", got)
	default:
	}
	f.Advance(500 * time.Millisecond)
	select {
	case <-ch:
	default:
		t.Fatal("timer did not fire after the clock passed its deadline")
	}
}

func TestFakeTickerRepeatsAndStops(t *testing.T) {
	f := NewFake()
	tk := f.NewTicker(time.Second)
	// Three periods elapsed at once, but like the standard library's tickers
	// the channel holds one pending tick: a driver steps the clock, consumes the
	// tick, and only then advances again.
	f.Advance(3 * time.Second)
	select {
	case <-tk.C():
	default:
		t.Fatal("no tick after three periods")
	}
	select {
	case <-tk.C():
		t.Fatal("more than one tick pending")
	default:
	}
	f.Advance(time.Second)
	select {
	case <-tk.C():
	default:
		t.Fatal("ticker stopped repeating")
	}
	tk.Stop()
	f.Advance(5 * time.Second)
	select {
	case <-tk.C():
		t.Fatal("stopped ticker still fired")
	default:
	}
}

func TestFakeStoppedTimerNeverFires(t *testing.T) {
	f := NewFake()
	fired := false
	tm := f.AfterFunc(time.Second, func() { fired = true })
	tm.Stop()
	f.Advance(2 * time.Second)
	if fired {
		t.Fatal("stopped timer fired")
	}
}
