package store

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/maci0/muninn-sidecar/internal/mcpclient"
	"github.com/maci0/muninn-sidecar/internal/redact"
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
		Agent:     "test",
		Path:      "/v1/messages",
		ReqBody:   json.RawMessage(`{"messages":[{"role":"user","content":"hello world"}]}`),
		RespBody:  json.RawMessage(`{"content":[{"type":"text","text":"hi there"}]}`),
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

func TestStoreRedactsSecrets(t *testing.T) {
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

	s := New(srv.URL, "", "test", &stats.Stats{})
	// Built from parts so no contiguous secret-shaped literal sits in source.
	secret := "sk-" + strings.Repeat("a", 30)
	s.Store(&CapturedExchange{
		Agent:    "test",
		Path:     "/v1/messages",
		ReqBody:  json.RawMessage(`{"messages":[{"role":"user","content":"my key is ` + secret + `"}]}`),
		RespBody: json.RawMessage(`{"content":[{"type":"text","text":"ok"}]}`),
	})
	s.Drain()

	mu.Lock()
	all := strings.Join(received, " ")
	mu.Unlock()
	if all == "" {
		t.Fatal("nothing flushed")
	}
	if strings.Contains(all, secret) {
		t.Errorf("secret leaked into stored memory: %q", all)
	}
	if !strings.Contains(all, "[REDACTED]") {
		t.Errorf("expected redaction marker in stored payload: %q", all)
	}
}

// TestStoreRedactsHomeDir pins the control at the point it has to hold: the
// bytes that leave the process for MuninnDB. A coding turn is wall-to-wall
// absolute paths, and the home directory is the one that names the operator, so
// storing it means a name outlives the session and is re-sent to the provider on
// every later recall.
func TestStoreRedactsHomeDir(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil || home == "" || home == "/" {
		t.Skip("no usable home directory to redact")
	}
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

	s := New(srv.URL, "", "test", &stats.Stats{})
	s.Store(&CapturedExchange{
		Agent:    "test",
		Path:     "/v1/messages",
		ReqBody:  json.RawMessage(`{"messages":[{"role":"user","content":"fix the build in ` + home + `/src"}]}`),
		RespBody: json.RawMessage(`{"content":[{"type":"text","text":"edited ` + home + `/src/main.go"}]}`),
	})
	s.Drain()

	mu.Lock()
	all := strings.Join(received, " ")
	mu.Unlock()
	if all == "" {
		t.Fatal("nothing flushed")
	}
	if strings.Contains(all, home) {
		t.Errorf("home directory leaked into stored memory: %q", all)
	}
	if !strings.Contains(all, redact.HomeMarker) {
		t.Errorf("expected home marker in stored payload: %q", all)
	}
}

func TestSetRedactionDisables(t *testing.T) {
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

	s := New(srv.URL, "", "test", &stats.Stats{})
	s.SetRedaction(false) // disabled before any capture flows
	secret := "sk-" + strings.Repeat("a", 30)
	s.Store(&CapturedExchange{
		Agent:    "test",
		Path:     "/v1/messages",
		ReqBody:  json.RawMessage(`{"messages":[{"role":"user","content":"key ` + secret + `"}]}`),
		RespBody: json.RawMessage(`{"content":[{"type":"text","text":"ok"}]}`),
	})
	s.Drain()

	mu.Lock()
	all := strings.Join(received, " ")
	mu.Unlock()
	// With redaction off, the secret is captured verbatim (full fidelity).
	if !strings.Contains(all, secret) {
		t.Errorf("expected verbatim capture with redaction disabled: %q", all)
	}
	if strings.Contains(all, "[REDACTED]") {
		t.Errorf("redaction marker present despite SetRedaction(false): %q", all)
	}
}

// A Preparer is the capture-side normalization the store runs on its worker
// goroutine, so the proxy can hand over raw bodies instead of parsing tens of
// MiB of JSON in the agent's turn. Its mutation must be what gets stored, and
// the usage counters must be read from the prepared exchange.
func TestPreparerRunsBeforeStorage(t *testing.T) {
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
	s.SetPreparer(func(ex *CapturedExchange) {
		ex.Model = "claude-3-opus"
		ex.TokensIn = 7
		ex.TokensOut = 3
		ex.ReqBody = json.RawMessage(`{"messages":[{"role":"user","content":"cleaned by the preparer"}]}`)
	})
	s.Store(&CapturedExchange{
		Agent:    "test",
		Path:     "/v1/messages",
		ReqBody:  json.RawMessage(`{"messages":[{"role":"user","content":"raw body"}]}`),
		RespBody: json.RawMessage(`{"content":[{"type":"text","text":"ok"}]}`),
	})
	s.Drain()

	mu.Lock()
	all := strings.Join(received, " ")
	mu.Unlock()
	if !strings.Contains(all, "cleaned by the preparer") {
		t.Errorf("stored payload did not reflect the preparer: %q", all)
	}
	if strings.Contains(all, "raw body") {
		t.Errorf("preparer did not replace the raw body: %q", all)
	}
	if got := st.Models(); len(got) != 1 || got[0].Name != "claude-3-opus" {
		t.Errorf("expected the preparer's model to be recorded, got %v", got)
	}
	if st.TokensIn.Load() != 7 || st.TokensOut.Load() != 3 {
		t.Errorf("expected usage from the prepared exchange, got in=%d out=%d",
			st.TokensIn.Load(), st.TokensOut.Load())
	}
}

// A store with no Preparer keeps what it is handed: no normalization, and the
// exchange's own fields are what the usage counters report.
func TestNoPreparerStoresExchangeAsGiven(t *testing.T) {
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
		Agent:    "test",
		Path:     "/v1/messages",
		ReqBody:  json.RawMessage(`{"messages":[{"role":"user","content":"untouched body"}]}`),
		RespBody: json.RawMessage(`{"content":[{"type":"text","text":"ok"}]}`),
	})
	s.Drain()

	mu.Lock()
	all := strings.Join(received, " ")
	mu.Unlock()
	if !strings.Contains(all, "untouched body") {
		t.Errorf("expected the body as captured: %q", all)
	}
	if st.TokensIn.Load() != 0 {
		t.Errorf("unexpected token count without a preparer: %d", st.TokensIn.Load())
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

	// Send 10 exchanges with unique messages — should batch into 1 call.
	for i := range 10 {
		msg := fmt.Sprintf(`{"messages":[{"role":"user","content":"question %d"}]}`, i)
		resp := fmt.Sprintf(`{"content":[{"type":"text","text":"answer %d"}]}`, i)
		s.Store(&CapturedExchange{
			Agent:    "test",
			Path:     "/v1/messages",
			ReqBody:  json.RawMessage(msg),
			RespBody: json.RawMessage(resp),
			TokensIn: i,
		})
	}

	s.Drain()

	mu.Lock()
	n := calls
	mu.Unlock()

	if n != 1 {
		t.Fatalf("expected exactly 1 batched MCP call for 10 unique items, got %d calls", n)
	}
	t.Logf("10 items sent in %d MCP call(s)", n)
}

// TestQueueOverflow pins the drop path at the real queue depth. The previous
// form raced 300 Store calls against a worker doing HTTP round-trips and only
// asserted "some were dropped", so it passed on a slow machine and failed on a
// fast one. Filling the channel first, with no worker draining it, makes the
// boundary exact.
func TestQueueOverflow(t *testing.T) {
	const depth = 256 // the depth New allocates

	st := &stats.Stats{}
	// No worker: nothing drains the channel, so filling it is deterministic.
	s := &MuninnStore{queue: make(chan queueItem, depth), stats: st}

	ex := func(i int) *CapturedExchange {
		return &CapturedExchange{
			Agent:    "test",
			Path:     "/v1/messages",
			ReqBody:  json.RawMessage(fmt.Sprintf(`{"messages":[{"role":"user","content":"overflow test %d"}]}`, i)),
			RespBody: json.RawMessage(fmt.Sprintf(`{"content":[{"type":"text","text":"response %d"}]}`, i)),
		}
	}

	// Exactly the queue depth: every exchange is accepted, nothing dropped.
	for i := range depth {
		s.Store(ex(i))
	}
	if got := st.Dropped.Load(); got != 0 {
		t.Fatalf("dropped %d exchanges before the queue was full, want 0", got)
	}
	if got := st.Captured.Load(); got != depth {
		t.Errorf("Captured = %d, want %d", got, depth)
	}

	// One past the depth: the surplus is dropped, not blocked on.
	const surplus = 44
	for i := range surplus {
		s.Store(ex(depth + i))
	}
	if got := st.Dropped.Load(); got != surplus {
		t.Errorf("Dropped = %d, want %d", got, surplus)
	}
	if got := s.dropped.Load(); got != surplus {
		t.Errorf("internal drop count = %d, want %d", got, surplus)
	}
	if got := st.Captured.Load(); got != depth+surplus {
		t.Errorf("Captured = %d, want %d (drops still count as captured attempts)",
			got, depth+surplus)
	}
	if got := len(s.queue); got != depth {
		t.Errorf("queue holds %d, want %d", got, depth)
	}
}
func TestDrainBoundedWhenUnreachable(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping bounded-drain test (needs ~8s)")
	}
	// Server always 500s. Many distinct batches would, unbounded, retry ~6s each
	// (~18s for 3 batches). Drain must instead be bounded near drainTimeout.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(500)
	}))
	defer srv.Close()

	s := New(srv.URL, "", "test", &stats.Stats{})
	for i := 0; i < 25; i++ { // > 2*maxBatchSize, distinct concepts (no dedup)
		s.Store(&CapturedExchange{
			Agent:    "test",
			Path:     "/v1/messages",
			ReqBody:  json.RawMessage(fmt.Sprintf(`{"messages":[{"role":"user","content":"unique message %d"}]}`, i)),
			RespBody: json.RawMessage(fmt.Sprintf(`{"content":[{"type":"text","text":"resp %d"}]}`, i)),
		})
	}

	start := time.Now()
	s.Drain()
	elapsed := time.Since(start)
	// Bounded to drainTimeout (+ margin for an in-flight call), well under the
	// unbounded ~18s+ a multi-batch backlog would otherwise take.
	if elapsed > drainTimeout+5*time.Second {
		t.Fatalf("Drain took %v, expected bounded near drainTimeout (%v)", elapsed, drainTimeout)
	}
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
		ReqBody:  json.RawMessage(`{"messages":[{"role":"user","content":"retry test"}]}`),
		RespBody: json.RawMessage(`{"content":[{"type":"text","text":"response"}]}`),
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
		ReqBody:  json.RawMessage(`{"messages":[{"role":"user","content":"client error test"}]}`),
		RespBody: json.RawMessage(`{"content":[{"type":"text","text":"response"}]}`),
	})

	s.Drain()

	if got := attempts.Load(); got != 1 {
		t.Fatalf("expected exactly 1 attempt (no retry on 4xx), got %d", got)
	}
	if st.FlushErrors.Load() != 1 {
		t.Fatalf("expected 1 flush error for 4xx, got %d", st.FlushErrors.Load())
	}
}

func TestNoRetryOnRPCError(t *testing.T) {
	var attempts atomic.Int32

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","error":{"code":-32000,"message":"vault not found"},"id":1}`))
	}))
	defer srv.Close()

	st := &stats.Stats{}
	s := New(srv.URL, "", "test", st)

	s.Store(&CapturedExchange{
		Agent:    "test",
		Path:     "/v1/messages",
		ReqBody:  json.RawMessage(`{"messages":[{"role":"user","content":"rpc error test"}]}`),
		RespBody: json.RawMessage(`{"content":[{"type":"text","text":"response"}]}`),
	})

	s.Drain()

	if got := attempts.Load(); got != 1 {
		t.Fatalf("expected exactly 1 attempt (no retry on rpc error), got %d", got)
	}
	if st.FlushErrors.Load() != 1 {
		t.Fatalf("expected 1 flush error for rpc error, got %d", st.FlushErrors.Load())
	}
}

func TestDeduplication(t *testing.T) {
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

	// Send the same user message 5 times — should be deduped to 1.
	for range 5 {
		s.Store(&CapturedExchange{
			Agent:    "claude",
			Path:     "/v1/messages",
			ReqBody:  json.RawMessage(`{"messages":[{"role":"user","content":[{"type":"text","text":"How do I sort a slice in Go?"}]}]}`),
			RespBody: json.RawMessage(`{"content":[{"type":"text","text":"Use sort.Slice()"}]}`),
		})
	}

	s.Drain()

	if deduped := st.Deduped.Load(); deduped != 4 {
		t.Fatalf("expected 4 deduped, got %d", deduped)
	}
}

func TestSkipEmptyCapture(t *testing.T) {
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

	// Exchange where user message is entirely system-reminder: should be skipped.
	s.Store(&CapturedExchange{
		Agent:    "claude",
		Path:     "/v1/messages",
		ReqBody:  json.RawMessage(`{"messages":[{"role":"user","content":[{"type":"text","text":"<system-reminder>metadata only</system-reminder>"}]}]}`),
		RespBody: json.RawMessage(`{}`),
	})

	// Exchange with no extractable messages at all.
	s.Store(&CapturedExchange{
		Agent:    "claude",
		Path:     "/v1/messages",
		ReqBody:  json.RawMessage(`{"model":"claude-3"}`),
		RespBody: json.RawMessage(`{"ok":true}`),
	})

	s.Drain()

	mu.Lock()
	n := len(received)
	mu.Unlock()

	if n != 0 {
		t.Fatalf("expected 0 MCP calls (all skipped), got %d", n)
	}
	if st.Skipped.Load() != 2 {
		t.Fatalf("expected 2 skipped (empty exchanges), got %d", st.Skipped.Load())
	}
}

func TestStripSystemReminderInCapture(t *testing.T) {
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

	// Exchange where user message has system-reminder mixed with real content.
	s.Store(&CapturedExchange{
		Agent:    "claude",
		Path:     "/v1/messages",
		ReqBody:  json.RawMessage(`{"messages":[{"role":"user","content":[{"type":"text","text":"<system-reminder>ignore this</system-reminder>\nActual user question about Go"}]}]}`),
		RespBody: json.RawMessage(`{"content":[{"type":"text","text":"Here is the answer about Go"}]}`),
	})

	s.Drain()

	mu.Lock()
	defer mu.Unlock()

	if len(received) == 0 {
		t.Fatal("expected at least 1 MCP call")
	}

	// The stored concept should NOT contain system-reminder content.
	combined := strings.Join(received, " ")
	if strings.Contains(combined, "system-reminder") {
		t.Error("stored memory should not contain system-reminder tags")
	}
	if !strings.Contains(combined, "Go") {
		t.Error("stored memory should contain actual user content")
	}
}

func TestFormatAndDedupAnthropic(t *testing.T) {
	st := &stats.Stats{}
	s := &MuninnStore{stats: st, vault: "test"}
	var ring [dedupRingSize]map[uint64]struct{}
	ringIdx := 0

	ex := &CapturedExchange{
		Agent:      "claude",
		Path:       "/v1/messages",
		StatusCode: 200,
		Model:      "claude-3-opus",
		TokensIn:   500,
		TokensOut:  200,
		ReqBody: json.RawMessage(`{
			"model":"claude-3-opus",
			"messages":[{"role":"user","content":[{"type":"text","text":"How do I implement a binary search in Go?"}]}]
		}`),
		RespBody: json.RawMessage(`{
			"content":[{"type":"text","text":"Here is a binary search implementation in Go..."}],
			"usage":{"input_tokens":500,"output_tokens":200}
		}`),
	}

	fm := s.formatAndDedup(ex, &ring, &ringIdx)
	if fm == nil {
		t.Fatal("expected non-nil formatted memory")
	}

	// Concept should include both user query and assistant preview.
	if !strings.Contains(fm.concept, "binary search") {
		t.Errorf("concept should contain user query: %q", fm.concept)
	}
	if !strings.Contains(fm.concept, "→") {
		t.Errorf("concept should contain arrow separator for user→assistant: %q", fm.concept)
	}
	if !strings.Contains(fm.concept, "binary search implementation") {
		t.Errorf("concept should contain assistant preview: %q", fm.concept)
	}
	if strings.Contains(fm.concept, "POST") {
		t.Errorf("concept should not contain HTTP method: %q", fm.concept)
	}

	// Content should have the conversation.
	if !strings.Contains(fm.content, "User:") {
		t.Errorf("content should contain User: section: %q", fm.content)
	}
	if !strings.Contains(fm.content, "binary search") {
		t.Errorf("content should contain user message: %q", fm.content)
	}
	if !strings.Contains(fm.content, "Assistant:") {
		t.Errorf("content should contain Assistant: section: %q", fm.content)
	}
	if !strings.Contains(fm.content, "binary search implementation") {
		t.Errorf("content should contain assistant response: %q", fm.content)
	}

	// Should NOT contain API metadata.
	if strings.Contains(fm.content, "Model:") {
		t.Errorf("content should not contain metadata: %q", fm.content)
	}
}

func TestFormatAndDedupOpenAI(t *testing.T) {
	st := &stats.Stats{}
	s := &MuninnStore{stats: st, vault: "test"}
	var ring [dedupRingSize]map[uint64]struct{}
	ringIdx := 0

	ex := &CapturedExchange{
		Agent: "codex",
		Path:  "/v1/chat/completions",
		Model: "gpt-4",
		ReqBody: json.RawMessage(`{
			"model":"gpt-4",
			"messages":[
				{"role":"system","content":"You are helpful"},
				{"role":"user","content":"Explain goroutines"}
			]
		}`),
		RespBody: json.RawMessage(`{
			"choices":[{"message":{"role":"assistant","content":"Goroutines are lightweight threads..."}}]
		}`),
	}

	fm := s.formatAndDedup(ex, &ring, &ringIdx)
	if fm == nil {
		t.Fatal("expected non-nil formatted memory")
	}

	if !strings.Contains(fm.concept, "goroutine") {
		t.Errorf("concept should contain user query: %q", fm.concept)
	}
	if !strings.Contains(fm.concept, "→") {
		t.Errorf("concept should contain arrow separator for user→assistant: %q", fm.concept)
	}
	if !strings.Contains(fm.content, "Goroutines are lightweight") {
		t.Errorf("content should contain assistant response: %q", fm.content)
	}
}

func TestFormatAndDedupGemini(t *testing.T) {
	st := &stats.Stats{}
	s := &MuninnStore{stats: st, vault: "test"}
	var ring [dedupRingSize]map[uint64]struct{}
	ringIdx := 0

	ex := &CapturedExchange{
		Agent: "gemini",
		Path:  "/v1/generateContent",
		ReqBody: json.RawMessage(`{
			"contents":[{"role":"user","parts":[{"text":"What is Kubernetes?"}]}]
		}`),
		RespBody: json.RawMessage(`{
			"candidates":[{"content":{"parts":[{"text":"Kubernetes is a container orchestration platform..."}]}}]
		}`),
	}

	fm := s.formatAndDedup(ex, &ring, &ringIdx)
	if fm == nil {
		t.Fatal("expected non-nil formatted memory")
	}

	if !strings.Contains(fm.concept, "Kubernetes") {
		t.Errorf("concept should contain user query: %q", fm.concept)
	}
	if !strings.Contains(fm.concept, "→") {
		t.Errorf("concept should contain arrow separator for user→assistant: %q", fm.concept)
	}
	if !strings.Contains(fm.content, "container orchestration") {
		t.Errorf("content should contain assistant response: %q", fm.content)
	}
}

func TestFormatAndDedupEmptySkipped(t *testing.T) {
	// Non-LLM body with no extractable messages: should be skipped.
	st := &stats.Stats{}
	s := &MuninnStore{stats: st, vault: "test"}
	var ring [dedupRingSize]map[uint64]struct{}
	ringIdx := 0

	ex := &CapturedExchange{
		Agent:      "claude",
		Path:       "/v1/messages",
		StatusCode: 200,
		ReqBody:    json.RawMessage(`{"model":"claude-3-opus"}`),
		RespBody:   json.RawMessage(`{"ok":true}`),
	}

	fm := s.formatAndDedup(ex, &ring, &ringIdx)
	if fm != nil {
		t.Error("expected nil for empty exchange (no extractable messages)")
	}
	if st.Skipped.Load() != 1 {
		t.Fatalf("expected 1 skipped, got %d", st.Skipped.Load())
	}
}

// TestBuildTags pins the exact tag list the store sends to muninn, in order.
// A subset check passes when buildTags emits an extra tag (a debug marker, a
// duplicate, a stray "model:" for an empty model) and cannot see a
// misformatted one, since the server routes on these strings.
func TestBuildTags(t *testing.T) {
	cases := []struct {
		name string
		ex   *CapturedExchange
		want []string
	}{
		{
			name: "all fields",
			ex:   &CapturedExchange{Agent: "claude", StatusCode: 200, Model: "claude-3-opus"},
			want: []string{"sidecar", "claude", "status:200", "model:claude-3-opus"},
		},
		{
			name: "no model: no model tag",
			ex:   &CapturedExchange{Agent: "codex", StatusCode: 429},
			want: []string{"sidecar", "codex", "status:429"},
		},
		{
			// A stream capture that never saw a model still gets the three
			// unconditional tags; an empty Agent is passed through rather than
			// silently dropped, so the tag count stays fixed.
			name: "status zero",
			ex:   &CapturedExchange{},
			want: []string{"sidecar", "", "status:0"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := buildTags(tc.ex); !slices.Equal(got, tc.want) {
				t.Errorf("buildTags = %q, want %q", got, tc.want)
			}
		})
	}
}
func TestPartialSystemReminderStrip(t *testing.T) {
	// User message has system-reminders interleaved with real content.
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
		Agent:    "claude",
		Path:     "/v1/messages",
		ReqBody:  json.RawMessage(`{"messages":[{"role":"user","content":[{"type":"text","text":"<system-reminder>first reminder</system-reminder>\nReal question about databases\n<system-reminder>second reminder</system-reminder>\nMore real content here"}]}]}`),
		RespBody: json.RawMessage(`{"content":[{"type":"text","text":"Here is the database answer"}]}`),
	})

	s.Drain()

	mu.Lock()
	defer mu.Unlock()

	if len(received) == 0 {
		t.Fatal("expected MCP call")
	}

	combined := strings.Join(received, " ")
	if strings.Contains(combined, "system-reminder") {
		t.Error("should not contain any system-reminder tags")
	}
	if strings.Contains(combined, "first reminder") {
		t.Error("should not contain reminder content")
	}
	if !strings.Contains(combined, "database") {
		t.Error("should contain real content about databases")
	}
	if !strings.Contains(combined, "More real content") {
		t.Error("should contain the second real content")
	}
}

// TestDedupIsPerStore covers the half of ring expiry that is reachable without
// waiting: a store starts with an empty ring, so a concept another instance
// already wrote is not suppressed. The rotating half (the worker's 2s ticker
// advancing a slot, which is what actually expires an entry) is not exercised
// here — a full expiry takes dedupRingSize flush cycles, and nothing pins the
// rotation at all.
func TestDedupIsPerStore(t *testing.T) {
	// The ring lives in the worker goroutine of one store; a new store does not
	// inherit the previous instance's hashes.
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

	st := &stats.Stats{}
	s := New(srv.URL, "", "test", st)

	msg := `{"messages":[{"role":"user","content":"same message"}]}`
	resp := `{"content":[{"type":"text","text":"same response"}]}`

	// Store the same message twice — second should be deduped.
	s.Store(&CapturedExchange{Agent: "claude", Path: "/v1/messages",
		ReqBody: json.RawMessage(msg), RespBody: json.RawMessage(resp)})
	s.Store(&CapturedExchange{Agent: "claude", Path: "/v1/messages",
		ReqBody: json.RawMessage(msg), RespBody: json.RawMessage(resp)})

	s.Drain()

	if deduped := st.Deduped.Load(); deduped != 1 {
		t.Fatalf("expected 1 deduped, got %d", deduped)
	}
	mu.Lock()
	firstCalls := calls
	mu.Unlock()
	if firstCalls != 1 {
		t.Fatalf("expected 1 MCP call, got %d", firstCalls)
	}

	// New store = fresh ring buffer. Same concept should be stored again.
	st2 := &stats.Stats{}
	s2 := New(srv.URL, "", "test", st2)

	s2.Store(&CapturedExchange{Agent: "claude", Path: "/v1/messages",
		ReqBody: json.RawMessage(msg), RespBody: json.RawMessage(resp)})

	s2.Drain()

	if deduped := st2.Deduped.Load(); deduped != 0 {
		t.Fatalf("expected 0 deduped in fresh store, got %d", deduped)
	}
}

func TestMixedBatchDedupAndValid(t *testing.T) {
	// Send a mix of valid, duplicate, and empty exchanges.
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

	// 1. Valid unique
	s.Store(&CapturedExchange{Agent: "claude", Path: "/v1/messages",
		ReqBody:  json.RawMessage(`{"messages":[{"role":"user","content":"unique question one"}]}`),
		RespBody: json.RawMessage(`{"content":[{"type":"text","text":"answer one"}]}`)})

	// 2. Exact duplicate of #1 (same user message AND same response).
	s.Store(&CapturedExchange{Agent: "claude", Path: "/v1/messages",
		ReqBody:  json.RawMessage(`{"messages":[{"role":"user","content":"unique question one"}]}`),
		RespBody: json.RawMessage(`{"content":[{"type":"text","text":"answer one"}]}`)})

	// 3. Empty (system-reminder only)
	s.Store(&CapturedExchange{Agent: "claude", Path: "/v1/messages",
		ReqBody:  json.RawMessage(`{"messages":[{"role":"user","content":[{"type":"text","text":"<system-reminder>only metadata</system-reminder>"}]}]}`),
		RespBody: json.RawMessage(`{}`)})

	// 4. Valid unique different
	s.Store(&CapturedExchange{Agent: "claude", Path: "/v1/messages",
		ReqBody:  json.RawMessage(`{"messages":[{"role":"user","content":"unique question two"}]}`),
		RespBody: json.RawMessage(`{"content":[{"type":"text","text":"answer two"}]}`)})

	// 5. No messages at all
	s.Store(&CapturedExchange{Agent: "claude", Path: "/v1/messages",
		ReqBody:  json.RawMessage(`{"model":"claude-3"}`),
		RespBody: json.RawMessage(`{"ok":true}`)})

	s.Drain()

	// Should have 2 valid + 1 deduped + 2 skipped.
	if captured := st.Captured.Load(); captured != 5 {
		t.Fatalf("expected 5 captured, got %d", captured)
	}
	if deduped := st.Deduped.Load(); deduped != 1 {
		t.Fatalf("expected 1 deduped (duplicate concept), got %d", deduped)
	}
	if skipped := st.Skipped.Load(); skipped != 2 {
		t.Fatalf("expected 2 skipped (1 empty-after-strip + 1 no-messages), got %d", skipped)
	}
	if flushed := st.Flushed.Load(); flushed != 2 {
		t.Fatalf("expected 2 flushed, got %d", flushed)
	}

	mu.Lock()
	defer mu.Unlock()
	combined := strings.Join(received, " ")
	if !strings.Contains(combined, "unique question one") {
		t.Error("should contain first unique question")
	}
	if !strings.Contains(combined, "unique question two") {
		t.Error("should contain second unique question")
	}
	if strings.Contains(combined, "system-reminder") {
		t.Error("should not contain system-reminder in stored data")
	}
}

func TestConcurrentStoreStats(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		w.Write([]byte(`{"jsonrpc":"2.0","result":{"id":"ok"},"id":1}`))
	}))
	defer srv.Close()

	st := &stats.Stats{}
	s := New(srv.URL, "", "test", st)

	// Concurrent stores from multiple goroutines.
	var wg sync.WaitGroup
	for i := range 20 {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			msg := fmt.Sprintf(`{"messages":[{"role":"user","content":"concurrent msg %d"}]}`, idx)
			resp := fmt.Sprintf(`{"content":[{"type":"text","text":"concurrent resp %d"}]}`, idx)
			s.Store(&CapturedExchange{
				Agent:    "claude",
				Path:     "/v1/messages",
				ReqBody:  json.RawMessage(msg),
				RespBody: json.RawMessage(resp),
				TokensIn: 10,
			})
		}(i)
	}
	wg.Wait()
	s.Drain()

	captured := st.Captured.Load()
	flushed := st.Flushed.Load()
	deduped := st.Deduped.Load()
	skipped := st.Skipped.Load()
	dropped := st.Dropped.Load()

	if captured != 20 {
		t.Fatalf("expected 20 captured, got %d", captured)
	}
	// All items should be accounted for.
	total := flushed + deduped + skipped + dropped
	if total != 20 {
		t.Fatalf("expected flushed(%d) + deduped(%d) + skipped(%d) + dropped(%d) = 20, got %d",
			flushed, deduped, skipped, dropped, total)
	}
	if st.TokensIn.Load() != 200 {
		t.Fatalf("expected 200 tokens in, got %d", st.TokensIn.Load())
	}
}

func TestFormatAndDedupAssistantOnly(t *testing.T) {
	// Exchange where user message is empty but assistant has content.
	// Should still be stored (assistant-only is valid).
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

	// No user message extractable, but assistant has content.
	s.Store(&CapturedExchange{
		Agent:    "claude",
		Path:     "/v1/messages",
		ReqBody:  json.RawMessage(`{"messages":[{"role":"user","content":[{"type":"tool_result","tool_use_id":"tu1","content":"tool output"}]}]}`),
		RespBody: json.RawMessage(`{"content":[{"type":"text","text":"Based on the tool output, here is my analysis"}]}`),
	})

	s.Drain()

	mu.Lock()
	defer mu.Unlock()

	// Even though user message is empty after strip, assistant has content.
	// The formatAndDedup should still store it.
	if len(received) == 0 {
		t.Fatal("expected MCP call for assistant-only exchange")
	}
	combined := strings.Join(received, " ")
	if !strings.Contains(combined, "analysis") {
		t.Error("should contain assistant response")
	}
}

func TestSkipContextContinuation(t *testing.T) {
	st := &stats.Stats{}
	s := &MuninnStore{stats: st, vault: "test"}
	var ring [dedupRingSize]map[uint64]struct{}
	ringIdx := 0

	ex := &CapturedExchange{
		Agent: "claude",
		Path:  "/v1/messages",
		ReqBody: json.RawMessage(`{
			"messages":[{"role":"user","content":"This session is being continued from a previous conversation that ran out of context. The summary below covers the earlier portion of the conversation. Summary: 1. Primary Request..."}]
		}`),
		RespBody: json.RawMessage(`{"content":[{"type":"text","text":"I'll continue from where we left off."}]}`),
	}

	fm := s.formatAndDedup(ex, &ring, &ringIdx)
	if fm != nil {
		t.Error("expected nil for context continuation message")
	}
	if st.Skipped.Load() != 1 {
		t.Fatalf("expected 1 skipped, got %d", st.Skipped.Load())
	}
}

func TestSkipSummaryTask(t *testing.T) {
	st := &stats.Stats{}
	s := &MuninnStore{stats: st, vault: "test"}
	var ring [dedupRingSize]map[uint64]struct{}
	ringIdx := 0

	ex := &CapturedExchange{
		Agent: "claude",
		Path:  "/v1/messages",
		ReqBody: json.RawMessage(`{
			"messages":[{"role":"user","content":"Your task is to create a detailed summary of the conversation so far, paying close attention to the user's explicit requests..."}]
		}`),
		RespBody: json.RawMessage(`{"content":[{"type":"text","text":"<analysis>..."}]}`),
	}

	fm := s.formatAndDedup(ex, &ring, &ringIdx)
	if fm != nil {
		t.Error("expected nil for summary task prompt")
	}
	if st.Skipped.Load() != 1 {
		t.Fatalf("expected 1 skipped, got %d", st.Skipped.Load())
	}
}

func TestIsNoiseContent(t *testing.T) {
	tests := []struct {
		msg   string
		noise bool
	}{
		{"This session is being continued from a previous conversation", true},
		{"This session is being continued from a previous conversation that ran out of context.", true},
		{"Your task is to create a detailed summary of the conversation", true},
		{"How do I sort a slice in Go?", false},
		{"fix the bug in the login handler", false},
		{"", false},
	}

	for _, tt := range tests {
		got := isNoiseContent(tt.msg)
		if got != tt.noise {
			t.Errorf("isNoiseContent(%q) = %v, want %v", tt.msg, got, tt.noise)
		}
	}
}

func TestDoubleDrainNoPanic(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		w.Write([]byte(`{"jsonrpc":"2.0","result":{"id":"ok"},"id":1}`))
	}))
	defer srv.Close()

	s := New(srv.URL, "", "test", nil)
	s.Store(&CapturedExchange{
		Agent:    "test",
		Path:     "/v1/messages",
		ReqBody:  json.RawMessage(`{"messages":[{"role":"user","content":"drain test"}]}`),
		RespBody: json.RawMessage(`{"content":[{"type":"text","text":"ok"}]}`),
	})

	// Double drain should not panic.
	s.Drain()
	s.Drain()
}

func TestStoreHealthCheck(t *testing.T) {
	ok := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) }))
	defer ok.Close()
	if err := New(ok.URL+"/mcp", "", "v", nil).HealthCheck(); err != nil {
		t.Errorf("expected healthy, got %v", err)
	}
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(503) }))
	defer bad.Close()
	if err := New(bad.URL+"/mcp", "", "v", nil).HealthCheck(); err == nil {
		t.Error("expected error on 503 health")
	}
}

// A retry of a remember must be recognisable as the same write: same JSON-RPC
// request id, same per-memory dedup_key. A 500 can arrive after the server
// already committed, so a per-attempt id would store the memory twice.
func TestRetryReusesRequestIDAndDedupKey(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping retry test in short mode (needs ~2s for backoff)")
	}

	type call struct {
		id      int64
		dedup   string
		concept string
	}
	var mu sync.Mutex
	var got []call

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var env struct {
			ID     int64 `json:"id"`
			Params struct {
				Name      string         `json:"name"`
				Arguments map[string]any `json:"arguments"`
			} `json:"params"`
		}
		body, _ := io.ReadAll(r.Body)
		if json.Unmarshal(body, &env) != nil {
			w.WriteHeader(400)
			return
		}
		mu.Lock()
		got = append(got, call{
			id:      env.ID,
			dedup:   env.Params.Arguments["dedup_key"].(string),
			concept: env.Params.Arguments["concept"].(string),
		})
		n := len(got)
		mu.Unlock()
		if n == 1 {
			w.WriteHeader(500) // ambiguous failure: the server may have committed
			return
		}
		w.Write([]byte(`{"jsonrpc":"2.0","result":{"id":"ok"},"id":1}`))
	}))
	defer srv.Close()

	s := New(srv.URL, "", "idempotency-test", &stats.Stats{})
	s.Store(&CapturedExchange{
		Agent:    "test",
		Path:     "/v1/messages",
		ReqBody:  json.RawMessage(`{"messages":[{"role":"user","content":"write me once"}]}`),
		RespBody: json.RawMessage(`{"content":[{"type":"text","text":"stored"}]}`),
	})
	s.Drain()

	mu.Lock()
	defer mu.Unlock()
	if len(got) < 2 {
		t.Fatalf("expected the first attempt plus a retry, got %d attempt(s)", len(got))
	}
	first := got[0]
	if first.dedup == "" {
		t.Fatal("first attempt carried no dedup_key")
	}
	for i, c := range got[1:] {
		if c.id != first.id {
			t.Errorf("attempt %d used request id %d, want %d (a fresh id makes a retry look like a new write)", i+2, c.id, first.id)
		}
		if c.dedup != first.dedup {
			t.Errorf("attempt %d used dedup_key %q, want %q", i+2, c.dedup, first.dedup)
		}
		if c.concept != first.concept {
			t.Errorf("attempt %d used concept %q, want %q", i+2, c.concept, first.concept)
		}
	}
}

// TestSetPreparerConcurrentWithStore pins the synchronization on the preparer
// field. The worker goroutine is already running when SetPreparer installs the
// Preparer, so a plain field would be an unsynchronized read/write pair; the
// mutex makes swapping the preparer while captures flow safe. Run under -race.
func TestSetPreparerConcurrentWithStore(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		w.Write([]byte(`{"jsonrpc":"2.0","result":{"id":"ok"},"id":1}`))
	}))
	defer srv.Close()

	s := New(srv.URL, "", "test", &stats.Stats{})

	stop := make(chan struct{})
	var wg sync.WaitGroup

	// Reinstall the preparer continuously while captures flow, so the worker's
	// read and the caller's write overlap no matter how the two are scheduled.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			s.SetPreparer(func(ex *CapturedExchange) { ex.Model = "test-model" })
		}
	}()

	deadline := time.Now().Add(250 * time.Millisecond)
	for i := 0; time.Now().Before(deadline); i++ {
		s.Store(&CapturedExchange{
			Agent:    "claude",
			Path:     "/v1/messages",
			ReqBody:  json.RawMessage(fmt.Sprintf(`{"messages":[{"role":"user","content":"msg %d"}]}`, i)),
			RespBody: json.RawMessage(fmt.Sprintf(`{"content":[{"type":"text","text":"resp %d"}]}`, i)),
		})
	}
	close(stop)
	wg.Wait()
	s.Drain()
}

func TestStoreQueueFullWarningIsThrottled(t *testing.T) {
	// A sustained MuninnDB outage drops one exchange per request; warning on
	// every drop buries the rest of the log. The first drop and every 100th
	// after it are logged, and the exact count goes to Stats.
	st := &stats.Stats{}
	var logs strings.Builder
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelWarn})))
	defer slog.SetDefault(prev)

	// No worker and a zero-capacity queue: every Store is a drop.
	s := &MuninnStore{queue: make(chan queueItem), stats: st}
	const drops = dropLogEvery*2 + 1
	for range drops {
		s.Store(&CapturedExchange{Path: "/v1/messages"})
	}

	if got := st.Dropped.Load(); got != drops {
		t.Errorf("Dropped = %d, want %d", got, drops)
	}
	if got := s.dropped.Load(); got != drops {
		t.Errorf("internal drop count = %d, want %d", got, drops)
	}
	warnings := strings.Count(logs.String(), "queue full")
	if want := 3; warnings != want {
		t.Errorf("queue-full warnings = %d, want %d (first, then every %d):\n%s", warnings, want, dropLogEvery, logs.String())
	}
}

// mcpCall is one JSON-RPC tool invocation as the server saw it.
type mcpCall struct {
	Name string         `json:"name"`
	Args map[string]any `json:"arguments"`
}

// recordingMCP captures every tool call and answers 200, so a test can assert
// what was written rather than how many calls were made.
func recordingMCP(t *testing.T, calls *[]mcpCall, mu *sync.Mutex) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var env struct {
			Params struct {
				Name      string         `json:"name"`
				Arguments map[string]any `json:"arguments"`
			} `json:"params"`
		}
		body, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(body, &env); err != nil {
			w.WriteHeader(400)
			return
		}
		mu.Lock()
		*calls = append(*calls, mcpCall{Name: env.Params.Name, Args: env.Params.Arguments})
		mu.Unlock()
		w.Write([]byte(`{"jsonrpc":"2.0","result":{"id":"ok"},"id":1}`))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func storedCall(t *testing.T, calls *[]mcpCall, mu *sync.Mutex, i int) mcpCall {
	t.Helper()
	mu.Lock()
	defer mu.Unlock()
	if i >= len(*calls) {
		t.Fatalf("expected at least %d MCP call(s), got %d", i+1, len(*calls))
	}
	return (*calls)[i]
}

// TestWritePayloadSingle pins the arguments of the one-memory write. A payload
// with the wrong tool name, a missing vault, type or dedup_key still gets a
// 200 from a permissive server, so counting calls cannot tell a correct write
// from a broken one; every field the server routes on is asserted here.
func TestWritePayloadSingle(t *testing.T) {
	var (
		mu    sync.Mutex
		calls []mcpCall
	)
	srv := recordingMCP(t, &calls, &mu)

	const vault = "payload-vault"
	s := New(srv.URL, "", vault, &stats.Stats{})
	s.Store(&CapturedExchange{
		Agent:    "claude",
		Path:     "/v1/messages",
		ReqBody:  json.RawMessage(`{"messages":[{"role":"user","content":"how does auth work"}]}`),
		RespBody: json.RawMessage(`{"content":[{"type":"text","text":"use jwt"}]}`),
	})
	s.Drain()

	c := storedCall(t, &calls, &mu, 0)
	if c.Name != "muninn_remember" {
		t.Fatalf("tool name = %q, want muninn_remember", c.Name)
	}
	if got := c.Args["vault"]; got != vault {
		t.Errorf("vault = %v, want %q", got, vault)
	}
	if got := c.Args["type"]; got != "observation" {
		t.Errorf("type = %v, want observation", got)
	}
	for _, key := range []string{"concept", "content", "dedup_key"} {
		if v, ok := c.Args[key]; !ok || v == "" {
			t.Errorf("args[%q] missing or empty in %v", key, c.Args)
		}
	}
	if tags, ok := c.Args["tags"]; !ok || tags == nil {
		t.Errorf("args[tags] missing in %v", c.Args)
	}
	// dedup_key is content-addressed: identical (vault, concept, content) must
	// reproduce it, so a retry lands on the same memory.
	if want := mcpclient.DedupKey(vault, c.Args["concept"].(string), c.Args["content"].(string)); c.Args["dedup_key"] != want {
		t.Errorf("dedup_key = %v, want %q", c.Args["dedup_key"], want)
	}
	// A batch payload here would be silently misrouted by the server.
	if _, ok := c.Args["memories"]; ok {
		t.Error("single write carried a memories array")
	}
}

// TestWritePayloadBatch pins the batched write: the batch tool, a top-level
// vault, and one entry per memory carrying its own dedup_key. Dropping
// dedup_key from this path would let the same memory be written twice and no
// other test would see it, since batch tests assert the call count only.
func TestWritePayloadBatch(t *testing.T) {
	var (
		mu    sync.Mutex
		calls []mcpCall
	)
	srv := recordingMCP(t, &calls, &mu)

	const vault = "batch-vault"
	s := New(srv.URL, "", vault, &stats.Stats{})
	for i := range maxBatchSize {
		s.Store(&CapturedExchange{
			Agent:    "test",
			Path:     "/v1/messages",
			ReqBody:  json.RawMessage(fmt.Sprintf(`{"messages":[{"role":"user","content":"question %d"}]}`, i)),
			RespBody: json.RawMessage(fmt.Sprintf(`{"content":[{"type":"text","text":"answer %d"}]}`, i)),
		})
	}
	s.Drain()

	c := storedCall(t, &calls, &mu, 0)
	if c.Name != "muninn_remember_batch" {
		t.Fatalf("tool name = %q, want muninn_remember_batch", c.Name)
	}
	if got := c.Args["vault"]; got != vault {
		t.Errorf("vault = %v, want %q", got, vault)
	}
	mems, ok := c.Args["memories"].([]any)
	if !ok {
		t.Fatalf("args[memories] is %T, want an array: %v", c.Args["memories"], c.Args)
	}
	if len(mems) != maxBatchSize {
		t.Fatalf("batched %d memories, want %d", len(mems), maxBatchSize)
	}

	seen := make(map[string]bool, len(mems))
	for i, raw := range mems {
		m, ok := raw.(map[string]any)
		if !ok {
			t.Fatalf("memories[%d] is %T, want an object", i, raw)
		}
		if m["type"] != "observation" {
			t.Errorf("memories[%d].type = %v, want observation", i, m["type"])
		}
		for _, key := range []string{"concept", "content", "tags", "dedup_key"} {
			if v, ok := m[key]; !ok || v == nil {
				t.Errorf("memories[%d] missing %q: %v", i, key, m)
			}
		}
		dk, _ := m["dedup_key"].(string)
		if dk == "" {
			t.Errorf("memories[%d] has an empty dedup_key", i)
			continue
		}
		if want := mcpclient.DedupKey(vault, m["concept"].(string), m["content"].(string)); dk != want {
			t.Errorf("memories[%d].dedup_key = %q, want %q", i, dk, want)
		}
		if seen[dk] {
			t.Errorf("memories[%d] reuses dedup_key %q; distinct memories must not collide", i, dk)
		}
		seen[dk] = true
	}
	// The per-memory fields must not leak to the batch envelope, where the
	// server would ignore them.
	for _, key := range []string{"concept", "content", "dedup_key"} {
		if _, ok := c.Args[key]; ok {
			t.Errorf("batch envelope carries a top-level %q", key)
		}
	}
}

func TestStoreQueueFullWarningCarriesRequestID(t *testing.T) {
	// The drop happens on the request path, interleaved with other turns, so the
	// warning must name the turn whose memory was thrown away; without it the
	// only lead is the total, which says how many, never which.
	st := &stats.Stats{}
	var logs strings.Builder
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelWarn})))
	defer slog.SetDefault(prev)

	s := &MuninnStore{queue: make(chan queueItem), stats: st}
	s.Store(&CapturedExchange{RequestID: "req-7", Path: "/v1/messages"})

	if !strings.Contains(logs.String(), "request_id=req-7") {
		t.Errorf("queue-full warning does not name the dropped turn:\n%s", logs.String())
	}
}

func TestQueueDepthReportsBackpressure(t *testing.T) {
	// The queue is where captures pile up when MuninnDB is slow; at capacity
	// the next capture is dropped, so the status endpoint needs to see it.
	s := New("", "", "", nil)
	s.queue <- queueItem{ex: &CapturedExchange{Path: "/v1/messages"}}

	depth, capacity := s.QueueDepth()
	if depth != 1 || capacity != 256 {
		t.Errorf("QueueDepth() = (%d, %d), want (1, 256)", depth, capacity)
	}
	s.Drain()
}

func TestQueueByteBudgetDropsLargeCaptures(t *testing.T) {
	// The depth alone is not a memory bound. A captured request repeats the
	// whole conversation and reaches tens of MiB, so a queue a handful of slots
	// short of full can still be holding gigabytes, and the process dies before
	// the depth says anything is wrong. The byte budget is the back-pressure
	// that does bound it.
	var logs strings.Builder
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelWarn})))
	defer slog.SetDefault(prev)

	st := &stats.Stats{}
	// No worker: nothing drains the channel, so the fill is deterministic.
	s := &MuninnStore{queue: make(chan queueItem, 256), stats: st}

	// Exactly maxQueuedBytes/8MiB captures fit; the next one does not.
	const chunk = 8 << 20
	big := func() *CapturedExchange {
		return &CapturedExchange{Path: "/v1/messages", ReqBody: json.RawMessage(bytes.Repeat([]byte("a"), chunk))}
	}
	for range maxQueuedBytes / chunk {
		s.Store(big())
	}
	if got := st.Dropped.Load(); got != 0 {
		t.Fatalf("dropped %d exchanges below the byte budget, want 0", got)
	}

	// The queue is nowhere near its 256 slots here, so the depth would read
	// healthy: only the byte budget catches this.
	s.Store(big())
	if got := st.Dropped.Load(); got != 1 {
		t.Errorf("Dropped = %d, want 1 past the byte budget", got)
	}
	if got := len(s.queue); got != maxQueuedBytes/chunk {
		t.Errorf("queue holds %d exchanges, want %d", got, maxQueuedBytes/chunk)
	}
	if inFlight, capacity := s.QueueBytes(); inFlight != maxQueuedBytes || capacity != maxQueuedBytes {
		t.Errorf("QueueBytes() = (%d, %d), want (%d, %d)",
			inFlight, capacity, int64(maxQueuedBytes), int64(maxQueuedBytes))
	}
	if !strings.Contains(logs.String(), "byte budget") {
		t.Errorf("byte-budget drop is not named in the log:\n%s", logs.String())
	}
}

func TestQueuedBytesReturnAfterDelivery(t *testing.T) {
	// The budget is only a bound while the accounting is exact: the worker
	// releases each exchange's bytes once it has formatted it, so a long
	// session does not slowly starve itself of budget and start dropping.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		w.Write([]byte(`{"jsonrpc":"2.0","result":{"id":"ok"},"id":1}`))
	}))
	defer srv.Close()

	s := New(srv.URL, "", "test", nil)
	s.Store(&CapturedExchange{
		Path:     "/v1/messages",
		ReqBody:  json.RawMessage(`{"messages":[{"role":"user","content":"hello"}]}`),
		RespBody: json.RawMessage(`{"content":[{"type":"text","text":"hi"}]}`),
	})
	s.Drain()

	if inFlight, _ := s.QueueBytes(); inFlight != 0 {
		t.Errorf("queued bytes = %d after drain, want 0", inFlight)
	}
}

func TestStoreRacingDrainDropsInsteadOfPanning(t *testing.T) {
	// Shutdown runs p.Shutdown then muninn.Drain(), and Shutdown is allowed to
	// time out with requests still in flight — a request goroutine can reach
	// Store() after the queue is closed. Sending into that closed channel is a
	// data race the runtime reports, and losing it panics the request goroutine,
	// so the drop has to be decided under a lock rather than caught from a
	// deferred recover. Assert both halves: no race, and every reservation
	// accounted for.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","result":{"id":"ok"},"id":1}`))
	}))
	defer srv.Close()

	for round := range 20 {
		s := New(srv.URL, "", "test", nil)

		var stores sync.WaitGroup
		stop := make(chan struct{})
		for range 8 {
			stores.Add(1)
			go func() {
				defer stores.Done()
				for {
					select {
					case <-stop:
						return
					default:
					}
					s.Store(&CapturedExchange{
						Path:     "/v1/messages",
						ReqBody:  json.RawMessage(`{"messages":[{"role":"user","content":"hi"}]}`),
						RespBody: json.RawMessage(`{"content":[{"type":"text","text":"ok"}]}`),
					})
				}
			}()
		}

		s.Drain()
		close(stop)
		stores.Wait()

		// Every Store that returned released what it reserved, so the budget is
		// back to zero and a later session is not starved by a leak.
		if inFlight, _ := s.QueueBytes(); inFlight != 0 {
			t.Fatalf("round %d: %d bytes still reserved after drain", round, inFlight)
		}
		s.Close()
	}
}

func TestStoreAfterDrainIsDroppedAndCounted(t *testing.T) {
	// The documented post-Drain contract: a late arrival is dropped with a
	// warning and counted, not delivered and not silently lost.
	st := &stats.Stats{}
	s := New("", "", "test", st)
	s.Drain()

	s.Store(&CapturedExchange{
		Path:     "/v1/messages",
		ReqBody:  json.RawMessage(`{"a":1}`),
		RespBody: json.RawMessage(`{"b":2}`),
	})

	if got := st.Dropped.Load(); got != 1 {
		t.Errorf("Dropped = %d after a post-drain Store, want 1", got)
	}
	if inFlight, _ := s.QueueBytes(); inFlight != 0 {
		t.Errorf("queued bytes = %d after a post-drain Store, want 0", inFlight)
	}
	s.Close()
}

func TestBatchRequestIDsNamesTheLostTurns(t *testing.T) {
	// A failed flush line is the only record that a turn's memories were not
	// written. Name the turns, cap the list so the error stays readable, and
	// say "unknown" rather than emit an empty field when nothing carries an ID.
	tests := []struct {
		name  string
		batch []formattedMemory
		want  string
	}{
		{"single", []formattedMemory{{requestID: "req-1"}}, "req-1"},
		{"batch", []formattedMemory{{requestID: "req-1"}, {requestID: "req-2"}}, "req-1,req-2"},
		{"capped", []formattedMemory{
			{requestID: "req-1"}, {requestID: "req-2"}, {requestID: "req-3"}, {requestID: "req-4"},
		}, "req-1,req-2,req-3,..."},
		{"missing", []formattedMemory{{}, {requestID: "req-2"}}, "unknown,req-2"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := batchRequestIDs(tc.batch); got != tc.want {
				t.Errorf("batchRequestIDs() = %q, want %q", got, tc.want)
			}
		})
	}
}
