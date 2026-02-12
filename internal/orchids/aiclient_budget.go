package orchids

import (
	"fmt"
	"strings"

	"orchids-api/internal/tiktoken"
)

const (
	aiClientMessageSoftLimit  = 2200
	aiClientMessageHardLimit  = 900
	aiClientSummaryKeepRecent = 8
	aiClientSummaryMaxChars   = 2600
	aiClientSummaryItemChars  = 220
	aiClientSummaryMaxDepth   = 2
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
// It tries "compress first, trim last":
// 1) compress single long messages,
// 2) summarize older history while keeping recent raw turns,
// 3) only if still over budget, keep the most recent window.
func enforceAIClientBudget(promptText string, history []map[string]string, maxTokens int) (string, []map[string]string) {
	budget := maxTokens
	// Default + hard cap as per user requirement.
	if budget <= 0 {
		budget = 12000
	}
	if budget > 12000 {
		budget = 12000
	}

	working := normalizeAIClientHistory(history)
	if len(working) == 0 {
		return promptText, nil
	}

	promptTokens := tiktoken.EstimateTextTokens(promptText)
	overhead := 200 // conservative wrapper/messaging overhead
	total, itemTokens := estimateAIClientHistoryTokens(promptTokens, overhead, working)
	if total <= budget {
		return promptText, working
	}

	compressionApplied := false
	summarizedMessages := 0

	if compressed, changed := compressAIClientMessages(working, aiClientMessageSoftLimit); changed {
		working = compressed
		compressionApplied = true
		total, itemTokens = estimateAIClientHistoryTokens(promptTokens, overhead, working)
		if total <= budget {
			return appendAIClientBudgetNote(promptText, false, summarizedMessages), working
		}
	}

	keepRecent := aiClientSummaryKeepRecent
	if keepRecent > len(working) {
		keepRecent = len(working)
	}
	for total > budget && len(working) > 2 {
		if keepRecent < 2 {
			keepRecent = 2
		}
		next, merged, changed := summarizeOlderAIClientHistory(working, keepRecent, aiClientSummaryMaxChars)
		if !changed {
			if keepRecent > 2 {
				keepRecent--
				continue
			}
			break
		}
		working = next
		summarizedMessages += merged
		compressionApplied = true
		total, itemTokens = estimateAIClientHistoryTokens(promptTokens, overhead, working)
		if total <= budget {
			return appendAIClientBudgetNote(promptText, false, summarizedMessages), working
		}
		if keepRecent > 2 {
			keepRecent--
		}
	}

	if total > budget {
		if compressed, changed := compressAIClientMessages(working, aiClientMessageHardLimit); changed {
			working = compressed
			compressionApplied = true
			total, itemTokens = estimateAIClientHistoryTokens(promptTokens, overhead, working)
			if total <= budget {
				return appendAIClientBudgetNote(promptText, false, summarizedMessages), working
			}
		}
	}

	// Hard fallback: keep the most recent messages that fit.
	kept := make([]map[string]string, 0, len(working))
	keptTokens := 0
	for i := len(working) - 1; i >= 0; i-- {
		if promptTokens+keptTokens+itemTokens[i]+overhead > budget {
			continue
		}
		keptTokens += itemTokens[i]
		kept = append(kept, working[i])
	}
	// reverse to restore order
	for i, j := 0, len(kept)-1; i < j; i, j = i+1, j-1 {
		kept[i], kept[j] = kept[j], kept[i]
	}

	// If we kept nothing, keep the last user message at minimum.
	if len(kept) == 0 && len(working) > 0 {
		last := working[len(working)-1]
		last["content"] = compactAIClientContent(last["content"], aiClientMessageHardLimit)
		kept = append(kept, last)
	}

	// Add a minimal note to avoid confusion on compressed/windowed history.
	if len(kept) < len(working) || compressionApplied {
		promptText = appendAIClientBudgetNote(promptText, true, summarizedMessages)
	}
	return promptText, kept
}

func estimateAIClientHistoryTokens(promptTokens int, overhead int, history []map[string]string) (int, []int) {
	historyTokens := 0
	itemTokens := make([]int, len(history))
	for i, it := range history {
		c := strings.TrimSpace(it["content"])
		// Add a small per-message overhead.
		t := tiktoken.EstimateTextTokens(c) + 15
		itemTokens[i] = t
		historyTokens += t
	}
	return promptTokens + historyTokens + overhead, itemTokens
}

func normalizeAIClientHistory(history []map[string]string) []map[string]string {
	if len(history) == 0 {
		return nil
	}
	out := make([]map[string]string, 0, len(history))
	for _, item := range history {
		role := strings.ToLower(strings.TrimSpace(item["role"]))
		if role != "user" && role != "assistant" {
			continue
		}
		content := strings.TrimSpace(item["content"])
		if content == "" {
			continue
		}
		out = append(out, map[string]string{
			"role":    role,
			"content": content,
		})
	}
	return out
}

func compressAIClientMessages(history []map[string]string, targetChars int) ([]map[string]string, bool) {
	if len(history) == 0 || targetChars <= 0 {
		return history, false
	}
	out := make([]map[string]string, 0, len(history))
	changed := false
	for _, item := range history {
		role := item["role"]
		before := strings.TrimSpace(item["content"])
		after := compactAIClientContent(before, targetChars)
		if after != before {
			changed = true
		}
		out = append(out, map[string]string{
			"role":    role,
			"content": after,
		})
	}
	return out, changed
}

func summarizeOlderAIClientHistory(history []map[string]string, keepRecent int, maxChars int) ([]map[string]string, int, bool) {
	if len(history) <= keepRecent+1 {
		return history, 0, false
	}
	if keepRecent < 1 {
		keepRecent = 1
	}
	if keepRecent >= len(history) {
		return history, 0, false
	}

	pivot := len(history) - keepRecent
	older := history[:pivot]
	recent := history[pivot:]
	if len(older) == 0 {
		return history, 0, false
	}

	summary := buildAIClientHistorySummary(older, maxChars)
	if summary == "" {
		return history, 0, false
	}

	out := make([]map[string]string, 0, 1+len(recent))
	out = append(out, map[string]string{
		"role":    "assistant",
		"content": summary,
	})
	out = append(out, recent...)
	return out, len(older), true
}

func buildAIClientHistorySummary(history []map[string]string, maxChars int) string {
	if len(history) == 0 {
		return ""
	}
	lines := make([]string, 0, len(history)+1)
	lines = append(lines, fmt.Sprintf("[history_summary] compressed %d earlier messages.", len(history)))
	for _, item := range history {
		roleTag := "U"
		if item["role"] == "assistant" {
			roleTag = "A"
		}
		snippet := compactAIClientContent(item["content"], aiClientSummaryItemChars)
		if snippet == "" {
			continue
		}
		lines = append(lines, fmt.Sprintf("%s: %s", roleTag, snippet))
	}
	if len(lines) == 1 {
		return ""
	}
	summary := strings.Join(lines, "\n")
	return recursivelyCompactHistorySummary(summary, maxChars, 0)
}

func recursivelyCompactHistorySummary(summary string, maxChars int, depth int) string {
	if maxChars <= 0 {
		return ""
	}
	if runeLen(summary) <= maxChars {
		return summary
	}
	if depth >= aiClientSummaryMaxDepth {
		return truncateTextWithEllipsis(summary, maxChars)
	}
	rawLines := strings.Split(summary, "\n")
	lines := make([]string, 0, len(rawLines))
	for _, line := range rawLines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		lines = append(lines, line)
	}
	if len(lines) <= 2 {
		return truncateTextWithEllipsis(summary, maxChars)
	}

	compacted := make([]string, 0, len(lines)/3+2)
	compacted = append(compacted, lines[0])
	for i := 1; i < len(lines); i += 3 {
		end := i + 3
		if end > len(lines) {
			end = len(lines)
		}
		chunk := strings.Join(lines[i:end], " | ")
		compacted = append(compacted, compactAIClientContent(chunk, aiClientSummaryItemChars))
	}
	return recursivelyCompactHistorySummary(strings.Join(compacted, "\n"), maxChars, depth+1)
}

func compactAIClientContent(text string, targetChars int) string {
	text = strings.TrimSpace(text)
	if text == "" {
		return ""
	}
	if targetChars <= 0 || runeLen(text) <= targetChars {
		return text
	}

	lines := strings.Split(text, "\n")
	keywords := []string{
		"error", "failed", "todo", "fix", "bug", "constraint", "must", "important",
		"错误", "失败", "修复", "约束", "必须", "结论", "决定", "下一步", "风险",
		"tool", "read", "write", "edit", "bash", "path", "file",
	}

	selected := make([]string, 0, 8)
	seen := make(map[string]struct{})
	add := func(line string) {
		line = strings.TrimSpace(line)
		if line == "" {
			return
		}
		line = collapseWhitespace(line)
		line = truncateTextWithEllipsis(line, aiClientSummaryItemChars)
		if _, ok := seen[line]; ok {
			return
		}
		seen[line] = struct{}{}
		selected = append(selected, line)
	}

	for _, line := range lines {
		lower := strings.ToLower(line)
		for _, kw := range keywords {
			if strings.Contains(lower, kw) {
				add(line)
				break
			}
		}
		if len(selected) >= 6 {
			break
		}
	}

	for _, line := range lines {
		if len(selected) >= 6 {
			break
		}
		add(line)
	}
	if len(lines) > 0 {
		add(lines[len(lines)-1])
	}

	if len(selected) == 0 {
		return truncateTextWithEllipsis(text, targetChars)
	}
	joined := strings.Join(selected, " | ")
	joined = truncateTextWithEllipsis(joined, targetChars-32)
	return fmt.Sprintf("[compressed %d chars] %s", runeLen(text), joined)
}

func appendAIClientBudgetNote(promptText string, fallbackWindow bool, summarizedMessages int) string {
	var sb strings.Builder
	sb.WriteString(promptText)
	sb.WriteString("\n\n<context_budget_note>\n")
	sb.WriteString("为了将输入控制在 12000 tokens 以内，已优先对历史 chatHistory 做压缩与摘要")
	if summarizedMessages > 0 {
		sb.WriteString(fmt.Sprintf("（已压缩 %d 条较早消息）", summarizedMessages))
	}
	if fallbackWindow {
		sb.WriteString("；仍超限时保留最近消息窗口。")
	} else {
		sb.WriteString("。")
	}
	sb.WriteString("\n</context_budget_note>\n")
	return sb.String()
}

func collapseWhitespace(text string) string {
	parts := strings.Fields(text)
	if len(parts) == 0 {
		return ""
	}
	return strings.Join(parts, " ")
}

func runeLen(text string) int {
	return len([]rune(text))
}
