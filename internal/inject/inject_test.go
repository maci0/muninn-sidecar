package inject

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

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

// newRecallServer creates a test server that handles both where_left_off
// (returning empty) and recall (returning the given handler).
func newRecallServer(handler http.HandlerFunc) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var rpc struct {
			Params struct {
				Name string `json:"name"`
			} `json:"params"`
		}
		json.Unmarshal(body, &rpc)
		if rpc.Params.Name == "muninn_where_left_off" {
			w.Write(fakeWhereLeftOffEmpty())
			return
		}
		if rpc.Params.Name == "muninn_guide" {
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
	json.Unmarshal(enriched, &doc)

	// System should be converted to array with original + injected.
	sys := doc["system"].([]any)
	if len(sys) != 2 {
		t.Fatalf("expected 2 system blocks, got %d", len(sys))
	}
	// First block: original system text.
	if sys[0].(map[string]any)["text"] != "You are helpful" {
		t.Error("first system block should be original")
	}
	// Second block: injected context with cache_control.
	injected := sys[1].(map[string]any)
	text := injected["text"].(string)
	if !strings.Contains(text, contextPrefix) {
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
	json.Unmarshal(enriched, &doc)
	msgs := doc["messages"].([]any)

	// Should have 3 messages: original system, injected system, user.
	if len(msgs) != 3 {
		t.Fatalf("expected 3 messages, got %d", len(msgs))
	}
	injectedMsg := msgs[1].(map[string]any)
	if injectedMsg["role"] != "system" {
		t.Error("injected message should be system role")
	}
	if !strings.Contains(injectedMsg["content"].(string), contextPrefix) {
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
	json.Unmarshal(enriched, &doc)
	si := doc["systemInstruction"].(map[string]any)
	parts := si["parts"].([]any)
	if len(parts) != 1 {
		t.Fatalf("expected 1 part, got %d", len(parts))
	}
	if !strings.Contains(parts[0].(map[string]any)["text"].(string), contextPrefix) {
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
		json.Unmarshal(body, &rpc)

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
		json.Unmarshal(body, &rpc)

		if rpc.Params.Name == "muninn_where_left_off" {
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
			w.Write(resp)
			return
		}
		
		if rpc.Params.Name == "muninn_guide" {
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
		json.Unmarshal(body, &rpc)

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
	json.Unmarshal(enriched, &doc)
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
		json.Unmarshal(body, &rpc)

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
	json.Unmarshal(enriched, &doc)
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
	srv := newRecallServer(func(w http.ResponseWriter, r *http.Request) {
		w.Write(fakeRecallResponse(mems))
	})
	defer srv.Close()

	inj := New(Config{MCPURL: srv.URL, Timeout: 2 * time.Second, Budget: 2048})

	// First call triggers where_left_off via sessionOnce; subsequent calls should not.
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
		json.Unmarshal(body, &rpc)

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

	t.Run("JSON-RPC error", func(t *testing.T) {
		resp, _ := json.Marshal(map[string]any{
			"jsonrpc": "2.0",
			"error":   map[string]any{"message": "vault not found"},
			"id":      1,
		})
		_, err := parseRecallResponse(resp)
		if err == nil {
			t.Error("expected error for JSON-RPC error response")
		}
	})
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
	body := []byte(`{"model":"claude-3","system":"You are helpful","messages":[{"role":"user","content":"hello"}]}`)

	// Turn 1: Should contain auth rules.
	enriched1, _, _ := inj.Enrich(t.Context(), body)
	r1 := string(enriched1)
	if !strings.Contains(r1, "auth rules") {
		t.Error("turn 1: should contain auth rules")
	}

	// Turn 2: Should contain BOTH test patterns (new) AND auth rules (decayed but present).
	enriched2, _, _ := inj.Enrich(t.Context(), body)
	r2 := string(enriched2)
	if !strings.Contains(r2, "test patterns") {
		t.Error("turn 2: should contain test patterns")
	}
	if !strings.Contains(r2, "auth rules") {
		t.Error("turn 2: auth rules should persist from turn 1 via session window decay")
	}

	// Turn 3: Both should still be present (B refreshed, A decayed 2 turns).
	enriched3, _, _ := inj.Enrich(t.Context(), body)
	r3 := string(enriched3)
	if !strings.Contains(r3, "test patterns") {
		t.Error("turn 3: should contain test patterns")
	}
	if !strings.Contains(r3, "auth rules") {
		t.Error("turn 3: auth rules should still persist (2 turns decay, score ~0.44)")
	}
}

func TestSessionMemoryEviction(t *testing.T) {
	// A memory with score 0.5 at decayFactor=0.7 drops below decayFloor=0.2
	// after ~3 turns: 0.5 * 0.7^3 = 0.17.
	turn := 0
	srv := newRecallServer(func(w http.ResponseWriter, r *http.Request) {
		turn++
		var mems []memory
		if turn == 1 {
			mems = []memory{{ID: "a", Concept: "old context", Content: "stale info", Score: 0.50}}
		}
		// Turns 2+ return nothing — memory A should decay and eventually be evicted.
		w.Write(fakeRecallResponse(mems))
	})
	defer srv.Close()

	inj := New(Config{MCPURL: srv.URL, Timeout: 2 * time.Second, Budget: 4096})
	body := []byte(`{"model":"claude-3","system":"You are helpful","messages":[{"role":"user","content":"hello"}]}`)

	// Turn 1: Contains old context.
	enriched1, _, _ := inj.Enrich(t.Context(), body)
	if !strings.Contains(string(enriched1), "old context") {
		t.Error("turn 1: should contain old context")
	}

	// Turns 2-4: Keep calling to decay.
	var last string
	for i := 2; i <= 5; i++ {
		enriched, _, _ := inj.Enrich(t.Context(), body)
		last = string(enriched)
	}

	// By turn 5, the memory should be evicted (0.5 * 0.7^4 = 0.12 < 0.2).
	if strings.Contains(last, "old context") {
		t.Error("turn 5: old context should have been evicted after decay")
	}
}
