package handler

import (
	"fmt"
	"strings"
	"testing"

	"orchids-api/internal/prompt"
)

func TestEnforceWarpBudget_PreservesLargeHistory(t *testing.T) {
	t.Parallel()

	messages := make([]prompt.Message, 0, 14)
	for i := 0; i < 14; i++ {
		role := "user"
		if i%2 == 1 {
			role = "assistant"
		}
		messages = append(messages, prompt.Message{
			Role: role,
			Content: prompt.MessageContent{
				Text: fmt.Sprintf("turn-%02d %s", i, strings.Repeat("token ", 240)),
			},
		})
	}

	trimmed, before, after, compressed, summarized, dropped := enforceWarpBudget("auto-efficient", messages, nil, true, 2600)
	if len(trimmed) != len(messages) {
		t.Fatalf("expected message count unchanged, got %d want %d", len(trimmed), len(messages))
	}
	if compressed != 0 || summarized != 0 || dropped != 0 {
		t.Fatalf("expected no budget actions, got compressed=%d summarized=%d dropped=%d", compressed, summarized, dropped)
	}
	if before.Total != after.Total {
		t.Fatalf("expected token estimate unchanged, before=%d after=%d", before.Total, after.Total)
	}
	for i := range trimmed {
		if trimmed[i].Role != messages[i].Role {
			t.Fatalf("message %d role changed: got %q want %q", i, trimmed[i].Role, messages[i].Role)
		}
		if trimmed[i].Content.GetText() != messages[i].Content.GetText() {
			t.Fatalf("message %d content changed", i)
		}
	}
}

func TestEnforceWarpBudget_PreservesOversizedMessages(t *testing.T) {
	t.Parallel()

	messages := []prompt.Message{
		{Role: "user", Content: prompt.MessageContent{Text: "u1 " + strings.Repeat("x", 4000)}},
		{Role: "assistant", Content: prompt.MessageContent{Text: "a1 " + strings.Repeat("x", 4000)}},
		{Role: "user", Content: prompt.MessageContent{Text: "u2 " + strings.Repeat("x", 4000)}},
	}

	trimmed, before, after, compressed, summarized, dropped := enforceWarpBudget("auto-efficient", messages, nil, true, 350)
	if len(trimmed) != len(messages) {
		t.Fatalf("expected oversized messages to be preserved, got %d want %d", len(trimmed), len(messages))
	}
	if compressed != 0 || summarized != 0 || dropped != 0 {
		t.Fatalf("expected no budget actions, got compressed=%d summarized=%d dropped=%d", compressed, summarized, dropped)
	}
	if before.Total != after.Total {
		t.Fatalf("expected token estimate unchanged, before=%d after=%d", before.Total, after.Total)
	}
	for i := range trimmed {
		if trimmed[i].Content.GetText() != messages[i].Content.GetText() {
			t.Fatalf("message %d content changed", i)
		}
	}
}

func TestEnforceWarpBudget_NoChangeWhenUnderBudget(t *testing.T) {
	t.Parallel()

	messages := []prompt.Message{
		{Role: "user", Content: prompt.MessageContent{Text: "hello"}},
		{Role: "assistant", Content: prompt.MessageContent{Text: "world"}},
	}

	trimmed, before, after, compressed, summarized, dropped := enforceWarpBudget("auto-efficient", messages, nil, true, 12000)
	if len(trimmed) != len(messages) {
		t.Fatalf("expected message count unchanged, got %d", len(trimmed))
	}
	if compressed != 0 || summarized != 0 || dropped != 0 {
		t.Fatalf("expected no compression/summarize/drop, got compressed=%d summarized=%d dropped=%d", compressed, summarized, dropped)
	}
	if before.Total != after.Total {
		t.Fatalf("expected tokens unchanged under budget")
	}
}
