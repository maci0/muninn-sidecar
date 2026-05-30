package inject

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/maci0/muninn-sidecar/internal/apiformat"
	"github.com/maci0/muninn-sidecar/internal/stats"
)

// fakeRecallResponse builds a JSON-RPC response with the given memories.
func fakeRecallResponse(memories []memory) []byte {
	items := make([]map[string]any, 0, len(memories))
	for _, m := range memories {
		items = append(items, map[string]any{
			"id":      m.ID,
			"concept": m.Concept,
			"content": m.Content,
			"score":   m.Score,
		})
	}
	inner, _ := json.Marshal(map[string]any{"memories": items})
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

// fakeWhereLeftOffEmpty returns an empty where_left_off response.
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

// fakeEmptyGuideResponse returns a muninn_guide JSON-RPC response with empty
// text, simulating a server that has no guide to provide.
func fakeEmptyGuideResponse() []byte {
	resp, _ := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"result": map[string]any{
			"content": []map[string]any{
				{"type": "text", "text": ""},
			},
		},
		"id": 1,
	})
	return resp
}

// newRecallServer creates a test server that handles both where_left_off
// (returning an empty memories array) and recall (delegating to the given handler).
func newRecallServer(handler http.HandlerFunc) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var rpc struct {
			Params struct {
				Name string `json:"name"`
			} `json:"params"`
		}
		if err := json.Unmarshal(body, &rpc); err != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		if rpc.Params.Name == "muninn_where_left_off" {
			w.Write(fakeWhereLeftOffEmpty())
			return
		}
		if rpc.Params.Name == "muninn_guide" {
			w.Write(fakeEmptyGuideResponse())
			return
		}
		// Re-create request body for downstream handler.
		r.Body = io.NopCloser(strings.NewReader(string(body)))
		handler(w, r)
	}))
}

func newFakeServer(handler http.HandlerFunc) *httptest.Server {
	return httptest.NewServer(handler)
}

func TestEnrichAnthropic(t *testing.T) {
	mems := []memory{
		{ID: "m1", Concept: "user preference", Content: "User prefers Go", Score: 0.85},
	}
	srv := newRecallServer(func(w http.ResponseWriter, r *http.Request) {
		w.Write(fakeRecallResponse(mems))
	})
	defer srv.Close()

	st := &stats.Stats{}
	inj := New(Config{
		MCPURL:  srv.URL,
		Vault:   "default",
		Budget:  2048,
		Timeout: 2 * time.Second,
		Stats:   st,
	})

	body := []byte(`{"model":"claude-3","system":"You are helpful","messages":[{"role":"user","content":"hello"}]}`)
	enriched, tokens, err := inj.Enrich(t.Context(), body)
	if err != nil {
		t.Fatal(err)
	}
	if tokens <= 0 {
		t.Error("expected positive token count")
	}

	var doc map[string]any
	if err := json.Unmarshal(enriched, &doc); err != nil {
		t.Fatal(err)
	}

	// System should be converted to array with original + injected.
	sys := doc["system"].([]any)
	if len(sys) != 2 {
		t.Fatalf("expected 2 system blocks, got %d", len(sys))
	}
	// First block: original system text.
	if sys[0].(map[string]any)["text"] != "You are helpful" {
		t.Error("first system block should be original")
	}
	// Second block: injected context.
	injected := sys[1].(map[string]any)
	text := injected["text"].(string)
	if !strings.Contains(text, apiformat.ContextPrefix) {
		t.Error("injected block should contain context prefix")
	}
	if !strings.Contains(text, "User prefers Go") {
		t.Error("injected block should contain recalled memory")
	}

	// Stats should be updated.
	if st.Injections.Load() != 1 {
		t.Errorf("expected 1 injection, got %d", st.Injections.Load())
	}
	if st.InjectedTokens.Load() <= 0 {
		t.Error("expected positive injected tokens")
	}
}

func TestEnrichOpenAI(t *testing.T) {
	mems := []memory{
		{ID: "m1", Concept: "context", Content: "Important context", Score: 0.9},
	}
	srv := newRecallServer(func(w http.ResponseWriter, r *http.Request) {
		w.Write(fakeRecallResponse(mems))
	})
	defer srv.Close()

	inj := New(Config{MCPURL: srv.URL, Timeout: 2 * time.Second})

	body := []byte(`{"model":"gpt-4","messages":[{"role":"system","content":"You are helpful"},{"role":"user","content":"hello"}]}`)
	enriched, _, err := inj.Enrich(t.Context(), body)
	if err != nil {
		t.Fatal(err)
	}

	var doc map[string]any
	if err := json.Unmarshal(enriched, &doc); err != nil {
		t.Fatal(err)
	}
	msgs := doc["messages"].([]any)

	// Should have 3 messages: original system, injected system, user.
	if len(msgs) != 3 {
		t.Fatalf("expected 3 messages, got %d", len(msgs))
	}
	injectedMsg := msgs[1].(map[string]any)
	if injectedMsg["role"] != "system" {
		t.Error("injected message should be system role")
	}
	if !strings.Contains(injectedMsg["content"].(string), apiformat.ContextPrefix) {
		t.Error("injected message should contain context prefix")
	}
}

func TestEnrichGemini(t *testing.T) {
	mems := []memory{
		{ID: "m1", Concept: "ctx", Content: "Gemini context", Score: 0.8},
	}
	srv := newRecallServer(func(w http.ResponseWriter, r *http.Request) {
		w.Write(fakeRecallResponse(mems))
	})
	defer srv.Close()

	inj := New(Config{MCPURL: srv.URL, Timeout: 2 * time.Second})

	body := []byte(`{"contents":[{"role":"user","parts":[{"text":"hello"}]}]}`)
	enriched, _, err := inj.Enrich(t.Context(), body)
	if err != nil {
		t.Fatal(err)
	}

	var doc map[string]any
	if err := json.Unmarshal(enriched, &doc); err != nil {
		t.Fatal(err)
	}
	si := doc["systemInstruction"].(map[string]any)
	parts := si["parts"].([]any)
	if len(parts) != 1 {
		t.Fatalf("expected 1 part, got %d", len(parts))
	}
	if !strings.Contains(parts[0].(map[string]any)["text"].(string), apiformat.ContextPrefix) {
		t.Error("part should contain context prefix")
	}
}

func TestEnrichTimeout(t *testing.T) {
	srv := newRecallServer(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(500 * time.Millisecond)
		w.Write(fakeRecallResponse([]memory{{ID: "m1", Concept: "late", Content: "too late", Score: 0.9}}))
	})
	defer srv.Close()

	inj := New(Config{MCPURL: srv.URL, Timeout: 50 * time.Millisecond})

	body := []byte(`{"model":"gpt-4","messages":[{"role":"user","content":"hello"}]}`)
	result, tokens, err := inj.Enrich(t.Context(), body)
	if err != nil {
		t.Fatal("expected no error on timeout (graceful fallback)")
	}
	if tokens != 0 {
		t.Error("expected 0 tokens on timeout")
	}
	// Should return original body.
	if string(result) != string(body) {
		t.Error("expected original body on timeout")
	}
}

func TestEnrichEmptyResults(t *testing.T) {
	srv := newRecallServer(func(w http.ResponseWriter, r *http.Request) {
		w.Write(fakeRecallResponse(nil))
	})
	defer srv.Close()

	inj := New(Config{MCPURL: srv.URL, Timeout: 2 * time.Second})

	body := []byte(`{"model":"gpt-4","messages":[{"role":"user","content":"hello"}]}`)
	result, tokens, _ := inj.Enrich(t.Context(), body)
	if tokens != 0 {
		t.Error("expected 0 tokens for empty results")
	}
	if string(result) != string(body) {
		t.Error("expected original body for empty results")
	}
}

func TestEnrichServerError(t *testing.T) {
	srv := newRecallServer(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(500)
		w.Write([]byte("internal error"))
	})
	defer srv.Close()

	inj := New(Config{MCPURL: srv.URL, Timeout: 2 * time.Second})

	body := []byte(`{"model":"gpt-4","messages":[{"role":"user","content":"hello"}]}`)
	result, tokens, _ := inj.Enrich(t.Context(), body)
	if tokens != 0 {
		t.Error("expected 0 tokens on server error")
	}
	if string(result) != string(body) {
		t.Error("expected original body on server error")
	}
}

func TestEnrichUnrecognizedFormat(t *testing.T) {
	inj := New(Config{MCPURL: "http://unused", Timeout: 2 * time.Second})

	body := []byte(`{"prompt":"hello","max_tokens":100}`)
	result, tokens, _ := inj.Enrich(t.Context(), body)
	if tokens != 0 {
		t.Error("expected 0 tokens for unrecognized format")
	}
	if string(result) != string(body) {
		t.Error("expected original body for unrecognized format")
	}
}

func TestTokenBudget(t *testing.T) {
	mems := []memory{
		{ID: "m1", Concept: "high score", Content: strings.Repeat("A", 500), Score: 0.95},
		{ID: "m2", Concept: "medium score", Content: strings.Repeat("B", 500), Score: 0.85},
		{ID: "m3", Concept: "low score", Content: strings.Repeat("C", 500), Score: 0.75},
	}
	srv := newRecallServer(func(w http.ResponseWriter, r *http.Request) {
		w.Write(fakeRecallResponse(mems))
	})
	defer srv.Close()

	// Budget of 200 tokens ≈ 800 chars. First memory alone is ~530 chars,
	// so it fits. Second would push over budget.
	inj := New(Config{MCPURL: srv.URL, Timeout: 2 * time.Second, Budget: 200})

	body := []byte(`{"model":"gpt-4","messages":[{"role":"user","content":"hello"}]}`)
	result, _, _ := inj.Enrich(t.Context(), body)

	resultStr := string(result)
	if !strings.Contains(resultStr, "high score") {
		t.Error("should include highest-score memory")
	}
	if strings.Contains(resultStr, "medium score") {
		t.Error("should not include medium-score memory (over budget)")
	}
	if strings.Contains(resultStr, "low score") {
		t.Error("should not include lowest-score memory (over budget)")
	}
}

func TestEnrichInvalidJSON(t *testing.T) {
	inj := New(Config{MCPURL: "http://unused", Timeout: 2 * time.Second})

	body := []byte(`not json at all`)
	result, tokens, _ := inj.Enrich(t.Context(), body)
	if tokens != 0 {
		t.Error("expected 0 tokens for invalid JSON")
	}
	if string(result) != string(body) {
		t.Error("expected original body for invalid JSON")
	}
}

func TestWhereLeftOffInjection(t *testing.T) {
	srv := newFakeServer(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var rpc struct {
			Params struct {
				Name string `json:"name"`
			} `json:"params"`
		}
		if err := json.Unmarshal(body, &rpc); err != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}

		if rpc.Params.Name == "muninn_where_left_off" {
			inner, _ := json.Marshal(map[string]any{
				"memories": []map[string]any{
					{"concept": "working on auth module", "summary": "Implementing OAuth flow"},
				},
			})
			resp, _ := json.Marshal(map[string]any{
				"jsonrpc": "2.0",
				"result": map[string]any{
					"content": []map[string]any{
						{"type": "text", "text": string(inner)},
					},
				},
				"id": 1,
			})
			w.Write(resp)
			return
		}

		if rpc.Params.Name == "muninn_guide" {
			w.Write(fakeEmptyGuideResponse())
			return
		}

		// recall response
		mems := []memory{
			{ID: "m1", Concept: "auth pattern", Content: "Use JWT tokens", Score: 0.85},
		}
		w.Write(fakeRecallResponse(mems))
	})
	defer srv.Close()

	st := &stats.Stats{}
	inj := New(Config{
		MCPURL:  srv.URL,
		Vault:   "default",
		Budget:  2048,
		Timeout: 2 * time.Second,
		Stats:   st,
	})

	body := []byte(`{"model":"claude-3","system":"You are helpful","messages":[{"role":"user","content":"hello"}]}`)

	// First call should include where_left_off context.
	enriched, tokens, err := inj.Enrich(t.Context(), body)
	if err != nil {
		t.Fatal(err)
	}
	if tokens <= 0 {
		t.Error("expected positive token count")
	}

	resultStr := string(enriched)
	if !strings.Contains(resultStr, "session-context") {
		t.Error("first enrichment should contain session-context from where_left_off")
	}
	if !strings.Contains(resultStr, "auth module") {
		t.Error("first enrichment should contain where_left_off memory concept")
	}

	// Second call should NOT contain session-context (only injected once).
	enriched2, _, _ := inj.Enrich(t.Context(), body)
	if strings.Contains(string(enriched2), "session-context") {
		t.Error("second enrichment should NOT contain session-context")
	}
}

func TestWhereLeftOffEmpty(t *testing.T) {
	srv := newFakeServer(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var rpc struct {
			Params struct {
				Name string `json:"name"`
			} `json:"params"`
		}
		if err := json.Unmarshal(body, &rpc); err != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}

		if rpc.Params.Name == "muninn_where_left_off" {
			w.Write(fakeWhereLeftOffEmpty())
			return
		}

		if rpc.Params.Name == "muninn_guide" {
			w.Write(fakeEmptyGuideResponse())
			return
		}

		w.Write(fakeRecallResponse(nil))
	})
	defer srv.Close()

	inj := New(Config{MCPURL: srv.URL, Timeout: 2 * time.Second})

	body := []byte(`{"model":"gpt-4","messages":[{"role":"user","content":"hello"}]}`)
	result, tokens, _ := inj.Enrich(t.Context(), body)

	if tokens != 0 {
		t.Error("expected 0 tokens when both where_left_off and recall are empty")
	}
	if string(result) != string(body) {
		t.Error("expected original body when nothing to inject")
	}
}

func TestParseWhereLeftOff(t *testing.T) {
	t.Run("with memories", func(t *testing.T) {
		inner, _ := json.Marshal(map[string]any{
			"memories": []map[string]any{
				{"concept": "auth work", "summary": "OAuth implementation"},
				{"concept": "db migration", "summary": "Adding new tables"},
			},
		})
		resp, _ := json.Marshal(map[string]any{
			"jsonrpc": "2.0",
			"result": map[string]any{
				"content": []map[string]any{
					{"type": "text", "text": string(inner)},
				},
			},
			"id": 1,
		})
		result := parseWhereLeftOff(resp)
		if !strings.Contains(result, "session-context") {
			t.Error("should contain session-context tag")
		}
		if !strings.Contains(result, "auth work") {
			t.Error("should contain first concept")
		}
		if !strings.Contains(result, "db migration") {
			t.Error("should contain second concept")
		}
	})

	t.Run("empty memories", func(t *testing.T) {
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
		result := parseWhereLeftOff(resp)
		if result != "" {
			t.Errorf("expected empty string for empty memories, got %q", result)
		}
	})

	t.Run("invalid JSON", func(t *testing.T) {
		result := parseWhereLeftOff([]byte("not json"))
		if result != "" {
			t.Error("expected empty string for invalid JSON")
		}
	})
}

func TestWhereLeftOffWithOpenAI(t *testing.T) {
	srv := newFakeServer(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var rpc struct {
			Params struct {
				Name string `json:"name"`
			} `json:"params"`
		}
		if err := json.Unmarshal(body, &rpc); err != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}

		if rpc.Params.Name == "muninn_where_left_off" {
			inner, _ := json.Marshal(map[string]any{
				"memories": []map[string]any{
					{"concept": "debugging API", "summary": "Working on rate limiter"},
				},
			})
			resp, _ := json.Marshal(map[string]any{
				"jsonrpc": "2.0",
				"result": map[string]any{
					"content": []map[string]any{
						{"type": "text", "text": string(inner)},
					},
				},
				"id": 1,
			})
			w.Write(resp)
			return
		}

		if rpc.Params.Name == "muninn_guide" {
			w.Write(fakeEmptyGuideResponse())
			return
		}

		mems := []memory{
			{ID: "m1", Concept: "API pattern", Content: "Use middleware", Score: 0.85},
		}
		w.Write(fakeRecallResponse(mems))
	})
	defer srv.Close()

	inj := New(Config{MCPURL: srv.URL, Timeout: 2 * time.Second, Budget: 2048})

	body := []byte(`{"model":"gpt-4","messages":[{"role":"system","content":"You are helpful"},{"role":"user","content":"hello"}]}`)
	enriched, tokens, err := inj.Enrich(t.Context(), body)
	if err != nil {
		t.Fatal(err)
	}
	if tokens <= 0 {
		t.Error("expected positive token count")
	}

	var doc map[string]any
	if err := json.Unmarshal(enriched, &doc); err != nil {
		t.Fatal(err)
	}
	msgs := doc["messages"].([]any)

	// Should have system + injected-system + user = 3 messages.
	if len(msgs) != 3 {
		t.Fatalf("expected 3 messages, got %d", len(msgs))
	}

	// The injected message should contain both session-context and recall.
	injMsg := msgs[1].(map[string]any)["content"].(string)
	if !strings.Contains(injMsg, "session-context") {
		t.Error("OpenAI injection should contain session-context")
	}
	if !strings.Contains(injMsg, "Use middleware") {
		t.Error("OpenAI injection should contain recalled memory")
	}
}

func TestWhereLeftOffWithGemini(t *testing.T) {
	srv := newFakeServer(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var rpc struct {
			Params struct {
				Name string `json:"name"`
			} `json:"params"`
		}
		if err := json.Unmarshal(body, &rpc); err != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}

		if rpc.Params.Name == "muninn_where_left_off" {
			inner, _ := json.Marshal(map[string]any{
				"memories": []map[string]any{
					{"concept": "Kubernetes setup", "summary": "Deploying pods"},
				},
			})
			resp, _ := json.Marshal(map[string]any{
				"jsonrpc": "2.0",
				"result": map[string]any{
					"content": []map[string]any{
						{"type": "text", "text": string(inner)},
					},
				},
				"id": 1,
			})
			w.Write(resp)
			return
		}

		if rpc.Params.Name == "muninn_guide" {
			w.Write(fakeEmptyGuideResponse())
			return
		}

		mems := []memory{
			{ID: "m1", Concept: "k8s", Content: "Use deployments", Score: 0.8},
		}
		w.Write(fakeRecallResponse(mems))
	})
	defer srv.Close()

	inj := New(Config{MCPURL: srv.URL, Timeout: 2 * time.Second, Budget: 2048})

	body := []byte(`{"contents":[{"role":"user","parts":[{"text":"hello"}]}]}`)
	enriched, tokens, err := inj.Enrich(t.Context(), body)
	if err != nil {
		t.Fatal(err)
	}
	if tokens <= 0 {
		t.Error("expected positive token count")
	}

	var doc map[string]any
	if err := json.Unmarshal(enriched, &doc); err != nil {
		t.Fatal(err)
	}
	si := doc["systemInstruction"].(map[string]any)
	parts := si["parts"].([]any)
	if len(parts) != 1 {
		t.Fatalf("expected 1 part, got %d", len(parts))
	}
	text := parts[0].(map[string]any)["text"].(string)
	if !strings.Contains(text, "session-context") {
		t.Error("Gemini injection should contain session-context")
	}
	if !strings.Contains(text, "Use deployments") {
		t.Error("Gemini injection should contain recalled memory")
	}
}

func TestConcurrentEnrichment(t *testing.T) {
	mems := []memory{
		{ID: "m1", Concept: "concurrent", Content: "test content", Score: 0.9},
	}

	var whereLeftOffCalls atomic.Int32
	srv := newFakeServer(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var rpc struct {
			Params struct {
				Name string `json:"name"`
			} `json:"params"`
		}
		if err := json.Unmarshal(body, &rpc); err != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}

		switch rpc.Params.Name {
		case "muninn_where_left_off":
			whereLeftOffCalls.Add(1)
			w.Write(fakeWhereLeftOffEmpty())
		case "muninn_guide":
			w.Write(fakeEmptyGuideResponse())
		default:
			w.Write(fakeRecallResponse(mems))
		}
	})
	defer srv.Close()

	inj := New(Config{MCPURL: srv.URL, Timeout: 2 * time.Second, Budget: 2048})

	// sessionOnce guarantees where_left_off is called exactly once regardless
	// of how many goroutines call Enrich concurrently.
	done := make(chan struct{}, 10)
	for i := range 10 {
		go func(idx int) {
			defer func() { done <- struct{}{} }()
			body := []byte(`{"model":"gpt-4","messages":[{"role":"user","content":"concurrent test"}]}`)
			result, _, err := inj.Enrich(t.Context(), body)
			if err != nil {
				t.Errorf("goroutine %d: unexpected error: %v", idx, err)
				return
			}
			if len(result) == 0 {
				t.Errorf("goroutine %d: got empty result", idx)
			}
		}(i)
	}
	for range 10 {
		<-done
	}

	if n := whereLeftOffCalls.Load(); n != 1 {
		t.Errorf("expected where_left_off called exactly once (sessionOnce), got %d", n)
	}
}

func TestMalformedRecallResponse(t *testing.T) {
	t.Run("empty content text", func(t *testing.T) {
		srv := newRecallServer(func(w http.ResponseWriter, r *http.Request) {
			resp, _ := json.Marshal(map[string]any{
				"jsonrpc": "2.0",
				"result": map[string]any{
					"content": []map[string]any{
						{"type": "text", "text": ""},
					},
				},
				"id": 1,
			})
			w.Write(resp)
		})
		defer srv.Close()

		inj := New(Config{MCPURL: srv.URL, Timeout: 2 * time.Second})
		body := []byte(`{"model":"gpt-4","messages":[{"role":"user","content":"hello"}]}`)
		result, tokens, _ := inj.Enrich(t.Context(), body)
		if tokens != 0 {
			t.Error("expected 0 tokens for empty recall content")
		}
		if string(result) != string(body) {
			t.Error("expected original body")
		}
	})

	t.Run("non-text content type", func(t *testing.T) {
		srv := newRecallServer(func(w http.ResponseWriter, r *http.Request) {
			resp, _ := json.Marshal(map[string]any{
				"jsonrpc": "2.0",
				"result": map[string]any{
					"content": []map[string]any{
						{"type": "image", "data": "base64stuff"},
					},
				},
				"id": 1,
			})
			w.Write(resp)
		})
		defer srv.Close()

		inj := New(Config{MCPURL: srv.URL, Timeout: 2 * time.Second})
		body := []byte(`{"model":"gpt-4","messages":[{"role":"user","content":"hello"}]}`)
		result, tokens, _ := inj.Enrich(t.Context(), body)
		if tokens != 0 {
			t.Error("expected 0 tokens for non-text content")
		}
		if string(result) != string(body) {
			t.Error("expected original body")
		}
	})

	t.Run("garbled JSON in text field", func(t *testing.T) {
		srv := newRecallServer(func(w http.ResponseWriter, r *http.Request) {
			resp, _ := json.Marshal(map[string]any{
				"jsonrpc": "2.0",
				"result": map[string]any{
					"content": []map[string]any{
						{"type": "text", "text": "{broken json!!!"},
					},
				},
				"id": 1,
			})
			w.Write(resp)
		})
		defer srv.Close()

		inj := New(Config{MCPURL: srv.URL, Timeout: 2 * time.Second})
		body := []byte(`{"model":"gpt-4","messages":[{"role":"user","content":"hello"}]}`)
		result, tokens, _ := inj.Enrich(t.Context(), body)
		if tokens != 0 {
			t.Error("expected 0 tokens for garbled recall response")
		}
		if string(result) != string(body) {
			t.Error("expected original body for garbled response")
		}
	})
}

func TestSessionContextOnlyInjection(t *testing.T) {
	// Recall returns nothing, but where_left_off has context.
	// The session context alone should be injected.
	srv := newFakeServer(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var rpc struct {
			Params struct {
				Name string `json:"name"`
			} `json:"params"`
		}
		if err := json.Unmarshal(body, &rpc); err != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}

		if rpc.Params.Name == "muninn_where_left_off" {
			inner, _ := json.Marshal(map[string]any{
				"memories": []map[string]any{
					{"concept": "session context only", "summary": "Previous work"},
				},
			})
			resp, _ := json.Marshal(map[string]any{
				"jsonrpc": "2.0",
				"result": map[string]any{
					"content": []map[string]any{
						{"type": "text", "text": string(inner)},
					},
				},
				"id": 1,
			})
			w.Write(resp)
			return
		}

		if rpc.Params.Name == "muninn_guide" {
			w.Write(fakeEmptyGuideResponse())
			return
		}

		// Empty recall.
		w.Write(fakeRecallResponse(nil))
	})
	defer srv.Close()

	inj := New(Config{MCPURL: srv.URL, Timeout: 2 * time.Second, Budget: 2048})

	body := []byte(`{"model":"claude-3","system":"You are helpful","messages":[{"role":"user","content":"hello"}]}`)
	enriched, tokens, err := inj.Enrich(t.Context(), body)
	if err != nil {
		t.Fatal(err)
	}
	if tokens <= 0 {
		t.Fatal("expected positive token count for session-context-only injection")
	}

	resultStr := string(enriched)
	if !strings.Contains(resultStr, "session-context") {
		t.Error("should contain session-context block")
	}
	if !strings.Contains(resultStr, "session context only") {
		t.Error("should contain the where_left_off concept")
	}
}

func TestWhereLeftOffRawText(t *testing.T) {
	// where_left_off returns non-JSON text (raw summary).
	resp, _ := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"result": map[string]any{
			"content": []map[string]any{
				{"type": "text", "text": "Previously working on auth module refactor"},
			},
		},
		"id": 1,
	})

	result := parseWhereLeftOff(resp)
	if !strings.Contains(result, "session-context") {
		t.Error("raw text should produce session-context block")
	}
	if !strings.Contains(result, "auth module refactor") {
		t.Error("should contain the raw text")
	}
}

func TestWhereLeftOffNullText(t *testing.T) {
	// Edge case: text is "null" or "[]".
	for _, text := range []string{"null", "[]", ""} {
		resp, _ := json.Marshal(map[string]any{
			"jsonrpc": "2.0",
			"result": map[string]any{
				"content": []map[string]any{
					{"type": "text", "text": text},
				},
			},
			"id": 1,
		})
		result := parseWhereLeftOff(resp)
		if result != "" {
			t.Errorf("expected empty for text=%q, got %q", text, result)
		}
	}
}

func TestParseRecallResponseFormats(t *testing.T) {
	t.Run("memories field", func(t *testing.T) {
		resp := fakeRecallResponse([]memory{
			{ID: "1", Concept: "test", Content: "content", Score: 0.8},
		})
		mems, err := parseRecallResponse(resp)
		if err != nil {
			t.Fatal(err)
		}
		if len(mems) != 1 {
			t.Fatalf("expected 1 memory, got %d", len(mems))
		}
		if mems[0].ID != "1" {
			t.Error("expected ID 1")
		}
	})

	t.Run("results field", func(t *testing.T) {
		inner, _ := json.Marshal(map[string]any{
			"results": []map[string]any{
				{"id": "r1", "concept": "res", "content": "result content", "score": 0.7},
			},
		})
		resp, _ := json.Marshal(map[string]any{
			"jsonrpc": "2.0",
			"result": map[string]any{
				"content": []map[string]any{
					{"type": "text", "text": string(inner)},
				},
			},
			"id": 1,
		})
		mems, err := parseRecallResponse(resp)
		if err != nil {
			t.Fatal(err)
		}
		if len(mems) != 1 || mems[0].ID != "r1" {
			t.Error("expected result with ID r1")
		}
	})

}

// TestWindowPreservesInjectMetadata locks in a non-obvious correctness property:
// the injection-fitness signals (State/Trust/CreatedAt) survive the session-window
// round-trip via trackedMemory, so the dead/untrusted filter and the
// fresher-wins dedup work on the live recall path (selectForInjection runs on
// mergeMemories' output), not only when selectForInjection is called directly.
// If trackedMemory ever stops embedding the full memory, this fails loudly.
func TestWindowPreservesInjectMetadata(t *testing.T) {
	inj := New(Config{MCPURL: "http://unused"})
	recalled := []memory{
		{ID: "dead", Concept: "x", Content: "retired decision", Score: 0.95, State: "archived"},
		{ID: "live", Concept: "y", Content: "current fact", Score: 0.80, State: "active", Trust: "verified", CreatedAt: "2026-05-01T00:00:00Z"},
	}
	merged := inj.mergeMemories(recalled)

	byID := map[string]memory{}
	for _, m := range merged {
		byID[m.ID] = m
	}
	if byID["dead"].State != "archived" {
		t.Fatalf("window stripped State: got %q", byID["dead"].State)
	}
	if byID["live"].CreatedAt == "" || byID["live"].Trust != "verified" {
		t.Fatalf("window stripped CreatedAt/Trust: %+v", byID["live"])
	}

	// The fitness filter must then apply to the merged window output.
	kept := selectForInjection(merged, 0.5)
	if len(kept) != 1 || kept[0].ID != "live" {
		t.Fatalf("expected only the live memory injected from the window (archived dropped), got %+v", kept)
	}
}

func TestSessionMemoryWindow(t *testing.T) {
	// Simulate 3 turns where different memories are recalled each time.
	// Turn 1: recall A. Turn 2: recall B (A should persist via decay).
	// Turn 3: recall B again (A should still be present but decayed further).
	turn := 0
	srv := newRecallServer(func(w http.ResponseWriter, r *http.Request) {
		turn++
		var mems []memory
		switch turn {
		case 1:
			mems = []memory{{ID: "a", Concept: "auth rules", Content: "Use JWT", Score: 0.90}}
		case 2:
			mems = []memory{{ID: "b", Concept: "test patterns", Content: "Use table tests", Score: 0.85}}
		case 3:
			mems = []memory{{ID: "b", Concept: "test patterns", Content: "Use table tests", Score: 0.85}}
		}
		w.Write(fakeRecallResponse(mems))
	})
	defer srv.Close()

	inj := New(Config{MCPURL: srv.URL, Timeout: 2 * time.Second, Budget: 4096})
	// Each turn uses a DISTINCT user message: a new intent fires a fresh recall
	// and advances the decay window (an identical message would be treated as a
	// continuation and reuse the window without re-recalling — see TestRecallReuse).
	turnBody := func(msg string) []byte {
		return []byte(`{"model":"claude-3","system":"You are helpful","messages":[{"role":"user","content":"` + msg + `"}]}`)
	}

	// Turn 1: Should contain auth rules.
	enriched1, _, _ := inj.Enrich(t.Context(), turnBody("how does auth work"))
	r1 := string(enriched1)
	if !strings.Contains(r1, "auth rules") {
		t.Error("turn 1: should contain auth rules")
	}

	// Turn 2: Should contain BOTH test patterns (new) AND auth rules (decayed but present).
	enriched2, _, _ := inj.Enrich(t.Context(), turnBody("how should I write tests"))
	r2 := string(enriched2)
	if !strings.Contains(r2, "test patterns") {
		t.Error("turn 2: should contain test patterns")
	}
	if !strings.Contains(r2, "auth rules") {
		t.Error("turn 2: auth rules should persist from turn 1 via session window decay")
	}

	// Turn 3: test patterns refreshed and present. auth rules has now decayed
	// two turns (0.90 * 0.7^2 = 0.441), dropping below the 0.6 injection
	// threshold — it remains in the window (above the 0.2 eviction floor, so a
	// re-recall could revive it) but is no longer confident enough to inject.
	enriched3, _, _ := inj.Enrich(t.Context(), turnBody("remind me about testing again"))
	r3 := string(enriched3)
	if !strings.Contains(r3, "test patterns") {
		t.Error("turn 3: should contain test patterns")
	}
	if strings.Contains(r3, "auth rules") {
		t.Error("turn 3: auth rules decayed to ~0.44, below the 0.6 inject threshold, should not be injected")
	}
}

func TestRecallReuseOnUnchangedQuery(t *testing.T) {
	// An identical user message (a tool-use continuation) must NOT fire a second
	// recall; the session window is reused instead. Counts recall calls.
	var recalls atomic.Int32
	srv := newRecallServer(func(w http.ResponseWriter, r *http.Request) {
		recalls.Add(1)
		w.Write(fakeRecallResponse([]memory{
			{ID: "a", Concept: "auth rules", Content: "Use JWT", Score: 0.90},
		}))
	})
	defer srv.Close()

	inj := New(Config{MCPURL: srv.URL, Timeout: 2 * time.Second, Budget: 4096})
	body := []byte(`{"model":"claude-3","system":"x","messages":[{"role":"user","content":"how does auth work"}]}`)

	for i := 0; i < 4; i++ {
		enriched, _, _ := inj.Enrich(t.Context(), body)
		if !strings.Contains(string(enriched), "auth rules") {
			t.Fatalf("call %d: expected auth rules injected (from window)", i)
		}
	}
	if n := recalls.Load(); n != 1 {
		t.Errorf("expected exactly 1 recall for 4 identical-query calls, got %d", n)
	}

	// A changed query must fire a fresh recall.
	body2 := []byte(`{"model":"claude-3","system":"x","messages":[{"role":"user","content":"something completely different now"}]}`)
	inj.Enrich(t.Context(), body2)
	if n := recalls.Load(); n != 2 {
		t.Errorf("expected a new recall on changed query, total got %d", n)
	}
}

func TestAutoCalibrateLowersGate(t *testing.T) {
	// A low-cosine vault: relevant ~0.45, noise ~0.25/0.22. The default 0.6 gate
	// would suppress everything (top 0.45 < 0.6). Auto-calibration should observe
	// the bimodal distribution, drop minScore into the valley, and start injecting.
	srv := newRecallServer(func(w http.ResponseWriter, r *http.Request) {
		w.Write(fakeRecallResponse([]memory{
			{ID: "rel", Concept: "relevant", Content: "the useful answer", Score: 0.45},
			{ID: "n1", Concept: "noise one", Content: "off topic alpha", Score: 0.25},
			{ID: "n2", Concept: "noise two", Content: "off topic beta", Score: 0.22},
		}))
	})
	defer srv.Close()

	inj := New(Config{MCPURL: srv.URL, Timeout: 2 * time.Second, AutoCalibrate: true})
	mk := func(i int) []byte {
		return []byte(`{"model":"claude-3","system":"x","messages":[{"role":"user","content":"distinct question ` + string(rune('a'+i)) + `"}]}`)
	}

	// Early on (threshold 0.6) nothing is injected.
	if out, _, _ := inj.Enrich(t.Context(), mk(0)); strings.Contains(string(out), "useful answer") {
		t.Fatal("turn 0: should suppress at the default 0.6 gate")
	}
	// Drive enough distinct-query recalls to trigger calibration.
	for i := 1; i < 20; i++ {
		inj.Enrich(t.Context(), mk(i))
	}
	if ms := inj.currentMinScore(); ms >= 0.45 {
		t.Fatalf("auto-calibration should drop minScore below 0.45, got %.3f", ms)
	}
	// Now the relevant (0.45) memory clears the calibrated gate.
	out, _, _ := inj.Enrich(t.Context(), mk(20))
	if !strings.Contains(string(out), "useful answer") {
		t.Error("after calibration the relevant memory should be injected")
	}
}

func TestNegativeCache(t *testing.T) {
	// Recall always returns nothing. A repeated identical query must NOT re-recall
	// (negative cache); the turn injects nothing either way.
	var recalls atomic.Int32
	srv := newRecallServer(func(w http.ResponseWriter, r *http.Request) {
		recalls.Add(1)
		w.Write(fakeRecallResponse(nil))
	})
	defer srv.Close()

	inj := New(Config{MCPURL: srv.URL, Timeout: 2 * time.Second})
	body := []byte(`{"model":"claude-3","system":"x","messages":[{"role":"user","content":"obscure thing not in memory"}]}`)

	for i := 0; i < 5; i++ {
		out, tok, _ := inj.Enrich(t.Context(), body)
		if tok != 0 || strings.Contains(string(out), apiformat.ContextPrefix) {
			t.Fatalf("call %d: expected no injection", i)
		}
	}
	if n := recalls.Load(); n != 1 {
		t.Errorf("negative cache: expected 1 recall for 5 identical empty-result calls, got %d", n)
	}
}

func TestSemanticReuseTrigger(t *testing.T) {
	// With QuerySimReuse < 1, a near-identical (high-Jaccard) query reuses the
	// window; a dissimilar query fires a fresh recall.
	var recalls atomic.Int32
	srv := newRecallServer(func(w http.ResponseWriter, r *http.Request) {
		recalls.Add(1)
		w.Write(fakeRecallResponse([]memory{{ID: "a", Concept: "auth", Content: "Use JWT", Score: 0.9}}))
	})
	defer srv.Close()

	inj := New(Config{MCPURL: srv.URL, Timeout: 2 * time.Second, QuerySimReuse: 0.6})
	mk := func(msg string) []byte {
		return []byte(`{"model":"claude-3","system":"x","messages":[{"role":"user","content":"` + msg + `"}]}`)
	}

	inj.Enrich(t.Context(), mk("how does the auth token refresh work here"))        // recall 1
	inj.Enrich(t.Context(), mk("how does the auth token refresh work here please")) // near-dup -> reuse
	if n := recalls.Load(); n != 1 {
		t.Errorf("expected near-identical query to reuse (1 recall), got %d", n)
	}
	inj.Enrich(t.Context(), mk("completely unrelated kubernetes helm deployment question")) // dissimilar -> recall 2
	if n := recalls.Load(); n != 2 {
		t.Errorf("expected dissimilar query to recall (2 total), got %d", n)
	}
}

func TestSessionMemoryEviction(t *testing.T) {
	// A memory with score 0.72 (above the 0.6 inject threshold) at decayFactor=0.7
	// drops below decayFloor=0.2 after a few turns: 0.72 * 0.7^4 = 0.17.
	turn := 0
	srv := newRecallServer(func(w http.ResponseWriter, r *http.Request) {
		turn++
		var mems []memory
		if turn == 1 {
			mems = []memory{{ID: "a", Concept: "old context", Content: "stale info", Score: 0.72}}
		}
		// Turns 2+ return nothing — memory A should decay and eventually be evicted.
		w.Write(fakeRecallResponse(mems))
	})
	defer srv.Close()

	inj := New(Config{MCPURL: srv.URL, Timeout: 2 * time.Second, Budget: 4096})
	// Distinct message per turn so each is a new intent (fresh recall + decay step).
	turnBody := func(i int) []byte {
		return []byte(`{"model":"claude-3","system":"You are helpful","messages":[{"role":"user","content":"distinct question number ` + string(rune('0'+i)) + `"}]}`)
	}

	// Turn 1: Contains old context.
	enriched1, _, _ := inj.Enrich(t.Context(), turnBody(1))
	if !strings.Contains(string(enriched1), "old context") {
		t.Error("turn 1: should contain old context")
	}

	// Turns 2-5: Keep calling (distinct queries) to decay.
	var last string
	for i := 2; i <= 5; i++ {
		enriched, _, _ := inj.Enrich(t.Context(), turnBody(i))
		last = string(enriched)
	}

	// By turn 5, the memory should be evicted (0.72 * 0.7^4 = 0.17 < 0.2).
	if strings.Contains(last, "old context") {
		t.Error("turn 5: old context should have been evicted after decay")
	}
}

func TestSelectForInjection(t *testing.T) {
	t.Run("empty input", func(t *testing.T) {
		if got := selectForInjection(nil, 0.5); len(got) != 0 {
			t.Errorf("expected empty, got %d", len(got))
		}
	})

	t.Run("suppressed when nothing clears the threshold", func(t *testing.T) {
		in := []memory{
			{ID: "a", Concept: "x", Content: "weak", Score: 0.48},
			{ID: "b", Concept: "y", Content: "weaker", Score: 0.42},
		}
		if got := selectForInjection(in, 0.5); len(got) != 0 {
			t.Errorf("expected suppression (empty) when all below threshold, got %d", len(got))
		}
	})

	t.Run("weak tail dropped, confident ones kept", func(t *testing.T) {
		in := []memory{
			{ID: "a", Concept: "strong", Content: "highly relevant", Score: 0.90},
			{ID: "b", Concept: "mid", Content: "still useful here", Score: 0.55},
			{ID: "c", Concept: "weak", Content: "barely related noise", Score: 0.30},
		}
		got := selectForInjection(in, 0.5)
		if len(got) != 2 {
			t.Fatalf("expected 2 kept (>= 0.5), got %d", len(got))
		}
		if got[0].ID != "a" || got[1].ID != "b" {
			t.Errorf("expected a,b kept; got %v,%v", got[0].ID, got[1].ID)
		}
	})

	t.Run("all kept when scores are uniformly high", func(t *testing.T) {
		in := []memory{
			{ID: "a", Concept: "p", Content: "alpha content", Score: 0.95},
			{ID: "b", Concept: "q", Content: "beta content", Score: 0.85},
			{ID: "c", Concept: "r", Content: "gamma content", Score: 0.75},
		}
		if got := selectForInjection(in, 0.5); len(got) != 3 {
			t.Errorf("expected 3 kept, got %d", len(got))
		}
	})

	t.Run("threshold zero keeps everything (disabled)", func(t *testing.T) {
		in := []memory{
			{ID: "a", Concept: "p", Content: "alpha content", Score: 0.45},
			{ID: "b", Concept: "q", Content: "beta content", Score: 0.10},
		}
		if got := selectForInjection(in, 0); len(got) != 2 {
			t.Errorf("threshold 0 should keep all, got %d", len(got))
		}
	})

	t.Run("duplicate concept dropped", func(t *testing.T) {
		in := []memory{
			{ID: "a", Concept: "Auth Pattern", Content: "use jwt tokens for sessions", Score: 0.90},
			{ID: "b", Concept: "auth pattern", Content: "completely different wording about login", Score: 0.80},
		}
		got := selectForInjection(in, 0.5)
		if len(got) != 1 || got[0].ID != "a" {
			t.Errorf("expected only higher-scored a kept on concept dup, got %v", got)
		}
	})

	t.Run("near-duplicate content dropped", func(t *testing.T) {
		in := []memory{
			{ID: "a", Concept: "one", Content: "the user prefers go modules and table driven tests", Score: 0.90},
			{ID: "b", Concept: "two", Content: "the user prefers go modules and table driven tests today", Score: 0.85},
			{ID: "c", Concept: "three", Content: "deployment uses kubernetes pods and helm charts", Score: 0.80},
		}
		got := selectForInjection(in, 0.5)
		if len(got) != 2 {
			t.Fatalf("expected 2 kept (b near-dup of a), got %d", len(got))
		}
		if got[0].ID != "a" || got[1].ID != "c" {
			t.Errorf("expected a and c kept, got %v,%v", got[0].ID, got[1].ID)
		}
	})

	t.Run("fresher same-concept memory supersedes the stale higher-cosine one", func(t *testing.T) {
		in := []memory{
			// Same concept; the stale one scores marginally higher, but the fresher
			// (later created_at) one is the current fact and must win.
			{ID: "stale", Concept: "auth-datastore", Content: "auth uses MySQL", Score: 0.88, CreatedAt: "2026-01-01T00:00:00Z"},
			{ID: "fresh", Concept: "auth-datastore", Content: "auth migrated to Postgres", Score: 0.82, CreatedAt: "2026-05-01T00:00:00Z"},
		}
		got := selectForInjection(in, 0.5)
		if len(got) != 1 {
			t.Fatalf("expected 1 kept (same concept), got %d", len(got))
		}
		if got[0].ID != "fresh" {
			t.Errorf("expected the fresher 'fresh' memory to win, got %q (%s)", got[0].ID, got[0].Content)
		}
	})

	t.Run("higher-cosine kept when it is also the freshest", func(t *testing.T) {
		in := []memory{
			{ID: "new", Concept: "k", Content: "current fact", Score: 0.90, CreatedAt: "2026-05-01T00:00:00Z"},
			{ID: "old", Concept: "k", Content: "old fact", Score: 0.80, CreatedAt: "2026-01-01T00:00:00Z"},
		}
		got := selectForInjection(in, 0.5)
		if len(got) != 1 || got[0].ID != "new" {
			t.Errorf("expected fresher+higher 'new' kept, got %v", got)
		}
	})

	t.Run("missing timestamps fall back to higher cosine", func(t *testing.T) {
		in := []memory{
			{ID: "a", Concept: "k", Content: "first", Score: 0.90},
			{ID: "b", Concept: "k", Content: "second", Score: 0.80},
		}
		got := selectForInjection(in, 0.5)
		if len(got) != 1 || got[0].ID != "a" {
			t.Errorf("with no timestamps, higher-cosine a should win, got %v", got)
		}
	})

	t.Run("dead and untrusted memories excluded", func(t *testing.T) {
		in := []memory{
			{ID: "arch", Concept: "a", Content: "retired decision", Score: 0.95, State: "archived"},
			{ID: "canc", Concept: "b", Content: "abandoned plan", Score: 0.93, State: "cancelled"},
			{ID: "untr", Concept: "c", Content: "unreliable claim", Score: 0.92, Trust: "untrusted"},
			{ID: "ok", Concept: "d", Content: "current verified fact", Score: 0.70, State: "active", Trust: "verified"},
			{ID: "done", Concept: "e", Content: "completed task decision", Score: 0.65, State: "completed"},
		}
		got := selectForInjection(in, 0.5)
		if len(got) != 2 {
			t.Fatalf("expected 2 kept (active + completed), got %d: %+v", len(got), got)
		}
		ids := map[string]bool{got[0].ID: true, got[1].ID: true}
		if !ids["ok"] || !ids["done"] {
			t.Errorf("expected 'ok' and 'done' kept (archived/cancelled/untrusted dropped), got %v", ids)
		}
	})
}

func TestInjectable(t *testing.T) {
	cases := []struct {
		m    memory
		want bool
	}{
		{memory{}, true},                              // empty fields → keep
		{memory{State: "active", Trust: "verified"}, true},
		{memory{State: "completed"}, true},            // finished but still relevant
		{memory{State: "planning"}, true},
		{memory{State: "archived"}, false},            // retired
		{memory{State: "cancelled"}, false},           // abandoned
		{memory{Trust: "untrusted"}, false},           // flagged unreliable
		{memory{Trust: "external"}, true},             // external is still usable
		{memory{State: "archived", Trust: "verified"}, false}, // dead state wins
	}
	for _, c := range cases {
		if got := injectable(c.m); got != c.want {
			t.Errorf("injectable(state=%q trust=%q) = %v, want %v", c.m.State, c.m.Trust, got, c.want)
		}
	}
}

func TestJaccard(t *testing.T) {
	if got := jaccard(nil, []string{"a"}); got != 0 {
		t.Errorf("empty set should be 0, got %v", got)
	}
	a := []string{"x", "y", "z"}
	if got := jaccard(a, a); got != 1 {
		t.Errorf("identical sets should be 1, got %v", got)
	}
	// {a,b} vs {b,c}: inter=1, union=3 -> 0.333
	if got := jaccard([]string{"a", "b"}, []string{"b", "c"}); got < 0.33 || got > 0.34 {
		t.Errorf("expected ~0.333, got %v", got)
	}
}

func TestEnrichDropsWeakTail(t *testing.T) {
	// Strong match present: weak memory below adaptive cutoff must not be injected.
	mems := []memory{
		{ID: "strong", Concept: "primary topic", Content: "highly relevant answer", Score: 0.90},
		{ID: "weak", Concept: "off topic", Content: "barely related tangent", Score: 0.30},
	}
	srv := newRecallServer(func(w http.ResponseWriter, r *http.Request) {
		w.Write(fakeRecallResponse(mems))
	})
	defer srv.Close()

	inj := New(Config{MCPURL: srv.URL, Timeout: 2 * time.Second, Budget: 4096})
	body := []byte(`{"model":"gpt-4","messages":[{"role":"user","content":"hello"}]}`)
	enriched, _, _ := inj.Enrich(t.Context(), body)
	s := string(enriched)
	if !strings.Contains(s, "primary topic") {
		t.Error("strong memory should be injected")
	}
	if strings.Contains(s, "off topic") {
		t.Error("weak tail memory below adaptive cutoff should NOT be injected")
	}
}

func TestParseGuide(t *testing.T) {
	t.Run("non-empty guide text is wrapped in global-guide tags", func(t *testing.T) {
		resp, _ := json.Marshal(map[string]any{
			"jsonrpc": "2.0",
			"result": map[string]any{
				"content": []map[string]any{
					{"type": "text", "text": "Always respond in the user's language."},
				},
			},
			"id": 1,
		})
		result := parseGuide(resp)
		if !strings.Contains(result, "global-guide") {
			t.Error("guide should be wrapped in global-guide tags")
		}
		if !strings.Contains(result, "Always respond in the user's language.") {
			t.Error("guide should contain the guide text")
		}
	})

	t.Run("empty text returns empty string", func(t *testing.T) {
		resp, _ := json.Marshal(map[string]any{
			"jsonrpc": "2.0",
			"result": map[string]any{
				"content": []map[string]any{
					{"type": "text", "text": ""},
				},
			},
			"id": 1,
		})
		if got := parseGuide(resp); got != "" {
			t.Errorf("expected empty string for empty text, got %q", got)
		}
	})

	t.Run("whitespace-only text returns empty string", func(t *testing.T) {
		resp, _ := json.Marshal(map[string]any{
			"jsonrpc": "2.0",
			"result": map[string]any{
				"content": []map[string]any{
					{"type": "text", "text": "   \n  "},
				},
			},
			"id": 1,
		})
		if got := parseGuide(resp); got != "" {
			t.Errorf("expected empty string for whitespace-only text, got %q", got)
		}
	})

	t.Run("invalid JSON returns empty string", func(t *testing.T) {
		if got := parseGuide([]byte("not json")); got != "" {
			t.Errorf("expected empty string for invalid JSON, got %q", got)
		}
	})
}
