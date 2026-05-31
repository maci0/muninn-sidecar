package inject

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/maci0/muninn-sidecar/internal/apiformat"
)

func TestFormatContextBlockRedactsSecrets(t *testing.T) {
	// Defense in depth: a recalled memory carrying a secret (stored by another
	// client or before write-side redaction) must not be injected verbatim.
	secret := "sk-" + strings.Repeat("a", 30)
	mems := []memory{
		{ID: "1", Concept: "deploy notes", Content: "the key is " + secret, Score: 0.9},
	}
	block, _, _ := formatContextBlock(mems, 2048)
	if strings.Contains(block, secret) {
		t.Errorf("secret leaked into injected block: %s", block)
	}
	if !strings.Contains(block, "[REDACTED]") {
		t.Errorf("expected redaction marker in injected block: %s", block)
	}
}

func TestFormatContextBlock(t *testing.T) {
	t.Run("multiple memories within budget", func(t *testing.T) {
		mems := []memory{
			{ID: "1", Concept: "test1", Content: "content one", Score: 0.9},
			{ID: "2", Concept: "test2", Content: "content two", Score: 0.8},
		}
		block, tokens, dropped := formatContextBlock(mems, 2048)
		if !strings.Contains(block, apiformat.ContextPrefix) {
			t.Error("block should contain context prefix")
		}
		if !strings.Contains(block, apiformat.ContextSuffix) {
			t.Error("block should contain context suffix")
		}
		if !strings.Contains(block, "test1") || !strings.Contains(block, "test2") {
			t.Error("block should contain both memories")
		}
		if tokens <= 0 {
			t.Error("tokens should be positive")
		}
		if dropped != 0 {
			t.Errorf("nothing should be dropped within budget, got %d", dropped)
		}
	})

	t.Run("budget limits memories", func(t *testing.T) {
		mems := []memory{
			{ID: "1", Concept: "first", Content: strings.Repeat("x", 1000), Score: 0.9},
			{ID: "2", Concept: "second", Content: strings.Repeat("y", 1000), Score: 0.8},
		}
		// Very tight budget that can fit first but not second.
		block, _, dropped := formatContextBlock(mems, 300)
		if !strings.Contains(block, "first") {
			t.Error("should include first memory")
		}
		if strings.Contains(block, "second") {
			t.Error("should not include second memory (over budget)")
		}
		if dropped != 1 {
			t.Errorf("budget should report 1 dropped memory, got %d", dropped)
		}
	})

	t.Run("empty memories", func(t *testing.T) {
		block, tokens, dropped := formatContextBlock(nil, 2048)
		if block != "" {
			t.Error("empty memories should return empty block")
		}
		if tokens != 0 {
			t.Error("empty memories should return 0 tokens")
		}
		if dropped != 0 {
			t.Errorf("empty memories should drop nothing, got %d", dropped)
		}
	})
}

func TestInjectContextAnthropic(t *testing.T) {
	t.Run("no system field", func(t *testing.T) {
		var doc map[string]any
		if err := json.Unmarshal([]byte(`{"model":"claude-3","messages":[]}`), &doc); err != nil {
			t.Fatalf("invalid test JSON: %v", err)
		}

		result, err := InjectContext(doc, apiformat.Anthropic, "test context")
		if err != nil {
			t.Fatal(err)
		}

		var out map[string]any
		if err := json.Unmarshal(result, &out); err != nil {
			t.Fatalf("failed to unmarshal result: %v", err)
		}
		sys := out["system"].([]any)
		if len(sys) != 1 {
			t.Fatalf("expected 1 system block, got %d", len(sys))
		}
		block := sys[0].(map[string]any)
		if block["text"] != "test context" {
			t.Error("expected injected text")
		}
	})

	t.Run("string system", func(t *testing.T) {
		var doc map[string]any
		if err := json.Unmarshal([]byte(`{"model":"claude-3","system":"You are helpful","messages":[]}`), &doc); err != nil {
			t.Fatalf("invalid test JSON: %v", err)
		}

		result, err := InjectContext(doc, apiformat.Anthropic, "test context")
		if err != nil {
			t.Fatal(err)
		}

		var out map[string]any
		if err := json.Unmarshal(result, &out); err != nil {
			t.Fatalf("failed to unmarshal result: %v", err)
		}
		sys := out["system"].([]any)
		if len(sys) != 2 {
			t.Fatalf("expected 2 system blocks, got %d", len(sys))
		}
		if sys[0].(map[string]any)["text"] != "You are helpful" {
			t.Error("first block should be original system text")
		}
		if sys[1].(map[string]any)["text"] != "test context" {
			t.Error("second block should be injected context")
		}
	})

	t.Run("array system", func(t *testing.T) {
		var doc map[string]any
		if err := json.Unmarshal([]byte(`{"model":"claude-3","system":[{"type":"text","text":"existing"}],"messages":[]}`), &doc); err != nil {
			t.Fatalf("invalid test JSON: %v", err)
		}

		result, err := InjectContext(doc, apiformat.Anthropic, "test context")
		if err != nil {
			t.Fatal(err)
		}

		var out map[string]any
		if err := json.Unmarshal(result, &out); err != nil {
			t.Fatalf("failed to unmarshal result: %v", err)
		}
		sys := out["system"].([]any)
		if len(sys) != 2 {
			t.Fatalf("expected 2 system blocks, got %d", len(sys))
		}
	})
}

func TestInjectContextOpenAI(t *testing.T) {
	t.Run("insert after system messages", func(t *testing.T) {
		var doc map[string]any
		if err := json.Unmarshal([]byte(`{"model":"gpt-4","messages":[
			{"role":"system","content":"You are helpful"},
			{"role":"user","content":"hello"}
		]}`), &doc); err != nil {
			t.Fatalf("invalid test JSON: %v", err)
		}

		result, err := InjectContext(doc, apiformat.OpenAI, "test context")
		if err != nil {
			t.Fatal(err)
		}

		var out map[string]any
		if err := json.Unmarshal(result, &out); err != nil {
			t.Fatalf("failed to unmarshal result: %v", err)
		}
		msgs := out["messages"].([]any)
		if len(msgs) != 3 {
			t.Fatalf("expected 3 messages, got %d", len(msgs))
		}
		if msgs[0].(map[string]any)["content"] != "You are helpful" {
			t.Error("first should be original system")
		}
		if msgs[1].(map[string]any)["content"] != "test context" {
			t.Error("second should be injected context")
		}
		if msgs[2].(map[string]any)["content"] != "hello" {
			t.Error("third should be user message")
		}
	})

	t.Run("no system messages", func(t *testing.T) {
		var doc map[string]any
		if err := json.Unmarshal([]byte(`{"model":"gpt-4","messages":[
			{"role":"user","content":"hello"}
		]}`), &doc); err != nil {
			t.Fatalf("invalid test JSON: %v", err)
		}

		result, err := InjectContext(doc, apiformat.OpenAI, "test context")
		if err != nil {
			t.Fatal(err)
		}

		var out map[string]any
		if err := json.Unmarshal(result, &out); err != nil {
			t.Fatalf("failed to unmarshal result: %v", err)
		}
		msgs := out["messages"].([]any)
		if len(msgs) != 2 {
			t.Fatalf("expected 2 messages, got %d", len(msgs))
		}
		if msgs[0].(map[string]any)["content"] != "test context" {
			t.Error("first should be injected context")
		}
	})
}

func TestInjectContextGemini(t *testing.T) {
	t.Run("no systemInstruction", func(t *testing.T) {
		var doc map[string]any
		if err := json.Unmarshal([]byte(`{"contents":[{"role":"user","parts":[{"text":"hello"}]}]}`), &doc); err != nil {
			t.Fatalf("invalid test JSON: %v", err)
		}

		result, err := InjectContext(doc, apiformat.Gemini, "test context")
		if err != nil {
			t.Fatal(err)
		}

		var out map[string]any
		if err := json.Unmarshal(result, &out); err != nil {
			t.Fatalf("failed to unmarshal result: %v", err)
		}
		si := out["systemInstruction"].(map[string]any)
		parts := si["parts"].([]any)
		if len(parts) != 1 {
			t.Fatalf("expected 1 part, got %d", len(parts))
		}
		if parts[0].(map[string]any)["text"] != "test context" {
			t.Error("expected injected context")
		}
	})

	t.Run("existing systemInstruction", func(t *testing.T) {
		var doc map[string]any
		if err := json.Unmarshal([]byte(`{"contents":[],"systemInstruction":{"parts":[{"text":"existing"}]}}`), &doc); err != nil {
			t.Fatalf("invalid test JSON: %v", err)
		}

		result, err := InjectContext(doc, apiformat.Gemini, "test context")
		if err != nil {
			t.Fatal(err)
		}

		var out map[string]any
		if err := json.Unmarshal(result, &out); err != nil {
			t.Fatalf("failed to unmarshal result: %v", err)
		}
		si := out["systemInstruction"].(map[string]any)
		parts := si["parts"].([]any)
		if len(parts) != 2 {
			t.Fatalf("expected 2 parts, got %d", len(parts))
		}
	})
}

func TestInjectContextOpenAIResponses(t *testing.T) {
	t.Run("no instructions", func(t *testing.T) {
		var doc map[string]any
		if err := json.Unmarshal([]byte(`{"model":"gpt-4o","input":"hello"}`), &doc); err != nil {
			t.Fatalf("invalid test JSON: %v", err)
		}

		result, err := InjectContext(doc, apiformat.OpenAIResponses, "test context")
		if err != nil {
			t.Fatal(err)
		}

		var out map[string]any
		if err := json.Unmarshal(result, &out); err != nil {
			t.Fatalf("failed to unmarshal result: %v", err)
		}
		if out["instructions"] != "test context" {
			t.Errorf("expected instructions = 'test context', got %v", out["instructions"])
		}
	})

	t.Run("existing instructions", func(t *testing.T) {
		var doc map[string]any
		if err := json.Unmarshal([]byte(`{"model":"gpt-4o","input":"hello","instructions":"Be helpful"}`), &doc); err != nil {
			t.Fatalf("invalid test JSON: %v", err)
		}

		result, err := InjectContext(doc, apiformat.OpenAIResponses, "test context")
		if err != nil {
			t.Fatal(err)
		}

		var out map[string]any
		if err := json.Unmarshal(result, &out); err != nil {
			t.Fatalf("failed to unmarshal result: %v", err)
		}
		instructions := out["instructions"].(string)
		if !strings.HasPrefix(instructions, "Be helpful") {
			t.Error("should start with original instructions")
		}
		if !strings.Contains(instructions, "test context") {
			t.Error("should contain injected context")
		}
	})
}

func TestInjectGeminiContext(t *testing.T) {
	const block = "CTX"
	partsOf := func(doc map[string]any) []any {
		si := doc["systemInstruction"].(map[string]any)
		return si["parts"].([]any)
	}

	t.Run("no systemInstruction creates one", func(t *testing.T) {
		doc := map[string]any{}
		injectGeminiContext(doc, block)
		p := partsOf(doc)
		if len(p) != 1 || p[0].(map[string]any)["text"] != block {
			t.Fatalf("expected one part with block, got %v", p)
		}
	})

	t.Run("non-map systemInstruction overwritten", func(t *testing.T) {
		doc := map[string]any{"systemInstruction": "a string"}
		injectGeminiContext(doc, block)
		p := partsOf(doc)
		if len(p) != 1 || p[0].(map[string]any)["text"] != block {
			t.Fatalf("expected overwrite to parts, got %v", doc["systemInstruction"])
		}
	})

	t.Run("map without parts gets parts", func(t *testing.T) {
		doc := map[string]any{"systemInstruction": map[string]any{"role": "system"}}
		injectGeminiContext(doc, block)
		p := partsOf(doc)
		if len(p) != 1 || p[0].(map[string]any)["text"] != block {
			t.Fatalf("expected parts set, got %v", p)
		}
	})

	t.Run("existing parts appended", func(t *testing.T) {
		doc := map[string]any{"systemInstruction": map[string]any{
			"parts": []any{map[string]any{"text": "orig"}},
		}}
		injectGeminiContext(doc, block)
		p := partsOf(doc)
		if len(p) != 2 || p[0].(map[string]any)["text"] != "orig" || p[1].(map[string]any)["text"] != block {
			t.Fatalf("expected append after orig, got %v", p)
		}
	})
}
