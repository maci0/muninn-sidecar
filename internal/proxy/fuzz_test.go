package proxy

import (
	"encoding/json"
	"testing"
)

// Fuzz targets for the proxy's body/stream filtering surface — anti-recursion
// stripping of injected context and MuninnDB tool calls from untrusted request
// and response bodies, plus SSE event parsing. Invariant: never panic; filtered
// output, when produced, stays valid JSON.

func FuzzCleanRequest(f *testing.F) {
	f.Add([]byte(`{"system":[{"type":"text","text":"<retrieved-context source=\"muninn\">m</retrieved-context>"}],"messages":[]}`))
	f.Add([]byte(`{"messages":[{"role":"assistant","content":[{"type":"tool_use","name":"mcp__muninn__muninn_recall","id":"t1"}]}]}`))
	f.Add([]byte(`{"tools":[{"name":"muninn_remember"},{"name":"Read"}]}`))
	f.Add([]byte(`not json`))
	f.Fuzz(func(t *testing.T, data []byte) {
		out := cleanRequest(data, defaultFilterPatterns)
		// cleanRequest always returns syntactically-valid JSON (it wraps non-JSON
		// via sanitizeJSON). json.Valid checks syntax without the float64-overflow
		// quirk of unmarshalling huge numbers into interface{}.
		if !json.Valid(out) {
			t.Fatalf("cleanRequest produced invalid JSON: %q", out)
		}
	})
}

func FuzzCleanResponse(f *testing.F) {
	f.Add([]byte(`{"content":[{"type":"tool_use","name":"mcp__muninn__muninn_recall","id":"t1"},{"type":"text","text":"hi"}]}`))
	f.Add([]byte(`{"choices":[{"message":{"tool_calls":[{"id":"c1","function":{"name":"muninn_recall"}}]}}]}`))
	f.Add([]byte(`{}`))
	f.Fuzz(func(t *testing.T, data []byte) {
		out := cleanResponse(json.RawMessage(data), defaultFilterPatterns)
		// Contract: cleanResponse passes non-JSON through unchanged (response
		// bodies are JSON in practice); when the input IS valid JSON, filtering
		// must preserve syntactic validity.
		if !json.Valid(data) {
			return
		}
		if !json.Valid(out) {
			t.Fatalf("cleanResponse turned valid JSON into invalid: %q", out)
		}
	})
}

func FuzzParseSSEDoc(f *testing.F) {
	f.Add([]byte(`data: {"type":"content_block_delta","delta":{"text":"x"}}`))
	f.Add([]byte(`event: ping`))
	f.Add([]byte(`{"raw":"json"}`))
	f.Add([]byte("data: [DONE]"))
	f.Fuzz(func(t *testing.T, data []byte) {
		_ = parseSSEDoc(data)
	})
}

func FuzzStripInjectedContextDoc(f *testing.F) {
	f.Add([]byte(`{"system":[{"type":"text","text":"<session-context source=\"muninn\">x</session-context>"}]}`))
	f.Add([]byte(`{"messages":[{"role":"user","content":"<global-guide source=\"muninn\">g</global-guide>"}]}`))
	f.Fuzz(func(t *testing.T, data []byte) {
		var doc map[string]any
		if err := json.Unmarshal(data, &doc); err != nil || doc == nil {
			return
		}
		_ = stripInjectedContextDoc(doc)
		// Whatever it mutated must remain marshalable.
		if _, err := json.Marshal(doc); err != nil {
			t.Fatalf("doc unmarshalable after strip: %v", err)
		}
	})
}

func FuzzInjectedBlockStart(f *testing.F) {
	f.Add("Be helpful\n\n<retrieved-context source=\"muninn\">m</retrieved-context>")
	f.Add("<session-context source=\"muninn\">x</session-context>")
	f.Add("no markers here")
	f.Add("")
	f.Fuzz(func(t *testing.T, s string) {
		i := injectedBlockStart(s)
		// Contract: -1 (absent) or a valid index into s.
		if i < -1 || i >= len(s) {
			t.Fatalf("injectedBlockStart(%q) = %d, out of range", s, i)
		}
		if i >= 0 {
			// The returned index must actually begin a known marker.
			ok := false
			for _, m := range []string{"<retrieved-context", "<session-context", "<global-guide"} {
				if len(s)-i >= 1 && len(m) > 0 && i+1 <= len(s) {
					if hasPrefixAt(s, i, m) {
						ok = true
					}
				}
			}
			if !ok {
				t.Fatalf("injectedBlockStart(%q)=%d not at a marker", s, i)
			}
		}
	})
}

// hasPrefixAt reports whether s has prefix p starting at index i.
func hasPrefixAt(s string, i int, p string) bool {
	return i >= 0 && i+len(p) <= len(s) && s[i:i+len(p)] == p
}
