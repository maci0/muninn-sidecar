package apiformat

import (
	"encoding/json"
	"strings"
	"testing"
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
			name: "anthropic",
			body: `{"content":[{"type":"text","text":"I can help with that."}]}`,
			want: "I can help with that.",
		},
		{
			name: "openai",
			body: `{"choices":[{"message":{"role":"assistant","content":"Sure thing."}}]}`,
			want: "Sure thing.",
		},
		{
			name: "gemini",
			body: `{"candidates":[{"content":{"parts":[{"text":"Here you go."}]}}]}`,
			want: "Here you go.",
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
}

func TestTruncateQuery(t *testing.T) {
	tests := []struct {
		name     string
		query    string
		maxRunes int
		wantLen  bool
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
			name:     "word boundary truncation",
			query:    "hello beautiful world today",
			maxRunes: 20,
			wantLen:  true,
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
			if tt.wantLen {
				runes := []rune(got)
				if len(runes) > tt.maxRunes {
					t.Errorf("TruncateQuery() rune len = %d, want <= %d", len(runes), tt.maxRunes)
				}
			} else if got != tt.want {
				t.Errorf("TruncateQuery() = %q, want %q", got, tt.want)
			}
		})
	}
}
