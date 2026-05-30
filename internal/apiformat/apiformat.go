// Package apiformat provides shared format detection, message extraction,
// and text truncation for LLM API request/response bodies. Used by both
// the inject and store packages to avoid duplicating format-specific logic.
package apiformat

import (
	"encoding/json"
	"regexp"
	"slices"
	"strings"
	"unicode/utf8"
)

// reSystemReminder matches <system-reminder>...</system-reminder> blocks
// (including content spanning multiple lines). Compiled once at init time.
var reSystemReminder = regexp.MustCompile(`(?s)<system-reminder>.*?</system-reminder>`)

// StripSystemReminders removes all <system-reminder>...</system-reminder>
// blocks from text and trims surrounding whitespace. Returns the cleaned
// string, which may be empty if the text was entirely system-reminder content.
func StripSystemReminders(text string) string {
	cleaned := reSystemReminder.ReplaceAllString(text, "")
	return strings.TrimSpace(cleaned)
}

// Format constants returned by DetectFormat.
const (
	Anthropic       = "anthropic"
	OpenAI          = "openai"
	OpenAIResponses = "openai-responses"
	Gemini          = "gemini"
	GeminiCloudCode = "gemini-cloudcode"
)

// Injection marker constants used by the inject package to wrap recalled
// context and by the proxy filter to detect and strip injected blocks before
// storage. Defined here so both packages share a single source of truth
// without creating a dependency from proxy to inject.
const (
	ContextPrefix       = "<retrieved-context source=\"muninn\">"
	ContextSuffix       = "</retrieved-context>"
	SessionContextOpen  = "<session-context source=\"muninn\">"
	SessionContextClose = "</session-context>"
	GlobalGuideOpen     = "<global-guide source=\"muninn\">"
	GlobalGuideClose    = "</global-guide>"
)

// DetectFormat identifies the API format of a request body.
// Returns Anthropic, OpenAI, OpenAIResponses, Gemini, GeminiCloudCode, or "" for unknown.
func DetectFormat(doc map[string]any) string {
	// Gemini: has "contents" array (check first since it's the most distinctive).
	if _, ok := doc["contents"]; ok {
		return Gemini
	}
	// Gemini Cloud Code: wraps standard payload in a "request" field.
	if req, ok := doc["request"].(map[string]any); ok {
		if _, ok := req["contents"]; ok {
			return GeminiCloudCode
		}
	}
	// Anthropic: has "system" field or content blocks with "type".
	if _, ok := doc["system"]; ok {
		return Anthropic
	}
	if messages, ok := doc["messages"].([]any); ok && len(messages) > 0 {
		if isAnthropicMessages(messages) {
			return Anthropic
		}
	}
	// OpenAI Responses API: has "input" field (string or array of items)
	// without "messages" or "contents". Must check before generic OpenAI.
	if _, ok := doc["input"]; ok {
		return OpenAIResponses
	}
	// OpenAI: has "messages" array (generic fallback).
	if _, ok := doc["messages"]; ok {
		return OpenAI
	}
	return ""
}

// ExtractUserMessage pulls the last user message text from a request body,
// handling Anthropic, OpenAI, OpenAIResponses, Gemini, and Gemini Cloud Code formats.
func ExtractUserMessage(body []byte) string {
	if len(body) == 0 {
		return ""
	}
	doc, ok := parseDoc(body)
	if !ok {
		return ""
	}
	return ExtractUserQuery(doc, DetectFormat(doc))
}

// ExtractRecentContext walks the messages/contents backward to collect the
// last 'turns' messages (user, assistant, or model) to provide a richer semantic context.
func ExtractRecentContext(doc map[string]any, format string, turns int) string {
	switch format {
	case Anthropic:
		return extractRecentFromMessages(doc, anthropicTextExtractor, turns)
	case OpenAI:
		return extractRecentFromMessages(doc, openaiTextExtractor, turns)
	case Gemini:
		return extractRecentGemini(doc, turns)
	case GeminiCloudCode:
		if req, ok := GetMap(doc, "request"); ok {
			return extractRecentGemini(req, turns)
		}
	case OpenAIResponses:
		return extractRecentOpenAIResponses(doc, turns)
	}
	return ""
}

func extractRecentFromMessages(doc map[string]any, extract textExtractor, turns int) string {
	messages, ok := GetArray(doc, "messages")
	if !ok {
		return ""
	}
	var texts []string
	count := 0
	for i := len(messages) - 1; i >= 0 && count < turns; i-- {
		m, ok := messages[i].(map[string]any)
		if !ok {
			continue
		}
		role, _ := GetString(m, "role")
		if role != "user" && role != "assistant" && role != "model" {
			continue
		}
		if text := extract(m); text != "" {
			texts = append(texts, role+": "+text)
			count++
		}
	}
	slices.Reverse(texts)
	return strings.Join(texts, "\n")
}

func extractRecentGemini(doc map[string]any, turns int) string {
	contents, ok := GetArray(doc, "contents")
	if !ok {
		return ""
	}
	var texts []string
	count := 0
	for i := len(contents) - 1; i >= 0 && count < turns; i-- {
		c, ok := contents[i].(map[string]any)
		if !ok {
			continue
		}
		role, _ := GetString(c, "role")
		if role != "user" && role != "model" && role != "assistant" {
			continue
		}
		parts, ok := GetArray(c, "parts")
		if !ok {
			continue
		}
		var partTexts []string
		for _, part := range parts {
			p, ok := part.(map[string]any)
			if !ok {
				continue
			}
			if t, ok := GetString(p, "text"); ok {
				partTexts = append(partTexts, t)
			}
		}
		if len(partTexts) > 0 {
			texts = append(texts, role+": "+strings.Join(partTexts, " "))
			count++
		}
	}
	slices.Reverse(texts)
	return strings.Join(texts, "\n")
}

// ExtractUserQuery walks the messages/contents backward to find the last
// user message and returns its text content for the given format.
func ExtractUserQuery(doc map[string]any, format string) string {
	switch format {
	case Anthropic:
		return extractFromMessages(doc, anthropicTextExtractor)
	case OpenAI:
		return extractFromMessages(doc, openaiTextExtractor)
	case Gemini:
		return extractGeminiUserQuery(doc)
	case GeminiCloudCode:
		if req, ok := doc["request"].(map[string]any); ok {
			return extractGeminiUserQuery(req)
		}
	case OpenAIResponses:
		return extractOpenAIResponsesUserQuery(doc)
	}
	return ""
}

// ExtractAssistantMessage pulls the assistant's response text and tool action
// summary from a response body, handling Anthropic, OpenAI, and Gemini formats.
// When the response contains tool_use blocks (common in coding agent sessions),
// a compact summary like "[Read /src/foo.go] [Edit /src/bar.go]" is included
// so the stored memory captures what the assistant was doing, not just text.
func ExtractAssistantMessage(body []byte) string {
	if len(body) == 0 {
		return ""
	}
	doc, ok := parseDoc(body)
	if !ok {
		return ""
	}

	// Anthropic response: top-level content[] array with text and tool_use blocks.
	if content, ok := GetArray(doc, "content"); ok {
		text := anthropicBlocksText(content)
		tools := toolUseSummary(content)
		return joinParts(text, tools)
	}

	// OpenAI response: choices[0].message.content + tool_calls.
	if choices, ok := GetArray(doc, "choices"); ok {
		for _, choice := range choices {
			c, ok := choice.(map[string]any)
			if !ok {
				continue
			}
			msg, ok := GetMap(c, "message")
			if !ok {
				msg, ok = GetMap(c, "delta")
				if !ok {
					continue
				}
			}
			text, _ := GetString(msg, "content")
			tools := openaiToolCallSummary(msg)
			if result := joinParts(text, tools); result != "" {
				return result
			}
		}
	}

	// OpenAI Responses API: output[] array with message and function_call items.
	if output, ok := GetArray(doc, "output"); ok {
		var texts, tools []string
		for _, item := range output {
			o, ok := item.(map[string]any)
			if !ok {
				continue
			}
			itemType, _ := GetString(o, "type")
			if itemType == "message" {
				if content, ok := GetArray(o, "content"); ok {
					for _, part := range content {
						p, ok := part.(map[string]any)
						if !ok {
							continue
						}
						if t, ok := GetString(p, "text"); ok {
							texts = append(texts, t)
						}
					}
				}
			}
			if itemType == "function_call" {
				if name, ok := GetString(o, "name"); ok {
					tools = append(tools, "["+name+"]")
				}
			}
		}
		if result := joinParts(strings.Join(texts, ""), strings.Join(tools, " ")); result != "" {
			return result
		}
	}

	// Gemini response: candidates[0].content.parts[].text and functionCall.
	if candidates, ok := GetArray(doc, "candidates"); ok {
		for _, cand := range candidates {
			c, ok := cand.(map[string]any)
			if !ok {
				continue
			}
			content, ok := GetMap(c, "content")
			if !ok {
				continue
			}
			parts, ok := GetArray(content, "parts")
			if !ok {
				continue
			}
			var texts, tools []string
			for _, part := range parts {
				p, ok := part.(map[string]any)
				if !ok {
					continue
				}
				if t, ok := GetString(p, "text"); ok {
					texts = append(texts, t)
				}
				if fc, ok := GetMap(p, "functionCall"); ok {
					if name, ok := GetString(fc, "name"); ok {
						tools = append(tools, "["+name+"]")
					}
				}
			}
			if result := joinParts(strings.Join(texts, ""), strings.Join(tools, " ")); result != "" {
				return result
			}
		}
	}

	return ""
}

// toolUseSummary returns a compact summary of Anthropic tool_use blocks.
// Each tool is formatted as "[ToolName key_arg]" to capture what the
// assistant was doing without storing full tool inputs.
func toolUseSummary(blocks []any) string {
	var parts []string
	for _, block := range blocks {
		b, ok := block.(map[string]any)
		if !ok {
			continue
		}
		if b["type"] != "tool_use" {
			continue
		}
		name, _ := b["name"].(string)
		if name == "" {
			continue
		}
		summary := "[" + name
		if input, ok := b["input"].(map[string]any); ok {
			if arg := toolInputKey(input); arg != "" {
				summary += " " + TruncateText(arg, 100)
			}
		}
		summary += "]"
		parts = append(parts, summary)
	}
	return strings.Join(parts, " ")
}

// openaiToolCallSummary returns a compact summary of OpenAI tool_calls.
func openaiToolCallSummary(msg map[string]any) string {
	toolCalls, ok := msg["tool_calls"].([]any)
	if !ok {
		return ""
	}
	var parts []string
	for _, tc := range toolCalls {
		t, ok := tc.(map[string]any)
		if !ok {
			continue
		}
		fn, ok := t["function"].(map[string]any)
		if !ok {
			continue
		}
		name, _ := fn["name"].(string)
		if name != "" {
			parts = append(parts, "["+name+"]")
		}
	}
	return strings.Join(parts, " ")
}

// toolInputKey extracts the most identifying argument from a tool input
// for use in the compact summary (file paths, commands, patterns, etc.).
func toolInputKey(input map[string]any) string {
	for _, key := range []string{"file_path", "command", "pattern", "query", "url", "path"} {
		if v, ok := input[key].(string); ok && v != "" {
			return v
		}
	}
	return ""
}

// joinParts joins text and tool summary with a newline, skipping empty parts.
func joinParts(text, tools string) string {
	text = strings.TrimSpace(text)
	tools = strings.TrimSpace(tools)
	switch {
	case text != "" && tools != "":
		return text + "\n" + tools
	case text != "":
		return text
	default:
		return tools
	}
}

// TruncateText truncates a string to maxRunes runes, breaking at a word
// boundary when possible. Appends "…" (U+2026 ellipsis) when truncated.
func TruncateText(s string, maxRunes int) string {
	return truncateAt(s, maxRunes, "\u2026", " \n")
}

// TruncateQuery truncates a query string to maxRunes runes, breaking at a
// word boundary when possible. Does not append an ellipsis.
func TruncateQuery(s string, maxRunes int) string {
	return truncateAt(s, maxRunes, "", " ")
}

// truncateAt is the shared truncation implementation. It breaks at the last
// occurrence of any character in breakChars within the final 20% of the string,
// appending suffix when truncated.
func truncateAt(s string, maxRunes int, suffix string, breakChars string) string {
	// A non-positive limit truncates to nothing; guarding here also prevents a
	// negative-length slice allocation below.
	if maxRunes <= 0 {
		return ""
	}
	// Fast path: byte count is an upper bound on rune count. If the byte
	// length fits, the rune count must too — no need to scan the string.
	if len(s) <= maxRunes {
		return s
	}

	minCut := maxRunes - maxRunes/5

	// Single forward pass: record byte offsets at rune positions [minCut, maxRunes-1]
	// to support the word-boundary search without converting the full string to []rune.
	// byteOffs[k] = byte offset where rune (minCut+k) starts.
	byteOffs := make([]int, maxRunes-minCut)
	bi := 0
	for ri := 0; ri < maxRunes; ri++ {
		if bi >= len(s) {
			return s // fewer than maxRunes runes — no truncation needed
		}
		if ri >= minCut {
			byteOffs[ri-minCut] = bi
		}
		_, sz := utf8.DecodeRuneInString(s[bi:])
		bi += sz
	}
	if bi >= len(s) {
		return s // exactly maxRunes runes — no truncation needed
	}

	// Search backward from maxRunes-1 to minCut for a word-boundary character.
	for i := maxRunes - 1; i >= minCut; i-- {
		runeStart := byteOffs[i-minCut]
		r, _ := utf8.DecodeRuneInString(s[runeStart:])
		if strings.ContainsRune(breakChars, r) {
			return s[:runeStart] + suffix
		}
	}
	return s[:bi] + suffix
}

func parseDoc(body []byte) (map[string]any, bool) {
	var doc map[string]any
	if err := json.Unmarshal(body, &doc); err != nil {
		return nil, false
	}
	return doc, true
}

// GetMap is a helper for safely extracting a nested map from a generic JSON object.
func GetMap(m map[string]any, key string) (map[string]any, bool) {
	val, ok := m[key].(map[string]any)
	return val, ok
}

// GetArray is a helper for safely extracting an array from a generic JSON object.
func GetArray(m map[string]any, key string) ([]any, bool) {
	val, ok := m[key].([]any)
	return val, ok
}

// GetString is a helper for safely extracting a string from a generic JSON object.
func GetString(m map[string]any, key string) (string, bool) {
	val, ok := m[key].(string)
	return val, ok
}

// extractFromMessages walks a "messages" array backward for the last user
// message and extracts text using the provided extractor.
func extractFromMessages(doc map[string]any, extract textExtractor) string {
	messages, ok := GetArray(doc, "messages")
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
		if text := extract(m); text != "" {
			return text
		}
	}
	return ""
}

// extractGeminiUserQuery extracts text from a Gemini format message.
func extractGeminiUserQuery(doc map[string]any) string {
	contents, ok := GetArray(doc, "contents")
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
		parts, ok := GetArray(c, "parts")
		if !ok {
			continue
		}
		var texts []string
		for _, part := range parts {
			p, ok := part.(map[string]any)
			if !ok {
				continue
			}
			if t, ok := GetString(p, "text"); ok {
				texts = append(texts, t)
			}
		}
		return strings.Join(texts, " ")
	}
	return ""
}

// extractOpenAIResponsesUserQuery extracts text from an OpenAI Responses API
// request. The input field can be a plain string or an array of structured items.
func extractOpenAIResponsesUserQuery(doc map[string]any) string {
	input, ok := doc["input"]
	if !ok {
		return ""
	}
	// String input: return directly.
	if s, ok := input.(string); ok {
		return s
	}
	// Array input: walk backward for last user message.
	items, ok := input.([]any)
	if !ok {
		return ""
	}
	for i := len(items) - 1; i >= 0; i-- {
		item, ok := items[i].(map[string]any)
		if !ok {
			continue
		}
		if item["role"] != "user" {
			continue
		}
		return openaiResponsesItemText(item)
	}
	return ""
}

// openaiResponsesItemText extracts text from an OpenAI Responses API item.
// Content can be a string or an array of typed parts (input_text, output_text).
func openaiResponsesItemText(item map[string]any) string {
	if s, ok := item["content"].(string); ok {
		return s
	}
	if parts, ok := item["content"].([]any); ok {
		var texts []string
		for _, part := range parts {
			p, ok := part.(map[string]any)
			if !ok {
				continue
			}
			if t, ok := GetString(p, "text"); ok {
				texts = append(texts, t)
			}
		}
		return strings.Join(texts, "")
	}
	return ""
}

// extractRecentOpenAIResponses extracts recent conversation context from an
// OpenAI Responses API request for semantic recall queries.
func extractRecentOpenAIResponses(doc map[string]any, turns int) string {
	input, ok := doc["input"]
	if !ok {
		return ""
	}
	if s, ok := input.(string); ok {
		return "user: " + s
	}
	items, ok := input.([]any)
	if !ok {
		return ""
	}
	var texts []string
	count := 0
	for i := len(items) - 1; i >= 0 && count < turns; i-- {
		item, ok := items[i].(map[string]any)
		if !ok {
			continue
		}
		role, _ := GetString(item, "role")
		if role != "user" && role != "assistant" {
			continue
		}
		text := openaiResponsesItemText(item)
		if text != "" {
			texts = append(texts, role+": "+text)
			count++
		}
	}
	slices.Reverse(texts)
	return strings.Join(texts, "\n")
}

// ExtractSSEDelta extracts a text delta from a pre-parsed SSE event map.
// Supports Anthropic, OpenAI (chat + responses), and Gemini delta formats.
func ExtractSSEDelta(doc map[string]any) string {
	// Anthropic: {"type":"content_block_delta","delta":{"type":"text_delta","text":"chunk"}}
	if doc["type"] == "content_block_delta" {
		if delta, ok := GetMap(doc, "delta"); ok {
			if text, ok := GetString(delta, "text"); ok {
				return text
			}
		}
		return ""
	}

	// OpenAI responses API: {"type":"response.output_text.delta","delta":"chunk"}
	if doc["type"] == "response.output_text.delta" {
		if text, ok := GetString(doc, "delta"); ok {
			return text
		}
		return ""
	}

	// OpenAI chat: {"choices":[{"delta":{"content":"chunk"}}]}
	if choices, ok := GetArray(doc, "choices"); ok && len(choices) > 0 {
		if choice, ok := choices[0].(map[string]any); ok {
			if delta, ok := GetMap(choice, "delta"); ok {
				if text, ok := GetString(delta, "content"); ok {
					return text
				}
			}
		}
		return ""
	}

	// Gemini: {"candidates":[{"content":{"parts":[{"text":"chunk"}]}}]}
	if candidates, ok := GetArray(doc, "candidates"); ok && len(candidates) > 0 {
		if cand, ok := candidates[0].(map[string]any); ok {
			if content, ok := GetMap(cand, "content"); ok {
				if parts, ok := GetArray(content, "parts"); ok && len(parts) > 0 {
					if part, ok := parts[0].(map[string]any); ok {
						if text, ok := GetString(part, "text"); ok {
							return text
						}
					}
				}
			}
		}
		return ""
	}

	return ""
}

// ExtractSSEToolName extracts a tool name from a pre-parsed SSE event map.
// Supports Anthropic, OpenAI (chat + responses), and Gemini formats.
//
// Supported formats:
//   - Anthropic: content_block_start with tool_use content_block
//   - OpenAI chat: choices[].delta.tool_calls[].function.name
//   - OpenAI responses: response.output_item.added with function_call item
//   - Gemini: candidates[].content.parts[].functionCall.name
func ExtractSSEToolName(doc map[string]any) string {
	// Anthropic: content_block_start with tool_use.
	if doc["type"] == "content_block_start" {
		if cb, ok := GetMap(doc, "content_block"); ok {
			if cb["type"] == "tool_use" {
				name, _ := cb["name"].(string)
				return name
			}
		}
		return ""
	}

	// OpenAI responses API: response.output_item.added with function_call.
	if doc["type"] == "response.output_item.added" {
		if item, ok := GetMap(doc, "item"); ok {
			if item["type"] == "function_call" {
				name, _ := item["name"].(string)
				return name
			}
		}
		return ""
	}

	// OpenAI chat: delta with tool_calls containing function.name.
	// Only the first chunk per tool call carries the name; subsequent
	// chunks contain argument deltas and are skipped.
	if choices, ok := GetArray(doc, "choices"); ok && len(choices) > 0 {
		if choice, ok := choices[0].(map[string]any); ok {
			if delta, ok := GetMap(choice, "delta"); ok {
				if toolCalls, ok := GetArray(delta, "tool_calls"); ok && len(toolCalls) > 0 {
					if tc, ok := toolCalls[0].(map[string]any); ok {
						if fn, ok := GetMap(tc, "function"); ok {
							name, _ := GetString(fn, "name")
							return name
						}
					}
				}
			}
		}
		return ""
	}

	// Gemini: candidates with functionCall parts.
	if candidates, ok := GetArray(doc, "candidates"); ok && len(candidates) > 0 {
		if cand, ok := candidates[0].(map[string]any); ok {
			if content, ok := GetMap(cand, "content"); ok {
				if parts, ok := GetArray(content, "parts"); ok {
					for _, part := range parts {
						p, ok := part.(map[string]any)
						if !ok {
							continue
						}
						if fc, ok := GetMap(p, "functionCall"); ok {
							if name, ok := GetString(fc, "name"); ok && name != "" {
								return name
							}
						}
					}
				}
			}
		}
		return ""
	}

	return ""
}

// textExtractor is a function that extracts text from a message object.
type textExtractor func(msg map[string]any) string

// isAnthropicMessages returns true if the messages array uses Anthropic format:
// at least one content block has a "type" field, no message has role "system"
// (Anthropic uses a top-level system field instead), and no content block uses
// the OpenAI-specific "image_url" type. Without these checks, OpenAI multimodal
// requests (which also use typed content arrays) would be misidentified as
// Anthropic, causing injected memories to be silently lost.
func isAnthropicMessages(messages []any) bool {
	hasTypedContent := false
	for _, msg := range messages {
		m, ok := msg.(map[string]any)
		if !ok {
			continue
		}
		if m["role"] == "system" {
			return false
		}
		if blocks, ok := m["content"].([]any); ok {
			for _, block := range blocks {
				if b, ok := block.(map[string]any); ok {
					if _, hasType := b["type"]; hasType {
						hasTypedContent = true
					}
					// image_url is OpenAI-specific; Anthropic uses "image".
					if b["type"] == "image_url" {
						return false
					}
				}
			}
		}
	}
	return hasTypedContent
}

// anthropicTextExtractor extracts text from an Anthropic message (string
// or content blocks array).
func anthropicTextExtractor(msg map[string]any) string {
	if s, ok := msg["content"].(string); ok {
		return s
	}
	if blocks, ok := msg["content"].([]any); ok {
		return anthropicBlocksText(blocks)
	}
	return ""
}

// anthropicBlocksText concatenates text from Anthropic content blocks.
func anthropicBlocksText(blocks []any) string {
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
	return strings.Join(parts, "")
}

// openaiTextExtractor extracts text from an OpenAI message.
func openaiTextExtractor(msg map[string]any) string {
	if s, ok := msg["content"].(string); ok {
		return s
	}
	// Array content (multimodal).
	if parts, ok := msg["content"].([]any); ok {
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
		return strings.Join(texts, "")
	}
	return ""
}
