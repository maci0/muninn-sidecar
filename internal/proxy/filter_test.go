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

	result := cleanResponse(body, []string{"muninn"})

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

	// messages[0]: user("hello") — unchanged.
	if messages[0].(map[string]any)["content"] != "hello" {
		t.Fatal("first message should be user hello")
	}

	// messages[1]: assistant with text + tool_use → tool_use stripped, text kept.
	assistantMsg := messages[1].(map[string]any)
	content := assistantMsg["content"].([]any)
	if len(content) != 1 {
		t.Fatalf("expected 1 content block in assistant message, got %d", len(content))
	}
	block := content[0].(map[string]any)
	if block["type"] != "text" {
		t.Fatalf("expected text block, got %v", block["type"])
	}
	if block["text"] != "Let me check my memory." {
		t.Fatalf("expected original assistant text, got %v", block["text"])
	}

	// messages[2]: assistant("Based on context, here is my answer.") — unchanged.
	assistant2 := messages[2].(map[string]any)
	content2 := assistant2["content"].([]any)
	if len(content2) != 1 {
		t.Fatalf("expected 1 content block in second assistant message, got %d", len(content2))
	}
	if content2[0].(map[string]any)["type"] != "text" {
		t.Fatal("second assistant message should be text")
	}

	// messages[3]: user("thanks") — unchanged.
	if messages[3].(map[string]any)["content"] != "thanks" {
		t.Fatal("last message should be user thanks")
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

	result := cleanResponse(body, []string{"muninn"})

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
	if messages[2].(map[string]any)["content"] != "thanks" {
		t.Fatal("third message should be user thanks")
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

	result := cleanResponse(body, []string{"muninn"})

	var doc map[string]any
	if err := json.Unmarshal(result, &doc); err != nil {
		t.Fatal(err)
	}
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

	result := cleanResponse(body, []string{"muninn"})

	// Should be byte-identical since nothing matched.
	if string(result) != string(body) {
		t.Fatal("expected unchanged body when no tools match")
	}
}

func TestFilterInvalidJSON(t *testing.T) {
	body := json.RawMessage(`not json at all`)
	result := cleanResponse(body, []string{"muninn"})

	if string(result) != string(body) {
		t.Fatal("expected invalid JSON to pass through unchanged")
	}
}

func TestFilterEmptyPatterns(t *testing.T) {
	body := json.RawMessage(`{"messages":[]}`)
	result := cleanResponse(body, nil)

	if string(result) != string(body) {
		t.Fatal("expected nil patterns to skip filtering")
	}

	result = cleanResponse(body, []string{})
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

	result := cleanResponse(body, []string{"muninn"})

	var doc map[string]any
	if err := json.Unmarshal(result, &doc); err != nil {
		t.Fatal(err)
	}

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

	result := cleanResponse(body, []string{"muninn"})

	var doc map[string]any
	if err := json.Unmarshal(result, &doc); err != nil {
		t.Fatal(err)
	}

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

// --- stripInjectedContext tests ---

func TestStripInjectedContextAnthropicArray(t *testing.T) {
	body := json.RawMessage(`{
		"model":"claude-3",
		"system":[
			{"type":"text","text":"You are helpful"},
			{"type":"text","text":"<retrieved-context source=\"muninn\">\n[test] (relevance: 0.90)\nsome memory\n</retrieved-context>","cache_control":{"type":"ephemeral"}}
		],
		"messages":[{"role":"user","content":"hello"}]
	}`)

	result := cleanRequest(body, nil)

	var doc map[string]any
	if err := json.Unmarshal(result, &doc); err != nil {
		t.Fatal(err)
	}

	sys := doc["system"].([]any)
	if len(sys) != 1 {
		t.Fatalf("expected 1 system block after stripping, got %d", len(sys))
	}
	if sys[0].(map[string]any)["text"] != "You are helpful" {
		t.Error("should keep original system text")
	}
}

func TestStripInjectedContextAnthropicString(t *testing.T) {
	body := json.RawMessage(`{
		"model":"claude-3",
		"system":"<retrieved-context source=\"muninn\">\nsome memory\n</retrieved-context>",
		"messages":[{"role":"user","content":"hello"}]
	}`)

	result := cleanRequest(body, nil)

	var doc map[string]any
	if err := json.Unmarshal(result, &doc); err != nil {
		t.Fatal(err)
	}

	if _, exists := doc["system"]; exists {
		t.Error("system field should be removed when it's entirely injected context")
	}
}

func TestStripInjectedContextOpenAI(t *testing.T) {
	body := json.RawMessage(`{
		"model":"gpt-4",
		"messages":[
			{"role":"system","content":"You are helpful"},
			{"role":"system","content":"<retrieved-context source=\"muninn\">\nsome memory\n</retrieved-context>"},
			{"role":"user","content":"hello"}
		]
	}`)

	result := cleanRequest(body, nil)

	var doc map[string]any
	if err := json.Unmarshal(result, &doc); err != nil {
		t.Fatal(err)
	}

	msgs := doc["messages"].([]any)
	if len(msgs) != 2 {
		t.Fatalf("expected 2 messages after stripping, got %d", len(msgs))
	}
	if msgs[0].(map[string]any)["content"] != "You are helpful" {
		t.Error("should keep original system message")
	}
	if msgs[1].(map[string]any)["content"] != "hello" {
		t.Error("should keep user message")
	}
}

func TestStripInjectedContextGemini(t *testing.T) {
	body := json.RawMessage(`{
		"contents":[{"role":"user","parts":[{"text":"hello"}]}],
		"systemInstruction":{
			"parts":[
				{"text":"existing instruction"},
				{"text":"<retrieved-context source=\"muninn\">\nsome memory\n</retrieved-context>"}
			]
		}
	}`)

	result := cleanRequest(body, nil)

	var doc map[string]any
	if err := json.Unmarshal(result, &doc); err != nil {
		t.Fatal(err)
	}

	si := doc["systemInstruction"].(map[string]any)
	parts := si["parts"].([]any)
	if len(parts) != 1 {
		t.Fatalf("expected 1 part after stripping, got %d", len(parts))
	}
	if parts[0].(map[string]any)["text"] != "existing instruction" {
		t.Error("should keep original instruction")
	}
}

func TestStripInjectedContextNoMarkers(t *testing.T) {
	body := json.RawMessage(`{
		"model":"gpt-4",
		"messages":[
			{"role":"system","content":"You are helpful"},
			{"role":"user","content":"hello"}
		]
	}`)

	result := cleanRequest(body, nil)

	if string(result) != string(body) {
		t.Error("should return original body when no markers found")
	}
}

func TestStripInjectedContextInvalidJSON(t *testing.T) {
	body := json.RawMessage(`not json`)
	result := cleanRequest(body, nil)
	if string(result) != `"not json"` {
		t.Errorf("expected sanitized string, got %s", string(result))
	}
}

func TestFilterOpenAIResponsesOutput(t *testing.T) {
	body := json.RawMessage(`{
		"output": [
			{"type": "message", "content": [{"type": "output_text", "text": "Answer"}]},
			{"type": "function_call", "name": "mcp__muninn__muninn_recall", "call_id": "call_1", "arguments": "{}"},
			{"type": "function_call", "name": "read_file", "call_id": "call_2", "arguments": "{}"}
		]
	}`)

	result := cleanResponse(body, []string{"muninn"})

	var doc map[string]any
	if err := json.Unmarshal(result, &doc); err != nil {
		t.Fatal(err)
	}

	output := doc["output"].([]any)
	if len(output) != 2 {
		t.Fatalf("expected 2 output items (message + read_file), got %d", len(output))
	}
	if output[0].(map[string]any)["type"] != "message" {
		t.Error("first item should be message")
	}
	if output[1].(map[string]any)["name"] != "read_file" {
		t.Error("second item should be read_file")
	}
}

func TestFilterOpenAIResponsesInput(t *testing.T) {
	body := json.RawMessage(`{
		"input": [
			{"type": "message", "role": "user", "content": "hello"},
			{"type": "function_call", "name": "mcp__muninn__muninn_recall", "call_id": "call_1", "arguments": "{}"},
			{"type": "function_call_output", "call_id": "call_1", "output": "memories"},
			{"type": "function_call", "name": "read_file", "call_id": "call_2", "arguments": "{}"},
			{"type": "function_call_output", "call_id": "call_2", "output": "file contents"},
			{"type": "message", "role": "user", "content": "thanks"}
		]
	}`)

	result := cleanRequest(body, []string{"muninn"})

	var doc map[string]any
	if err := json.Unmarshal(result, &doc); err != nil {
		t.Fatal(err)
	}

	input := doc["input"].([]any)
	// Should keep: user "hello", read_file call, read_file output, user "thanks"
	if len(input) != 4 {
		t.Fatalf("expected 4 input items, got %d", len(input))
	}
	if input[0].(map[string]any)["content"] != "hello" {
		t.Error("first should be user hello")
	}
	if input[1].(map[string]any)["name"] != "read_file" {
		t.Error("second should be read_file")
	}
}

func TestStripInjectedContextOpenAIResponses(t *testing.T) {
	body := json.RawMessage(`{
		"model":"gpt-4o",
		"input":"hello",
		"instructions":"Be helpful\n\n<retrieved-context source=\"muninn\">\nsome memory\n</retrieved-context>"
	}`)

	result := cleanRequest(body, nil)

	var doc map[string]any
	if err := json.Unmarshal(result, &doc); err != nil {
		t.Fatal(err)
	}

	if doc["instructions"] != "Be helpful" {
		t.Errorf("original instructions should be preserved, got %q", doc["instructions"])
	}
	if doc["input"] != "hello" {
		t.Error("input should be preserved")
	}
}

func TestStripInjectedContextOpenAIResponsesOnlyBlock(t *testing.T) {
	// When there were no original instructions, the whole field should be removed.
	body := json.RawMessage(`{
		"model":"gpt-4o",
		"instructions":"<retrieved-context source=\"muninn\">\nsome memory\n</retrieved-context>"
	}`)

	result := cleanRequest(body, nil)

	var doc map[string]any
	if err := json.Unmarshal(result, &doc); err != nil {
		t.Fatal(err)
	}

	if _, exists := doc["instructions"]; exists {
		t.Error("instructions should be removed when it contains only injected context")
	}
}

func TestFilterSyntheticToolUseWithoutID(t *testing.T) {
	// Synthetic tool_use blocks from streaming captures lack an "id" field.
	// filterContentBlocks must still remove them by name pattern match.
	body := json.RawMessage(`{
		"content": [
			{"type": "text", "text": "Checking memory..."},
			{"type": "tool_use", "name": "mcp__muninn__muninn_recall", "input": {}},
			{"type": "tool_use", "name": "Read", "input": {}}
		]
	}`)

	result := cleanResponse(body, []string{"muninn"})

	var doc map[string]any
	if err := json.Unmarshal(result, &doc); err != nil {
		t.Fatal(err)
	}

	content := doc["content"].([]any)
	if len(content) != 2 {
		t.Fatalf("expected 2 content blocks after filtering (text + Read), got %d", len(content))
	}
	if content[0].(map[string]any)["type"] != "text" {
		t.Fatal("expected first block to be text")
	}
	if content[1].(map[string]any)["name"] != "Read" {
		t.Fatal("expected second block to be Read tool_use")
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

	result := cleanResponse(body, []string{"muninn"})

	var doc map[string]any
	if err := json.Unmarshal(result, &doc); err != nil {
		t.Fatal(err)
	}

	messages := doc["messages"].([]any)
	if len(messages) != 0 {
		t.Fatalf("expected 0 messages after filtering case-insensitive match, got %d", len(messages))
	}
}
