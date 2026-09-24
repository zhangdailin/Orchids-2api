package handler

import (
	"bytes"
	"context"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"

	"orchids-api/internal/adapter"
	"orchids-api/internal/config"
	"orchids-api/internal/loadbalancer"
	"orchids-api/internal/prompt"
	"orchids-api/internal/store"
)

func TestPuterReasoningReplayRestoresDroppedThinkingByToolCallID(t *testing.T) {
	mini := miniredis.RunT(t)
	accountStore, err := store.New(store.Options{
		RedisAddr:               mini.Addr(),
		RedisPrefix:             "handler-puter-replay-test:",
		CredentialEncryptionKey: bytes.Repeat([]byte{9}, 32),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = accountStore.Close() }()

	h := NewWithLoadBalancer(&config.Config{}, loadbalancer.NewWithCacheTTL(accountStore, time.Second))
	const reasoning = "inspect the service logs before invoking the diagnostic tool"
	if err := h.savePuterReasoningForTool(context.Background(), "deepseek-v4-flash", "toolu_123", reasoning); err != nil {
		t.Fatalf("savePuterReasoningForTool() error = %v", err)
	}

	messages := []prompt.Message{{
		Role: "assistant",
		Content: prompt.MessageContent{Blocks: []prompt.ContentBlock{
			{Type: "text", Text: "..."},
			{Type: "tool_use", ID: "toolu_123", Name: "pwsh", Input: map[string]interface{}{"command": "Get-Date"}},
		}},
	}}
	restored, missing := h.restorePuterReasoning(context.Background(), "deepseek-v4-flash", messages)
	if restored != 1 || missing != 0 {
		t.Fatalf("restorePuterReasoning() = (%d, %d), want (1, 0)", restored, missing)
	}
	if messages[0].ReasoningContent != reasoning {
		t.Fatalf("reasoning_content = %q, want %q", messages[0].ReasoningContent, reasoning)
	}

	otherModel := []prompt.Message{messages[0]}
	otherModel[0].ReasoningContent = ""
	restored, missing = h.restorePuterReasoning(context.Background(), "deepseek-v4-pro", otherModel)
	if restored != 0 || missing != 1 || otherModel[0].ReasoningContent != "" {
		t.Fatalf("cross-model restore = (%d, %d, %q), want (0, 1, empty)", restored, missing, otherModel[0].ReasoningContent)
	}
}

func TestBuildOpenAINonStreamResponseIncludesReasoningContent(t *testing.T) {
	sh := newStreamHandler(
		&config.Config{}, httptest.NewRecorder(), nil, false, false, adapter.FormatOpenAI,
	)
	defer sh.release()

	sh.contentBlocks = []map[string]interface{}{
		{"type": "thinking", "thinking": ""},
		{"type": "text", "text": ""},
		{"type": "tool_use", "id": "call_1", "name": "pwsh", "input": map[string]interface{}{"command": "Get-Date"}},
	}
	// History builders are indexed by block position, so the fixture places them
	// at the positions the blocks occupy rather than keying a map.
	sh.thinkingBlockBuilders = []*strings.Builder{{}, nil}
	sh.thinkingBlockBuilders[0].WriteString("first reason")
	sh.textBlockBuilders = []*strings.Builder{nil, {}}
	sh.textBlockBuilders[1].WriteString("done")

	response := buildOpenAINonStreamResponse(sh, "deepseek-v4-flash", "tool_use")
	message := response.Choices[0].Message
	if message.ReasoningContent != "first reason" {
		t.Fatalf("reasoning_content = %q", message.ReasoningContent)
	}
	if message.Content != "done" || len(message.ToolCalls) != 1 {
		t.Fatalf("unexpected non-stream message: %#v", message)
	}
}
