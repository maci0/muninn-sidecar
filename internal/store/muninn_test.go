package store

import (
	"encoding/json"
	"fmt"
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
			Timestamp: time.Now(),
			Agent:     "test",
			Path:      "/v1/messages",
			ReqBody:   json.RawMessage(msg),
			RespBody:  json.RawMessage(resp),
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
	for i := range 300 {
		msg := fmt.Sprintf(`{"messages":[{"role":"user","content":"overflow test %d"}]}`, i)
		resp := fmt.Sprintf(`{"content":[{"type":"text","text":"response %d"}]}`, i)
		s.Store(&CapturedExchange{
			Agent:    "test",
			Path:     "/v1/messages",
			ReqBody:  json.RawMessage(msg),
			RespBody: json.RawMessage(resp),
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
			Timestamp: time.Now(),
			Agent:     "claude",
			Method:    "POST",
			Path:      "/v1/messages",
			ReqBody:   json.RawMessage(`{"messages":[{"role":"user","content":[{"type":"text","text":"How do I sort a slice in Go?"}]}]}`),
			RespBody:  json.RawMessage(`{"content":[{"type":"text","text":"Use sort.Slice()"}]}`),
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
		Timestamp: time.Now(),
		Agent:     "claude",
		Method:    "POST",
		Path:      "/v1/messages",
		ReqBody:   json.RawMessage(`{"messages":[{"role":"user","content":[{"type":"text","text":"<system-reminder>metadata only</system-reminder>"}]}]}`),
		RespBody:  json.RawMessage(`{}`),
	})

	// Exchange with no extractable messages at all.
	s.Store(&CapturedExchange{
		Timestamp: time.Now(),
		Agent:     "claude",
		Method:    "POST",
		Path:      "/v1/messages",
		ReqBody:   json.RawMessage(`{"model":"claude-3"}`),
		RespBody:  json.RawMessage(`{"ok":true}`),
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
		Timestamp: time.Now(),
		Agent:     "claude",
		Method:    "POST",
		Path:      "/v1/messages",
		ReqBody:   json.RawMessage(`{"messages":[{"role":"user","content":[{"type":"text","text":"<system-reminder>ignore this</system-reminder>\nActual user question about Go"}]}]}`),
		RespBody:  json.RawMessage(`{"content":[{"type":"text","text":"Here is the answer about Go"}]}`),
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
		Timestamp:  time.Date(2026, 3, 10, 12, 0, 0, 0, time.UTC),
		Agent:      "claude",
		Method:     "POST",
		Path:       "/v1/messages",
		StatusCode: 200,
		DurationMs: 1500,
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

	// Concept should be the user's message, not HTTP metadata.
	if !strings.Contains(fm.concept, "binary search") {
		t.Errorf("concept should contain user query: %q", fm.concept)
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
		Timestamp: time.Date(2026, 3, 10, 12, 0, 0, 0, time.UTC),
		Agent:     "codex",
		Method:    "POST",
		Path:      "/v1/chat/completions",
		Model:     "gpt-4",
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
		Timestamp: time.Date(2026, 3, 10, 12, 0, 0, 0, time.UTC),
		Agent:     "gemini",
		Method:    "POST",
		Path:      "/v1/generateContent",
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
		Timestamp:  time.Date(2026, 3, 10, 12, 0, 0, 0, time.UTC),
		Agent:      "claude",
		Method:     "POST",
		Path:       "/v1/messages",
		StatusCode: 200,
		DurationMs: 1500,
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
		Timestamp: time.Now(),
		Agent:     "claude",
		Method:    "POST",
		Path:      "/v1/messages",
		ReqBody: json.RawMessage(`{"messages":[{"role":"user","content":[{"type":"text","text":"<system-reminder>first reminder</system-reminder>\nReal question about databases\n<system-reminder>second reminder</system-reminder>\nMore real content here"}]}]}`),
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

func TestDedupRingExpiry(t *testing.T) {
	// After enough time passes (ticker advances ring), the same concept
	// should be storable again. We simulate this by draining and creating
	// a new store (since the ring resets).
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

	// 2. Duplicate of #1
	s.Store(&CapturedExchange{Agent: "claude", Path: "/v1/messages",
		ReqBody:  json.RawMessage(`{"messages":[{"role":"user","content":"unique question one"}]}`),
		RespBody: json.RawMessage(`{"content":[{"type":"text","text":"answer one again"}]}`)})

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
		Method:   "POST",
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
