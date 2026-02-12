package orchids

import (
	"strings"

	"orchids-api/internal/tiktoken"
)

// truncateAIClientHistory truncates each history item's content to reduce large tool outputs.
// It keeps the semantics but avoids multi-thousand-line blobs.
func truncateAIClientHistory(history []map[string]string) []map[string]string {
	if len(history) == 0 {
		return history
	}
	out := make([]map[string]string, 0, len(history))
	for _, item := range history {
		role := strings.TrimSpace(item["role"])
		content := item["content"]
		// Trim and truncate aggressively. Tool outputs can be enormous.
		content = strings.TrimSpace(content)
		content = truncateTextWithEllipsis(content, 1800)
		out = append(out, map[string]string{
			"role":    role,
			"content": content,
		})
	}
	return out
}

// enforceAIClientBudget enforces a hard max token budget for prompt+chatHistory.
// If we exceed the budget, we keep the most recent history first.
// We do NOT drop the system context from promptText; we only truncate and window chatHistory.
func enforceAIClientBudget(promptText string, history []map[string]string, maxTokens int) (string, []map[string]string) {
	budget := maxTokens
	// Default + hard cap as per user requirement.
	if budget <= 0 {
		budget = 12000
	}
	if budget > 12000 {
		budget = 12000
	}

	promptTokens := tiktoken.EstimateTextTokens(promptText)
	overhead := 200 // conservative wrapper/messaging overhead
	// token estimate for history
	historyTokens := 0
	itemTokens := make([]int, len(history))
	for i, it := range history {
		c := strings.TrimSpace(it["content"])
		// Add a small per-message overhead
		t := tiktoken.EstimateTextTokens(c) + 15
		itemTokens[i] = t
		historyTokens += t
	}
	total := promptTokens + historyTokens + overhead
	if total <= budget {
		return promptText, history
	}

	// Keep the most recent messages that fit.
	kept := make([]map[string]string, 0, len(history))
	keptTokens := 0
	for i := len(history) - 1; i >= 0; i-- {
		if promptTokens+keptTokens+itemTokens[i]+overhead > budget {
			continue
		}
		keptTokens += itemTokens[i]
		kept = append(kept, history[i])
	}
	// reverse to restore order
	for i, j := 0, len(kept)-1; i < j; i, j = i+1, j-1 {
		kept[i], kept[j] = kept[j], kept[i]
	}

	// If we kept nothing, keep the last user message at minimum.
	if len(kept) == 0 && len(history) > 0 {
		kept = append(kept, history[len(history)-1])
	}

	// Add a minimal note to the prompt to avoid confusion.
	if len(kept) < len(history) {
		promptText = promptText + "\n\n<context_budget_note>\n为了将输入控制在 12000 tokens 以内，已对历史 chatHistory 做了截断与窗口保留（保留最近消息）。\n</context_budget_note>\n"
	}
	return promptText, kept
}
