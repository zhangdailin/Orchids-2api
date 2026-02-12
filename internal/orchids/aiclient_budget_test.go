package orchids

import (
	"fmt"
	"strings"
	"testing"
)

func TestEnforceAIClientBudget_CompressesOlderHistoryBeforeWindowTrim(t *testing.T) {
	t.Parallel()

	history := make([]map[string]string, 0, 14)
	for i := 0; i < 14; i++ {
		role := "user"
		if i%2 == 1 {
			role = "assistant"
		}
		history = append(history, map[string]string{
			"role":    role,
			"content": fmt.Sprintf("turn-%02d %s", i, strings.Repeat("token ", 240)),
		})
	}

	prompt, compressed := enforceAIClientBudget("test prompt", history, 3000)
	if len(compressed) == 0 {
		t.Fatalf("expected compressed history, got empty")
	}
	if len(compressed) > aiClientSummaryKeepRecent+1 {
		t.Fatalf("expected summary + recent turns, got %d items", len(compressed))
	}
	if compressed[0]["role"] != "assistant" || !strings.Contains(compressed[0]["content"], "[history_summary]") {
		t.Fatalf("expected first item to be history summary, got %#v", compressed[0])
	}
	if !strings.Contains(compressed[len(compressed)-1]["content"], "turn-13") {
		t.Fatalf("expected most recent turn signal to be preserved")
	}
	if !strings.Contains(prompt, "<context_budget_note>") {
		t.Fatalf("expected budget note in prompt")
	}
	if !strings.Contains(prompt, "优先对历史 chatHistory 做压缩与摘要") {
		t.Fatalf("expected compression-first note in prompt")
	}
}

func TestEnforceAIClientBudget_UsesWindowFallbackWhenStillOverBudget(t *testing.T) {
	t.Parallel()

	history := []map[string]string{
		{"role": "user", "content": "u1 " + strings.Repeat("x", 4000)},
		{"role": "assistant", "content": "a1 " + strings.Repeat("x", 4000)},
		{"role": "user", "content": "u2 " + strings.Repeat("x", 4000)},
		{"role": "assistant", "content": "a2 " + strings.Repeat("x", 4000)},
	}

	prompt, compressed := enforceAIClientBudget("test prompt", history, 120)
	if len(compressed) == 0 {
		t.Fatalf("expected at least one message after fallback")
	}
	if !strings.Contains(prompt, "仍超限时保留最近消息窗口") {
		t.Fatalf("expected fallback window note, got %q", prompt)
	}
}

func TestEnforceAIClientBudget_PreservesSingleMessageWithinBudget(t *testing.T) {
	t.Parallel()

	longText := strings.Repeat("z", 5000)
	history := []map[string]string{
		{"role": "user", "content": longText},
	}

	prompt, compressed := enforceAIClientBudget("test prompt", history, 12000)
	if prompt != "test prompt" {
		t.Fatalf("prompt should remain unchanged within budget")
	}
	if len(compressed) != 1 {
		t.Fatalf("expected one history message, got %d", len(compressed))
	}
	if compressed[0]["content"] != longText {
		t.Fatalf("expected history content preserved within budget")
	}
}
