package proxy

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"strings"
	"sync"
	"unicode/utf8"

	"github.com/maci0/muninn-sidecar/internal/apiformat"
	"github.com/maci0/muninn-sidecar/internal/stats"
)

// maxToolNames caps tracked tool names to prevent unbounded growth in
// long tool-use chains.
const maxToolNames = 20

// sseDataPrefix and sseDone are package-level byte slices to avoid per-call
// []byte conversions inside the SSE hot path.
var (
	sseDataPrefix = []byte("data:")
	sseDone       = []byte("[DONE]")
)

// streamCapture wraps a streaming response body (SSE or ndjson). Data flows
// through to the agent via Read() while text deltas are accumulated from the
// stream to build a synthetic Anthropic-format response, falling back to the
// last data line only if no text deltas or tool names are captured.
//
// sync.Once ensures the store call happens exactly once even if Read returns
// EOF multiple times (which http.Response.Body contracts allow).
type streamCapture struct {
	io.ReadCloser
	ctx        *captureCtx
	store      Storer
	stats      *stats.Stats // optional session counters (nil = no recording)
	statusCode int
	clock      Clock
	once       sync.Once

	// Incremental SSE parsing: we track the last non-[DONE] data line
	// and a line buffer for partial reads, avoiding unbounded memory.
	lineBuf  []byte // partial line carried across Read calls
	lastData string // last complete "data: ..." value seen
	totalLen int    // total bytes seen (for fallback summary)

	// Accumulated assistant text from SSE content deltas.
	textAccum strings.Builder // capped at maxTextAccum
	usageJSON string          // last data line containing usage metadata

	// Tool names from content_block_start events. Captures what the
	// assistant was doing (file reads, edits, commands) even when the
	// response is tool-use-only with no text output.
	toolNames []string
}

func (sc *streamCapture) Read(p []byte) (int, error) {
	n, err := sc.ReadCloser.Read(p)
	if n > 0 {
		sc.processChunk(p[:n])
	}
	if err == io.EOF {
		sc.finalize()
	}
	return n, err
}

// Close overrides the embedded ReadCloser's Close to ensure the exchange is
// captured even if the stream is interrupted before EOF (e.g. client disconnect).
func (sc *streamCapture) Close() error {
	sc.finalize()
	return sc.ReadCloser.Close()
}

// finalize stores the captured exchange exactly once, whether triggered by
// EOF in Read() or by Close(). The exchange's DurationMs is the full stream
// time (arrival to last byte), so the latency sample is recorded here rather
// than in captureResponse, which would only have seen the first byte.
func (sc *streamCapture) finalize() {
	sc.once.Do(func() {
		respBody := sc.buildRespBody()
		ex := buildExchange(clockOrSystem(sc.clock), sc.ctx, sc.statusCode, respBody)
		// Latency is a property of the turn, not of whether it was stored, so
		// it is recorded on every stream. The non-streaming path
		// (captureResponse) samples unconditionally too; skipping here would
		// silently drop every streaming turn from the latency stats under
		// --no-store.
		if sc.stats != nil {
			sc.stats.ObserveLatency(ex.DurationMs)
		}
		if sc.store == nil {
			return
		}
		sc.store.Store(ex)
	})
}

// isNDJSONLine reports whether a line carries a bare JSON event payload, the
// shape an ndjson stream uses instead of an SSE "data: " line. SSE control
// lines never start with '{', so the first byte separates the two formats.
func isNDJSONLine(line []byte) bool {
	i := 0
	for i < len(line) && (line[i] == ' ' || line[i] == '\t') {
		i++
	}
	return i < len(line) && line[i] == '{'
}

// processChunk scans the chunk for complete event lines ("data: ..." for SSE,
// bare JSON for ndjson), updating lastData incrementally. Partial lines are
// carried in lineBuf.
func (sc *streamCapture) processChunk(chunk []byte) {
	sc.totalLen += len(chunk)

	// Prepend any leftover from the previous read.
	data := chunk
	if len(sc.lineBuf) > 0 {
		data = append(sc.lineBuf, chunk...)
		sc.lineBuf = nil // clear to avoid aliasing data's backing array
	}

	for len(data) > 0 {
		idx := bytes.IndexByte(data, '\n')
		if idx == -1 {
			// Incomplete line — stash for next Read, but cap to avoid
			// accumulating a huge partial line.
			if len(data) <= maxStreamBuf {
				sc.lineBuf = append(sc.lineBuf[:0], data...)
			} else {
				slog.Warn("SSE line buffer exceeded limit, dropping partial line", "request_id", sc.ctx.id, "len", len(data), "path", sc.ctx.path)
			}
			break
		}

		// Trim trailing \r without converting to string. The slice is a
		// view into data — no allocation until we actually need a string.
		lineBytes := data[:idx]
		if len(lineBytes) > 0 && lineBytes[len(lineBytes)-1] == '\r' {
			lineBytes = lineBytes[:len(lineBytes)-1]
		}
		data = data[idx+1:]

		dBytes := lineBytes
		if bytes.HasPrefix(lineBytes, sseDataPrefix) {
			dBytes = lineBytes[len(sseDataPrefix):]
			// The space after "data:" is optional per the SSE spec; a single leading
			// space is stripped. The big-3 APIs send "data: ", but OpenAI-compatible
			// proxies and local servers (e.g. via custom upstreams) may omit it.
			if len(dBytes) > 0 && dBytes[0] == ' ' {
				dBytes = dBytes[1:]
			}
		} else if !isNDJSONLine(dBytes) {
			// SSE control lines (event:, id:, retry:, ":") carry no event payload.
			continue
		}
		if bytes.Equal(dBytes, sseDone) {
			continue
		}

		// Convert to string once; reuse for all string operations below.
		d := string(dBytes)
		sc.lastData = d

		// Parse once and reuse for both delta and tool name extraction.
		// This avoids double json.Unmarshal on the SSE hot path.
		if sseDoc := parseSSEDoc(dBytes); sseDoc != nil {
			// Accumulate text deltas from content events.
			if delta := apiformat.ExtractSSEDelta(sseDoc); delta != "" && sc.textAccum.Len() < maxTextAccum {
				remaining := maxTextAccum - sc.textAccum.Len()
				if len(delta) > remaining {
					delta = clampBytes(delta, remaining)
				}
				sc.textAccum.WriteString(delta)
			}

			// Track tool_use block starts for context about what
			// the assistant is doing (file reads, edits, commands).
			if len(sc.toolNames) < maxToolNames {
				if name := apiformat.ExtractSSEToolName(sseDoc); name != "" {
					sc.toolNames = append(sc.toolNames, name)
				}
			}
		}

		// Track usage metadata separately.
		if strings.Contains(d, `"usage"`) || strings.Contains(d, `"usageMetadata"`) {
			sc.usageJSON = d
		}
	}
}

// buildRespBody returns the response body for storage. When assistant text
// or tool actions were captured from the SSE stream, it builds a synthetic
// Anthropic-format response that ExtractAssistantMessage already understands.
// Usage metadata is merged from the last usage-bearing SSE event. Falls back
// to raw lastData when no meaningful content was captured, or a minimal
// {"_stream":true,"_bytes":N} marker if the stream produced no data lines.
func (sc *streamCapture) buildRespBody() json.RawMessage {
	if sc.textAccum.Len() > 0 || len(sc.toolNames) > 0 {
		return sc.buildSyntheticResp()
	}
	if sc.lastData != "" && json.Valid([]byte(sc.lastData)) {
		return json.RawMessage(sc.lastData)
	}
	if sc.lastData != "" {
		b, _ := json.Marshal(sc.lastData)
		return json.RawMessage(b)
	}
	b, _ := json.Marshal(map[string]any{
		"_stream": true,
		"_bytes":  sc.totalLen,
	})
	return b
}

// buildSyntheticResp constructs an Anthropic-format response body from
// accumulated text deltas, tool_use names, and usage metadata.
func (sc *streamCapture) buildSyntheticResp() json.RawMessage {
	var content []any
	if sc.textAccum.Len() > 0 {
		content = append(content, map[string]string{
			"type": "text", "text": sc.textAccum.String(),
		})
	}
	for _, name := range sc.toolNames {
		content = append(content, map[string]any{
			"type":  "tool_use",
			"name":  name,
			"input": map[string]any{},
		})
	}
	resp := map[string]any{"content": content}

	// Merge usage from the dedicated usage event or lastData.
	usageSrc := sc.usageJSON
	if usageSrc == "" {
		usageSrc = sc.lastData
	}
	if usageSrc != "" {
		var event map[string]any
		if json.Unmarshal([]byte(usageSrc), &event) == nil {
			if u, ok := event["usage"]; ok {
				resp["usage"] = u
			}
			if u, ok := event["usageMetadata"]; ok {
				resp["usageMetadata"] = u
			}
			// OpenAI Responses API: usage is nested under response.usage
			// in the response.completed event.
			if r, ok := event["response"].(map[string]any); ok {
				if u, ok := r["usage"]; ok {
					resp["usage"] = u
				}
			}
		}
	}

	b, _ := json.Marshal(resp)
	return json.RawMessage(b)
}

// clampBytes truncates s to at most max bytes, backing the cut off to a
// character boundary so a cap landing mid-rune cannot leave a partial rune
// (which would marshal as a replacement character) in the accumulated text.
//
// Only the tail of the clip is trimmed. A pre-existing invalid byte earlier in
// s is left in place: re-validating the whole string and dropping bytes from
// the end would discard every character after that byte, losing text the cap
// never touched.
func clampBytes(s string, max int) string {
	if max <= 0 {
		return ""
	}
	if len(s) <= max {
		return s
	}
	s = s[:max]
	for len(s) > 0 {
		// A last rune that decodes to RuneError at width 1 is either a
		// continuation byte of a sequence the cap cut in half or a stray
		// invalid byte; both go, and only they: a valid character ending
		// inside the cap ends the trim.
		if r, size := utf8.DecodeLastRuneInString(s); r == utf8.RuneError && size <= 1 {
			s = s[:len(s)-1]
			continue
		}
		break
	}
	return s
}

// parseSSEDoc parses a single SSE data line as JSON. Returns nil if the data
// is empty, not a JSON object, or fails to parse. Used to share a single
// json.Unmarshal call between delta and tool name extraction in processChunk.
func parseSSEDoc(data []byte) map[string]any {
	if len(data) == 0 || data[0] != '{' {
		return nil
	}
	var doc map[string]any
	if json.Unmarshal(data, &doc) != nil {
		return nil
	}
	return doc
}
