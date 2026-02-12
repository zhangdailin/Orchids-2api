package orchids

import (
	"strings"
	"sync"
	"testing"

	"orchids-api/internal/config"
	"orchids-api/internal/prompt"
	"orchids-api/internal/upstream"
)

func TestHandleOrchidsMessageCreditsExhausted(t *testing.T) {
	t.Parallel()

	c := &Client{}
	state := &requestState{}
	var got []upstream.SSEMessage
	var fsWG sync.WaitGroup

	msg := map[string]interface{}{
		"type": EventCreditsExhausted,
		"data": map[string]interface{}{
			"message": "You have run out of credits. Please upgrade your plan to continue.",
		},
	}

	shouldBreak := c.handleOrchidsMessage(
		msg,
		[]byte(`{"type":"coding_agent.credits_exhausted"}`),
		state,
		func(m upstream.SSEMessage) { got = append(got, m) },
		nil,
		nil,
		&fsWG,
		"",
	)

	if !shouldBreak {
		t.Fatal("expected credits exhausted to break message loop")
	}
	if state.errorMsg == "" {
		t.Fatal("expected credits exhausted to set request error")
	}
	if len(got) != 1 {
		t.Fatalf("expected one forwarded error message, got %d", len(got))
	}
	if got[0].Type != "error" {
		t.Fatalf("expected error message type, got %q", got[0].Type)
	}
	if got[0].Event["code"] != "credits_exhausted" {
		t.Fatalf("expected credits_exhausted code, got %#v", got[0].Event["code"])
	}
}

func TestConvertChatHistoryAIClient_DoesNotTruncateUserText(t *testing.T) {
	t.Parallel()

	longText := strings.Repeat("a", maxHistoryContentLen+500)
	messages := []prompt.Message{
		{
			Role: "user",
			Content: prompt.MessageContent{
				Text: longText,
			},
		},
	}

	history, _ := convertChatHistoryAIClient(messages)
	if len(history) != 1 {
		t.Fatalf("expected one history item, got %d", len(history))
	}
	if history[0]["content"] != longText {
		t.Fatalf("expected user text to be preserved without truncation")
	}
}

func TestBuildWSRequestAIClient_PrefersProvidedChatHistory(t *testing.T) {
	t.Parallel()

	c := &Client{
		config: &config.Config{
			ContextMaxTokens: 12000,
			Email:            "tester@example.com",
			UserID:           "u1",
		},
	}

	req := upstream.UpstreamRequest{
		Model:  "claude-sonnet-4-5",
		Prompt: "hello",
		ChatHistory: []interface{}{
			map[string]string{"role": "user", "content": "from-provided-history"},
		},
		Messages: []prompt.Message{
			{Role: "user", Content: prompt.MessageContent{Text: "from-messages"}},
		},
	}

	wsReq, err := c.buildWSRequestAIClient(req)
	if err != nil {
		t.Fatalf("buildWSRequestAIClient returned error: %v", err)
	}

	rawHistory, ok := wsReq.Data["chatHistory"]
	if !ok {
		t.Fatalf("chatHistory missing from payload")
	}
	history, ok := rawHistory.([]map[string]string)
	if !ok {
		t.Fatalf("chatHistory has unexpected type %T", rawHistory)
	}
	if len(history) != 1 {
		t.Fatalf("chatHistory len = %d, want 1", len(history))
	}
	if history[0]["content"] != "from-provided-history" {
		t.Fatalf("chatHistory content = %q, want provided history", history[0]["content"])
	}
}

func TestBuildWSRequestAIClient_PreservesProvidedChatHistoryWithinBudget(t *testing.T) {
	t.Parallel()

	c := &Client{
		config: &config.Config{
			ContextMaxTokens: 12000,
			Email:            "tester@example.com",
			UserID:           "u1",
		},
	}

	longText := strings.Repeat("x", 5000)
	req := upstream.UpstreamRequest{
		Model:  "claude-sonnet-4-5",
		Prompt: "hello",
		ChatHistory: []interface{}{
			map[string]interface{}{
				"role":    "user",
				"content": longText,
			},
		},
	}

	wsReq, err := c.buildWSRequestAIClient(req)
	if err != nil {
		t.Fatalf("buildWSRequestAIClient returned error: %v", err)
	}

	rawHistory, ok := wsReq.Data["chatHistory"]
	if !ok {
		t.Fatalf("chatHistory missing from payload")
	}
	history, ok := rawHistory.([]map[string]string)
	if !ok {
		t.Fatalf("chatHistory has unexpected type %T", rawHistory)
	}
	if len(history) != 1 {
		t.Fatalf("chatHistory len = %d, want 1", len(history))
	}
	got := history[0]["content"]
	if got != longText {
		t.Fatalf("expected provided history to be preserved within budget")
	}
}
