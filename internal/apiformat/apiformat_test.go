package apiformat

import (
	"encoding/json"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestStripSystemReminders(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{
			name:  "no reminders",
			input: "plain text",
			want:  "plain text",
		},
		{
			name:  "single reminder",
			input: "before <system-reminder>stuff</system-reminder> after",
			want:  "before  after",
		},
		{
			name:  "multiline reminder",
			input: "before\n<system-reminder>\nline1\nline2\n</system-reminder>\nafter",
			want:  "before\n\nafter",
		},
		{
			name:  "entirely reminder",
			input: "<system-reminder>all metadata</system-reminder>",
			want:  "",
		},
		{
			name:  "multiple reminders",
			input: "<system-reminder>a</system-reminder> text <system-reminder>b</system-reminder>",
			want:  "text",
		},
		{
			name:  "empty string",
			input: "",
			want:  "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := StripSystemReminders(tt.input)
			if got != tt.want {
				t.Errorf("StripSystemReminders() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestDetectFormat(t *testing.T) {
	tests := []struct {
		name string
		json string
		want string
	}{
		{
			name: "anthropic with system string",
			json: `{"model":"claude-3","system":"You are helpful","messages":[]}`,
			want: Anthropic,
		},
		{
			name: "anthropic with content blocks",
			json: `{"model":"claude-3","messages":[{"role":"user","content":[{"type":"text","text":"hi"}]}]}`,
			want: Anthropic,
		},
		{
			name: "openai",
			json: `{"model":"gpt-4","messages":[{"role":"user","content":"hello"}]}`,
			want: OpenAI,
		},
		{
			name: "gemini",
			json: `{"contents":[{"role":"user","parts":[{"text":"hello"}]}]}`,
			want: Gemini,
		},
		{
			name: "gemini cloudcode",
			json: `{"request":{"contents":[{"role":"user","parts":[{"text":"hello"}]}]}}`,
			want: GeminiCloudCode,
		},
		{
			name: "openai responses string input",
			json: `{"model":"gpt-4o","input":"Hello world"}`,
			want: OpenAIResponses,
		},
		{
			name: "openai responses array input",
			json: `{"model":"gpt-4o","input":[{"type":"message","role":"user","content":"hello"}]}`,
			want: OpenAIResponses,
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
		{
			name: "openai multimodal without system",
			json: `{"model":"gpt-4","messages":[{"role":"user","content":[{"type":"text","text":"describe this"},{"type":"image_url","image_url":{"url":"https://example.com/img.png"}}]}]}`,
			want: OpenAI,
		},
		{
			name: "openai multimodal with system",
			json: `{"model":"gpt-4","messages":[{"role":"system","content":"You are helpful"},{"role":"user","content":[{"type":"text","text":"describe this"},{"type":"image_url","image_url":{"url":"https://example.com/img.png"}}]}]}`,
			want: OpenAI,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var doc map[string]any
			if err := json.Unmarshal([]byte(tt.json), &doc); err != nil {
				t.Fatalf("invalid test JSON: %v", err)
			}
			if got := DetectFormat(doc); got != tt.want {
				t.Errorf("DetectFormat() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestExtractUserMessage(t *testing.T) {
	tests := []struct {
		name string
		body string
		want string
	}{
		{
			name: "anthropic",
			body: `{"messages":[{"role":"user","content":[{"type":"text","text":"hello world"}]}]}`,
			want: "hello world",
		},
		{
			name: "anthropic string content",
			body: `{"messages":[{"role":"user","content":"hello"}]}`,
			want: "hello",
		},
		{
			name: "openai",
			body: `{"messages":[{"role":"user","content":"hello openai"}]}`,
			want: "hello openai",
		},
		{
			name: "gemini",
			body: `{"contents":[{"role":"user","parts":[{"text":"hello gemini"}]}]}`,
			want: "hello gemini",
		},
		{
			name: "openai responses string input",
			body: `{"model":"gpt-4o","input":"hello responses"}`,
			want: "hello responses",
		},
		{
			name: "openai responses array input",
			body: `{"model":"gpt-4o","input":[{"type":"message","role":"user","content":"first"},{"type":"message","role":"assistant","content":"reply"},{"type":"message","role":"user","content":"second"}]}`,
			want: "second",
		},
		{
			name: "openai responses array content parts",
			body: `{"model":"gpt-4o","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"part1"},{"type":"input_text","text":"part2"}]}]}`,
			want: "part1part2",
		},
		{
			name: "empty",
			body: `{}`,
			want: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ExtractUserMessage([]byte(tt.body))
			if got != tt.want {
				t.Errorf("ExtractUserMessage() = %q, want %q", got, tt.want)
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
			format: Anthropic,
			want:   "hello world",
		},
		{
			name:   "anthropic array content",
			json:   `{"messages":[{"role":"user","content":[{"type":"text","text":"part1"},{"type":"text","text":"part2"}]}]}`,
			format: Anthropic,
			want:   "part1part2",
		},
		{
			name:   "anthropic last user message",
			json:   `{"messages":[{"role":"user","content":"first"},{"role":"assistant","content":"reply"},{"role":"user","content":"second"}]}`,
			format: Anthropic,
			want:   "second",
		},
		{
			name:   "openai string content",
			json:   `{"messages":[{"role":"user","content":"hello openai"}]}`,
			format: OpenAI,
			want:   "hello openai",
		},
		{
			name:   "openai array content",
			json:   `{"messages":[{"role":"user","content":[{"type":"text","text":"multi"},{"type":"text","text":"part"}]}]}`,
			format: OpenAI,
			want:   "multipart",
		},
		{
			name:   "gemini",
			json:   `{"contents":[{"role":"user","parts":[{"text":"hello gemini"}]}]}`,
			format: Gemini,
			want:   "hello gemini",
		},
		{
			name:   "gemini cloudcode",
			json:   `{"request":{"contents":[{"role":"user","parts":[{"text":"hello cc"}]}]}}`,
			format: GeminiCloudCode,
			want:   "hello cc",
		},
		{
			name:   "openai responses string input",
			json:   `{"model":"gpt-4o","input":"hello responses"}`,
			format: OpenAIResponses,
			want:   "hello responses",
		},
		{
			name:   "openai responses array input last user",
			json:   `{"model":"gpt-4o","input":[{"type":"message","role":"user","content":"first"},{"type":"message","role":"assistant","content":"reply"},{"type":"message","role":"user","content":"second"}]}`,
			format: OpenAIResponses,
			want:   "second",
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
			if err := json.Unmarshal([]byte(tt.json), &doc); err != nil {
				t.Fatalf("invalid test JSON: %v", err)
			}
			if got := ExtractUserQuery(doc, tt.format); got != tt.want {
				t.Errorf("ExtractUserQuery() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestExtractAssistantMessage(t *testing.T) {
	tests := []struct {
		name string
		body string
		want string
	}{
		{
			name: "anthropic text only",
			body: `{"content":[{"type":"text","text":"I can help with that."}]}`,
			want: "I can help with that.",
		},
		{
			name: "anthropic tool_use only",
			body: `{"content":[{"type":"tool_use","name":"Read","input":{"file_path":"/src/main.go"}}]}`,
			want: "[Read /src/main.go]",
		},
		{
			name: "anthropic text plus tool_use",
			body: `{"content":[{"type":"text","text":"Let me read that file."},{"type":"tool_use","name":"Read","input":{"file_path":"/src/main.go"}}]}`,
			want: "Let me read that file.\n[Read /src/main.go]",
		},
		{
			name: "anthropic multiple tools",
			body: `{"content":[{"type":"tool_use","name":"Read","input":{"file_path":"/a.go"}},{"type":"tool_use","name":"Edit","input":{"file_path":"/b.go"}}]}`,
			want: "[Read /a.go] [Edit /b.go]",
		},
		{
			name: "anthropic tool with command input",
			body: `{"content":[{"type":"tool_use","name":"Bash","input":{"command":"go test ./..."}}]}`,
			want: "[Bash go test ./...]",
		},
		{
			name: "openai text only",
			body: `{"choices":[{"message":{"role":"assistant","content":"Sure thing."}}]}`,
			want: "Sure thing.",
		},
		{
			name: "openai tool_calls only",
			body: `{"choices":[{"message":{"role":"assistant","tool_calls":[{"function":{"name":"read_file"}}]}}]}`,
			want: "[read_file]",
		},
		{
			name: "openai text plus tool_calls",
			body: `{"choices":[{"message":{"role":"assistant","content":"Let me check.","tool_calls":[{"function":{"name":"read_file"}}]}}]}`,
			want: "Let me check.\n[read_file]",
		},
		{
			name: "gemini text only",
			body: `{"candidates":[{"content":{"parts":[{"text":"Here you go."}]}}]}`,
			want: "Here you go.",
		},
		{
			name: "gemini functionCall",
			body: `{"candidates":[{"content":{"parts":[{"functionCall":{"name":"readFile"}}]}}]}`,
			want: "[readFile]",
		},
		{
			name: "openai responses text only",
			body: `{"output":[{"type":"message","content":[{"type":"output_text","text":"Hello from responses!"}]}]}`,
			want: "Hello from responses!",
		},
		{
			name: "openai responses function_call only",
			body: `{"output":[{"type":"function_call","name":"read_file","arguments":"{}","call_id":"call_1"}]}`,
			want: "[read_file]",
		},
		{
			name: "openai responses text plus function_call",
			body: `{"output":[{"type":"message","content":[{"type":"output_text","text":"Let me check."}]},{"type":"function_call","name":"read_file","arguments":"{}","call_id":"call_1"}]}`,
			want: "Let me check.\n[read_file]",
		},
		{
			name: "empty",
			body: `{}`,
			want: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ExtractAssistantMessage([]byte(tt.body))
			if got != tt.want {
				t.Errorf("ExtractAssistantMessage() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestTruncateText(t *testing.T) {
	short := "hello"
	if got := TruncateText(short, 100); got != "hello" {
		t.Errorf("short string should be unchanged: %q", got)
	}

	long := strings.Repeat("word ", 100)
	got := TruncateText(long, 50)
	runes := []rune(got)
	if len(runes) > 51 { // 50 + "…" (1 rune)
		t.Errorf("truncated text too long: %d runes", len(runes))
	}
	if !strings.HasSuffix(got, "…") {
		t.Errorf("truncated text should end with ellipsis: %q", got)
	}

	// Multi-byte: should not split UTF-8 characters.
	cjk := strings.Repeat("日本語", 20) // 60 runes, each 3 bytes
	got = TruncateText(cjk, 10)
	runes = []rune(got)
	if len(runes) > 11 { // 10 + "…"
		t.Errorf("CJK truncation too long: %d runes", len(runes))
	}
	// Must be valid UTF-8 — a byte-slice cut would split a 3-byte rune and
	// corrupt the stored memory. (Count alone wouldn't catch it: invalid bytes
	// decode to U+FFFD and could still pass the length check.)
	if !utf8.ValidString(got) {
		t.Errorf("CJK truncation split a rune (invalid UTF-8): %q", got)
	}
}

func TestExtractRecentContext(t *testing.T) {
	tests := []struct {
		name   string
		json   string
		format string
		turns  int
		want   string
	}{
		{
			name:   "anthropic single turn",
			json:   `{"messages":[{"role":"user","content":"hello"}]}`,
			format: Anthropic,
			turns:  3,
			want:   "user: hello",
		},
		{
			name:   "anthropic last 2 of 3 turns",
			json:   `{"messages":[{"role":"user","content":"first"},{"role":"assistant","content":"a1"},{"role":"user","content":"second"}]}`,
			format: Anthropic,
			turns:  2,
			want:   "assistant: a1\nuser: second",
		},
		{
			name:   "anthropic all 3 turns",
			json:   `{"messages":[{"role":"user","content":"first"},{"role":"assistant","content":"a1"},{"role":"user","content":"second"}]}`,
			format: Anthropic,
			turns:  3,
			want:   "user: first\nassistant: a1\nuser: second",
		},
		{
			name:   "openai last 1 turn",
			json:   `{"messages":[{"role":"user","content":"first"},{"role":"assistant","content":"a1"},{"role":"user","content":"second"}]}`,
			format: OpenAI,
			turns:  1,
			want:   "user: second",
		},
		{
			name:   "gemini last 2 turns",
			json:   `{"contents":[{"role":"user","parts":[{"text":"q1"}]},{"role":"model","parts":[{"text":"a1"}]},{"role":"user","parts":[{"text":"q2"}]}]}`,
			format: Gemini,
			turns:  2,
			want:   "model: a1\nuser: q2",
		},
		{
			name:   "gemini cloudcode",
			json:   `{"request":{"contents":[{"role":"user","parts":[{"text":"question"}]}]}}`,
			format: GeminiCloudCode,
			turns:  3,
			want:   "user: question",
		},
		{
			name:   "openai responses string input",
			json:   `{"input":"hello world"}`,
			format: OpenAIResponses,
			turns:  3,
			want:   "user: hello world",
		},
		{
			name:   "openai responses array last 2",
			json:   `{"input":[{"type":"message","role":"user","content":"q1"},{"type":"message","role":"assistant","content":"a1"},{"type":"message","role":"user","content":"q2"}]}`,
			format: OpenAIResponses,
			turns:  2,
			want:   "assistant: a1\nuser: q2",
		},
		{
			name:   "unknown format returns empty",
			json:   `{}`,
			format: "",
			turns:  3,
			want:   "",
		},
		{
			name:   "zero turns returns empty",
			json:   `{"messages":[{"role":"user","content":"hello"}]}`,
			format: Anthropic,
			turns:  0,
			want:   "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var doc map[string]any
			if err := json.Unmarshal([]byte(tt.json), &doc); err != nil {
				t.Fatalf("invalid test JSON: %v", err)
			}
			if got := ExtractRecentContext(doc, tt.format, tt.turns); got != tt.want {
				t.Errorf("ExtractRecentContext() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestTruncateQuery(t *testing.T) {
	tests := []struct {
		name     string
		query    string
		maxRunes int
		want     string
	}{
		{
			name:     "short string unchanged",
			query:    "hello",
			maxRunes: 100,
			want:     "hello",
		},
		{
			name:     "exact length unchanged",
			query:    "hello",
			maxRunes: 5,
			want:     "hello",
		},
		{
			// maxRunes=16: search window is [12,15]; position 15 is the
			// space between "beautiful" and "world", so word-boundary cut
			// yields "hello beautiful" (15 runes, no trailing space).
			name:     "word boundary truncation",
			query:    "hello beautiful world today",
			maxRunes: 16,
			want:     "hello beautiful",
		},
		{
			name:     "hard truncation when no space",
			query:    "abcdefghijklmnopqrstuvwxyz",
			maxRunes: 10,
			want:     "abcdefghij",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := TruncateQuery(tt.query, tt.maxRunes)
			if got != tt.want {
				t.Errorf("TruncateQuery() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestExtractSSEToolNameBranches(t *testing.T) {
	cases := []struct {
		name string
		doc  string
		want string
	}{
		{"anthropic tool_use", `{"type":"content_block_start","content_block":{"type":"tool_use","name":"Read"}}`, "Read"},
		{"anthropic text block", `{"type":"content_block_start","content_block":{"type":"text"}}`, ""},
		{"anthropic no content_block", `{"type":"content_block_start"}`, ""},
		{"responses function_call", `{"type":"response.output_item.added","item":{"type":"function_call","name":"shell"}}`, "shell"},
		{"responses non-fn item", `{"type":"response.output_item.added","item":{"type":"message"}}`, ""},
		{"responses no item", `{"type":"response.output_item.added"}`, ""},
		{"openai chat tool_calls", `{"choices":[{"delta":{"tool_calls":[{"function":{"name":"Edit"}}]}}]}`, "Edit"},
		{"openai chat arg-only chunk", `{"choices":[{"delta":{"tool_calls":[{"function":{"arguments":"{}"}}]}}]}`, ""},
		{"openai chat no tool_calls", `{"choices":[{"delta":{"content":"hi"}}]}`, ""},
		{"gemini functionCall", `{"candidates":[{"content":{"parts":[{"text":"x"},{"functionCall":{"name":"grep"}}]}}]}`, "grep"},
		{"gemini no functionCall", `{"candidates":[{"content":{"parts":[{"text":"x"}]}}]}`, ""},
		{"gemini empty candidates content", `{"candidates":[{}]}`, ""},
		{"gemini content no parts", `{"candidates":[{"content":{}}]}`, ""},
		{"gemini part not object", `{"candidates":[{"content":{"parts":["str"]}}]}`, ""},
		{"unknown shape", `{"foo":"bar"}`, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var doc map[string]any
			if err := json.Unmarshal([]byte(c.doc), &doc); err != nil {
				t.Fatalf("bad test doc: %v", err)
			}
			if got := ExtractSSEToolName(doc); got != c.want {
				t.Errorf("ExtractSSEToolName(%s) = %q, want %q", c.doc, got, c.want)
			}
		})
	}
}
