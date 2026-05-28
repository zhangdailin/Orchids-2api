package orchids

import (
	"strings"
	"testing"

	"github.com/goccy/go-json"

	"orchids-api/internal/config"
	"orchids-api/internal/prompt"
	"orchids-api/internal/upstream"
)

func TestBuildSSEAgentRequestCarriesProtocolContext(t *testing.T) {
	t.Parallel()

	client := &Client{
		config: &config.Config{
			ProjectID:        "proj_cfg",
			AgentMode:        "auto",
			Email:            "dev@example.com",
			UserID:           "user_cfg",
			ContextMaxTokens: 4096,
		},
	}

	req := upstream.UpstreamRequest{
		Prompt:        "say hello",
		ChatHistory:   []interface{}{map[string]string{"role": "assistant", "content": "previous"}},
		Model:         "claude-sonnet-4-6",
		Messages:      sampleProtocolMessages(),
		System:        sampleSystemItems(),
		ChatSessionID: "chat_fixed",
	}

	payload := client.buildSSEAgentRequest(req)

	if got := orchidsProjectID(client.config, req); got != "proj_cfg" {
		t.Fatalf("orchidsProjectID()=%q want proj_cfg", got)
	}
	if got := orchidsChatSessionID(req); got != "chat_fixed" {
		t.Fatalf("orchidsChatSessionID()=%q want chat_fixed", got)
	}
	if payload.MaxTokens != 4096 {
		t.Fatalf("MaxTokens=%d want 4096", payload.MaxTokens)
	}
	if got := payload.Config["thinkingMode"]; got != "enabled" {
		t.Fatalf("Config[thinkingMode]=%#v want enabled", got)
	}
	if got := payload.Config["system"]; got != "system rules" {
		t.Fatalf("Config[system]=%#v want system rules", got)
	}
	if len(payload.Messages) != 2 {
		t.Fatalf("len(Messages)=%d want 2", len(payload.Messages))
	}
	if got := extractRequestMessageText(t, payload.Messages[0]); got != "hello" {
		t.Fatalf("first message text=%q want hello", got)
	}
	if payload.System != "system rules" {
		t.Fatalf("System=%q want system rules", payload.System)
	}
	assertBareOrchidsRequestJSON(t, payload)
}

func TestBuildSSEAgentRequestOmitsPromptWhenProtocolContextIsPresent(t *testing.T) {
	t.Parallel()

	client := &Client{
		config: &config.Config{
			ProjectID: "proj_cfg",
			AgentMode: "auto",
		},
	}

	req := upstream.UpstreamRequest{
		Prompt:      sampleWrappedPrompt("hello"),
		ChatHistory: []interface{}{map[string]string{"role": "assistant", "content": "previous"}},
		Model:       "claude-sonnet-4-6",
		Messages:    sampleProtocolMessages(),
		System:      sampleSystemItems(),
	}

	payload := client.buildSSEAgentRequest(req)
	if got := payload.Config["system"]; got != "system rules" {
		t.Fatalf("Config[system]=%#v want system rules", got)
	}
	assertBareOrchidsRequestJSON(t, payload)
}

func TestBuildSSEAgentRequestKeepsPromptWithoutProtocolContext(t *testing.T) {
	t.Parallel()

	client := &Client{
		config: &config.Config{
			ProjectID: "proj_cfg",
			AgentMode: "auto",
		},
	}

	req := upstream.UpstreamRequest{
		Prompt: "hello from prompt only",
		Model:  "claude-sonnet-4-6",
	}

	payload := client.buildSSEAgentRequest(req)
	if len(payload.Messages) != 1 {
		t.Fatalf("len(Messages)=%d want 1", len(payload.Messages))
	}
	if got := payload.Config["system"]; got != "" {
		t.Fatalf("Config[system]=%#v want empty string", got)
	}
	if got := extractRequestMessageText(t, payload.Messages[0]); got != "hello from prompt only" {
		t.Fatalf("prompt fallback=%q want prompt-only fallback", got)
	}
	assertBareOrchidsRequestJSON(t, payload)
}

func TestBuildSSEAgentRequestNormalizesModelAliases(t *testing.T) {
	t.Parallel()

	client := &Client{
		config: &config.Config{
			ProjectID: "proj_cfg",
		},
	}

	req := upstream.UpstreamRequest{
		Prompt: "hello",
		Model:  "claude-opus-4.5",
	}

	payload := client.buildSSEAgentRequest(req)
	if payload.ModelName != "claude-opus-4-6" {
		t.Fatalf("ModelName=%q want claude-opus-4-6", payload.ModelName)
	}
	if payload.Model != "claude-opus-4-6" {
		t.Fatalf("Model=%#v want claude-opus-4-6", payload.Model)
	}
}

func TestBuildSSEAgentRequestPreservesUnknownModelInsteadOfFallingBack(t *testing.T) {
	t.Parallel()

	client := &Client{
		config: &config.Config{
			ProjectID: "proj_cfg",
		},
	}

	req := upstream.UpstreamRequest{
		Prompt: "hello",
		Model:  "gpt-5.4",
	}

	payload := client.buildSSEAgentRequest(req)
	if payload.ModelName != "gpt-5.4" {
		t.Fatalf("ModelName=%q want gpt-5.4", payload.ModelName)
	}
	if payload.Model != "gpt-5.4" {
		t.Fatalf("Model=%#v want gpt-5.4", payload.Model)
	}
}

func TestBuildWSRequestIncludesProtocolContext(t *testing.T) {
	t.Parallel()

	client := &Client{
		config: &config.Config{
			ProjectID:        "proj_ws",
			Email:            "dev@example.com",
			UserID:           "user_ws",
			ContextMaxTokens: 2048,
		},
	}

	req := upstream.UpstreamRequest{
		Prompt:        sampleWrappedPrompt("hello"),
		ChatHistory:   []interface{}{map[string]string{"role": "assistant", "content": "previous"}},
		Model:         "claude-sonnet-4-6",
		Messages:      sampleProtocolMessages(),
		System:        sampleSystemItems(),
		ChatSessionID: "chat_ws",
		NoThinking:    true,
	}

	payload, err := client.buildWSRequest(req)
	if err != nil {
		t.Fatalf("buildWSRequest() error = %v", err)
	}

	if got := orchidsProjectID(client.config, req); got != "proj_ws" {
		t.Fatalf("orchidsProjectID()=%q want proj_ws", got)
	}
	if got := payload.Config["thinkingMode"]; got != "disabled" {
		t.Fatalf("Config[thinkingMode]=%#v want disabled", got)
	}
	if got := payload.Config["system"]; got != "system rules" {
		t.Fatalf("Config[system]=%#v want system rules", got)
	}
	if payload.MaxTokens != 2048 {
		t.Fatalf("maxTokens=%d want 2048", payload.MaxTokens)
	}
	msgs := payload.Messages
	if len(msgs) != 2 || extractRequestMessageText(t, msgs[1]) != "ok" {
		t.Fatalf("messages=%#v want original protocol messages", msgs)
	}
	if payload.System != "" {
		t.Fatalf("system=%q want empty for bare WS request", payload.System)
	}
	if payload.Model != nil {
		t.Fatalf("model=%#v want nil for bare WS request", payload.Model)
	}
	if payload.ModelName != "claude-sonnet-4-6" {
		t.Fatalf("modelName=%q want claude-sonnet-4-6", payload.ModelName)
	}
	assertBareOrchidsWSRequestJSON(t, *payload)
}

func TestBuildWSRequestSkipsSyntheticToolResultsForProtocolMessages(t *testing.T) {
	t.Parallel()

	client := &Client{
		config: &config.Config{
			ProjectID: "proj_ws",
			Email:     "dev@example.com",
			UserID:    "user_ws",
		},
	}

	req := upstream.UpstreamRequest{
		Prompt:      sampleWrappedPrompt("check demo"),
		ChatHistory: []interface{}{map[string]string{"role": "assistant", "content": "previous"}},
		Model:       "claude-sonnet-4-6",
		Messages:    sampleProtocolMessagesWithToolResult(),
		System:      sampleSystemItems(),
	}

	payload, err := client.buildWSRequest(req)
	if err != nil {
		t.Fatalf("buildWSRequest() error = %v", err)
	}

	if len(payload.Messages) != 3 {
		t.Fatalf("len(Messages)=%d want 3", len(payload.Messages))
	}
	if got := len(payload.Messages[2].ToolResults); got != 1 {
		t.Fatalf("len(Messages[2].ToolResults)=%d want 1", got)
	}
	assertBareOrchidsWSRequestJSON(t, *payload)
}

func TestBuildWSRequestPreservesStructuredToolResultFields(t *testing.T) {
	t.Parallel()

	client := &Client{
		config: &config.Config{
			ProjectID: "proj_ws",
		},
	}

	req := upstream.UpstreamRequest{
		Model: "claude-sonnet-4-6",
		Messages: []prompt.Message{
			{
				Role: "user",
				Content: prompt.MessageContent{
					Blocks: []prompt.ContentBlock{
						{Type: "text", Text: "check demo"},
					},
				},
			},
			{
				Role: "user",
				Content: prompt.MessageContent{
					Blocks: []prompt.ContentBlock{
						{Type: "tool_result", ToolUseID: "tool_1", Name: "Read", Content: "demo result", IsError: true, HasInput: true},
					},
				},
			},
		},
	}

	payload, err := client.buildWSRequest(req)
	if err != nil {
		t.Fatalf("buildWSRequest() error = %v", err)
	}

	if len(payload.Messages) != 2 {
		t.Fatalf("len(Messages)=%d want 2", len(payload.Messages))
	}
	if len(payload.Messages[1].ToolResults) != 1 {
		t.Fatalf("len(Messages[1].ToolResults)=%d want 1", len(payload.Messages[1].ToolResults))
	}

	toolResult := payload.Messages[1].ToolResults[0]
	if toolResult.Name != "Read" {
		t.Fatalf("ToolResults[0].Name=%q want Read", toolResult.Name)
	}
	if toolResult.ToolUseID != "tool_1" {
		t.Fatalf("ToolResults[0].ToolUseID=%q want tool_1", toolResult.ToolUseID)
	}
	if toolResult.Content != "demo result" {
		t.Fatalf("ToolResults[0].Content=%#v want demo result", toolResult.Content)
	}
	if !toolResult.IsError {
		t.Fatal("ToolResults[0].IsError=false want true")
	}
	if !toolResult.HasInput {
		t.Fatal("ToolResults[0].HasInput=false want true")
	}
	assertBareOrchidsWSRequestJSON(t, *payload)
}

func sampleProtocolMessages() []prompt.Message {
	return []prompt.Message{
		{
			Role: "user",
			Content: prompt.MessageContent{
				Blocks: []prompt.ContentBlock{
					{Type: "text", Text: "hello"},
				},
			},
		},
		{
			Role: "assistant",
			Content: prompt.MessageContent{
				Blocks: []prompt.ContentBlock{
					{Type: "text", Text: "ok"},
				},
			},
		},
	}
}

func extractRequestMessageText(t *testing.T, msg OrchidsMessage) string {
	t.Helper()

	switch content := msg.Content.(type) {
	case string:
		return content
	case []interface{}:
		for _, raw := range content {
			block, ok := raw.(map[string]interface{})
			if !ok {
				continue
			}
			if text, _ := block["text"].(string); strings.TrimSpace(text) != "" {
				return text
			}
		}
	}

	t.Fatalf("unable to extract request message text from %#v", msg)
	return ""
}

func sampleSystemItems() []prompt.SystemItem {
	return []prompt.SystemItem{
		{Type: "text", Text: "system rules"},
	}
}

func sampleProtocolMessagesWithToolResult() []prompt.Message {
	return []prompt.Message{
		{
			Role: "user",
			Content: prompt.MessageContent{
				Blocks: []prompt.ContentBlock{
					{Type: "text", Text: "check demo"},
				},
			},
		},
		{
			Role: "assistant",
			Content: prompt.MessageContent{
				Blocks: []prompt.ContentBlock{
					{Type: "tool_use", ID: "tool_1", Name: "Read", Input: map[string]interface{}{"file_path": "/tmp/demo.txt"}},
				},
			},
		},
		{
			Role: "user",
			Content: prompt.MessageContent{
				Blocks: []prompt.ContentBlock{
					{Type: "tool_result", ToolUseID: "tool_1", Content: "demo result"},
				},
			},
		},
	}
}

func sampleWrappedPrompt(userText string) string {
	return "<env>\ndate: 2026-03-15\n</env>\n\n<rules>\n- concise\n</rules>\n\n<user>\n" + userText + "\n</user>\n"
}

func assertBareOrchidsRequestJSON(t *testing.T, req OrchidsRequest) {
	t.Helper()

	raw, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("json.Marshal() error = %v", err)
	}

	var top map[string]interface{}
	if err := json.Unmarshal(raw, &top); err != nil {
		t.Fatalf("json.Unmarshal() error = %v", err)
	}
	for _, forbidden := range []string{"projectId", "chatSessionId", "prompt", "tools", "type", "data"} {
		if _, exists := top[forbidden]; exists {
			t.Fatalf("request json=%s unexpectedly contains top-level key %s", string(raw), forbidden)
		}
	}
}

func assertBareOrchidsWSRequestJSON(t *testing.T, req OrchidsRequest) {
	t.Helper()
	assertBareOrchidsRequestJSON(t, req)

	raw, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("json.Marshal() error = %v", err)
	}

	var top map[string]interface{}
	if err := json.Unmarshal(raw, &top); err != nil {
		t.Fatalf("json.Unmarshal() error = %v", err)
	}
	for _, forbidden := range []string{"model", "system"} {
		if _, exists := top[forbidden]; exists {
			t.Fatalf("ws request json=%s unexpectedly contains top-level key %s", string(raw), forbidden)
		}
	}
}
