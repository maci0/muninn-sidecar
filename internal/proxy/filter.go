package proxy

import (
	"encoding/json"
	"strings"
)

// hasInjectedMarker checks if a text block contains a muninn injected context marker.
// This is used to detect and strip injected memory context blocks before storing captured
// exchanges. Must stay in sync with inject/format.go's prefixes.
func hasInjectedMarker(text string) bool {
	t := strings.TrimSpace(text)
	return strings.HasPrefix(t, "<retrieved-context") ||
		strings.HasPrefix(t, "<session-context") ||
		strings.HasPrefix(t, "<global-guide") ||
		strings.Contains(t, "source=\"muninn\"")
}

// cleanRequest removes injected context and muninn tool calls from a request body.
// Parses and serializes JSON at most once.
func cleanRequest(body []byte, patterns []string) json.RawMessage {
	msg := sanitizeJSON(body)
	if len(msg) == 0 || string(msg) == "null" {
		return msg
	}

	var doc map[string]any
	if err := json.Unmarshal(msg, &doc); err != nil {
		return msg
	}

	changed1 := stripInjectedContextDoc(doc)
	changed2 := false
	if len(patterns) > 0 {
		changed2 = filterMCPToolsDoc(doc, patterns)
	}

	if !changed1 && !changed2 {
		return msg
	}

	result, err := json.Marshal(doc)
	if err != nil {
		return msg
	}
	return json.RawMessage(result)
}

// cleanResponse removes muninn tool calls from a response body.
func cleanResponse(body json.RawMessage, patterns []string) json.RawMessage {
	if len(patterns) == 0 || len(body) == 0 {
		return body
	}

	var doc map[string]any
	if err := json.Unmarshal(body, &doc); err != nil {
		return body
	}

	if !filterMCPToolsDoc(doc, patterns) {
		return body
	}

	result, err := json.Marshal(doc)
	if err != nil {
		return body
	}
	return json.RawMessage(result)
}

// stripInjectedContextDoc removes injected memory context blocks from a parsed JSON doc.
// Returns true if any modifications were made.
//
// Handles all API formats:
//   - Anthropic: removes system[] blocks containing Muninn injected context markers
//   - OpenAI: removes system-role messages containing Muninn injected context markers
//   - Gemini: removes systemInstruction.parts containing Muninn injected context markers
//   - Gemini Cloud Code: removes systemInstruction.parts containing Muninn injected context markers
func stripInjectedContextDoc(doc map[string]any) bool {
	changed := false

	// Anthropic: system field (string or array).
	if sys, ok := doc["system"]; ok {
		switch v := sys.(type) {
		case string:
			if hasInjectedMarker(v) {
				delete(doc, "system")
				changed = true
			}
		case []any:
			kept, removed := filterArray(v, func(b map[string]any) bool {
				text, _ := b["text"].(string)
				return !hasInjectedMarker(text)
			})
			if removed {
				changed = true
				if len(kept) == 0 {
					delete(doc, "system")
				} else {
					doc["system"] = kept
				}
			}
		}
	}

	// OpenAI: messages array — remove system role messages with marker.
	if messages, ok := doc["messages"].([]any); ok {
		kept, removed := filterArray(messages, func(m map[string]any) bool {
			if m["role"] == "system" {
				content, _ := m["content"].(string)
				if hasInjectedMarker(content) {
					return false
				}
			}
			return true
		})
		if removed {
			changed = true
			doc["messages"] = kept
		}
	}

	// Gemini Cloud Code: request.systemInstruction.parts — remove parts with marker.
	if req, ok := doc["request"].(map[string]any); ok {
		if si, ok := req["systemInstruction"].(map[string]any); ok {
			if parts, ok := si["parts"].([]any); ok {
				kept, removed := filterArray(parts, func(p map[string]any) bool {
					text, _ := p["text"].(string)
					return !hasInjectedMarker(text)
				})
				if removed {
					changed = true
					if len(kept) == 0 {
						delete(req, "systemInstruction")
					} else {
						si["parts"] = kept
					}
				}
			}
		}
	}

	// Gemini: systemInstruction.parts — remove parts with marker.
	if si, ok := doc["systemInstruction"].(map[string]any); ok {
		if parts, ok := si["parts"].([]any); ok {
			kept, removed := filterArray(parts, func(p map[string]any) bool {
				text, _ := p["text"].(string)
				return !hasInjectedMarker(text)
			})
			if removed {
				changed = true
				if len(kept) == 0 {
					delete(doc, "systemInstruction")
				} else {
					si["parts"] = kept
				}
			}
		}
	}

	return changed
}

// defaultFilterPatterns matches tool names containing these substrings
// (case-insensitive). Matched tool_use/tool_result blocks are stripped
// from captured request/response bodies before MuninnDB storage. This
// prevents recursive reinforcement: without filtering, each captured
// exchange embeds the full conversation history — including previous
// MuninnDB tool calls — which would compound on every recall cycle.
var defaultFilterPatterns = []string{"muninn"}

// filterMCPToolsDoc strips tool-call content matching any of the given
// patterns from a parsed JSON doc. Returns true if modifications were made.
//
// Anthropic format:
//
//	messages[].content[].{type:"tool_use", name:"mcp__muninn__*"}
//	messages[].content[].{type:"tool_result", tool_use_id:...}
//
// OpenAI format:
//
//	messages[].tool_calls[].function.name
//	messages[].{role:"tool", tool_call_id:...}
//
// Response bodies (single-turn) are also handled:
//
//	content[].{type:"tool_use", name:...}   (Anthropic response)
//	choices[].message.tool_calls             (OpenAI response)
func filterMCPToolsDoc(doc map[string]any, patterns []string) bool {
	changed := false

	// Request bodies: filter the messages array (conversation history).
	if messages, ok := doc["messages"].([]any); ok {
		filtered, ok := filterMessages(messages, patterns)
		if ok {
			doc["messages"] = filtered
			changed = true
		}
	}

	// Anthropic response: filter top-level content array.
	if content, ok := doc["content"].([]any); ok {
		filtered, ids := filterContentBlocks(content, patterns, nil)
		if len(ids) > 0 {
			doc["content"] = filtered
			changed = true
		}
	}

	// OpenAI response: filter choices[].message.tool_calls.
	if choices, ok := doc["choices"].([]any); ok {
		for _, choice := range choices {
			c, ok := choice.(map[string]any)
			if !ok {
				continue
			}
			msg, ok := c["message"].(map[string]any)
			if !ok {
				continue
			}
			if filterOpenAIToolCalls(msg, patterns) {
				changed = true
			}
		}
	}

	// Strip muninn tool definitions from the tools array. These are large
	// schema objects that add noise to stored memories without contributing
	// meaningful content.
	if tools, ok := doc["tools"].([]any); ok {
		filtered := filterToolDefs(tools, patterns)
		if len(filtered) != len(tools) {
			doc["tools"] = filtered
			changed = true
		}
	}

	return changed
}

// filterMessages processes a conversation messages array, removing
// muninn-related tool calls and their paired results. Returns the
// filtered messages and true if anything was removed.
func filterMessages(messages []any, patterns []string) ([]any, bool) {
	// First pass: collect IDs of tool_use blocks that match.
	removedIDs := collectMatchingToolIDs(messages, patterns)
	if len(removedIDs) == 0 {
		return messages, false
	}

	// Second pass: remove matching blocks and empty messages.
	filtered := make([]any, 0, len(messages))
	for _, msg := range messages {
		m, ok := msg.(map[string]any)
		if !ok {
			filtered = append(filtered, msg)
			continue
		}

		// OpenAI: skip entire "tool" role messages paired with removed calls.
		if m["role"] == "tool" {
			if id, _ := m["tool_call_id"].(string); removedIDs[id] {
				continue
			}
		}

		// Anthropic: filter content blocks array.
		if content, ok := m["content"].([]any); ok {
			kept, _ := filterContentBlocks(content, patterns, removedIDs)
			if len(kept) == 0 {
				continue // drop empty message
			}
			m["content"] = kept
		}

		// OpenAI: filter tool_calls array.
		if filterOpenAIToolCalls(m, patterns) {
			// If no content and no tool_calls remain, drop the message.
			if m["content"] == nil && m["tool_calls"] == nil {
				continue
			}
		}

		filtered = append(filtered, m)
	}

	return filtered, true
}

// collectMatchingToolIDs scans messages for tool_use/tool_calls blocks
// that match patterns and returns a set of their IDs (used to find
// paired tool_result/tool messages).
func collectMatchingToolIDs(messages []any, patterns []string) map[string]bool {
	ids := map[string]bool{}
	for _, msg := range messages {
		m, ok := msg.(map[string]any)
		if !ok {
			continue
		}

		// Anthropic: content[].type == "tool_use"
		if content, ok := m["content"].([]any); ok {
			for _, block := range content {
				b, ok := block.(map[string]any)
				if !ok {
					continue
				}
				if b["type"] == "tool_use" {
					name, _ := b["name"].(string)
					if matchesAnyPattern(name, patterns) {
						if id, ok := b["id"].(string); ok {
							ids[id] = true
						}
					}
				}
			}
		}

		// OpenAI: tool_calls[].function.name
		if toolCalls, ok := m["tool_calls"].([]any); ok {
			for _, tc := range toolCalls {
				t, ok := tc.(map[string]any)
				if !ok {
					continue
				}
				fn, _ := t["function"].(map[string]any)
				if fn == nil {
					continue
				}
				name, _ := fn["name"].(string)
				if matchesAnyPattern(name, patterns) {
					if id, ok := t["id"].(string); ok {
						ids[id] = true
					}
				}
			}
		}
	}
	return ids
}

// filterContentBlocks removes tool_use and tool_result blocks from an
// Anthropic content array. Returns the kept blocks and a set of removed
// tool_use IDs (which may extend the passed-in removedIDs).
func filterContentBlocks(content []any, patterns []string, removedIDs map[string]bool) ([]any, map[string]bool) {
	if removedIDs == nil {
		removedIDs = map[string]bool{}
	}

	// Collect tool_use IDs that match patterns in this content array.
	for _, block := range content {
		b, ok := block.(map[string]any)
		if !ok {
			continue
		}
		if b["type"] == "tool_use" {
			name, _ := b["name"].(string)
			if matchesAnyPattern(name, patterns) {
				if id, ok := b["id"].(string); ok {
					removedIDs[id] = true
				}
			}
		}
	}

	if len(removedIDs) == 0 {
		return content, removedIDs
	}

	kept, _ := filterArray(content, func(b map[string]any) bool {
		// Remove matched tool_use blocks.
		if b["type"] == "tool_use" {
			if id, _ := b["id"].(string); removedIDs[id] {
				return false
			}
		}
		// Remove tool_result blocks paired with removed tool_use.
		if b["type"] == "tool_result" {
			if id, _ := b["tool_use_id"].(string); removedIDs[id] {
				return false
			}
		}
		return true
	})
	return kept, removedIDs
}

// filterOpenAIToolCalls removes matching tool_calls entries from an
// OpenAI-format message. Returns true if any were removed.
func filterOpenAIToolCalls(msg map[string]any, patterns []string) bool {
	toolCalls, ok := msg["tool_calls"].([]any)
	if !ok {
		return false
	}

	kept, removed := filterArray(toolCalls, func(t map[string]any) bool {
		fn, _ := t["function"].(map[string]any)
		if fn != nil {
			name, _ := fn["name"].(string)
			if matchesAnyPattern(name, patterns) {
				return false
			}
		}
		return true
	})

	if !removed {
		return false
	}
	if len(kept) == 0 {
		delete(msg, "tool_calls")
	} else {
		msg["tool_calls"] = kept
	}
	return true
}

// filterArray creates a new array containing only items that pass the keep predicate.
// Returns the filtered array and a boolean indicating if any items were removed.
func filterArray(arr []any, keep func(item map[string]any) bool) ([]any, bool) {
	kept := make([]any, 0, len(arr))
	removed := false
	for _, item := range arr {
		m, ok := item.(map[string]any)
		if !ok {
			kept = append(kept, item)
			continue
		}
		if keep(m) {
			kept = append(kept, item)
		} else {
			removed = true
		}
	}
	return kept, removed
}
// filterToolDefs removes tool definitions whose names match patterns
// from the tools array. These are large JSON schema objects that add
// noise without meaningful content.
func filterToolDefs(tools []any, patterns []string) []any {
	kept, _ := filterArray(tools, func(t map[string]any) bool {
		name, _ := t["name"].(string)
		// OpenAI nests under function.name.
		if name == "" {
			if fn, ok := t["function"].(map[string]any); ok {
				name, _ = fn["name"].(string)
			}
		}
		return !matchesAnyPattern(name, patterns)
	})
	return kept
}

// matchesAnyPattern returns true if name contains any pattern
// (case-insensitive substring match).
func matchesAnyPattern(name string, patterns []string) bool {
	lower := strings.ToLower(name)
	for _, p := range patterns {
		if strings.Contains(lower, strings.ToLower(p)) {
			return true
		}
	}
	return false
}
