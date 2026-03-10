package inject

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestDetectFormat(t *testing.T) {
	tests := []struct {
		name string
		json string
		want string
	}{
		{
			name: "anthropic with system string",
			json: `{"model":"claude-3","system":"You are helpful","messages":[]}`,
			want: "anthropic",
		},
		{
			name: "anthropic with content blocks",
			json: `{"model":"claude-3","messages":[{"role":"user","content":[{"type":"text","text":"hi"}]}]}`,
			want: "anthropic",
		},
		{
			name: "openai",
			json: `{"model":"gpt-4","messages":[{"role":"user","content":"hello"}]}`,
			want: "openai",
		},
		{
			name: "gemini",
			json: `{"contents":[{"role":"user","parts":[{"text":"hello"}]}]}`,
			want: "gemini",
		},
		{
			name: "unknown",
			json: `{"prompt":"hello","max_tokens":100}`,
			want: "",
		},
		{
			name: "empty object",
			json: `{}`,
			want: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var doc map[string]any
			json.Unmarshal([]byte(tt.json), &doc)
			if got := detectFormat(doc); got != tt.want {
				t.Errorf("detectFormat() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestExtractUserQuery(t *testing.T) {
	tests := []struct {
		name   string
		json   string
		format string
		want   string
	}{
		{
			name:   "anthropic string content",
			json:   `{"messages":[{"role":"user","content":"hello world"}]}`,
			format: "anthropic",
			want:   "hello world",
		},
		{
			name:   "anthropic array content",
			json:   `{"messages":[{"role":"user","content":[{"type":"text","text":"part1"},{"type":"text","text":"part2"}]}]}`,
			format: "anthropic",
			want:   "part1 part2",
		},
		{
			name:   "anthropic last user message",
			json:   `{"messages":[{"role":"user","content":"first"},{"role":"assistant","content":"reply"},{"role":"user","content":"second"}]}`,
			format: "anthropic",
			want:   "second",
		},
		{
			name:   "anthropic empty messages",
			json:   `{"messages":[]}`,
			format: "anthropic",
			want:   "",
		},
		{
			name:   "openai string content",
			json:   `{"messages":[{"role":"user","content":"hello openai"}]}`,
			format: "openai",
			want:   "hello openai",
		},
		{
			name:   "openai array content",
			json:   `{"messages":[{"role":"user","content":[{"type":"text","text":"multi"},{"type":"text","text":"part"}]}]}`,
			format: "openai",
			want:   "multi part",
		},
		{
			name:   "openai no user messages",
			json:   `{"messages":[{"role":"system","content":"you are helpful"}]}`,
			format: "openai",
			want:   "",
		},
		{
			name:   "gemini",
			json:   `{"contents":[{"role":"user","parts":[{"text":"hello gemini"}]}]}`,
			format: "gemini",
			want:   "hello gemini",
		},
		{
			name:   "gemini last user",
			json:   `{"contents":[{"role":"user","parts":[{"text":"first"}]},{"role":"model","parts":[{"text":"reply"}]},{"role":"user","parts":[{"text":"second"}]}]}`,
			format: "gemini",
			want:   "second",
		},
		{
			name:   "gemini empty",
			json:   `{"contents":[]}`,
			format: "gemini",
			want:   "",
		},
		{
			name:   "unknown format",
			json:   `{}`,
			format: "",
			want:   "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var doc map[string]any
			json.Unmarshal([]byte(tt.json), &doc)
			if got := extractUserQuery(doc, tt.format); got != tt.want {
				t.Errorf("extractUserQuery() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestTruncateQuery(t *testing.T) {
	tests := []struct {
		name     string
		query    string
		maxChars int
		wantLen  bool // true = check length, false = check exact
		want     string
	}{
		{
			name:     "short string unchanged",
			query:    "hello",
			maxChars: 100,
			want:     "hello",
		},
		{
			name:     "exact length unchanged",
			query:    "hello",
			maxChars: 5,
			want:     "hello",
		},
		{
			name:     "word boundary truncation",
			query:    "hello beautiful world today",
			maxChars: 20,
			wantLen:  true,
		},
		{
			name:     "hard truncation when no space",
			query:    "abcdefghijklmnopqrstuvwxyz",
			maxChars: 10,
			want:     "abcdefghij",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := truncateQuery(tt.query, tt.maxChars)
			if tt.wantLen {
				if len(got) > tt.maxChars {
					t.Errorf("truncateQuery() len = %d, want <= %d", len(got), tt.maxChars)
				}
			} else if got != tt.want {
				t.Errorf("truncateQuery() = %q, want %q", got, tt.want)
			}
		})
	}
}

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

		result, err := injectContext(doc, "anthropic", "test context")
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

		result, err := injectContext(doc, "anthropic", "test context")
		if err != nil {
			t.Fatal(err)
		}

		var out map[string]any
		json.Unmarshal(result, &out)
		sys := out["system"].([]any)
		if len(sys) != 2 {
			t.Fatalf("expected 2 system blocks, got %d", len(sys))
		}
		// First block should be original text.
		if sys[0].(map[string]any)["text"] != "You are helpful" {
			t.Error("first block should be original system text")
		}
		// Second block should be injected context.
		if sys[1].(map[string]any)["text"] != "test context" {
			t.Error("second block should be injected context")
		}
	})

	t.Run("array system", func(t *testing.T) {
		var doc map[string]any
		json.Unmarshal([]byte(`{"model":"claude-3","system":[{"type":"text","text":"existing"}],"messages":[]}`), &doc)

		result, err := injectContext(doc, "anthropic", "test context")
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

		result, err := injectContext(doc, "openai", "test context")
		if err != nil {
			t.Fatal(err)
		}

		var out map[string]any
		json.Unmarshal(result, &out)
		msgs := out["messages"].([]any)
		if len(msgs) != 3 {
			t.Fatalf("expected 3 messages, got %d", len(msgs))
		}
		// Original system first, then injected system, then user.
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

		result, err := injectContext(doc, "openai", "test context")
		if err != nil {
			t.Fatal(err)
		}

		var out map[string]any
		json.Unmarshal(result, &out)
		msgs := out["messages"].([]any)
		if len(msgs) != 2 {
			t.Fatalf("expected 2 messages, got %d", len(msgs))
		}
		// Injected system first, then user.
		if msgs[0].(map[string]any)["content"] != "test context" {
			t.Error("first should be injected context")
		}
	})
}

func TestInjectContextGemini(t *testing.T) {
	t.Run("no systemInstruction", func(t *testing.T) {
		var doc map[string]any
		json.Unmarshal([]byte(`{"contents":[{"role":"user","parts":[{"text":"hello"}]}]}`), &doc)

		result, err := injectContext(doc, "gemini", "test context")
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

		result, err := injectContext(doc, "gemini", "test context")
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
