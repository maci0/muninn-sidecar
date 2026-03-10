// Package inject provides automatic memory retrieval and injection into
// LLM API requests. It recalls relevant memories from MuninnDB and injects
// them as system-level context before forwarding requests upstream.
package inject

import (
	"encoding/json"
	"fmt"
	"strings"
)

// contextPrefix is the marker used to identify injected context blocks.
// stripInjectedContext in the proxy package uses this to remove injected
// content before capturing exchanges, preventing recursive reinforcement.
const contextPrefix = "<retrieved-context source=\"muninn\">"
const contextSuffix = "</retrieved-context>"

// detectFormat identifies the API format of a request body.
// Returns "anthropic", "openai", "gemini", or "" for unknown.
func detectFormat(doc map[string]any) string {
	// Gemini: has "contents" array (check first since it's the most distinctive)
	if _, ok := doc["contents"]; ok {
		return "gemini"
	}
	// Gemini Cloud Code: wraps standard payload in a "request" field
	if req, ok := doc["request"].(map[string]any); ok {
		if _, ok := req["contents"]; ok {
			return "gemini-cloudcode"
		}
	}
	// Anthropic: has "system" field or content blocks with "type"
	if _, ok := doc["system"]; ok {
		return "anthropic"
	}
	// Check for Anthropic content block pattern in messages
	if messages, ok := doc["messages"].([]any); ok && len(messages) > 0 {
		for _, msg := range messages {
			m, ok := msg.(map[string]any)
			if !ok {
				continue
			}
			if content, ok := m["content"].([]any); ok {
				for _, block := range content {
					if b, ok := block.(map[string]any); ok {
						if _, hasType := b["type"]; hasType {
							return "anthropic"
						}
					}
				}
			}
		}
	}
	// OpenAI: has "messages" array (generic fallback)
	if _, ok := doc["messages"]; ok {
		return "openai"
	}
	return ""
}

// extractUserQuery walks the messages/contents backward to find the last
// user message and returns its text content.
func extractUserQuery(doc map[string]any, format string) string {
	switch format {
	case "anthropic":
		return extractAnthropicUserQuery(doc)
	case "openai":
		return extractOpenAIUserQuery(doc)
	case "gemini":
		return extractGeminiUserQuery(doc)
	case "gemini-cloudcode":
		if req, ok := doc["request"].(map[string]any); ok {
			return extractGeminiUserQuery(req)
		}
	}
	return ""
}

func extractAnthropicUserQuery(doc map[string]any) string {
	messages, ok := doc["messages"].([]any)
	if !ok {
		return ""
	}
	// Walk backward for last user message.
	for i := len(messages) - 1; i >= 0; i-- {
		m, ok := messages[i].(map[string]any)
		if !ok {
			continue
		}
		if m["role"] != "user" {
			continue
		}
		// Content can be string or array of blocks.
		if s, ok := m["content"].(string); ok {
			return s
		}
		if blocks, ok := m["content"].([]any); ok {
			var parts []string
			for _, block := range blocks {
				b, ok := block.(map[string]any)
				if !ok {
					continue
				}
				if b["type"] == "text" {
					if t, ok := b["text"].(string); ok {
						parts = append(parts, t)
					}
				}
			}
			return strings.Join(parts, " ")
		}
	}
	return ""
}

func extractOpenAIUserQuery(doc map[string]any) string {
	messages, ok := doc["messages"].([]any)
	if !ok {
		return ""
	}
	for i := len(messages) - 1; i >= 0; i-- {
		m, ok := messages[i].(map[string]any)
		if !ok {
			continue
		}
		if m["role"] != "user" {
			continue
		}
		// Content can be string or array.
		if s, ok := m["content"].(string); ok {
			return s
		}
		if parts, ok := m["content"].([]any); ok {
			var texts []string
			for _, part := range parts {
				p, ok := part.(map[string]any)
				if !ok {
					continue
				}
				if p["type"] == "text" {
					if t, ok := p["text"].(string); ok {
						texts = append(texts, t)
					}
				}
			}
			return strings.Join(texts, " ")
		}
	}
	return ""
}

func extractGeminiUserQuery(doc map[string]any) string {
	contents, ok := doc["contents"].([]any)
	if !ok {
		return ""
	}
	for i := len(contents) - 1; i >= 0; i-- {
		c, ok := contents[i].(map[string]any)
		if !ok {
			continue
		}
		if c["role"] != "user" {
			continue
		}
		parts, ok := c["parts"].([]any)
		if !ok {
			continue
		}
		var texts []string
		for _, part := range parts {
			p, ok := part.(map[string]any)
			if !ok {
				continue
			}
			if t, ok := p["text"].(string); ok {
				texts = append(texts, t)
			}
		}
		return strings.Join(texts, " ")
	}
	return ""
}

// truncateQuery truncates a query string to maxChars, breaking at a word
// boundary when possible.
func truncateQuery(query string, maxChars int) string {
	if len(query) <= maxChars {
		return query
	}
	// Try to break at a space within the last 20% of the budget.
	cutoff := maxChars
	minCut := maxChars - maxChars/5
	for i := cutoff - 1; i >= minCut; i-- {
		if query[i] == ' ' {
			return query[:i]
		}
	}
	return query[:maxChars]
}

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

// injectContext injects a context block into the request document based on
// the API format. Returns the modified JSON body.
func injectContext(doc map[string]any, format, block string) ([]byte, error) {
	switch format {
	case "anthropic":
		injectAnthropicContext(doc, block)
	case "openai":
		injectOpenAIContext(doc, block)
	case "gemini":
		injectGeminiContext(doc, block)
	case "gemini-cloudcode":
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

// injectAnthropicContext appends a text block with cache_control to the
// system array. Converts string system to array if needed, creates if absent.
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
