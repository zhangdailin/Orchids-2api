package handler

import (
	"fmt"
	"strings"
	"testing"

	"orchids-api/internal/prompt"
)

func TestEnforceWarpBudget_SummarizesOlderMessagesBeforeDropping(t *testing.T) {
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

	trimmed, _, after, _, summarized, dropped := enforceWarpBudget("prompt", messages, 2600)
	if len(trimmed) == 0 {
		t.Fatalf("expected non-empty trimmed messages")
	}
	if summarized == 0 {
		t.Fatalf("expected summarized messages > 0")
	}
	if dropped != 0 {
		t.Fatalf("expected summary stage to fit budget without dropping, dropped=%d", dropped)
	}
	if !trimmed[0].Content.IsString() || !strings.Contains(trimmed[0].Content.GetText(), "[history_summary]") {
		t.Fatalf("expected first message to be summary")
	}
	if after.Total > 2600 {
		t.Fatalf("expected tokens within budget after summarization, got %d", after.Total)
	}
}

func TestEnforceWarpBudget_FallsBackToWindowWhenStillOverBudget(t *testing.T) {
	t.Parallel()

	messages := []prompt.Message{
		{Role: "user", Content: prompt.MessageContent{Text: "u1 " + strings.Repeat("x", 4000)}},
		{Role: "assistant", Content: prompt.MessageContent{Text: "a1 " + strings.Repeat("x", 4000)}},
		{Role: "user", Content: prompt.MessageContent{Text: "u2 " + strings.Repeat("x", 4000)}},
	}

	trimmed, _, after, compressed, summarized, dropped := enforceWarpBudget("prompt", messages, 350)
	if len(trimmed) == 0 {
		t.Fatalf("expected at least one message")
	}
	if compressed == 0 && summarized == 0 && dropped == 0 {
		t.Fatalf("expected at least one budget action to be applied")
	}
	if after.Total > 350 {
		t.Fatalf("expected final tokens within budget, got %d", after.Total)
	}
}

func TestEnforceWarpBudget_NoChangeWhenUnderBudget(t *testing.T) {
	t.Parallel()

	messages := []prompt.Message{
		{Role: "user", Content: prompt.MessageContent{Text: "hello"}},
		{Role: "assistant", Content: prompt.MessageContent{Text: "world"}},
	}

	trimmed, before, after, compressed, summarized, dropped := enforceWarpBudget("prompt", messages, 12000)
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
