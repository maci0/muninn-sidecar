package proxy

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

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
	resp, _ := http.Post(base+"/v1internal:streamGenerateContent", "application/json", strings.NewReader(`{"model":"gemini"}`))
	resp.Body.Close()

	// Should be captured: matches "GenerateContent" case-insensitively (API key mode, lowercase)
	resp, _ = http.Post(base+"/v1beta/models/gemini-pro:generateContent", "application/json", strings.NewReader(`{"model":"gemini"}`))
	resp.Body.Close()

	// Should be captured: matches "/v1/messages"
	resp, _ = http.Post(base+"/v1/messages", "application/json", strings.NewReader(`{"model":"claude"}`))
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
	resp, err := http.Post(base+"/v1/messages", "application/json", strings.NewReader(`{"model":"claude","stream":true}`))
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
