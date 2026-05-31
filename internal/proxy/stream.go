package proxy

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"strings"
	"sync"

	"github.com/maci0/muninn-sidecar/internal/apiformat"
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
	statusCode int
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
// EOF in Read() or by Close().
func (sc *streamCapture) finalize() {
	sc.once.Do(func() {
		if sc.store == nil {
			return
		}
		respBody := sc.buildRespBody()
		ex := buildExchange(sc.ctx, sc.statusCode, respBody)
		sc.store.Store(ex)
	})
}

// processChunk scans the chunk for complete "data: ..." lines, updating
// lastData incrementally. Partial lines are carried in lineBuf.
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
				slog.Warn("SSE line buffer exceeded limit, dropping partial line", "len", len(data), "path", sc.ctx.path)
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

		if !bytes.HasPrefix(lineBytes, sseDataPrefix) {
			continue
		}
		dBytes := lineBytes[len(sseDataPrefix):]
		// The space after "data:" is optional per the SSE spec; a single leading
		// space is stripped. The big-3 APIs send "data: ", but OpenAI-compatible
		// proxies and local servers (e.g. via custom upstreams) may omit it.
		if len(dBytes) > 0 && dBytes[0] == ' ' {
			dBytes = dBytes[1:]
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
					delta = delta[:remaining]
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
