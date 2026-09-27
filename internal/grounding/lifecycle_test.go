package grounding

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

// The grounder is called on the request hot path, so a transport per call would
// strand that call's keep-alive connection (and its read/write goroutines) on a
// transport nothing can ever close. Grounded turns must share one connection.
func TestHTTPGrounderReusesOneConnection(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	conns := map[string]bool{}
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		conns[r.RemoteAddr] = true
		mu.Unlock()
		w.Write([]byte(`{"choices":[{"message":{"content":"1: no"}}]}`))
	}))
	srv.Listener.Close()
	srv.Listener = ln
	srv.Start()
	defer srv.Close()

	g := New("", srv.URL, "m", "", 5*time.Second)
	for range 5 {
		if m := g.Relevant(context.Background(), "q", []string{"p"}); len(m) != 1 || m[0] {
			t.Fatalf("expected [false] per call, got %v", m)
		}
	}

	mu.Lock()
	n := len(conns)
	mu.Unlock()
	if n != 1 {
		t.Errorf("5 calls used %d connections, want 1: the client is not reused", n)
	}
}

// A judge that prints without bound must not be able to grow the sidecar's heap
// without limit; the verdicts it prints last are what ParseMask needs, so the
// capped buffer has to keep the tail.
func TestTailBufferKeepsTailWithinLimit(t *testing.T) {
	tb := &tailBuffer{limit: 8}
	tb.Write([]byte("aaaa"))
	tb.Write([]byte("bbbbbb"))
	if tb.Len() != 8 {
		t.Fatalf("len=%d, want 8", tb.Len())
	}
	if got := tb.String(); got != "aabbbbbb" {
		t.Errorf("buffer=%q, want the newest 8 bytes %q", got, "aabbbbbb")
	}
	// A single write larger than the limit still yields exactly the limit.
	tb2 := &tailBuffer{limit: 4}
	tb2.Write([]byte("123456789"))
	if got := tb2.String(); got != "6789" {
		t.Errorf("buffer=%q, want %q", got, "6789")
	}
}
