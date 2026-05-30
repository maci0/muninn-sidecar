package apiformat

import (
	"encoding/json"
	"testing"
)

// These fuzz targets cover the apiformat parsing surface — the functions that
// ingest untrusted agent request and model response bodies in-flight. The
// invariant is simply: never panic on arbitrary input.

func docFrom(data []byte) (map[string]any, bool) {
	var doc map[string]any
	if err := json.Unmarshal(data, &doc); err != nil {
		return nil, false
	}
	return doc, true
}

func FuzzExtractUserMessage(f *testing.F) {
	f.Add([]byte(`{"messages":[{"role":"user","content":"hi"}]}`))
	f.Add([]byte(`{"contents":[{"role":"user","parts":[{"text":"hi"}]}]}`))
	f.Add([]byte(`{"system":"x","messages":[{"role":"user","content":[{"type":"text","text":"hi"}]}]}`))
	f.Add([]byte(`{"input":"hello"}`))
	f.Add([]byte(`not json`))
	f.Fuzz(func(t *testing.T, data []byte) {
		_ = ExtractUserMessage(data)
	})
}

func FuzzExtractAssistantMessage(f *testing.F) {
	f.Add([]byte(`{"content":[{"type":"text","text":"hi"},{"type":"tool_use","name":"X","input":{"file_path":"/a"}}]}`))
	f.Add([]byte(`{"choices":[{"message":{"content":"hi","tool_calls":[{"function":{"name":"f"}}]}}]}`))
	f.Add([]byte(`{"candidates":[{"content":{"parts":[{"text":"hi"},{"functionCall":{"name":"g"}}]}}]}`))
	f.Add([]byte(`{"output":[{"type":"message","content":[{"text":"hi"}]}]}`))
	f.Fuzz(func(t *testing.T, data []byte) {
		_ = ExtractAssistantMessage(data)
	})
}

func FuzzDetectAndExtract(f *testing.F) {
	f.Add([]byte(`{"messages":[{"role":"user","content":"hi"},{"role":"assistant","content":"yo"}]}`))
	f.Add([]byte(`{"contents":[{"role":"model","parts":[{"text":"a"}]}]}`))
	f.Add([]byte(`{"request":{"contents":[{"role":"user","parts":[{"text":"q"}]}]}}`))
	f.Fuzz(func(t *testing.T, data []byte) {
		doc, ok := docFrom(data)
		if !ok {
			return
		}
		format := DetectFormat(doc)
		_ = ExtractUserQuery(doc, format)
		// turns kept small but variable; ExtractRecentContext must tolerate any.
		_ = ExtractRecentContext(doc, format, len(data)%7)
	})
}

func FuzzStripSystemReminders(f *testing.F) {
	f.Add("hello <system-reminder>secret</system-reminder> world")
	f.Add("<system-reminder>unclosed")
	f.Add("plain text")
	f.Fuzz(func(t *testing.T, s string) {
		_ = StripSystemReminders(s)
	})
}

func FuzzTruncate(f *testing.F) {
	f.Add("hello world this is a longer string", 10)
	f.Add("multibyte: 日本語テキストの切り詰め試験", 5)
	f.Add("", -3)
	f.Add("x", 0)
	f.Fuzz(func(t *testing.T, s string, n int) {
		// Bound n so the test explores realistic limits without allocating huge
		// slices; negatives and zero are still exercised (guarded in truncateAt).
		if n > 1<<16 {
			n = 1 << 16
		}
		_ = TruncateText(s, n)
		_ = TruncateQuery(s, n)
	})
}

func FuzzExtractSSE(f *testing.F) {
	f.Add([]byte(`{"type":"content_block_delta","delta":{"type":"text_delta","text":"x"}}`))
	f.Add([]byte(`{"choices":[{"delta":{"content":"x","tool_calls":[{"function":{"name":"f"}}]}}]}`))
	f.Add([]byte(`{"candidates":[{"content":{"parts":[{"text":"x"}]}}]}`))
	f.Add([]byte(`{"type":"response.output_text.delta","delta":"x"}`))
	f.Fuzz(func(t *testing.T, data []byte) {
		doc, ok := docFrom(data)
		if !ok {
			return
		}
		_ = ExtractSSEDelta(doc)
		_ = ExtractSSEToolName(doc)
	})
}
