package store

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/maci0/muninn-sidecar/internal/clock"
	"github.com/maci0/muninn-sidecar/internal/stats"
)

// recorder is a MuninnDB stand-in that keeps the concepts of every call, so a
// test can assert what the worker's timers produced.
type recorder struct {
	mu       sync.Mutex
	concepts [][]string
}

func (r *recorder) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	body, _ := io.ReadAll(req.Body)
	var call struct {
		Params struct {
			Arguments struct {
				Concept  string `json:"concept"`
				Memories []struct {
					Concept string `json:"concept"`
				} `json:"memories"`
			} `json:"arguments"`
		} `json:"params"`
	}
	_ = json.Unmarshal(body, &call)
	var concepts []string
	if c := call.Params.Arguments.Concept; c != "" {
		concepts = append(concepts, c)
	}
	for _, m := range call.Params.Arguments.Memories {
		concepts = append(concepts, m.Concept)
	}
	r.mu.Lock()
	r.concepts = append(r.concepts, concepts)
	r.mu.Unlock()
	w.Write([]byte(`{"jsonrpc":"2.0","result":{"id":"ok"},"id":1}`))
}

func (r *recorder) calls() [][]string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([][]string(nil), r.concepts...)
}

func (r *recorder) waitCalls(t *testing.T, n int) [][]string {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if got := r.calls(); len(got) >= n {
			return got
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("timed out waiting for %d MCP calls, got %v", n, r.calls())
	return nil
}

// concept is the store's concept for an exchange whose only text is user.
func concept(user string) string {
	return user + " → reply to " + user
}

// stepper drives a store on a scripted clock: it reports when the worker has
// formatted an exchange and when the server has seen a call, so a test can
// advance the clock only once the previous step has landed.
type stepper struct {
	store    *MuninnStore
	clock    *clock.Fake
	rec      *recorder
	prepared chan struct{}
}

func newStepper(t *testing.T) *stepper {
	t.Helper()
	s := &stepper{clock: clock.NewFake(), rec: &recorder{}, prepared: make(chan struct{}, 64)}
	srv := httptest.NewServer(s.rec)
	t.Cleanup(srv.Close)
	s.store = NewWithClock(srv.URL, "", "test", &stats.Stats{}, s.clock)
	s.store.SetPreparer(func(*CapturedExchange) {
		select {
		case s.prepared <- struct{}{}:
		default:
		}
	})
	return s
}

// enqueue stores one exchange whose concept is user, and waits for the worker to
// format it.
func (s *stepper) enqueue(t *testing.T, user string) {
	t.Helper()
	s.store.Store(&CapturedExchange{
		Agent:    "test",
		Path:     "/v1/messages",
		ReqBody:  json.RawMessage(fmt.Sprintf(`{"messages":[{"role":"user","content":%q}]}`, user)),
		RespBody: json.RawMessage(fmt.Sprintf(`{"content":[{"type":"text","text":"reply to %s"}]}`, user)),
	})
	select {
	case <-s.prepared:
	case <-time.After(5 * time.Second):
		t.Fatalf("worker never formatted %q", user)
	}
}

// tick advances the clock one flush period and waits for the resulting call.
func (s *stepper) tick(t *testing.T, wantCalls int) {
	t.Helper()
	s.clock.Advance(2 * time.Second)
	s.rec.waitCalls(t, wantCalls)
}

func TestFlushesRunOnTheInjectedClock(t *testing.T) {
	st := newStepper(t)
	// Nothing is delivered until the clock reaches a flush period, and nothing
	// more is delivered by a period that finds an empty batch.
	st.enqueue(t, "first")
	if got := st.rec.calls(); len(got) != 0 {
		t.Fatalf("flushed %v before the clock advanced", got)
	}
	st.tick(t, 1)

	st.enqueue(t, "second")
	if got := st.rec.calls(); len(got) != 1 {
		t.Fatalf("flushed %v without a flush period", got)
	}
	st.tick(t, 2)

	if got := st.rec.calls(); len(got) != 2 || len(got[0]) != 1 ||
		got[0][0] != concept("first") || got[1][0] != concept("second") {
		t.Fatalf("call sequence %v, want [first] then [second]", got)
	}
	st.store.Drain()
}

func TestRetryBackoffRunsOnTheInjectedClock(t *testing.T) {
	var mu sync.Mutex
	attempts := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		attempts++
		mu.Unlock()
		w.WriteHeader(500)
	}))
	defer srv.Close()

	count := func() int {
		mu.Lock()
		defer mu.Unlock()
		return attempts
	}

	clk := clock.NewFake()
	s := NewWithClock(srv.URL, "", "test", &stats.Stats{}, clk)
	prepared := make(chan struct{}, 1)
	s.SetPreparer(func(*CapturedExchange) {
		select {
		case prepared <- struct{}{}:
		default:
		}
	})
	s.Store(&CapturedExchange{
		Agent:    "test",
		Path:     "/v1/messages",
		ReqBody:  json.RawMessage(`{"messages":[{"role":"user","content":"retry me"}]}`),
		RespBody: json.RawMessage(`{"content":[{"type":"text","text":"nope"}]}`),
	})
	<-prepared

	// The first attempt rides the first flush period.
	clk.Advance(2 * time.Second)
	waitAttempts(t, clk, count, 1)
	// The 2s and 4s backoffs elapse on the scripted clock, so a frozen clock
	// must leave the retry budget unspent.
	time.Sleep(50 * time.Millisecond)
	if n := count(); n != 1 {
		t.Fatalf("%d attempts with the clock frozen, want 1", n)
	}

	waitAttempts(t, clk, count, 2)
	waitAttempts(t, clk, count, 3)
	time.Sleep(50 * time.Millisecond)
	if n := count(); n != 3 {
		t.Fatalf("%d attempts, want the 3-attempt budget and no more", n)
	}
	s.Drain()
}

// waitAttempts drives the scripted clock forward until the store has made want
// attempts. It advances the clock itself rather than taking a fixed schedule:
// the backoff timer for the next attempt is registered by the worker only after
// the previous response has been classified, so an advance issued the moment
// the attempt is observed can land before that timer exists, leaving it due in
// the future with no further advance coming.
func waitAttempts(t *testing.T, clk *clock.Fake, count func() int, want int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if count() >= want {
			return
		}
		clk.Advance(time.Second)
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("timed out waiting for %d attempts, got %d", want, count())
}

func TestDedupRingExpiresOnTheInjectedClock(t *testing.T) {
	st := newStepper(t)
	st.enqueue(t, "alpha")
	st.tick(t, 1)

	// Same concept, inside the ring's window: dropped without a call.
	st.enqueue(t, "alpha")
	if got := st.rec.calls(); len(got) != 1 {
		t.Fatalf("deduped concept produced calls %v", got)
	}

	// Fillers mark the ticks: each one is delivered by its own flush period, so
	// exactly dedupRingSize periods elapse and the ring comes back around to the
	// slot holding "alpha".
	for i := 0; i < dedupRingSize; i++ {
		st.enqueue(t, fmt.Sprintf("filler %d", i))
		st.tick(t, i+2)
	}

	st.enqueue(t, "alpha")
	st.enqueue(t, "probe")
	st.tick(t, dedupRingSize+2)
	calls := st.rec.calls()
	last := calls[len(calls)-1]
	var seen bool
	for _, c := range last {
		if c == concept("alpha") {
			seen = true
		}
	}
	if !seen {
		t.Fatalf("expired concept was still deduped: final batch %v", last)
	}
	st.store.Drain()
}
