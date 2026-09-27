package mcpclient

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// A Client owns a private Transport, so a caller that builds one per call and
// drops it leaves the call's keep-alive connection — and the transport's
// read/write goroutines — with no owner, for the life of the process. Close must
// actually close it.
func TestCloseReleasesIdleConnection(t *testing.T) {
	closed := make(chan struct{}, 4)
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{}}`))
	}))
	srv.Config.ConnState = func(_ net.Conn, s http.ConnState) {
		if s == http.StateClosed {
			closed <- struct{}{}
		}
	}
	srv.Start()
	defer srv.Close()

	c := New(srv.URL, "", 5*time.Second)
	defer c.Close()
	if _, err := c.Call(context.Background(), "muninn_status", nil); err != nil {
		t.Fatalf("Call: %v", err)
	}

	// The response was read in full, so the socket sits idle on the client's
	// transport until Close or the idle timeout.
	select {
	case <-closed:
		t.Fatal("connection already gone before Close; the test proves nothing")
	case <-time.After(200 * time.Millisecond):
	}

	c.Close()
	select {
	case <-closed:
	case <-time.After(2 * time.Second):
		t.Error("connection still open after Close")
	}
}

// HealthCheckAt builds a throwaway client, so the connection its drained health
// body leaves behind must be released before it returns, not left on an idle
// pool nobody can reach.
func TestHealthCheckAtClosesItsConnection(t *testing.T) {
	closed := make(chan struct{}, 4)
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("ok"))
	}))
	srv.Config.ConnState = func(_ net.Conn, s http.ConnState) {
		if s == http.StateClosed {
			closed <- struct{}{}
		}
	}
	srv.Start()
	defer srv.Close()

	if err := HealthCheckAt(srv.URL+"/mcp", ""); err != nil {
		t.Fatalf("HealthCheckAt: %v", err)
	}
	select {
	case <-closed:
	case <-time.After(2 * time.Second):
		t.Error("health-check connection was still open after HealthCheckAt returned")
	}
}
