package proxy

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/maci0/muninn-sidecar/internal/clock"
)

// fakeClock is a manually advanced clock: every capture timestamp and duration
// is a function of how far it has been advanced, so replaying the same request
// sequence produces byte-identical exchanges.
//
// It is read from the upstream handler goroutine and advanced from the test
// goroutine, so clock.Fake's own locking covers both.
type fakeClock struct {
	*clock.Fake
}

func newFakeClock() *fakeClock {
	return &fakeClock{Fake: clock.NewFake()}
}

func TestCaptureIsReplayableFromClock(t *testing.T) {
	// The upstream advances the scripted clock while the request is in flight, so
	// each captured duration is exactly the scripted 1500ms.
	var clk *fakeClock
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		clk.Advance(1500 * time.Millisecond)
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"id":"msg_1","content":[{"type":"text","text":"hi"}]}`)
	}))
	defer upstream.Close()

	// Two runs of the same request sequence against the same scripted clock must
	// produce identical exchange bytes, and the clock must end up exactly where
	// the script puts it: every time read on the capture path comes from the
	// injected clock, not the host wall clock.
	//
	// CapturedExchange carries no wall-clock field of its own (see
	// store.CapturedExchange), so the clock's own position is what proves the
	// capture path is reading it: three requests, each preceded by a 10s jump
	// taken at request start and followed by the upstream's 1500ms advance while
	// the request is in flight.
	run := func() (string, time.Duration) {
		clk = newFakeClock()
		base := clk.Now()
		rec := &recordStore{}
		p, err := New(Config{
			ListenAddr: "127.0.0.1:0",
			Upstream:   upstream.URL,
			AgentName:  "test-agent",
			Store:      rec,
			Clock:      clk,
		})
		if err != nil {
			t.Fatal(err)
		}
		addr, err := p.Start()
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { p.Shutdown(context.Background()) })

		for i := 0; i < 3; i++ {
			clk.Advance(10 * time.Second)
			resp, err := http.Post("http://"+addr+"/v1/messages", "application/json",
				strings.NewReader(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`))
			if err != nil {
				t.Fatal(err)
			}
			resp.Body.Close()
		}

		var out strings.Builder
		for _, ex := range rec.all() {
			b, _ := json.Marshal(ex)
			out.Write(b)
			out.WriteByte('\n')
		}
		return out.String(), clk.Now().Sub(base)
	}

	first, firstElapsed := run()
	second, secondElapsed := run()
	if first != second {
		t.Fatalf("replay diverged:\nfirst:\n%s\nsecond:\n%s", first, second)
	}
	if want := 3 * (10*time.Second + 1500*time.Millisecond); firstElapsed != want || secondElapsed != want {
		t.Errorf("scripted clock advanced %s and %s, want %s each", firstElapsed, secondElapsed, want)
	}
}
