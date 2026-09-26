package qoder

import (
	"strings"
	"testing"

	"github.com/goccy/go-json"

	"orchids-api/internal/prompt"
	"orchids-api/internal/upstream"
)

func decodeChatBodyForTest(t *testing.T, req upstream.UpstreamRequest) map[string]interface{} {
	t.Helper()
	encoded, err := buildChatBody(req, modelEntry{Key: "qmodel_latest", Source: "system"}, "session-id", "request-id")
	if err != nil {
		t.Fatalf("buildChatBody() error = %v", err)
	}
	raw, err := decodeBodyForTest(encoded)
	if err != nil {
		t.Fatalf("DecodeBody() error = %v", err)
	}
	var body map[string]interface{}
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatalf("unmarshal body: %v", err)
	}
	return body
}

func TestBuildChatBodyDropsDanglingAndDuplicateToolResults(t *testing.T) {
	t.Parallel()
	body := decodeChatBodyForTest(t, upstream.UpstreamRequest{Messages: []prompt.Message{
		{Role: "assistant", Content: prompt.MessageContent{Blocks: []prompt.ContentBlock{{
			Type: "tool_use", ID: "call-1", Name: "read_file", Input: map[string]interface{}{},
		}}}},
		{Role: "user", Content: prompt.MessageContent{Blocks: []prompt.ContentBlock{
			{Type: "tool_result", ToolUseID: "call-1", Content: "ok"},
			{Type: "tool_result", ToolUseID: "call-1", Content: "duplicate"},
			{Type: "tool_result", ToolUseID: "missing", Content: "dangling"},
		}}},
	}})
	messages, _ := body["messages"].([]interface{})
	toolMessages := 0
	for _, raw := range messages {
		message, _ := raw.(map[string]interface{})
		if message["role"] == "tool" {
			toolMessages++
			if message["tool_call_id"] != "call-1" || message["content"] != "ok" {
				t.Fatalf("unexpected tool message: %#v", message)
			}
		}
	}
	if toolMessages != 1 {
		t.Fatalf("tool messages=%d want 1: %#v", toolMessages, messages)
	}
}

func sampleQoderTool() map[string]interface{} {
	return map[string]interface{}{
		"name":        "read_file",
		"description": "Read a file",
		"input_schema": map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"path": map[string]interface{}{"type": "string"},
			},
		},
	}
}

func TestBuildChatBodyPlacesToolControlsAtTopLevel(t *testing.T) {
	t.Parallel()
	body := decodeChatBodyForTest(t, upstream.UpstreamRequest{
		Tools: []interface{}{sampleQoderTool()},
	})
	if got := body["tool_choice"]; got != "auto" {
		t.Fatalf("tool_choice = %#v, want top-level auto", got)
	}
	parameters, _ := body["parameters"].(map[string]interface{})
	if _, exists := parameters["tool_choice"]; exists {
		t.Fatalf("parameters.tool_choice must be absent: %#v", parameters)
	}
	tools, _ := body["tools"].([]interface{})
	if len(tools) != 1 {
		t.Fatalf("tools len = %d, want 1", len(tools))
	}
}

func TestBuildChatBodyNormalizesAnthropicToolControls(t *testing.T) {
	t.Parallel()
	body := decodeChatBodyForTest(t, upstream.UpstreamRequest{
		Tools: []interface{}{sampleQoderTool()},
		ToolChoice: map[string]interface{}{
			"type":                      "tool",
			"name":                      "read_file",
			"disable_parallel_tool_use": true,
		},
	})
	choice, _ := body["tool_choice"].(map[string]interface{})
	function, _ := choice["function"].(map[string]interface{})
	if choice["type"] != "function" || function["name"] != "read_file" {
		t.Fatalf("tool_choice = %#v", body["tool_choice"])
	}
	if parallel, ok := body["parallel_tool_calls"].(bool); !ok || parallel {
		t.Fatalf("parallel_tool_calls = %#v, want false", body["parallel_tool_calls"])
	}
}

func TestConsumeStreamConvertsTextToolFallback(t *testing.T) {
	t.Parallel()
	body := envelope(`{"choices":[{"delta":{"content":"Tool ca"}}]}`) +
		envelope(`{"choices":[{"delta":{"content":"lls: [{\"id\":\"call_1\",\"type\":\"function\",\"function\":{\"name\":\"read_file\",\"arguments\":\"{\\\"path\\\":\\\"README.md\\\"}\"}}]"},"finish_reason":"stop"}]}`) +
		"event:finish\ndata: {}\n\n"

	var events []upstream.SSEMessage
	result, err := consumeStreamWithTools(strings.NewReader(body), true, func(message upstream.SSEMessage) {
		events = append(events, message)
	})
	if err != nil {
		t.Fatalf("consumeStreamWithTools() error = %v", err)
	}
	if got := result.FinishReason(); got != "tool_use" {
		t.Fatalf("FinishReason() = %q, want tool_use", got)
	}
	if len(events) != 1 || events[0].Type != "model.tool-call" {
		t.Fatalf("events = %#v, want one tool call", events)
	}
	if got := events[0].Event["toolName"]; got != "read_file" {
		t.Fatalf("toolName = %#v", got)
	}
	if got := events[0].Event["input"]; got != `{"path":"README.md"}` {
		t.Fatalf("input = %#v", got)
	}
}

func TestConsumeStreamDoesNotParseTextFallbackWithoutTools(t *testing.T) {
	t.Parallel()
	body := envelope(`{"choices":[{"delta":{"content":"Tool calls: []"},"finish_reason":"stop"}]}`) +
		"event:finish\ndata: {}\n\n"
	var events []upstream.SSEMessage
	_, err := consumeStreamWithTools(strings.NewReader(body), false, func(message upstream.SSEMessage) {
		events = append(events, message)
	})
	if err != nil {
		t.Fatalf("consumeStreamWithTools() error = %v", err)
	}
	if len(events) != 1 || events[0].Type != "model.text-delta" {
		t.Fatalf("events = %#v, want ordinary text", events)
	}
}

func TestConsumeStreamInvalidTextToolFallbackRemainsText(t *testing.T) {
	t.Parallel()
	want := `Tool calls: [{not-json}]`
	body := envelope(`{"choices":[{"delta":{"content":"Tool calls: [{not-json}]"},"finish_reason":"stop"}]}`) +
		"event:finish\ndata: {}\n\n"
	var text strings.Builder
	result, err := consumeStreamWithTools(strings.NewReader(body), true, func(message upstream.SSEMessage) {
		if message.Type == "model.text-delta" {
			text.WriteString(message.Event["delta"].(string))
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	if text.String() != want || result.ToolCallCount != 0 {
		t.Fatalf("text=%q tool calls=%d", text.String(), result.ToolCallCount)
	}
}

func TestConsumeStreamConvertsParallelTextToolFallback(t *testing.T) {
	t.Parallel()
	body := envelope(`{"choices":[{"delta":{"content":"Tool calls: [{\"id\":\"a\",\"function\":{\"name\":\"read\",\"arguments\":{\"path\":\"a\"}}},{\"id\":\"b\",\"function\":{\"name\":\"read\",\"arguments\":\"{\\\"path\\\":\\\"b\\\"}\"}}]"},"finish_reason":"stop"}]}`) +
		"event:finish\ndata: {}\n\n"
	var calls []upstream.SSEMessage
	result, err := consumeStreamWithTools(strings.NewReader(body), true, func(message upstream.SSEMessage) {
		if message.Type == "model.tool-call" {
			calls = append(calls, message)
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.ToolCallCount != 2 || len(calls) != 2 {
		t.Fatalf("calls=%#v count=%d", calls, result.ToolCallCount)
	}
}

func TestConsumeStreamNativeToolSuppressesTextDuplicate(t *testing.T) {
	t.Parallel()
	body := envelope(`{"choices":[{"delta":{"content":"Tool calls: "}}]}`) +
		envelope(`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call-1","function":{"name":"read","arguments":"{}"}}]},"finish_reason":"tool_calls"}]}`) +
		"event:finish\ndata: {}\n\n"
	var events []upstream.SSEMessage
	result, err := consumeStreamWithTools(strings.NewReader(body), true, func(message upstream.SSEMessage) {
		events = append(events, message)
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.ToolCallCount != 1 || len(events) != 1 || events[0].Type != "model.tool-call" {
		t.Fatalf("events=%#v result=%#v", events, result)
	}
}

func TestConsumeStreamLongSplitWhitespacePrefixStaysLinearAndFlushes(t *testing.T) {
	const chunks = 4096
	var body strings.Builder
	for range chunks {
		body.WriteString(envelope(`{"choices":[{"delta":{"content":" "}}]}`))
	}
	body.WriteString(envelope(`{"choices":[{"delta":{"content":"ordinary text"},"finish_reason":"stop"}]}`))
	body.WriteString("event:finish\ndata: {}\n\n")
	var text strings.Builder
	result, err := consumeStreamWithTools(strings.NewReader(body.String()), true, func(message upstream.SSEMessage) {
		if message.Type == "model.text-delta" {
			text.WriteString(message.Event["delta"].(string))
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	if text.Len() != chunks+len("ordinary text") || result.ToolCallCount != 0 {
		t.Fatalf("text bytes=%d calls=%d", text.Len(), result.ToolCallCount)
	}
}

func TestConsumeStreamOversizedTextFallbackDegradesToText(t *testing.T) {
	large := "Tool calls: " + strings.Repeat("x", maxTextToolFallbackBytes+1)
	inner, err := json.Marshal(map[string]interface{}{"choices": []interface{}{map[string]interface{}{
		"delta": map[string]interface{}{"content": large}, "finish_reason": "stop",
	}}})
	if err != nil {
		t.Fatal(err)
	}
	body := envelope(string(inner)) + "event:finish\ndata: {}\n\n"
	textBytes := 0
	result, err := consumeStreamWithTools(strings.NewReader(body), true, func(message upstream.SSEMessage) {
		if message.Type == "model.text-delta" {
			textBytes += len(message.Event["delta"].(string))
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	if textBytes != len(large) || result.ToolCallCount != 0 {
		t.Fatalf("text bytes=%d want=%d, tool calls=%d", textBytes, len(large), result.ToolCallCount)
	}
}

// TestBuildChatBodyCarriesThinkingSwitch pins the reasoning wire contract: a
// reasoning-capable model row leaves thinking off by default, explicit client
// effort enables it, and "none" turns it off. A non-reasoning model must
// not grow a default thinking flag or effort.
func TestBuildChatBodyCarriesThinkingSwitch(t *testing.T) {
	reasoning := modelEntry{Key: "qwen-plus", Source: "system", IsReasoning: true}
	plain := modelEntry{Key: "qwen-turbo", Source: "system"}

	// Default: even a reasoning-capable model has no reasoning controls.
	body := decodeChatBodyForTestWithModel(t, reasoning, upstream.UpstreamRequest{})
	params, _ := body["parameters"].(map[string]interface{})
	if _, present := params["enable_thinking"]; present {
		t.Fatalf("default reasoning model must not enable thinking: %#v", params)
	}
	if _, present := params["reasoning_effort"]; present {
		t.Fatalf("default reasoning model must not set effort: %#v", params)
	}
	if body["model_config"].(map[string]interface{})["is_reasoning"] != false {
		t.Fatalf("default model_config unexpectedly enables reasoning: %#v", body)
	}
	body = decodeChatBodyForTestWithModel(t, plain, upstream.UpstreamRequest{})
	params, _ = body["parameters"].(map[string]interface{})
	if _, present := params["enable_thinking"]; present {
		t.Fatalf("plain model must not carry enable_thinking, got %#v", params)
	}
	if _, present := params["reasoning_effort"]; present {
		t.Fatalf("plain model must not carry default reasoning_effort, got %#v", params)
	}

	// An explicit effort turns thinking on, including a mixed-case value.
	body = decodeChatBodyForTestWithModel(t, reasoning, upstream.UpstreamRequest{ReasoningEffort: "HIGH"})
	params, _ = body["parameters"].(map[string]interface{})
	if params["enable_thinking"] != true || params["reasoning_effort"] != "high" || body["model_config"].(map[string]interface{})["is_reasoning"] != true {
		t.Fatalf("parameters = %#v, want explicit thinking on with effort=high", params)
	}

	// qfmodel is catalogued as reasoning-capable, but reference behavior does
	// not switch it into thinking mode without an explicit client request.
	body = decodeChatBodyForTestWithModel(t, modelEntry{Key: "qfmodel", IsReasoning: true}, upstream.UpstreamRequest{})
	params, _ = body["parameters"].(map[string]interface{})
	if body["model_config"].(map[string]interface{})["is_reasoning"] != false {
		t.Fatalf("qfmodel default enabled reasoning: %#v", body)
	}
	if _, present := params["reasoning_effort"]; present {
		t.Fatalf("qfmodel default carried reasoning_effort: %#v", params)
	}

	// "none" disables thinking explicitly.
	body = decodeChatBodyForTestWithModel(t, reasoning, upstream.UpstreamRequest{ReasoningEffort: "none"})
	params, _ = body["parameters"].(map[string]interface{})
	if params["enable_thinking"] != false {
		t.Fatalf("parameters = %#v, want enable_thinking=false", params)
	}
	if _, present := params["reasoning_effort"]; present {
		t.Fatalf("none must not forward an effort level, got %#v", params["reasoning_effort"])
	}
}

func decodeChatBodyForTestWithModel(t *testing.T, model modelEntry, req upstream.UpstreamRequest) map[string]interface{} {
	t.Helper()
	encoded, err := buildChatBody(req, model, "session-id", "request-id")
	if err != nil {
		t.Fatalf("buildChatBody() error = %v", err)
	}
	raw, err := decodeBodyForTest(encoded)
	if err != nil {
		t.Fatalf("DecodeBody() error = %v", err)
	}
	var body map[string]interface{}
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatalf("unmarshal body: %v", err)
	}
	return body
}
