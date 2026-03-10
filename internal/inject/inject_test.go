package inject

import (
	"encoding/json"
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

func newFakeServer(handler http.HandlerFunc) *httptest.Server {
	return httptest.NewServer(handler)
}

func TestEnrichAnthropic(t *testing.T) {
	mems := []memory{
		{ID: "m1", Concept: "user preference", Content: "User prefers Go", Score: 0.85},
	}
	srv := newFakeServer(func(w http.ResponseWriter, r *http.Request) {
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
	srv := newFakeServer(func(w http.ResponseWriter, r *http.Request) {
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
	srv := newFakeServer(func(w http.ResponseWriter, r *http.Request) {
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
	srv := newFakeServer(func(w http.ResponseWriter, r *http.Request) {
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
	srv := newFakeServer(func(w http.ResponseWriter, r *http.Request) {
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
	srv := newFakeServer(func(w http.ResponseWriter, r *http.Request) {
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

func TestTurnTracking(t *testing.T) {
	callCount := 0
	srv := newFakeServer(func(w http.ResponseWriter, r *http.Request) {
		callCount++
		mems := []memory{
			{ID: "m1", Concept: "ctx", Content: "repeated memory", Score: 0.9},
		}
		w.Write(fakeRecallResponse(mems))
	})
	defer srv.Close()

	st := &stats.Stats{}
	inj := New(Config{MCPURL: srv.URL, Timeout: 2 * time.Second, Stats: st})

	body := []byte(`{"model":"gpt-4","messages":[{"role":"user","content":"hello"}]}`)

	// Turn 1: should inject.
	result1, tokens1, _ := inj.Enrich(t.Context(), body)
	if tokens1 == 0 {
		t.Fatal("turn 1: expected injection")
	}
	if !strings.Contains(string(result1), "repeated memory") {
		t.Error("turn 1: should contain memory")
	}

	// Turn 2: same memory should be suppressed (2-turn cooldown).
	result2, tokens2, _ := inj.Enrich(t.Context(), body)
	if tokens2 != 0 {
		t.Error("turn 2: expected suppression (2-turn cooldown)")
	}
	if string(result2) != string(body) {
		t.Error("turn 2: should return original body")
	}

	// Turn 3: still within cooldown (turn 1 wrote to slot, turn 2 had no injection
	// so no new slot was written — but the injector only records on actual injection).
	// Actually, since turn 2 had no injection, recordInjected was not called.
	// So turnIdx is still at 1 (from turn 1's injection). When turn 3 calls
	// filterRecent, it checks slots turnIdx (1) and turnIdx-1 (0), which still has m1.
	// So turn 3 should also be suppressed.
	result3, tokens3, _ := inj.Enrich(t.Context(), body)
	if tokens3 != 0 {
		t.Error("turn 3: expected suppression (still in cooldown window)")
	}
	if string(result3) != string(body) {
		t.Error("turn 3: should return original body")
	}

	// Manually advance turns by injecting different memories to push m1 out.
	inj.recordInjected([]string{"other1"})
	inj.recordInjected([]string{"other2"})

	// Now m1 should be allowed back.
	result4, tokens4, _ := inj.Enrich(t.Context(), body)
	if tokens4 == 0 {
		t.Error("after cooldown: expected injection")
	}
	if !strings.Contains(string(result4), "repeated memory") {
		t.Error("after cooldown: should contain memory again")
	}
}

func TestTokenBudget(t *testing.T) {
	mems := []memory{
		{ID: "m1", Concept: "high score", Content: strings.Repeat("A", 500), Score: 0.95},
		{ID: "m2", Concept: "medium score", Content: strings.Repeat("B", 500), Score: 0.85},
		{ID: "m3", Concept: "low score", Content: strings.Repeat("C", 500), Score: 0.75},
	}
	srv := newFakeServer(func(w http.ResponseWriter, r *http.Request) {
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
