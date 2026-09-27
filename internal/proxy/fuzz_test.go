package proxy

import (
	"encoding/json"
	"testing"

	"github.com/maci0/muninn-sidecar/internal/store"
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
		_, out := cleanRequest(data, defaultFilterPatterns)
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
		_, out := cleanResponse(json.RawMessage(data), defaultFilterPatterns)
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
		doc := parseSSEDoc(data)
		// Invariant: a parsed SSE doc is consumed downstream by the JSON-based
		// delta/tool extractors, so any non-nil result must be re-marshalable.
		if doc != nil {
			if _, err := json.Marshal(doc); err != nil {
				t.Fatalf("parseSSEDoc result not marshalable: %v", err)
			}
		}
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

// FuzzExtractModelAndTokens drives the model/token-usage extractor over
// untrusted request and response bodies. The risky paths are the float64->int
// conversions for token counts (huge, negative, fractional, or non-finite
// numbers must not panic or corrupt the exchange) and JSON that is valid but
// not an object. Invariant: never panic.
func FuzzExtractModelAndTokens(f *testing.F) {
	f.Add(
		[]byte(`{"model":"claude-3-opus"}`),
		[]byte(`{"usage":{"input_tokens":500,"output_tokens":200,"cache_read_input_tokens":10}}`))
	f.Add(
		[]byte(`{"model":"gpt-4"}`),
		[]byte(`{"usage":{"prompt_tokens":1e308,"completion_tokens":-5}}`))
	f.Add(
		[]byte(`{}`),
		[]byte(`{"modelVersion":"gemini-pro","usageMetadata":{"promptTokenCount":1,"candidatesTokenCount":2}}`))
	f.Add([]byte(`"just a string"`), []byte(`12345`))
	f.Add([]byte(`not json`), []byte(`not json either`))
	f.Fuzz(func(t *testing.T, req, resp []byte) {
		ex := &store.CapturedExchange{
			ReqBody:  json.RawMessage(req),
			RespBody: json.RawMessage(resp),
		}
		(&Proxy{filterPatterns: defaultFilterPatterns}).prepareExchange(ex)
	})
}

// FuzzSanitizeJSON checks the storage-sanitization contract: arbitrary bytes
// (plain-text error pages, partial JSON, binary) must always emerge as
// syntactically-valid JSON suitable for MuninnDB, never a panic or invalid doc.
func FuzzSanitizeJSON(f *testing.F) {
	f.Add([]byte(`{"ok":true}`))
	f.Add([]byte(`plain text error page`))
	f.Add([]byte(``))
	f.Add([]byte("\x00\xff partial {"))
	f.Fuzz(func(t *testing.T, data []byte) {
		out := sanitizeJSON(data)
		if !json.Valid(out) {
			t.Fatalf("sanitizeJSON produced invalid JSON from %q: %q", data, out)
		}
	})
}

// FuzzProcessChunk exercises the incremental SSE chunk parser on untrusted
// upstream bytes. The two-chunk split fuzzes the partial-line carry-over path
// (sc.lineBuf), where line boundaries fall mid-chunk and the slicing of
// "data:" prefixes / trailing \r must never read out of range. Invariants:
// never panic; buffered state stays within its caps; the synthetic response
// built from whatever was accumulated is always valid JSON.
func FuzzProcessChunk(f *testing.F) {
	f.Add([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n"), []byte("data:[DONE]\n"))
	f.Add([]byte("data:{\"type\":\"content_block_delta\",\"delta\":{\"text\":\"x\"}}"), []byte("\nevent: ping\n"))
	f.Add([]byte("data: "), []byte("partial-then\r\n"))
	f.Add([]byte{}, []byte{})
	f.Fuzz(func(t *testing.T, a, b []byte) {
		sc := &streamCapture{ctx: &captureCtx{}, statusCode: 200}
		sc.processChunk(a)
		sc.processChunk(b)

		if sc.textAccum.Len() > maxTextAccum {
			t.Fatalf("textAccum %d exceeds cap %d", sc.textAccum.Len(), maxTextAccum)
		}
		if len(sc.lineBuf) > maxStreamBuf {
			t.Fatalf("lineBuf %d exceeds cap %d", len(sc.lineBuf), maxStreamBuf)
		}
		if len(sc.toolNames) > maxToolNames {
			t.Fatalf("toolNames %d exceeds cap %d", len(sc.toolNames), maxToolNames)
		}
		if out := sc.buildRespBody(); !json.Valid(out) {
			t.Fatalf("buildRespBody produced invalid JSON after processChunk: %q", out)
		}
	})
}
