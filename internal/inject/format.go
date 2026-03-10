// Package inject provides automatic memory retrieval and injection into
// LLM API requests. It recalls relevant memories from MuninnDB and injects
// them as system-level context before forwarding requests upstream.
package inject

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/maci0/muninn-sidecar/internal/apiformat"
)

// contextPrefix is the marker used to identify injected context blocks.
// proxy/filter.go's injectedContextPrefix must match this prefix for
// stripInjectedContext to remove injected content before capturing
// exchanges, preventing recursive reinforcement.
const contextPrefix = "<retrieved-context source=\"muninn\">"
const contextSuffix = "</retrieved-context>"

// memory represents a recalled memory from MuninnDB.
type memory struct {
	ID      string
	Concept string
	Content string
	Score   float64
}

// formatContextBlock formats memories into a retrieved-context XML block,
// greedily including memories by score within the token budget. Returns the
// formatted block and estimated token count (4 chars ≈ 1 token).
func formatContextBlock(memories []memory, budget int) (string, int) {
	if len(memories) == 0 {
		return "", 0
	}

	var sb strings.Builder
	sb.WriteString(contextPrefix)
	sb.WriteString("\n")

	totalChars := len(contextPrefix) + len(contextSuffix) + 2 // newlines
	budgetChars := budget * 4                                  // 4 chars per token

	included := 0
	for _, m := range memories {
		entry := fmt.Sprintf("[%s] (relevance: %.2f)\n%s\n\n", m.Concept, m.Score, m.Content)
		if totalChars+len(entry) > budgetChars && included > 0 {
			break
		}
		sb.WriteString(entry)
		totalChars += len(entry)
		included++
	}

	sb.WriteString(contextSuffix)

	tokens := totalChars / 4
	if tokens == 0 && totalChars > 0 {
		tokens = 1
	}
	return sb.String(), tokens
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
		// Convert string system to array format.
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

// injectOpenAIContext inserts a system message after existing system messages.
func injectOpenAIContext(doc map[string]any, block string) {
	messages, ok := doc["messages"].([]any)
	if !ok {
		doc["messages"] = []any{
			map[string]any{"role": "system", "content": block},
		}
		return
	}

	// Find the last system message index.
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

	// Insert after last system message.
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
