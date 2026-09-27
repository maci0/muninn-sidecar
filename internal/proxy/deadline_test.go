package proxy

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// A response that keeps writing past the idle window must reach the agent whole.
// http.Server.WriteTimeout would have cut it: Go arms that deadline once, when
// the request headers are read, so a long-but-healthy stream is indistinguishable
// from a stuck one to it.
func TestResponseStreamOutlivesIdleWindow(t *testing.T) {
	const (
		idle   = 300 * time.Millisecond
		gap    = 50 * time.Millisecond
		chunks = 12 // 600ms of streaming, twice the idle window
	)
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		for i := range chunks {
			fmt.Fprintf(w, "data: %d\n\n", i)
			w.(http.Flusher).Flush()
			time.Sleep(gap)
		}
	}))
	defer up.Close()

	p, err := New(Config{ListenAddr: "127.0.0.1:0", Upstream: up.URL, AgentName: "test-agent"})
	if err != nil {
		t.Fatal(err)
	}
	p.writeIdleTimeout = idle
	addr, err := p.Start()
	if err != nil {
		t.Fatal(err)
	}
	defer p.Shutdown(t.Context())

	resp, err := http.Post("http://"+addr+"/v1/messages", "application/json", strings.NewReader(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	got := strings.Count(string(body), "data:")
	if err != nil {
		t.Fatalf("stream cut off after %d of %d chunks: %v", got, chunks, err)
	}
	if got != chunks {
		t.Fatalf("got %d of %d chunks", got, chunks)
	}
}

// deadlineRecorder is a ResponseWriter that records the write deadlines the
// wrapper arms, standing in for the *http.response whose connection the
// deadline actually lands on.
type deadlineRecorder struct {
	mu        sync.Mutex
	deadlines []time.Time
	flushes   int
	body      strings.Builder
}

func (d *deadlineRecorder) Header() http.Header { return http.Header{} }

func (d *deadlineRecorder) WriteHeader(int) {}

func (d *deadlineRecorder) Write(p []byte) (int, error) { return d.body.Write(p) }

func (d *deadlineRecorder) SetWriteDeadline(t time.Time) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.deadlines = append(d.deadlines, t)
	return nil
}

func (d *deadlineRecorder) Flush() {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.flushes++
}

// The deadline is re-armed at the start of the response and on every write and
// flush, each time one idle window ahead of the moment it is needed. Arming it
// only on writes would leave a response that sends headers and then stalls with
// no bound at all.
func TestIdleDeadlineRearmedOnHeaderWriteAndFlush(t *testing.T) {
	const idle = 2 * time.Second
	rec := &deadlineRecorder{}
	w := newIdleDeadlineWriter(rec, idle)

	start := time.Now()
	w.WriteHeader(http.StatusOK)
	w.Write([]byte("chunk"))
	w.FlushError()

	rec.mu.Lock()
	defer rec.mu.Unlock()
	if len(rec.deadlines) != 3 {
		t.Fatalf("want 3 armed deadlines (header, write, flush), got %d", len(rec.deadlines))
	}
	for i, d := range rec.deadlines {
		// Each deadline is armed a moment after start (the three calls run back
		// to back), so every one should land about one idle window out. The slack
		// is generous on purpose: the contract is "never stale", not an exact
		// clock reading.
		if ahead := d.Sub(start); ahead < idle/2 || ahead > idle+time.Second {
			t.Errorf("deadline %d armed %v ahead, want ~%v", i, ahead, idle)
		}
	}
}

// TestIdleDeadlineReachesTheConnectionThroughUnwrap covers the case the
// deadline count above hides on its own: the wrapper exposes neither
// SetWriteDeadline nor Flush itself, so http.ResponseController can only reach
// the connection by unwrapping. That is the production shape — the wrapper
// holds a *http.response — and without Unwrap the deadline is armed nowhere and
// the reverse proxy's per-write flush is dropped, so a response that stalls
// holds the agent's read open forever with every other test still green.
func TestIdleDeadlineReachesTheConnectionThroughUnwrap(t *testing.T) {
	const idle = 2 * time.Second
	conn := &deadlineRecorder{}
	w := newIdleDeadlineWriter(conn, idle)

	if w.Unwrap() != http.ResponseWriter(conn) {
		t.Fatal("Unwrap did not return the wrapped writer")
	}

	w.WriteHeader(http.StatusOK)
	w.Write([]byte("chunk"))
	if err := w.FlushError(); err != nil {
		t.Fatalf("FlushError: %v", err)
	}

	conn.mu.Lock()
	defer conn.mu.Unlock()
	if len(conn.deadlines) != 3 {
		t.Fatalf("connection saw %d armed deadlines, want 3: the deadline did not reach through Unwrap", len(conn.deadlines))
	}
	// A drop on Unwrap would also cost the flush: the reverse proxy flushes
	// every write, and a stream that never reaches the agent is a broken
	// response rather than a slow one.
	if conn.flushes != 1 {
		t.Errorf("connection saw %d flushes, want 1: the flush did not reach through Unwrap", conn.flushes)
	}
}
