package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"orchids-api/internal/client"
	"orchids-api/internal/config"
	"orchids-api/internal/debug"
	"orchids-api/internal/prompt"
	"orchids-api/internal/tiktoken"
)

type fakeClient struct {
	events []client.SSEMessage
	err    error
}

func (f *fakeClient) SendRequest(ctx context.Context, prompt string, chatHistory []interface{}, model string, onMessage func(client.SSEMessage), logger *debug.Logger) error {
	for _, evt := range f.events {
		onMessage(evt)
	}
	return f.err
}

func TestHandleMessages_NonStreamReturnsAnthropicMessage(t *testing.T) {
	reqPayload := ClaudeRequest{
		Model: "gpt-test",
		Messages: []prompt.Message{
			{
				Role:    "user",
				Content: prompt.MessageContent{Text: "Hi"},
			},
		},
		Stream: false,
		Tools:  []interface{}{},
	}

	body, err := json.Marshal(reqPayload)
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}

	h := &Handler{
		config: &config.Config{DebugEnabled: false},
		client: &fakeClient{
			events: []client.SSEMessage{
				{Type: "model", Event: map[string]interface{}{"type": "text-start"}},
				{Type: "model", Event: map[string]interface{}{"type": "text-delta", "delta": "Hello"}},
				{Type: "model", Event: map[string]interface{}{"type": "text-end"}},
				{Type: "model", Event: map[string]interface{}{"type": "finish", "finishReason": "stop"}},
			},
		},
	}

	req := httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader(body))
	rec := httptest.NewRecorder()

	h.HandleMessages(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("unexpected status: %d", rec.Code)
	}

	if contentType := rec.Header().Get("Content-Type"); !strings.Contains(contentType, "application/json") {
		t.Fatalf("expected application/json content-type, got %q", contentType)
	}

	var resp struct {
		ID           string `json:"id"`
		Type         string `json:"type"`
		Role         string `json:"role"`
		Content      []struct {
			Type string `json:"type"`
			Text string `json:"text,omitempty"`
		} `json:"content"`
		Model        string  `json:"model"`
		StopReason   string  `json:"stop_reason"`
		StopSequence *string `json:"stop_sequence"`
		Usage        struct {
			InputTokens  int `json:"input_tokens"`
			OutputTokens int `json:"output_tokens"`
		} `json:"usage"`
	}

	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}

	if !strings.HasPrefix(resp.ID, "msg_") {
		t.Fatalf("unexpected id: %q", resp.ID)
	}

	if resp.Type != "message" {
		t.Fatalf("unexpected type: %q", resp.Type)
	}

	if resp.Role != "assistant" {
		t.Fatalf("unexpected role: %q", resp.Role)
	}

	if resp.Model != reqPayload.Model {
		t.Fatalf("unexpected model: %q", resp.Model)
	}

	if len(resp.Content) != 1 {
		t.Fatalf("expected 1 content block, got %d", len(resp.Content))
	}

	if resp.Content[0].Type != "text" {
		t.Fatalf("unexpected content type: %q", resp.Content[0].Type)
	}

	if resp.Content[0].Text != "Hello" {
		t.Fatalf("unexpected content: %q", resp.Content[0].Text)
	}

	if resp.StopReason != "end_turn" {
		t.Fatalf("unexpected stop_reason: %q", resp.StopReason)
	}

	if resp.StopSequence != nil {
		t.Fatalf("expected stop_sequence nil, got %v", *resp.StopSequence)
	}

	builtPrompt := prompt.BuildPromptV2(prompt.ClaudeAPIRequest{
		Model:    reqPayload.Model,
		Messages: reqPayload.Messages,
		System:   reqPayload.System,
		Tools:    reqPayload.Tools,
		Stream:   reqPayload.Stream,
	})

	expectedInputTokens := tiktoken.EstimateTextTokens(builtPrompt)
	expectedOutputTokens := tiktoken.EstimateTextTokens("Hello")

	if resp.Usage.InputTokens != expectedInputTokens {
		t.Fatalf("unexpected input_tokens: %d", resp.Usage.InputTokens)
	}
	if resp.Usage.OutputTokens != expectedOutputTokens {
		t.Fatalf("unexpected output_tokens: %d", resp.Usage.OutputTokens)
	}
}

func TestHandleMessages_NonStreamIncludesToolUse(t *testing.T) {
	reqPayload := ClaudeRequest{
		Model: "gpt-test",
		Messages: []prompt.Message{
			{
				Role:    "user",
				Content: prompt.MessageContent{Text: "Use tool"},
			},
		},
		Stream: false,
		Tools:  []interface{}{},
	}

	body, err := json.Marshal(reqPayload)
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}

	h := &Handler{
		config: &config.Config{DebugEnabled: false},
		client: &fakeClient{
			events: []client.SSEMessage{
				{Type: "model", Event: map[string]interface{}{"type": "tool-call", "toolCallId": "tool_1", "toolName": "sum", "input": "{\"a\":\"1\"}"}},
				{Type: "model", Event: map[string]interface{}{"type": "finish", "finishReason": "tool-calls"}},
			},
		},
	}

	req := httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader(body))
	rec := httptest.NewRecorder()

	h.HandleMessages(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("unexpected status: %d", rec.Code)
	}

	var resp struct {
		Content []struct {
			Type  string                 `json:"type"`
			ID    string                 `json:"id,omitempty"`
			Name  string                 `json:"name,omitempty"`
			Input map[string]interface{} `json:"input,omitempty"`
		} `json:"content"`
		StopReason string `json:"stop_reason"`
		Usage      struct {
			InputTokens  int `json:"input_tokens"`
			OutputTokens int `json:"output_tokens"`
		} `json:"usage"`
	}

	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}

	if resp.StopReason != "tool_use" {
		t.Fatalf("unexpected stop_reason: %q", resp.StopReason)
	}

	if len(resp.Content) != 1 {
		t.Fatalf("expected 1 content block, got %d", len(resp.Content))
	}

	block := resp.Content[0]
	if block.Type != "tool_use" {
		t.Fatalf("unexpected content type: %q", block.Type)
	}
	if block.ID != "tool_1" {
		t.Fatalf("unexpected tool_use id: %q", block.ID)
	}
	if block.Name != "sum" {
		t.Fatalf("unexpected tool name: %q", block.Name)
	}
	value, ok := block.Input["a"]
	if !ok {
		t.Fatalf("missing tool input field")
	}
	if value != float64(1) {
		t.Fatalf("unexpected tool input value: %v", value)
	}

	builtPrompt := prompt.BuildPromptV2(prompt.ClaudeAPIRequest{
		Model:    reqPayload.Model,
		Messages: reqPayload.Messages,
		System:   reqPayload.System,
		Tools:    reqPayload.Tools,
		Stream:   reqPayload.Stream,
	})

	expectedInputTokens := tiktoken.EstimateTextTokens(builtPrompt)
	expectedOutputTokens := tiktoken.EstimateTextTokens("sum") + tiktoken.EstimateTextTokens("{\"a\":\"1\"}")

	if resp.Usage.InputTokens != expectedInputTokens {
		t.Fatalf("unexpected input_tokens: %d", resp.Usage.InputTokens)
	}
	if resp.Usage.OutputTokens != expectedOutputTokens {
		t.Fatalf("unexpected output_tokens: %d", resp.Usage.OutputTokens)
	}
}
