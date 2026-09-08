package grok

import (
	"bytes"
	"strings"
	"testing"

	"github.com/goccy/go-json"
)

func TestAnthropicRequestToChatPreservesToolsAndMultimodalContent(t *testing.T) {
	req := anthropicMessagesRequest{
		Model:     "grok-4.6",
		MaxTokens: 512,
		System: []interface{}{
			map[string]interface{}{"type": "text", "text": "Be precise."},
		},
		Messages: []anthropicMessage{
			{Role: "user", Content: []interface{}{
				map[string]interface{}{"type": "text", "text": "Inspect this"},
				map[string]interface{}{"type": "image", "source": map[string]interface{}{
					"type": "base64", "media_type": "image/png", "data": "YWJj",
				}},
			}},
			{Role: "assistant", Content: []interface{}{
				map[string]interface{}{"type": "tool_use", "id": "tool_1", "name": "Read", "input": map[string]interface{}{"path": "a.txt"}},
			}},
			{Role: "user", Content: []interface{}{
				map[string]interface{}{"type": "tool_result", "tool_use_id": "tool_1", "content": "done"},
				map[string]interface{}{"type": "text", "text": "Now summarize it."},
			}},
		},
		Tools:      []anthropicTool{{Name: "Read", Description: "Read a file", InputSchema: map[string]interface{}{"type": "object"}}},
		ToolChoice: map[string]interface{}{"type": "tool", "name": "Read"},
	}

	chat, err := anthropicRequestToChat(req)
	if err != nil {
		t.Fatalf("conversion error = %v", err)
	}
	if chat.MaxTokens == nil || *chat.MaxTokens != 512 || len(chat.Messages) != 5 {
		t.Fatalf("unexpected converted request: %#v", chat)
	}
	if chat.Messages[0].Role != "system" || chat.Messages[3].Role != "tool" || chat.Messages[3].ToolCallID != "tool_1" || chat.Messages[4].Role != "user" {
		t.Fatalf("message roles/history mismatch: %#v", chat.Messages)
	}
	parts, ok := chat.Messages[1].Content.([]interface{})
	if !ok || len(parts) != 2 || !strings.Contains(parts[1].(map[string]interface{})["image_url"].(map[string]interface{})["url"].(string), "data:image/png;base64,YWJj") {
		t.Fatalf("multimodal content mismatch: %#v", chat.Messages[1].Content)
	}
	if len(chat.Messages[2].ToolCalls) != 1 || len(chat.Tools) != 1 {
		t.Fatalf("tool conversion mismatch: %#v / %#v", chat.Messages[2].ToolCalls, chat.Tools)
	}
}

func TestAnthropicResponseFromChat(t *testing.T) {
	chat := map[string]interface{}{
		"id": "chat_1",
		"choices": []interface{}{map[string]interface{}{
			"finish_reason": "tool_calls",
			"message": map[string]interface{}{
				"content": "checking",
				"tool_calls": []interface{}{map[string]interface{}{
					"id": "call_1", "function": map[string]interface{}{"name": "Read", "arguments": `{"path":"a.txt"}`},
				}},
			},
		}},
		"usage": map[string]interface{}{
			"prompt_tokens": 11, "completion_tokens": 7,
			"prompt_tokens_details": map[string]interface{}{"cached_tokens": 3},
		},
	}
	got := anthropicResponseFromChat("grok-4.6", chat)
	if got["id"] != "chat_1" || got["stop_reason"] != "tool_use" {
		t.Fatalf("response envelope mismatch: %#v", got)
	}
	content := got["content"].([]interface{})
	if len(content) != 2 || content[1].(map[string]interface{})["type"] != "tool_use" {
		t.Fatalf("content mismatch: %#v", content)
	}
	usage := got["usage"].(map[string]interface{})
	if usage["input_tokens"] != 8 || usage["output_tokens"] != 7 || usage["cache_read_input_tokens"] != 3 {
		t.Fatalf("usage mismatch: %#v", usage)
	}
}

func TestTranslateOpenAIChatStreamToAnthropic(t *testing.T) {
	input := strings.Join([]string{
		`data: {"id":"chat_1","choices":[{"delta":{"role":"assistant","content":""},"finish_reason":null}]}`,
		``,
		`data: {"id":"chat_1","choices":[{"delta":{"reasoning_content":"think"},"finish_reason":null}]}`,
		``,
		`data: {"id":"chat_1","choices":[{"delta":{"content":"hello"},"finish_reason":null}]}`,
		``,
		`data: {"id":"chat_1","choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_1","function":{"name":"Read","arguments":"{\"path\":\"a.txt\"}"}}]},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":4,"completion_tokens":5}}`,
		``,
		`data: [DONE]`,
		``,
	}, "\n")
	var out bytes.Buffer
	if err := translateOpenAIChatStreamToAnthropic(&out, strings.NewReader(input), "grok-4.6"); err != nil {
		t.Fatalf("translate error = %v", err)
	}
	text := out.String()
	for _, expected := range []string{
		"event: message_start", `"type":"thinking_delta"`, `"text":"hello"`,
		`"type":"tool_use"`, `"type":"input_json_delta"`, `"stop_reason":"tool_use"`, "event: message_stop",
	} {
		if !strings.Contains(text, expected) {
			t.Fatalf("stream missing %q:\n%s", expected, text)
		}
	}
}

func TestConsolePayloadIncludesMaxOutputTokens(t *testing.T) {
	maxTokens := 321
	payload, err := (&Handler{}).consolePayload(ModelSpec{ConsoleModel: "grok-4.6"}, &ChatCompletionsRequest{
		Messages: []ChatMessage{{Role: "user", Content: "hello"}}, MaxTokens: &maxTokens,
	})
	if err != nil {
		t.Fatal(err)
	}
	if payload["max_output_tokens"] != 321 {
		t.Fatalf("max_output_tokens=%#v", payload["max_output_tokens"])
	}
}

func TestChatRequestUnmarshalPreservesMaxTokens(t *testing.T) {
	var req ChatCompletionsRequest
	if err := json.Unmarshal([]byte(`{"model":"grok-4.6","messages":[{"role":"user","content":"hi"}],"max_tokens":123}`), &req); err != nil {
		t.Fatal(err)
	}
	if req.MaxTokens == nil || *req.MaxTokens != 123 {
		t.Fatalf("max_tokens=%v", req.MaxTokens)
	}
}
