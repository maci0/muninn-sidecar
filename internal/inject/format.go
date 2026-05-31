// This file contains format-specific injection logic: how to insert a context
// block into each supported API format (Anthropic, OpenAI, Gemini, etc.) and
// how to format the context block itself within a token budget.
package inject

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"github.com/maci0/muninn-sidecar/internal/apiformat"
	"github.com/maci0/muninn-sidecar/internal/redact"
)

// entryChars estimates the character length a memory contributes to a context
// block without allocating an intermediate string. Format is
// "[" + concept + "] (relevance: X.XX)\n" + content + "\n\n"; the fixed overhead
// is 23 chars since the "%.2f" score is always 4 chars for values in [0,1].
func entryChars(m memory) int {
	return len(m.Concept) + len(m.Content) + 23
}

// withinBudget returns the longest score-ordered prefix of memories whose
// combined context-block size fits the token budget. The first memory is always
// included even if it alone exceeds the budget, matching formatContextBlock's
// guarantee that something relevant is injected when anything qualifies.
// Memories are expected to be pre-sorted by score (descending).
func withinBudget(memories []memory, budget int) []memory {
	if len(memories) == 0 {
		return memories
	}

	budgetChars := budget * charPerToken
	totalChars := len(apiformat.ContextPrefix) + len(apiformat.ContextSuffix) + 2 // newlines

	kept := make([]memory, 0, len(memories))
	for _, m := range memories {
		entryLen := entryChars(m)
		if totalChars+entryLen > budgetChars && len(kept) > 0 {
			break
		}
		kept = append(kept, m)
		totalChars += entryLen
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

	totalChars := len(apiformat.ContextPrefix) + len(apiformat.ContextSuffix) + 2 // newlines
	for _, m := range kept {
		// Defense in depth: scrub secrets from recalled content before it is
		// injected into the outgoing request. A memory stored by another client
		// (or before write-side redaction existed) must not be re-transmitted to
		// the provider in a session where it wasn't otherwise present.
		concept := redact.Secrets(m.Concept)
		content := redact.Secrets(m.Content)
		sb.WriteByte('[')
		sb.WriteString(concept)
		sb.WriteString("] (relevance: ")
		sb.WriteString(strconv.FormatFloat(m.Score, 'f', 2, 64))
		sb.WriteString(")\n")
		sb.WriteString(content)
		sb.WriteString("\n\n")
		totalChars += len(concept) + len(content) + 23
	}

	sb.WriteString(apiformat.ContextSuffix)

	tokens := totalChars / charPerToken
	if tokens == 0 && totalChars > 0 {
		tokens = 1
	}
	return sb.String(), tokens, dropped
}

// InjectContext injects a context block into the request document based on
// the API format. Returns the modified JSON body.
func InjectContext(doc map[string]any, format, block string) ([]byte, error) {
	switch format {
	case apiformat.Anthropic:
		injectAnthropicContext(doc, block)
	case apiformat.OpenAI:
		injectOpenAIContext(doc, block)
	case apiformat.Gemini:
		injectGeminiContext(doc, block)
	case apiformat.GeminiCloudCode:
		req, ok := doc["request"].(map[string]any)
		if !ok {
			return nil, fmt.Errorf("gemini-cloudcode missing request field")
		}
		injectGeminiContext(req, block)
	case apiformat.OpenAIResponses:
		injectOpenAIResponsesContext(doc, block)
	default:
		return nil, fmt.Errorf("unsupported format: %s", format)
	}
	return json.Marshal(doc)
}

// injectAnthropicContext appends a text block to the system array.
// Converts string system to array if needed, creates if absent.
func injectAnthropicContext(doc map[string]any, block string) {
	contextBlock := map[string]any{
		"type": "text",
		"text": block,
	}

	sys, exists := doc["system"]
	if !exists {
		doc["system"] = []any{contextBlock}
		return
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
		// Unexpected type, create new array.
		doc["system"] = []any{contextBlock}
	}
}

// injectOpenAIContext inserts a system message after existing system messages,
// or at position 0 if none exist. Creates the messages array if absent.
func injectOpenAIContext(doc map[string]any, block string) {
	messages, ok := doc["messages"].([]any)
	if !ok {
		doc["messages"] = []any{
			map[string]any{"role": "system", "content": block},
		}
		return
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
}

// injectGeminiContext appends a text part to systemInstruction.parts,
// creating the structure if absent.
func injectGeminiContext(doc map[string]any, block string) {
	part := map[string]any{"text": block}

	si, exists := doc["systemInstruction"]
	if !exists {
		doc["systemInstruction"] = map[string]any{
			"parts": []any{part},
		}
		return
	}

	siMap, ok := si.(map[string]any)
	if !ok {
		doc["systemInstruction"] = map[string]any{
			"parts": []any{part},
		}
		return
	}

	parts, ok := siMap["parts"].([]any)
	if !ok {
		siMap["parts"] = []any{part}
		return
	}

	siMap["parts"] = append(parts, part)
}

// injectOpenAIResponsesContext appends a context block to the instructions
// field used by the OpenAI Responses API as the system prompt.
func injectOpenAIResponsesContext(doc map[string]any, block string) {
	if instructions, ok := doc["instructions"].(string); ok {
		doc["instructions"] = instructions + "\n\n" + block
	} else {
		doc["instructions"] = block
	}
}
