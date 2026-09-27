// This file contains format-specific injection logic: how to insert a context
// block into each supported API format (Anthropic, OpenAI, Gemini, etc.) and
// how to format the context block itself within a token budget.
package inject

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"math"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/maci0/muninn-sidecar/internal/apiformat"
	"github.com/maci0/muninn-sidecar/internal/redact"
)

// entryFraming is the fixed part of one context-block entry's format:
// "[" + "] (relevance: " + ")\n\n". The score sits between the label and the
// closing bracket and is measured by entryOverhead.
const entryFraming = len("[] (relevance: ") + len(")\n\n")

// entryOverhead is the framing cost of one entry at a given score: entryFraming
// plus the width the score actually prints at. The width is measured, not
// assumed at 4 bytes: a score in [0,1] renders as "0.60", but the gate also
// reads the composite fallback score when MuninnDB returns no vector_score, and
// that one legitimately exceeds 1.0 ("12.50", five bytes) or is negative
// ("-0.20", five). Assuming 4 there undercounts every such entry, so a turn
// built from composite scores packs past the budget it was given and the
// reported token count is short of the bytes written.
func entryOverhead(score float64) int {
	return entryFraming + len(strconv.FormatFloat(score, 'f', 2, 64))
}

// entryBytes estimates how many bytes a memory contributes to a context block
// without allocating an intermediate string. Format is
// "[" + concept + "] (relevance: X.XX)\n" + content + "\n\n".
//
// The unit is bytes, not characters: charPerToken is a bytes-per-token
// heuristic, and tokenizers charge by encoded length, so a CJK or emoji memory
// costs proportionally more than its character count suggests. Renaming from
// entryChars keeps the "chars" in the names and in charPerToken from claiming
// a precision the measurement does not have.
func entryBytes(m memory) int {
	return neutralizedLen(m.Concept) + neutralizedLen(m.Content) + entryOverhead(m.Score)
}

// neutralizedLen is the byte length NeutralizeMarkers would produce for s:
// each matched tag grows by 3 bytes ("<" becomes "&lt;"), and neutralization
// never shrinks text. Counting matches without building the result keeps the
// budget estimator allocation-free on the hot path.
func neutralizedLen(s string) int {
	if !strings.Contains(s, "<") {
		return len(s)
	}
	return len(s) + 3*apiformat.CountBlockTags(s)
}

// contextOverheadBytes is the fixed cost of the block wrapper: the markers, the
// two separating newlines, and the data-not-instructions notice. Shared by the
// budget estimator and the formatter so the token count the injector reports
// matches the bytes it actually writes.
const contextOverheadBytes = len(apiformat.ContextPrefix) + len(apiformat.ContextNotice) +
	len(apiformat.ContextSuffix) + 3

// minOversizedMemoryBytes is the floor for an over-budget memory's truncated
// content. A budget below this would otherwise make the first memory
// unprintable, and the point of keeping it is that something relevant still
// reaches the agent.
const minOversizedMemoryBytes = 200

// truncateToBytes clips s to at most maxBytes bytes and appends an ellipsis
// when it clipped, breaking at a word boundary when one lies in the final 20%
// of the clip. The budget is accounted in bytes (see entryBytes), so clipping by
// rune count — apiformat.TruncateText — would let a CJK or emoji memory overshoot
// it by its UTF-8 expansion factor, silently. A cut that lands mid-sequence
// backs off to the preceding rune boundary, so the result never decodes to a
// replacement character.
func truncateToBytes(s string, maxBytes int) string {
	const ellipsis = "…"
	if maxBytes <= 0 {
		return ""
	}
	if len(s) <= maxBytes {
		return s
	}
	limit := maxBytes - len(ellipsis)
	if limit < 0 {
		limit = 0
	}
	for limit > 0 && !utf8.RuneStart(s[limit]) {
		limit--
	}
	cut := s[:limit]
	win := breakWindowStart(len(cut))
	if i := strings.LastIndexAny(cut[win:], " \n"); i >= 0 {
		cut = cut[:win+i]
	}
	return cut + ellipsis
}

// breakWindowStart is the first byte offset of the final 20% of an n-byte
// clip, the window truncateToBytes searches for a word boundary in.
func breakWindowStart(n int) int { return n - n/5 }

// withinBudget returns the longest score-ordered prefix of memories whose
// combined context-block size fits the token budget. The first memory is always
// included even if it alone exceeds the budget, matching formatContextBlock's
// guarantee that something relevant is injected when anything qualifies; in
// that case its content is truncated to the budget rather than injected whole,
// so one oversized memory cannot spend arbitrarily many tokens on every turn.
// Memories are expected to be pre-sorted by score (descending).
func withinBudget(memories []memory, budget int) []memory {
	if len(memories) == 0 {
		return memories
	}

	// budget is user-supplied (`--inject-budget`); `budget * charPerToken`
	// overflows int for a budget above MaxInt/4, and the wrapped negative
	// budgetBytes would then make every memory after the first exceed the
	// budget, silently degrading a huge budget to a single memory. Clamp
	// instead: a budget that large already admits every memory.
	budgetBytes := budget
	if budgetBytes > math.MaxInt/charPerToken {
		budgetBytes = math.MaxInt
	} else {
		budgetBytes *= charPerToken
	}
	totalBytes := contextOverheadBytes

	kept := make([]memory, 0, len(memories))
	for _, m := range memories {
		entryLen := entryBytes(m)
		if totalBytes+entryLen > budgetBytes {
			if len(kept) > 0 {
				break
			}
			// First memory and it alone blows the budget: keep it, clipped.
			room := budgetBytes - totalBytes - neutralizedLen(m.Concept) - entryOverhead(m.Score)
			if room < minOversizedMemoryBytes {
				room = minOversizedMemoryBytes
			}
			// room is bytes, so re-clip until the *neutralized* content fits:
			// a tag surviving the clip keeps adding bytes per tag, and each
			// pass shortens the content, so this converges.
			for {
				m.Content = truncateToBytes(m.Content, room)
				entry := neutralizedLen(m.Concept) + neutralizedLen(m.Content) + entryOverhead(m.Score)
				if totalBytes+entry <= budgetBytes {
					break
				}
				room -= (totalBytes + entry) - budgetBytes
				if room < 1 {
					m.Content = ""
					break
				}
			}
			kept = append(kept, m)
			slog.Debug("inject: first memory exceeds the inject budget, truncating its content",
				"id", m.ID, "budget", budget, "kept_bytes", len(m.Content))
			break
		}
		kept = append(kept, m)
		totalBytes += entryLen
	}
	return kept
}

// formatContextBlock formats memories into a retrieved-context XML block,
// greedily including memories within the token budget (memories are expected
// to be pre-sorted by score). The first memory is always included even if it
// alone exceeds the budget. Returns the formatted block, estimated token count
// (4 chars ≈ 1 token), and how many gated memories the budget dropped (so a
// caller can surface silent truncation on large-memory vaults).
func formatContextBlock(memories []memory, budget int) (string, int, int) {
	kept := withinBudget(memories, budget)
	dropped := len(memories) - len(kept)
	if len(kept) == 0 {
		return "", 0, dropped
	}

	var sb strings.Builder
	sb.WriteString(apiformat.ContextPrefix)
	sb.WriteString("\n")
	sb.WriteString(apiformat.ContextNotice)
	sb.WriteString("\n")

	totalBytes := contextOverheadBytes
	for _, m := range kept {
		// Defense in depth: scrub secrets from recalled content before it is
		// injected into the outgoing request. A memory stored by another client
		// (or before write-side redaction existed) must not be re-transmitted to
		// the provider in a session where it wasn't otherwise present.
		//
		// Neutralizing the block markers is the injection counterpart: memory
		// content is attacker-influenced (any client that can write to the vault,
		// and any captured turn whose text came from a web page or a tool result),
		// and a memory carrying "</retrieved-context>" would close the block and
		// have everything after it read as top-level system prompt.
		concept := apiformat.NeutralizeMarkers(redact.Secrets(m.Concept))
		content := apiformat.NeutralizeMarkers(redact.Secrets(m.Content))
		sb.WriteByte('[')
		sb.WriteString(concept)
		sb.WriteString("] (relevance: ")
		sb.WriteString(strconv.FormatFloat(m.Score, 'f', 2, 64))
		sb.WriteString(")\n")
		sb.WriteString(content)
		sb.WriteString("\n\n")
		totalBytes += len(concept) + len(content) + entryOverhead(m.Score)
	}

	sb.WriteString(apiformat.ContextSuffix)

	tokens := totalBytes / charPerToken
	if tokens == 0 && totalBytes > 0 {
		tokens = 1
	}
	return sb.String(), tokens, dropped
}

// InjectContext injects a context block into the request document based on
// the API format. Returns the modified JSON body.
//
// Every per-format injector extends the field it knows; none of them overwrites
// a value whose shape it does not recognize. An unrecognized shape returns an
// error, and the caller forwards the agent's original body unchanged, because
// the one outcome msc may never produce is a request that has quietly lost the
// agent's own system prompt on its way upstream.
func InjectContext(doc map[string]any, format, block string) ([]byte, error) {
	switch format {
	case apiformat.Anthropic:
		if err := injectAnthropicContext(doc, block); err != nil {
			return nil, err
		}
	case apiformat.OpenAI:
		if err := injectOpenAIContext(doc, block); err != nil {
			return nil, err
		}
	case apiformat.Gemini:
		if err := injectGeminiContext(doc, block); err != nil {
			return nil, err
		}
	case apiformat.GeminiCloudCode:
		req, ok := doc["request"].(map[string]any)
		if !ok {
			return nil, fmt.Errorf("gemini-cloudcode missing request field")
		}
		if err := injectGeminiContext(req, block); err != nil {
			return nil, err
		}
	case apiformat.OpenAIResponses:
		if err := injectOpenAIResponsesContext(doc, block); err != nil {
			return nil, err
		}
	default:
		return nil, fmt.Errorf("unsupported format: %s", format)
	}
	return json.Marshal(doc)
}

// unexpectedShape names a value msc cannot extend without destroying it. The
// %T keeps the actual type in the message, since the whole point is that msc
// did not expect it.
func unexpectedShape(field string, v any) error {
	return fmt.Errorf("cannot inject: %s has unrecognized shape %T", field, v)
}

// injectAnthropicContext appends a text block to the system array.
// Converts string system to array if needed, creates if absent.
func injectAnthropicContext(doc map[string]any, block string) error {
	contextBlock := map[string]any{
		"type": "text",
		"text": block,
	}

	sys, exists := doc["system"]
	if !exists || sys == nil { // nil is a JSON null: carries no content to lose
		doc["system"] = []any{contextBlock}
		return nil
	}

	switch v := sys.(type) {
	case string:
		doc["system"] = []any{
			map[string]any{"type": "text", "text": v},
			contextBlock,
		}
	case []any:
		doc["system"] = append(v, contextBlock)
	default:
		return unexpectedShape("system", v)
	}
	return nil
}

// injectOpenAIContext inserts a system message after existing system messages,
// or at position 0 if none exist. Creates the messages array if absent.
func injectOpenAIContext(doc map[string]any, block string) error {
	raw, exists := doc["messages"]
	if !exists || raw == nil {
		doc["messages"] = []any{
			map[string]any{"role": "system", "content": block},
		}
		return nil
	}
	messages, ok := raw.([]any)
	if !ok {
		return unexpectedShape("messages", raw)
	}

	insertAt := 0
	for i, msg := range messages {
		m, ok := msg.(map[string]any)
		if !ok {
			continue
		}
		if m["role"] == "system" {
			insertAt = i + 1
		}
	}

	sysMsg := map[string]any{"role": "system", "content": block}

	result := make([]any, 0, len(messages)+1)
	result = append(result, messages[:insertAt]...)
	result = append(result, sysMsg)
	result = append(result, messages[insertAt:]...)
	doc["messages"] = result
	return nil
}

// injectGeminiContext appends a text part to systemInstruction.parts,
// creating the structure if absent.
func injectGeminiContext(doc map[string]any, block string) error {
	part := map[string]any{"text": block}

	si, exists := doc["systemInstruction"]
	if !exists || si == nil {
		doc["systemInstruction"] = map[string]any{
			"parts": []any{part},
		}
		return nil
	}

	siMap, ok := si.(map[string]any)
	if !ok {
		return unexpectedShape("systemInstruction", si)
	}

	rawParts, hasParts := siMap["parts"]
	if !hasParts || rawParts == nil {
		siMap["parts"] = []any{part}
		return nil
	}
	parts, ok := rawParts.([]any)
	if !ok {
		return unexpectedShape("systemInstruction.parts", rawParts)
	}

	siMap["parts"] = append(parts, part)
	return nil
}

// injectOpenAIResponsesContext appends a context block to the instructions
// field used by the OpenAI Responses API as the system prompt.
func injectOpenAIResponsesContext(doc map[string]any, block string) error {
	raw, exists := doc["instructions"]
	if !exists || raw == nil {
		doc["instructions"] = block
		return nil
	}
	instructions, ok := raw.(string)
	if !ok {
		return unexpectedShape("instructions", raw)
	}
	doc["instructions"] = instructions + "\n\n" + block
	return nil
}
