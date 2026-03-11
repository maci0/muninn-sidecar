// Package apiformat provides shared format detection, message extraction,
// and text truncation for LLM API request/response bodies. Used by both
// the inject and store packages to avoid duplicating format-specific logic.
package apiformat

import (
	"encoding/json"
	"regexp"
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
	Gemini          = "gemini"
	GeminiCloudCode = "gemini-cloudcode"
)

// DetectFormat identifies the API format of a request body.
// Returns Anthropic, OpenAI, Gemini, GeminiCloudCode, or "" for unknown.
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
	// OpenAI: has "messages" array (generic fallback).
	if _, ok := doc["messages"]; ok {
		return OpenAI
	}
	return ""
}

// ExtractUserMessage pulls the last user message text from a request body,
// handling Anthropic, OpenAI, Gemini, and Gemini Cloud Code formats.
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
	// Reverse the collected texts to restore chronological order.
	for i, j := 0, len(texts)-1; i < j; i, j = i+1, j-1 {
		texts[i], texts[j] = texts[j], texts[i]
	}
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
	// Reverse the collected texts to restore chronological order.
	for i, j := 0, len(texts)-1; i < j; i, j = i+1, j-1 {
		texts[i], texts[j] = texts[j], texts[i]
	}
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
	}
	return ""
}

// ExtractAssistantMessage pulls the assistant's response text from a
// response body, handling Anthropic, OpenAI, and Gemini formats.
func ExtractAssistantMessage(body []byte) string {
	if len(body) == 0 {
		return ""
	}
	doc, ok := parseDoc(body)
	if !ok {
		return ""
	}

	// Anthropic response: top-level content[] array with text blocks.
	if content, ok := GetArray(doc, "content"); ok {
		return anthropicBlocksText(content)
	}

	// OpenAI response: choices[0].message.content.
	if choices, ok := GetArray(doc, "choices"); ok {
		for _, choice := range choices {
			c, ok := choice.(map[string]any)
			if !ok {
				continue
			}
			msg, ok := GetMap(c, "message")
			if !ok {
				// Streaming delta format.
				msg, ok = GetMap(c, "delta")
				if !ok {
					continue
				}
			}
			if s, ok := GetString(msg, "content"); ok && s != "" {
				return s
			}
		}
	}

	// Gemini response: candidates[0].content.parts[].text.
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
			if len(texts) > 0 {
				return strings.Join(texts, "")
			}
		}
	}

	return ""
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
	if utf8.RuneCountInString(s) <= maxRunes {
		return s
	}
	runes := []rune(s)
	minCut := maxRunes - maxRunes/5
	for i := maxRunes - 1; i >= minCut; i-- {
		if strings.ContainsRune(breakChars, runes[i]) {
			return string(runes[:i]) + suffix
		}
	}
	return string(runes[:maxRunes]) + suffix
}

// parseDoc unmarshals a JSON body into a generic map.
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

// textExtractor is a function that extracts text from a message object.
type textExtractor func(msg map[string]any) string

// isAnthropicMessages checks if a messages array uses Anthropic format
// (content blocks with "type" field). Returns false if any message has
// role "system", since Anthropic uses a top-level system field rather
// than system-role messages — this prevents misdetecting OpenAI
// multimodal requests (which also use typed content arrays) as Anthropic.
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
