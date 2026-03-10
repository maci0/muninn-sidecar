package inject

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/maci0/muninn-sidecar/internal/apiformat"
)

func TestFormatContextBlock(t *testing.T) {
	t.Run("multiple memories within budget", func(t *testing.T) {
		mems := []memory{
			{ID: "1", Concept: "test1", Content: "content one", Score: 0.9},
			{ID: "2", Concept: "test2", Content: "content two", Score: 0.8},
		}
		block, tokens := formatContextBlock(mems, 2048)
		if !strings.Contains(block, contextPrefix) {
			t.Error("block should contain context prefix")
		}
		if !strings.Contains(block, contextSuffix) {
			t.Error("block should contain context suffix")
		}
		if !strings.Contains(block, "test1") || !strings.Contains(block, "test2") {
			t.Error("block should contain both memories")
		}
		if tokens <= 0 {
			t.Error("tokens should be positive")
		}
	})

	t.Run("budget limits memories", func(t *testing.T) {
		mems := []memory{
			{ID: "1", Concept: "first", Content: strings.Repeat("x", 1000), Score: 0.9},
			{ID: "2", Concept: "second", Content: strings.Repeat("y", 1000), Score: 0.8},
		}
		// Very tight budget that can fit first but not second.
		block, _ := formatContextBlock(mems, 300)
		if !strings.Contains(block, "first") {
			t.Error("should include first memory")
		}
		if strings.Contains(block, "second") {
			t.Error("should not include second memory (over budget)")
		}
	})

	t.Run("empty memories", func(t *testing.T) {
		block, tokens := formatContextBlock(nil, 2048)
		if block != "" {
			t.Error("empty memories should return empty block")
		}
		if tokens != 0 {
			t.Error("empty memories should return 0 tokens")
		}
	})
}

func TestInjectContextAnthropic(t *testing.T) {
	t.Run("no system field", func(t *testing.T) {
		var doc map[string]any
		json.Unmarshal([]byte(`{"model":"claude-3","messages":[]}`), &doc)

		result, err := InjectContext(doc, apiformat.Anthropic, "test context")
		if err != nil {
			t.Fatal(err)
		}

		var out map[string]any
		json.Unmarshal(result, &out)
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
		json.Unmarshal([]byte(`{"model":"claude-3","system":"You are helpful","messages":[]}`), &doc)

		result, err := InjectContext(doc, apiformat.Anthropic, "test context")
		if err != nil {
			t.Fatal(err)
		}

		var out map[string]any
		json.Unmarshal(result, &out)
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
		json.Unmarshal([]byte(`{"model":"claude-3","system":[{"type":"text","text":"existing"}],"messages":[]}`), &doc)

		result, err := InjectContext(doc, apiformat.Anthropic, "test context")
		if err != nil {
			t.Fatal(err)
		}

		var out map[string]any
		json.Unmarshal(result, &out)
		sys := out["system"].([]any)
		if len(sys) != 2 {
			t.Fatalf("expected 2 system blocks, got %d", len(sys))
		}
	})
}

func TestInjectContextOpenAI(t *testing.T) {
	t.Run("insert after system messages", func(t *testing.T) {
		var doc map[string]any
		json.Unmarshal([]byte(`{"model":"gpt-4","messages":[
			{"role":"system","content":"You are helpful"},
			{"role":"user","content":"hello"}
		]}`), &doc)

		result, err := InjectContext(doc, apiformat.OpenAI, "test context")
		if err != nil {
			t.Fatal(err)
		}

		var out map[string]any
		json.Unmarshal(result, &out)
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
		json.Unmarshal([]byte(`{"model":"gpt-4","messages":[
			{"role":"user","content":"hello"}
		]}`), &doc)

		result, err := InjectContext(doc, apiformat.OpenAI, "test context")
		if err != nil {
			t.Fatal(err)
		}

		var out map[string]any
		json.Unmarshal(result, &out)
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
		json.Unmarshal([]byte(`{"contents":[{"role":"user","parts":[{"text":"hello"}]}]}`), &doc)

		result, err := InjectContext(doc, apiformat.Gemini, "test context")
		if err != nil {
			t.Fatal(err)
		}

		var out map[string]any
		json.Unmarshal(result, &out)
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
		json.Unmarshal([]byte(`{"contents":[],"systemInstruction":{"parts":[{"text":"existing"}]}}`), &doc)

		result, err := InjectContext(doc, apiformat.Gemini, "test context")
		if err != nil {
			t.Fatal(err)
		}

		var out map[string]any
		json.Unmarshal(result, &out)
		si := out["systemInstruction"].(map[string]any)
		parts := si["parts"].([]any)
		if len(parts) != 2 {
			t.Fatalf("expected 2 parts, got %d", len(parts))
		}
	})
}
