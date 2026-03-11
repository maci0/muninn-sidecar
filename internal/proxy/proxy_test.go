package proxy

import (
	"compress/gzip"
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

	"github.com/maci0/muninn-sidecar/internal/inject"
	"github.com/maci0/muninn-sidecar/internal/stats"
	"github.com/maci0/muninn-sidecar/internal/store"
)

func TestProxyForwardsAndCaptures(t *testing.T) {
	// Fake upstream API.
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req map[string]any
		json.Unmarshal(body, &req)

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"id":    "msg_123",
			"model": req["model"],
			"usage": map[string]any{
				"input_tokens":  100,
				"output_tokens": 50,
			},
			"content": []map[string]string{
				{"type": "text", "text": "Hello!"},
			},
		})
	}))
	defer upstream.Close()

	// Fake muninn: accept and count calls. Protected by mu for concurrent
	// handler access.
	var (
		mu       sync.Mutex
		captured [][]byte
	)
	muninn := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		captured = append(captured, body)
		mu.Unlock()
		w.WriteHeader(200)
		w.Write([]byte(`{"jsonrpc":"2.0","result":{"id":"test"},"id":1}`))
	}))
	defer muninn.Close()

	st := store.New(muninn.URL, "", "test", nil)

	p, err := New(Config{
		ListenAddr: "127.0.0.1:0",
		Upstream:   upstream.URL,
		AgentName:  "test-agent",
		Store:      st,
	})
	if err != nil {
		t.Fatal(err)
	}

	addr, err := p.Start()
	if err != nil {
		t.Fatal(err)
	}

	// Send a request through the proxy.
	reqBody := `{"model":"claude-3-opus","messages":[{"role":"user","content":"hi"}]}`
	resp, err := http.Post("http://"+addr+"/v1/messages", "application/json", strings.NewReader(reqBody))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}

	// Verify response was forwarded.
	var respData map[string]any
	json.NewDecoder(resp.Body).Decode(&respData)
	if respData["id"] != "msg_123" {
		t.Fatalf("expected id=msg_123, got %v", respData["id"])
	}

	// Deterministic flush: Drain waits for all queued items to be sent.
	st.Drain()

	mu.Lock()
	n := len(captured)
	mu.Unlock()

	if n == 0 {
		t.Fatal("expected muninn to receive captured data")
	}

	t.Logf("captured %d MCP calls to muninn", n)
}

func TestCapturePathFiltering(t *testing.T) {
	var (
		mu       sync.Mutex
		captured []string // MCP request bodies received by fake muninn
	)

	// Upstream echoes back the request path.
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"path": r.URL.Path})
	}))
	defer upstream.Close()

	muninn := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		captured = append(captured, string(body))
		mu.Unlock()
		w.WriteHeader(200)
		w.Write([]byte(`{"jsonrpc":"2.0","result":{"id":"test"},"id":1}`))
	}))
	defer muninn.Close()

	st := store.New(muninn.URL, "", "test", nil)

	p, err := New(Config{
		ListenAddr:   "127.0.0.1:0",
		Upstream:     upstream.URL,
		AgentName:    "test-agent",
		Store:        st,
		CapturePaths: []string{"GenerateContent", "/v1/messages"},
	})
	if err != nil {
		t.Fatal(err)
	}

	addr, err := p.Start()
	if err != nil {
		t.Fatal(err)
	}
	base := "http://" + addr

	// Should be captured: matches "GenerateContent" (OAuth mode, camelCase)
	resp, _ := http.Post(base+"/v1internal:streamGenerateContent", "application/json", strings.NewReader(`{"contents":[{"role":"user","parts":[{"text":"gemini q1"}]}]}`))
	resp.Body.Close()

	// Should be captured: matches "GenerateContent" case-insensitively (API key mode, lowercase)
	resp, _ = http.Post(base+"/v1beta/models/gemini-pro:generateContent", "application/json", strings.NewReader(`{"contents":[{"role":"user","parts":[{"text":"gemini q2"}]}]}`))
	resp.Body.Close()

	// Should be captured: matches "/v1/messages"
	resp, _ = http.Post(base+"/v1/messages", "application/json", strings.NewReader(`{"messages":[{"role":"user","content":"claude q1"}]}`))
	resp.Body.Close()

	// Should NOT be captured: no match
	resp, _ = http.Post(base+"/loadCodeAssist", "application/json", strings.NewReader(`{}`))
	resp.Body.Close()

	// Should NOT be captured: no match
	resp, _ = http.Post(base+"/retrieveUserQuota", "application/json", strings.NewReader(`{}`))
	resp.Body.Close()

	st.Drain()

	mu.Lock()
	defer mu.Unlock()

	// The store batches exchanges, so count memories inside the MCP calls
	// rather than raw HTTP calls. Each call is a JSON-RPC envelope.
	totalMemories := 0
	for _, body := range captured {
		if strings.Contains(body, "muninn_remember_batch") {
			// Batch call: count the entries in the "memories" array.
			var rpc struct {
				Params struct {
					Arguments struct {
						Memories []json.RawMessage `json:"memories"`
					} `json:"arguments"`
				} `json:"params"`
			}
			if json.Unmarshal([]byte(body), &rpc) == nil {
				totalMemories += len(rpc.Params.Arguments.Memories)
			}
		} else if strings.Contains(body, "muninn_remember") {
			totalMemories++
		}
	}

	if totalMemories != 3 {
		t.Fatalf("expected exactly 3 captured exchanges, got %d (in %d MCP calls)", totalMemories, len(captured))
	}
	t.Logf("correctly captured 3 out of 5 requests (in %d MCP call(s))", len(captured))
}

func TestExcludePaths(t *testing.T) {
	var (
		mu       sync.Mutex
		captured []string
	)

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"ok": true})
	}))
	defer upstream.Close()

	muninn := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		captured = append(captured, string(body))
		mu.Unlock()
		w.WriteHeader(200)
		w.Write([]byte(`{"jsonrpc":"2.0","result":{"id":"test"},"id":1}`))
	}))
	defer muninn.Close()

	st := store.New(muninn.URL, "", "test", nil)

	p, err := New(Config{
		ListenAddr:   "127.0.0.1:0",
		Upstream:     upstream.URL,
		AgentName:    "test-agent",
		Store:        st,
		CapturePaths: []string{"/v1/messages"},
		ExcludePaths: []string{"/count_tokens"},
	})
	if err != nil {
		t.Fatal(err)
	}

	addr, err := p.Start()
	if err != nil {
		t.Fatal(err)
	}
	base := "http://" + addr

	// Should be captured: matches /v1/messages, not excluded.
	resp, _ := http.Post(base+"/v1/messages", "application/json", strings.NewReader(`{"messages":[{"role":"user","content":"hello"}]}`))
	resp.Body.Close()

	// Should NOT be captured: matches exclude /count_tokens.
	resp, _ = http.Post(base+"/v1/messages/count_tokens", "application/json", strings.NewReader(`{"messages":[{"role":"user","content":"count"}]}`))
	resp.Body.Close()

	st.Drain()

	mu.Lock()
	defer mu.Unlock()

	totalMemories := 0
	for _, body := range captured {
		if strings.Contains(body, "muninn_remember_batch") {
			var rpc struct {
				Params struct {
					Arguments struct {
						Memories []json.RawMessage `json:"memories"`
					} `json:"arguments"`
				} `json:"params"`
			}
			if json.Unmarshal([]byte(body), &rpc) == nil {
				totalMemories += len(rpc.Params.Arguments.Memories)
			}
		} else if strings.Contains(body, "muninn_remember") {
			totalMemories++
		}
	}

	if totalMemories != 1 {
		t.Fatalf("expected 1 captured exchange (count_tokens excluded), got %d", totalMemories)
	}
	t.Logf("correctly captured 1, excluded count_tokens")
}

func TestCapturePathStreamingSSE(t *testing.T) {
	// Upstream returns SSE stream.
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		flusher, ok := w.(http.Flusher)
		if !ok {
			t.Fatal("expected flusher")
		}
		w.WriteHeader(200)

		events := []string{
			`data: {"type":"content_block_delta","delta":{"text":"Hello"}}`,
			`data: {"type":"message_stop","usage":{"input_tokens":10,"output_tokens":5}}`,
			`data: [DONE]`,
		}
		for _, e := range events {
			w.Write([]byte(e + "\n\n"))
			flusher.Flush()
		}
	}))
	defer upstream.Close()

	var (
		mu       sync.Mutex
		captured []string
	)
	muninn := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		captured = append(captured, string(body))
		mu.Unlock()
		w.WriteHeader(200)
		w.Write([]byte(`{"jsonrpc":"2.0","result":{"id":"test"},"id":1}`))
	}))
	defer muninn.Close()

	st := store.New(muninn.URL, "", "test", nil)

	p, err := New(Config{
		ListenAddr:   "127.0.0.1:0",
		Upstream:     upstream.URL,
		AgentName:    "test-agent",
		Store:        st,
		CapturePaths: []string{"/v1/messages"},
	})
	if err != nil {
		t.Fatal(err)
	}

	addr, err := p.Start()
	if err != nil {
		t.Fatal(err)
	}
	base := "http://" + addr

	// SSE request that SHOULD be captured.
	resp, err := http.Post(base+"/v1/messages", "application/json", strings.NewReader(`{"model":"claude","stream":true,"messages":[{"role":"user","content":"stream test"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	// Must fully read body for streamCapture to see EOF.
	io.ReadAll(resp.Body)
	resp.Body.Close()

	// SSE request that should NOT be captured (path doesn't match).
	resp2, err := http.Post(base+"/v1/other", "application/json", strings.NewReader(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	io.ReadAll(resp2.Body)
	resp2.Body.Close()

	st.Drain()

	mu.Lock()
	n := len(captured)
	mu.Unlock()

	if n != 1 {
		t.Fatalf("expected exactly 1 captured SSE exchange, got %d", n)
	}
	t.Logf("correctly captured 1 SSE stream, skipped 1 non-matching path")
}

// --- End-to-end injection tests ---

// fakeWhereLeftOffEmpty returns an empty where_left_off JSON-RPC response.
func fakeWhereLeftOffEmpty() []byte {
	inner, _ := json.Marshal(map[string]any{"memories": []any{}})
	resp, _ := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"result": map[string]any{
			"content": []map[string]any{
				{"type": "text", "text": string(inner)},
			},
		},
		"id": 1,
	})
	return resp
}

// fakeRecallResponse builds a JSON-RPC recall response with the given memories.
func fakeRecallResponse(memories []map[string]any) []byte {
	inner, _ := json.Marshal(map[string]any{"memories": memories})
	resp, _ := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"result": map[string]any{
			"content": []map[string]any{
				{"type": "text", "text": string(inner)},
			},
		},
		"id": 1,
	})
	return resp
}

func TestProxyInjectionE2E(t *testing.T) {
	// This tests the full pipeline:
	// 1. Request hits proxy with injection enabled
	// 2. Injector recalls memories from MuninnDB
	// 3. Request is enriched with recalled memories and forwarded upstream
	// 4. Upstream sees the enriched request
	// 5. Response is captured to MuninnDB WITHOUT the injected context

	var (
		upstreamMu   sync.Mutex
		upstreamBody string // request body as received by upstream
	)

	// Upstream: capture what it receives and respond.
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		upstreamMu.Lock()
		upstreamBody = string(body)
		upstreamMu.Unlock()

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"id":    "msg_enriched",
			"model": "claude-3-opus",
			"content": []map[string]string{
				{"type": "text", "text": "I see you prefer Go! Here is my answer."},
			},
			"usage": map[string]any{
				"input_tokens":  200,
				"output_tokens": 50,
			},
		})
	}))
	defer upstream.Close()

	// MuninnDB: handle both recall (from injector) and remember (from store).
	var (
		storeMu   sync.Mutex
		storeCalls []string // MCP calls for storing memories
	)
	muninn := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		bodyStr := string(body)

		var rpc struct {
			Params struct {
				Name string `json:"name"`
			} `json:"params"`
		}
		json.Unmarshal(body, &rpc)

		switch rpc.Params.Name {
		case "muninn_where_left_off":
			w.Write(fakeWhereLeftOffEmpty())
		case "muninn_recall":
			w.Write(fakeRecallResponse([]map[string]any{
				{"id": "mem1", "concept": "Go preference", "content": "User prefers Go for backend services", "score": 0.92},
			}))
		case "muninn_remember", "muninn_remember_batch":
			storeMu.Lock()
			storeCalls = append(storeCalls, bodyStr)
			storeMu.Unlock()
			w.WriteHeader(200)
			w.Write([]byte(`{"jsonrpc":"2.0","result":{"id":"ok"},"id":1}`))
		default:
			w.WriteHeader(200)
			w.Write([]byte(`{"jsonrpc":"2.0","result":{},"id":1}`))
		}
	}))
	defer muninn.Close()

	sessionStats := &stats.Stats{}
	st := store.New(muninn.URL, "", "test", sessionStats)

	injector := inject.New(inject.Config{
		MCPURL:  muninn.URL,
		Vault:   "test",
		Budget:  2048,
		Timeout: 2 * time.Second,
		Stats:   sessionStats,
	})

	p, err := New(Config{
		ListenAddr:   "127.0.0.1:0",
		Upstream:     upstream.URL,
		AgentName:    "claude",
		Store:        st,
		CapturePaths: []string{"/v1/messages"},
		Injector:     injector,
	})
	if err != nil {
		t.Fatal(err)
	}

	addr, err := p.Start()
	if err != nil {
		t.Fatal(err)
	}

	// Send request through proxy.
	reqBody := `{"model":"claude-3-opus","system":"You are helpful","messages":[{"role":"user","content":"What language should I use?"}]}`
	resp, err := http.Post("http://"+addr+"/v1/messages", "application/json", strings.NewReader(reqBody))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 200, got %d: %s", resp.StatusCode, body)
	}

	// Verify response was forwarded correctly.
	var respData map[string]any
	json.NewDecoder(resp.Body).Decode(&respData)
	if respData["id"] != "msg_enriched" {
		t.Fatalf("expected id=msg_enriched, got %v", respData["id"])
	}

	st.Drain()

	// 1. Verify upstream received ENRICHED request (with recalled memories).
	upstreamMu.Lock()
	upBody := upstreamBody
	upstreamMu.Unlock()

	if !strings.Contains(upBody, "retrieved-context") {
		t.Error("upstream should receive enriched request with retrieved-context")
	}
	if !strings.Contains(upBody, "Go preference") || !strings.Contains(upBody, "prefers Go") {
		t.Error("upstream should receive recalled memory content")
	}

	// 2. Verify captured exchange in MuninnDB does NOT contain injected context.
	storeMu.Lock()
	calls := storeCalls
	storeMu.Unlock()

	if len(calls) == 0 {
		t.Fatal("expected at least one store call to MuninnDB")
	}

	combined := strings.Join(calls, " ")
	if strings.Contains(combined, "retrieved-context") {
		t.Error("captured exchange should NOT contain injected context (should be stripped)")
	}

	// 3. The captured exchange SHOULD contain the user's actual message.
	if !strings.Contains(combined, "What language should I use") {
		t.Error("captured exchange should contain the original user message")
	}

	// 4. Verify injection stats.
	if sessionStats.Injections.Load() != 1 {
		t.Errorf("expected 1 injection, got %d", sessionStats.Injections.Load())
	}
	if sessionStats.InjectedTokens.Load() <= 0 {
		t.Error("expected positive injected token count")
	}

	t.Logf("e2e injection: upstream enriched, capture clean, stats correct")
}

func TestProxyInjectionStrippedFromCapture(t *testing.T) {
	// Verify that MCP tool traces in the request body are filtered from
	// the captured exchange stored to MuninnDB.

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"content": []map[string]string{
				{"type": "text", "text": "Response"},
			},
		})
	}))
	defer upstream.Close()

	var (
		storeMu   sync.Mutex
		storeCalls []string
	)
	muninn := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		bodyStr := string(body)

		var rpc struct {
			Params struct {
				Name string `json:"name"`
			} `json:"params"`
		}
		json.Unmarshal(body, &rpc)

		if rpc.Params.Name == "muninn_remember" || rpc.Params.Name == "muninn_remember_batch" {
			storeMu.Lock()
			storeCalls = append(storeCalls, bodyStr)
			storeMu.Unlock()
		}

		w.WriteHeader(200)
		w.Write([]byte(`{"jsonrpc":"2.0","result":{"id":"ok"},"id":1}`))
	}))
	defer muninn.Close()

	st := store.New(muninn.URL, "", "test", nil)

	p, err := New(Config{
		ListenAddr:   "127.0.0.1:0",
		Upstream:     upstream.URL,
		AgentName:    "claude",
		Store:        st,
		CapturePaths: []string{"/v1/messages"},
	})
	if err != nil {
		t.Fatal(err)
	}

	addr, err := p.Start()
	if err != nil {
		t.Fatal(err)
	}

	// Request body with muninn tool calls in the conversation history.
	reqBody := `{
		"model":"claude-3-opus",
		"messages":[
			{"role":"user","content":"hello"},
			{"role":"assistant","content":[
				{"type":"text","text":"Let me check memory."},
				{"type":"tool_use","id":"tu1","name":"mcp__muninn__muninn_recall","input":{"context":["hello"]}}
			]},
			{"role":"user","content":[
				{"type":"tool_result","tool_use_id":"tu1","content":"some memory"}
			]},
			{"role":"assistant","content":[{"type":"text","text":"Based on memory..."}]},
			{"role":"user","content":"thanks"}
		],
		"tools":[
			{"name":"Read","description":"Read a file","input_schema":{}},
			{"name":"mcp__muninn__muninn_recall","description":"Recall memories","input_schema":{}}
		]
	}`

	resp, err := http.Post("http://"+addr+"/v1/messages", "application/json", strings.NewReader(reqBody))
	if err != nil {
		t.Fatal(err)
	}
	io.ReadAll(resp.Body)
	resp.Body.Close()

	st.Drain()

	storeMu.Lock()
	defer storeMu.Unlock()

	if len(storeCalls) == 0 {
		t.Fatal("expected store call")
	}

	combined := strings.Join(storeCalls, " ")

	// Muninn tool calls should be filtered from the stored body.
	if strings.Contains(combined, "muninn_recall") {
		t.Error("captured exchange should not contain muninn tool calls")
	}

	// Non-muninn tools and real messages should remain.
	if !strings.Contains(combined, "thanks") {
		t.Error("captured exchange should contain the user's actual message")
	}
}

// --- Proxy edge-case tests ---

func TestProxyUpstreamError(t *testing.T) {
	// Upstream returns 500 — proxy should forward the error to the client.
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(500)
		w.Write([]byte(`{"error":"internal server error"}`))
	}))
	defer upstream.Close()

	st := store.New("http://localhost:1", "", "test", nil) // unreachable store is fine
	p, err := New(Config{
		ListenAddr: "127.0.0.1:0",
		Upstream:   upstream.URL,
		AgentName:  "test",
		Store:      st,
	})
	if err != nil {
		t.Fatal(err)
	}
	addr, err := p.Start()
	if err != nil {
		t.Fatal(err)
	}

	resp, err := http.Post("http://"+addr+"/v1/messages", "application/json",
		strings.NewReader(`{"messages":[{"role":"user","content":"test"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 500 {
		t.Fatalf("expected 500 from upstream, got %d", resp.StatusCode)
	}

	st.Drain()
}

func TestProxyConcurrentRequests(t *testing.T) {
	// Verify proxy handles concurrent requests without data races.
	var requestCount atomic.Int32

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestCount.Add(1)
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"content": []map[string]string{
				{"type": "text", "text": fmt.Sprintf("response %d", requestCount.Load())},
			},
		})
	}))
	defer upstream.Close()

	muninn := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		w.Write([]byte(`{"jsonrpc":"2.0","result":{"id":"ok"},"id":1}`))
	}))
	defer muninn.Close()

	sessionStats := &stats.Stats{}
	st := store.New(muninn.URL, "", "test", sessionStats)

	p, err := New(Config{
		ListenAddr:   "127.0.0.1:0",
		Upstream:     upstream.URL,
		AgentName:    "test",
		Store:        st,
		CapturePaths: []string{"/v1/messages"},
	})
	if err != nil {
		t.Fatal(err)
	}
	addr, err := p.Start()
	if err != nil {
		t.Fatal(err)
	}

	const n = 20
	var wg sync.WaitGroup
	wg.Add(n)
	for i := range n {
		go func(idx int) {
			defer wg.Done()
			body := fmt.Sprintf(`{"messages":[{"role":"user","content":"concurrent request %d"}]}`, idx)
			resp, err := http.Post("http://"+addr+"/v1/messages", "application/json", strings.NewReader(body))
			if err != nil {
				t.Errorf("request %d failed: %v", idx, err)
				return
			}
			io.ReadAll(resp.Body)
			resp.Body.Close()
			if resp.StatusCode != 200 {
				t.Errorf("request %d: expected 200, got %d", idx, resp.StatusCode)
			}
		}(i)
	}
	wg.Wait()

	st.Drain()

	if got := requestCount.Load(); got != n {
		t.Fatalf("expected %d upstream requests, got %d", n, got)
	}
	if got := sessionStats.Captured.Load(); got != int64(n) {
		t.Fatalf("expected %d captured, got %d", n, got)
	}
	t.Logf("concurrent: %d requests, %d captured, %d flushed",
		n, sessionStats.Captured.Load(), sessionStats.Flushed.Load())
}

func TestProxyNonJSONBody(t *testing.T) {
	// Non-JSON request bodies should be forwarded without crashing.
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "text/plain")
		w.Write(body) // echo back
	}))
	defer upstream.Close()

	st := store.New("http://localhost:1", "", "test", nil)
	p, err := New(Config{
		ListenAddr:   "127.0.0.1:0",
		Upstream:     upstream.URL,
		AgentName:    "test",
		Store:        st,
		CapturePaths: []string{"/v1/messages"},
	})
	if err != nil {
		t.Fatal(err)
	}
	addr, err := p.Start()
	if err != nil {
		t.Fatal(err)
	}

	// Send plain text through a capture path.
	resp, err := http.Post("http://"+addr+"/v1/messages", "text/plain",
		strings.NewReader("this is not json"))
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()

	if resp.StatusCode != 200 {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	if string(body) != "this is not json" {
		t.Fatalf("expected echo, got %q", body)
	}
	st.Drain()
}

func TestProxyGzipResponse(t *testing.T) {
	// Upstream returns gzip-encoded response — proxy should decompress for capture.
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Content-Encoding", "gzip")

		gw := gzip.NewWriter(w)
		json.NewEncoder(gw).Encode(map[string]any{
			"content": []map[string]string{
				{"type": "text", "text": "gzipped response"},
			},
		})
		gw.Close()
	}))
	defer upstream.Close()

	muninn := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		w.Write([]byte(`{"jsonrpc":"2.0","result":{"id":"ok"},"id":1}`))
	}))
	defer muninn.Close()

	st := store.New(muninn.URL, "", "test", nil)
	p, err := New(Config{
		ListenAddr:   "127.0.0.1:0",
		Upstream:     upstream.URL,
		AgentName:    "test",
		Store:        st,
		CapturePaths: []string{"/v1/messages"},
	})
	if err != nil {
		t.Fatal(err)
	}
	addr, err := p.Start()
	if err != nil {
		t.Fatal(err)
	}

	resp, err := http.Post("http://"+addr+"/v1/messages", "application/json",
		strings.NewReader(`{"messages":[{"role":"user","content":"gzip test"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()

	if resp.StatusCode != 200 {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}

	// The proxy decompresses gzip for the client.
	if !strings.Contains(string(body), "gzipped response") {
		t.Fatalf("expected decompressed response, got %q", body)
	}

	st.Drain()
}

func TestProxyNonCapturedPathsPassThrough(t *testing.T) {
	// Paths not matching CapturePaths should be forwarded but not captured.
	var upstreamCalls atomic.Int32

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamCalls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"ok":true}`))
	}))
	defer upstream.Close()

	var storeCalls atomic.Int32
	muninn := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		storeCalls.Add(1)
		w.WriteHeader(200)
		w.Write([]byte(`{"jsonrpc":"2.0","result":{"id":"ok"},"id":1}`))
	}))
	defer muninn.Close()

	st := store.New(muninn.URL, "", "test", nil)
	p, err := New(Config{
		ListenAddr:   "127.0.0.1:0",
		Upstream:     upstream.URL,
		AgentName:    "test",
		Store:        st,
		CapturePaths: []string{"/v1/messages"},
	})
	if err != nil {
		t.Fatal(err)
	}
	addr, err := p.Start()
	if err != nil {
		t.Fatal(err)
	}

	// Non-captured paths.
	for _, path := range []string{"/models", "/health", "/v1/tokenize"} {
		resp, err := http.Get("http://" + addr + path)
		if err != nil {
			t.Fatalf("GET %s: %v", path, err)
		}
		resp.Body.Close()
		if resp.StatusCode != 200 {
			t.Fatalf("GET %s: expected 200, got %d", path, resp.StatusCode)
		}
	}

	st.Drain()

	if got := upstreamCalls.Load(); got != 3 {
		t.Fatalf("expected 3 upstream calls, got %d", got)
	}
	if got := storeCalls.Load(); got != 0 {
		t.Fatalf("expected 0 store calls for non-captured paths, got %d", got)
	}
}

func TestProxyInjectionTimeout(t *testing.T) {
	// If injection times out, the original (unenriched) request should
	// still be forwarded to upstream successfully.
	var (
		upstreamMu   sync.Mutex
		upstreamBody string
	)

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		upstreamMu.Lock()
		upstreamBody = string(body)
		upstreamMu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"content": []map[string]string{
				{"type": "text", "text": "response"},
			},
		})
	}))
	defer upstream.Close()

	// Slow MuninnDB: recall takes too long, should timeout.
	muninn := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var rpc struct {
			Params struct {
				Name string `json:"name"`
			} `json:"params"`
		}
		json.Unmarshal(body, &rpc)

		if rpc.Params.Name == "muninn_recall" || rpc.Params.Name == "muninn_where_left_off" {
			time.Sleep(500 * time.Millisecond) // exceed timeout
		}
		w.Write(fakeWhereLeftOffEmpty())
	}))
	defer muninn.Close()

	st := store.New(muninn.URL, "", "test", nil)
	injector := inject.New(inject.Config{
		MCPURL:  muninn.URL,
		Vault:   "test",
		Timeout: 50 * time.Millisecond, // very short timeout
	})

	p, err := New(Config{
		ListenAddr:   "127.0.0.1:0",
		Upstream:     upstream.URL,
		AgentName:    "test",
		Store:        st,
		CapturePaths: []string{"/v1/messages"},
		Injector:     injector,
	})
	if err != nil {
		t.Fatal(err)
	}
	addr, err := p.Start()
	if err != nil {
		t.Fatal(err)
	}

	resp, err := http.Post("http://"+addr+"/v1/messages", "application/json",
		strings.NewReader(`{"model":"claude","system":"You are helpful","messages":[{"role":"user","content":"timeout test"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	io.ReadAll(resp.Body)
	resp.Body.Close()

	if resp.StatusCode != 200 {
		t.Fatalf("expected 200 even on injection timeout, got %d", resp.StatusCode)
	}

	// Upstream should receive the ORIGINAL request (no injection).
	upstreamMu.Lock()
	upBody := upstreamBody
	upstreamMu.Unlock()

	if strings.Contains(upBody, "retrieved-context") {
		t.Error("timed-out injection should not produce enriched request")
	}
	if !strings.Contains(upBody, "timeout test") {
		t.Error("upstream should receive the original user message")
	}

	st.Drain()
}

func TestExtractStreamDelta(t *testing.T) {
	tests := []struct {
		name string
		data string
		want string
	}{
		{
			name: "anthropic content_block_delta",
			data: `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Hello world"}}`,
			want: "Hello world",
		},
		{
			name: "openai chat delta",
			data: `{"choices":[{"index":0,"delta":{"content":"Sure thing"}}]}`,
			want: "Sure thing",
		},
		{
			name: "openai responses delta",
			data: `{"type":"response.output_text.delta","delta":"chunk text"}`,
			want: "chunk text",
		},
		{
			name: "gemini candidate",
			data: `{"candidates":[{"content":{"parts":[{"text":"Gemini says hi"}]}}]}`,
			want: "Gemini says hi",
		},
		{
			name: "anthropic message_stop (no text)",
			data: `{"type":"message_stop"}`,
			want: "",
		},
		{
			name: "anthropic message_delta with usage (no text)",
			data: `{"type":"message_delta","usage":{"output_tokens":42}}`,
			want: "",
		},
		{
			name: "invalid json",
			data: `not json`,
			want: "",
		},
		{
			name: "empty input",
			data: ``,
			want: "",
		},
		{
			name: "openai chat finish (no content)",
			data: `{"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`,
			want: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := extractStreamDelta([]byte(tt.data))
			if got != tt.want {
				t.Errorf("extractStreamDelta() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestStreamCaptureAccumulatesText(t *testing.T) {
	// Test that SSE streaming correctly accumulates assistant text across
	// multiple delta events and produces a synthetic response body.
	tests := []struct {
		name   string
		events []string
		want   string // expected substring in captured response
	}{
		{
			name: "anthropic deltas",
			events: []string{
				`data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
				`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Hello "}}`,
				`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"world!"}}`,
				`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":10}}`,
				`data: {"type":"message_stop"}`,
				`data: [DONE]`,
			},
			want: "Hello world!",
		},
		{
			name: "openai chat deltas",
			events: []string{
				`data: {"choices":[{"index":0,"delta":{"role":"assistant","content":""}}]}`,
				`data: {"choices":[{"index":0,"delta":{"content":"I can "}}]}`,
				`data: {"choices":[{"index":0,"delta":{"content":"help."}}]}`,
				`data: {"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`,
				`data: [DONE]`,
			},
			want: "I can help.",
		},
		{
			name: "openai responses deltas",
			events: []string{
				`data: {"type":"response.output_text.delta","delta":"First "}`,
				`data: {"type":"response.output_text.delta","delta":"second."}`,
				`data: {"type":"response.completed","response":{"usage":{"input_tokens":5,"output_tokens":8}}}`,
				`data: [DONE]`,
			},
			want: "First second.",
		},
		{
			name: "gemini deltas",
			events: []string{
				`data: {"candidates":[{"content":{"parts":[{"text":"Gemini "}]}}]}`,
				`data: {"candidates":[{"content":{"parts":[{"text":"response."}]}}]}`,
				`data: {"candidates":[],"usageMetadata":{"promptTokenCount":10,"candidatesTokenCount":5}}`,
				`data: [DONE]`,
			},
			want: "Gemini response.",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Build the SSE stream.
			var stream strings.Builder
			for _, e := range tt.events {
				stream.WriteString(e + "\n\n")
			}

			// Upstream serves this stream.
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				flusher, ok := w.(http.Flusher)
				if !ok {
					t.Fatal("expected flusher")
				}
				w.WriteHeader(200)
				w.Write([]byte(stream.String()))
				flusher.Flush()
			}))
			defer upstream.Close()

			var (
				mu       sync.Mutex
				captured []string
			)
			muninn := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				body, _ := io.ReadAll(r.Body)
				mu.Lock()
				captured = append(captured, string(body))
				mu.Unlock()
				w.WriteHeader(200)
				w.Write([]byte(`{"jsonrpc":"2.0","result":{"id":"test"},"id":1}`))
			}))
			defer muninn.Close()

			st := store.New(muninn.URL, "", "test", nil)

			p, err := New(Config{
				ListenAddr:   "127.0.0.1:0",
				Upstream:     upstream.URL,
				AgentName:    "test-agent",
				Store:        st,
				CapturePaths: []string{"/v1/messages"},
			})
			if err != nil {
				t.Fatal(err)
			}
			addr, err := p.Start()
			if err != nil {
				t.Fatal(err)
			}

			resp, err := http.Post("http://"+addr+"/v1/messages", "application/json",
				strings.NewReader(`{"model":"test","stream":true,"messages":[{"role":"user","content":"test"}]}`))
			if err != nil {
				t.Fatal(err)
			}
			io.ReadAll(resp.Body)
			resp.Body.Close()

			st.Drain()

			mu.Lock()
			defer mu.Unlock()

			if len(captured) == 0 {
				t.Fatal("expected at least one captured exchange")
			}

			combined := strings.Join(captured, " ")
			if !strings.Contains(combined, tt.want) {
				t.Errorf("captured exchange should contain %q, got: %s", tt.want, combined)
			}
		})
	}
}

func TestStreamCapturePreservesUsage(t *testing.T) {
	// Verify that usage metadata from SSE events is preserved in the
	// synthetic response body and correctly extracted into token counts.
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		flusher, _ := w.(http.Flusher)
		w.WriteHeader(200)

		events := []string{
			`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"answer"}}`,
			`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":42}}`,
			`data: {"type":"message_stop"}`,
			`data: [DONE]`,
		}
		for _, e := range events {
			w.Write([]byte(e + "\n\n"))
		}
		flusher.Flush()
	}))
	defer upstream.Close()

	// Test buildSyntheticResp directly by examining the streamCapture output.
	sc := &streamCapture{
		ReadCloser: io.NopCloser(strings.NewReader("")),
		ctx:        &captureCtx{start: time.Now()},
		statusCode: 200,
	}

	// Simulate processChunk with Anthropic deltas.
	chunk := []byte(strings.Join([]string{
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"answer"}}`,
		``,
		`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":42}}`,
		``,
		`data: {"type":"message_stop"}`,
		``,
		`data: [DONE]`,
		``,
	}, "\n"))
	sc.processChunk(chunk)

	respBody := sc.buildRespBody()

	// Verify the synthetic response is valid JSON with content and usage.
	var doc map[string]any
	if err := json.Unmarshal(respBody, &doc); err != nil {
		t.Fatalf("buildRespBody produced invalid JSON: %v", err)
	}

	// Check content.
	content, ok := doc["content"].([]any)
	if !ok || len(content) == 0 {
		t.Fatal("expected content array in synthetic response")
	}
	block, _ := content[0].(map[string]any)
	if block["text"] != "answer" {
		t.Errorf("expected text='answer', got %v", block["text"])
	}

	// Check usage.
	usage, ok := doc["usage"].(map[string]any)
	if !ok {
		t.Fatal("expected usage object in synthetic response")
	}
	if v, ok := usage["output_tokens"].(float64); !ok || int(v) != 42 {
		t.Errorf("expected output_tokens=42, got %v", usage["output_tokens"])
	}
}

func TestStreamCaptureCRLFLineEndings(t *testing.T) {
	// Verify that \r\n line endings (valid per SSE spec) are handled correctly.
	// Before the fix, [DONE] wouldn't be detected and data lines would have
	// trailing \r causing JSON parse issues.
	sc := &streamCapture{
		ReadCloser: io.NopCloser(strings.NewReader("")),
		ctx:        &captureCtx{start: time.Now()},
		statusCode: 200,
	}

	// Simulate SSE events with \r\n line endings.
	chunk := []byte(
		"data: {\"type\":\"content_block_delta\",\"delta\":{\"type\":\"text_delta\",\"text\":\"Hello\"}}\r\n" +
			"\r\n" +
			"data: {\"type\":\"message_stop\"}\r\n" +
			"\r\n" +
			"data: [DONE]\r\n" +
			"\r\n",
	)
	sc.processChunk(chunk)

	if sc.textAccum.String() != "Hello" {
		t.Errorf("expected text accumulation 'Hello', got %q", sc.textAccum.String())
	}
	// [DONE] should NOT be in lastData (it should have been skipped).
	if strings.Contains(sc.lastData, "DONE") {
		t.Errorf("lastData should not contain [DONE], got %q", sc.lastData)
	}
	// lastData should be clean JSON without trailing \r.
	if strings.Contains(sc.lastData, "\r") {
		t.Errorf("lastData should not contain \\r, got %q", sc.lastData)
	}
}
