package proxy

import (
	"encoding/json"
	"testing"
)

func TestFilterAnthropicToolUse(t *testing.T) {
	body := json.RawMessage(`{
		"model": "claude-3-opus",
		"messages": [
			{"role": "user", "content": "hello"},
			{"role": "assistant", "content": [
				{"type": "text", "text": "Let me check my memory."},
				{"type": "tool_use", "id": "tu1", "name": "mcp__muninn__muninn_recall", "input": {"context": ["hello"]}}
			]},
			{"role": "user", "content": [
				{"type": "tool_result", "tool_use_id": "tu1", "content": "found some memories"}
			]},
			{"role": "assistant", "content": [
				{"type": "text", "text": "Based on context, here is my answer."}
			]},
			{"role": "user", "content": "thanks"}
		]
	}`)

	result := filterMCPTools(body, []string{"muninn"})

	var doc map[string]any
	if err := json.Unmarshal(result, &doc); err != nil {
		t.Fatal(err)
	}

	messages := doc["messages"].([]any)

	// Original had 5 messages. The tool_result message should be gone,
	// and the assistant message should only have the text block.
	// Expected: user("hello"), assistant("Let me check..."), assistant("Based on..."), user("thanks")
	if len(messages) != 4 {
		t.Fatalf("expected 4 messages after filtering, got %d", len(messages))
	}

	// Check that the assistant message lost its tool_use block.
	assistantMsg := messages[1].(map[string]any)
	content := assistantMsg["content"].([]any)
	if len(content) != 1 {
		t.Fatalf("expected 1 content block in assistant message, got %d", len(content))
	}
	block := content[0].(map[string]any)
	if block["type"] != "text" {
		t.Fatalf("expected text block, got %v", block["type"])
	}
}

func TestFilterOpenAIToolCalls(t *testing.T) {
	body := json.RawMessage(`{
		"model": "gpt-4",
		"messages": [
			{"role": "user", "content": "hello"},
			{"role": "assistant", "content": null, "tool_calls": [
				{"id": "tc1", "type": "function", "function": {"name": "muninn_recall", "arguments": "{}"}}
			]},
			{"role": "tool", "tool_call_id": "tc1", "content": "memory results"},
			{"role": "assistant", "content": "Here is my answer."},
			{"role": "user", "content": "thanks"}
		]
	}`)

	result := filterMCPTools(body, []string{"muninn"})

	var doc map[string]any
	if err := json.Unmarshal(result, &doc); err != nil {
		t.Fatal(err)
	}

	messages := doc["messages"].([]any)

	// The tool_calls assistant message (empty after stripping) and the
	// tool role message should both be removed.
	// Expected: user("hello"), assistant("Here is my answer."), user("thanks")
	if len(messages) != 3 {
		t.Fatalf("expected 3 messages after filtering, got %d", len(messages))
	}

	// Verify remaining message content.
	if messages[0].(map[string]any)["content"] != "hello" {
		t.Fatal("first message should be user hello")
	}
	if messages[1].(map[string]any)["content"] != "Here is my answer." {
		t.Fatal("second message should be assistant answer")
	}
}

func TestFilterPreservesNonMuninnTools(t *testing.T) {
	body := json.RawMessage(`{
		"model": "claude-3-opus",
		"messages": [
			{"role": "assistant", "content": [
				{"type": "tool_use", "id": "tu1", "name": "Read", "input": {"path": "/tmp/foo"}},
				{"type": "tool_use", "id": "tu2", "name": "mcp__muninn__muninn_remember", "input": {"content": "test"}}
			]},
			{"role": "user", "content": [
				{"type": "tool_result", "tool_use_id": "tu1", "content": "file contents"},
				{"type": "tool_result", "tool_use_id": "tu2", "content": "stored"}
			]}
		]
	}`)

	result := filterMCPTools(body, []string{"muninn"})

	var doc map[string]any
	json.Unmarshal(result, &doc)
	messages := doc["messages"].([]any)

	if len(messages) != 2 {
		t.Fatalf("expected 2 messages, got %d", len(messages))
	}

	// Assistant message should keep the Read tool_use.
	assistantContent := messages[0].(map[string]any)["content"].([]any)
	if len(assistantContent) != 1 {
		t.Fatalf("expected 1 content block (Read), got %d", len(assistantContent))
	}
	if assistantContent[0].(map[string]any)["name"] != "Read" {
		t.Fatal("expected Read tool_use to be preserved")
	}

	// User message should keep the Read tool_result.
	userContent := messages[1].(map[string]any)["content"].([]any)
	if len(userContent) != 1 {
		t.Fatalf("expected 1 content block (Read result), got %d", len(userContent))
	}
}

func TestFilterNoMatchReturnsOriginal(t *testing.T) {
	body := json.RawMessage(`{
		"model": "claude-3-opus",
		"messages": [
			{"role": "user", "content": "hello"},
			{"role": "assistant", "content": [
				{"type": "text", "text": "world"}
			]}
		]
	}`)

	result := filterMCPTools(body, []string{"muninn"})

	// Should be byte-identical since nothing matched.
	if string(result) != string(body) {
		t.Fatal("expected unchanged body when no tools match")
	}
}

func TestFilterInvalidJSON(t *testing.T) {
	body := json.RawMessage(`not json at all`)
	result := filterMCPTools(body, []string{"muninn"})

	if string(result) != string(body) {
		t.Fatal("expected invalid JSON to pass through unchanged")
	}
}

func TestFilterEmptyPatterns(t *testing.T) {
	body := json.RawMessage(`{"messages":[]}`)
	result := filterMCPTools(body, nil)

	if string(result) != string(body) {
		t.Fatal("expected nil patterns to skip filtering")
	}

	result = filterMCPTools(body, []string{})
	if string(result) != string(body) {
		t.Fatal("expected empty patterns to skip filtering")
	}
}

func TestFilterToolDefinitions(t *testing.T) {
	body := json.RawMessage(`{
		"model": "claude-3-opus",
		"tools": [
			{"name": "Read", "description": "Read a file", "input_schema": {}},
			{"name": "mcp__muninn__muninn_recall", "description": "Search memory", "input_schema": {"type": "object"}},
			{"name": "mcp__muninn__muninn_remember", "description": "Store memory", "input_schema": {"type": "object"}},
			{"name": "Write", "description": "Write a file", "input_schema": {}}
		],
		"messages": [{"role": "user", "content": "hello"}]
	}`)

	result := filterMCPTools(body, []string{"muninn"})

	var doc map[string]any
	json.Unmarshal(result, &doc)

	tools := doc["tools"].([]any)
	if len(tools) != 2 {
		t.Fatalf("expected 2 tools after filtering (Read, Write), got %d", len(tools))
	}

	names := make([]string, len(tools))
	for i, tool := range tools {
		names[i] = tool.(map[string]any)["name"].(string)
	}
	if names[0] != "Read" || names[1] != "Write" {
		t.Fatalf("expected [Read, Write], got %v", names)
	}
}

func TestFilterAnthropicResponse(t *testing.T) {
	// Anthropic response body with tool_use in content.
	body := json.RawMessage(`{
		"id": "msg_123",
		"type": "message",
		"role": "assistant",
		"content": [
			{"type": "text", "text": "Let me check."},
			{"type": "tool_use", "id": "tu1", "name": "mcp__muninn__muninn_recall", "input": {"context": ["test"]}}
		],
		"usage": {"input_tokens": 100, "output_tokens": 50}
	}`)

	result := filterMCPTools(body, []string{"muninn"})

	var doc map[string]any
	json.Unmarshal(result, &doc)

	content := doc["content"].([]any)
	if len(content) != 1 {
		t.Fatalf("expected 1 content block after filtering, got %d", len(content))
	}
	if content[0].(map[string]any)["type"] != "text" {
		t.Fatal("expected remaining block to be text")
	}

	// Verify usage is preserved.
	if doc["usage"] == nil {
		t.Fatal("usage should be preserved")
	}
}

func TestFilterCaseInsensitive(t *testing.T) {
	body := json.RawMessage(`{
		"messages": [
			{"role": "assistant", "content": [
				{"type": "tool_use", "id": "tu1", "name": "MCP__MUNINN__Muninn_Recall", "input": {}}
			]},
			{"role": "user", "content": [
				{"type": "tool_result", "tool_use_id": "tu1", "content": "result"}
			]}
		]
	}`)

	result := filterMCPTools(body, []string{"muninn"})

	var doc map[string]any
	json.Unmarshal(result, &doc)

	messages := doc["messages"].([]any)
	if len(messages) != 0 {
		t.Fatalf("expected 0 messages after filtering case-insensitive match, got %d", len(messages))
	}
}
