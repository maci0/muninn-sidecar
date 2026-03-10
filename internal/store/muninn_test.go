package store

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/maci0/muninn-sidecar/internal/stats"
)

func TestStoreAndDrain(t *testing.T) {
	var (
		mu       sync.Mutex
		received []string
	)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		received = append(received, string(body))
		mu.Unlock()
		w.WriteHeader(200)
		w.Write([]byte(`{"jsonrpc":"2.0","result":{"id":"ok"},"id":1}`))
	}))
	defer srv.Close()

	st := &stats.Stats{}
	s := New(srv.URL, "", "test", st)

	s.Store(&CapturedExchange{
		Timestamp: time.Now(),
		Agent:     "test",
		Method:    "POST",
		Path:      "/v1/messages",
		ReqBody:   json.RawMessage(`{"model":"test"}`),
		RespBody:  json.RawMessage(`{"ok":true}`),
		TokensIn:  100,
		TokensOut: 50,
	})

	s.Drain()

	mu.Lock()
	n := len(received)
	mu.Unlock()

	if n == 0 {
		t.Fatal("expected at least one MCP call after drain")
	}

	// Check stats were updated.
	if st.Captured.Load() != 1 {
		t.Fatalf("expected 1 captured, got %d", st.Captured.Load())
	}
	if st.Flushed.Load() != 1 {
		t.Fatalf("expected 1 flushed, got %d", st.Flushed.Load())
	}
	if st.TokensIn.Load() != 100 {
		t.Fatalf("expected 100 tokens in, got %d", st.TokensIn.Load())
	}
}

func TestBatching(t *testing.T) {
	var (
		mu    sync.Mutex
		calls int
	)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		calls++
		mu.Unlock()
		w.WriteHeader(200)
		w.Write([]byte(`{"jsonrpc":"2.0","result":{"id":"ok"},"id":1}`))
	}))
	defer srv.Close()

	s := New(srv.URL, "", "test", nil)

	// Send 10 exchanges — should batch into 1 call.
	for i := range 10 {
		s.Store(&CapturedExchange{
			Timestamp: time.Now(),
			Agent:     "test",
			Path:      "/v1/messages",
			ReqBody:   json.RawMessage(`{}`),
			RespBody:  json.RawMessage(`{}`),
			TokensIn:  i,
		})
	}

	s.Drain()

	mu.Lock()
	n := calls
	mu.Unlock()

	if n == 0 {
		t.Fatal("expected at least one MCP call")
	}
	if n > 2 {
		t.Fatalf("expected batching (<=2 calls for 10 items), got %d calls", n)
	}
	t.Logf("10 items sent in %d MCP call(s)", n)
}

func TestQueueOverflow(t *testing.T) {
	// Server responds normally — the queue overflows because we store
	// faster than the worker can flush via HTTP round-trips.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Small delay to ensure queue fills before worker drains.
		time.Sleep(10 * time.Millisecond)
		w.WriteHeader(200)
		w.Write([]byte(`{"jsonrpc":"2.0","result":{"id":"ok"},"id":1}`))
	}))
	defer srv.Close()

	st := &stats.Stats{}
	s := New(srv.URL, "", "test", st)

	// Fill the queue (buffer size is 256). Tight loop outpaces the worker.
	for range 300 {
		s.Store(&CapturedExchange{
			Agent:    "test",
			Path:     "/v1/messages",
			ReqBody:  json.RawMessage(`{}`),
			RespBody: json.RawMessage(`{}`),
		})
	}

	dropped := st.Dropped.Load()
	if dropped == 0 {
		t.Fatal("expected some exchanges to be dropped when queue is full")
	}
	t.Logf("dropped %d out of 300 (queue size 256)", dropped)

	s.Drain()
}

func TestRetryOnServerError(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping retry test in short mode (needs ~6s for backoff)")
	}

	var attempts atomic.Int32

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := attempts.Add(1)
		if n < 3 {
			w.WriteHeader(500) // fail first 2 attempts
			return
		}
		w.WriteHeader(200)
		w.Write([]byte(`{"jsonrpc":"2.0","result":{"id":"ok"},"id":1}`))
	}))
	defer srv.Close()

	st := &stats.Stats{}
	s := New(srv.URL, "", "test", st)

	s.Store(&CapturedExchange{
		Agent:    "test",
		Path:     "/v1/messages",
		ReqBody:  json.RawMessage(`{}`),
		RespBody: json.RawMessage(`{}`),
	})

	s.Drain()

	if got := attempts.Load(); got < 3 {
		t.Fatalf("expected at least 3 attempts (2 failures + 1 success), got %d", got)
	}
	if st.FlushErrors.Load() != 0 {
		t.Fatalf("expected 0 flush errors (retry succeeded), got %d", st.FlushErrors.Load())
	}
}

func TestNoRetryOnClientError(t *testing.T) {
	var attempts atomic.Int32

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts.Add(1)
		w.WriteHeader(400) // client error
	}))
	defer srv.Close()

	st := &stats.Stats{}
	s := New(srv.URL, "", "test", st)

	s.Store(&CapturedExchange{
		Agent:    "test",
		Path:     "/v1/messages",
		ReqBody:  json.RawMessage(`{}`),
		RespBody: json.RawMessage(`{}`),
	})

	s.Drain()

	if got := attempts.Load(); got != 1 {
		t.Fatalf("expected exactly 1 attempt (no retry on 4xx), got %d", got)
	}
}

func TestFormatExchange(t *testing.T) {
	ex := &CapturedExchange{
		Timestamp:  time.Date(2026, 3, 10, 12, 0, 0, 0, time.UTC),
		Agent:      "claude",
		Method:     "POST",
		Path:       "/v1/messages",
		StatusCode: 200,
		DurationMs: 1500,
		Model:      "claude-3-opus",
		TokensIn:   500,
		TokensOut:  200,
		CacheWrite: 100,
		CacheRead:  50,
		ReqBody:    json.RawMessage(`{"model":"claude-3-opus"}`),
		RespBody:   json.RawMessage(`{"ok":true}`),
	}

	concept, content := formatExchange(ex)

	if !strings.Contains(concept, "claude") {
		t.Errorf("concept should contain agent name: %q", concept)
	}
	if !strings.Contains(concept, "200") {
		t.Errorf("concept should contain status code: %q", concept)
	}
	if !strings.Contains(content, "Model: claude-3-opus") {
		t.Errorf("content should contain model: %q", content)
	}
	if !strings.Contains(content, "Tokens: 500 in / 200 out") {
		t.Errorf("content should contain token counts: %q", content)
	}
	if !strings.Contains(content, "Cache: 100 write / 50 read") {
		t.Errorf("content should contain cache counts: %q", content)
	}
}

func TestBuildTags(t *testing.T) {
	ex := &CapturedExchange{
		Agent:      "claude",
		StatusCode: 200,
		Model:      "claude-3-opus",
	}

	tags := buildTags(ex)

	expected := map[string]bool{
		"sidecar":           true,
		"claude":            true,
		"status:200":        true,
		"model:claude-3-opus": true,
	}

	for _, tag := range tags {
		delete(expected, tag)
	}
	if len(expected) > 0 {
		missing := make([]string, 0, len(expected))
		for k := range expected {
			missing = append(missing, k)
		}
		t.Fatalf("missing tags: %v", missing)
	}
}

func TestTruncateJSON(t *testing.T) {
	// Empty data.
	if got := truncateJSON(nil, 100); got != "(empty)" {
		t.Fatalf("expected (empty), got %q", got)
	}

	// Short data.
	short := json.RawMessage(`{"a":1}`)
	if got := truncateJSON(short, 100); got != `{"a":1}` {
		t.Fatalf("expected short data unchanged, got %q", got)
	}

	// Long data.
	long := json.RawMessage(strings.Repeat("x", 200))
	got := truncateJSON(long, 100)
	if !strings.HasSuffix(got, "(truncated)") {
		t.Fatalf("expected truncated suffix, got %q", got)
	}
}

func TestHealthURLFromMCP(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"http://localhost:8750/mcp", "http://localhost:8750/mcp/health"},
		{"http://localhost:8750/mcp/", "http://localhost:8750/mcp/health"},
		{"http://example.com/api/mcp", "http://example.com/api/mcp/health"},
	}

	for _, tt := range tests {
		got, err := healthURLFromMCP(tt.input)
		if err != nil {
			t.Fatalf("healthURLFromMCP(%q) error: %v", tt.input, err)
		}
		if got != tt.want {
			t.Fatalf("healthURLFromMCP(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}
