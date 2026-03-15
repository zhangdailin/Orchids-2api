package promptbuilder

import (
	"fmt"
	"strings"

	"orchids-api/internal/tiktoken"
)

const (
	warpPromptMessageSoftLimit  = 2200
	warpPromptMessageHardLimit  = 900
	warpPromptSummaryKeepRecent = 8
	warpPromptSummaryMaxChars   = 2600
	warpPromptSummaryItemChars  = 220
	warpPromptSummaryMaxDepth   = 2
)

func enforceWarpPromptBudget(promptText string, history []map[string]string, maxTokens int) (string, []map[string]string) {
	budget := maxTokens
	if budget <= 0 {
		budget = 12000
	}
	if budget > 12000 {
		budget = 12000
	}

	working := normalizeWarpPromptHistory(history)
	if len(working) == 0 {
		return promptText, nil
	}

	promptTokens := tiktoken.EstimateTextTokens(promptText)
	overhead := 200
	total, itemTokens := estimateWarpPromptHistoryTokens(promptTokens, overhead, working)
	if total <= budget {
		return promptText, working
	}

	compressionApplied := false
	summarizedMessages := 0

	if compressed, changed := compressWarpPromptMessages(working, warpPromptMessageSoftLimit); changed {
		working = compressed
		compressionApplied = true
		total, itemTokens = estimateWarpPromptHistoryTokens(promptTokens, overhead, working)
		if total <= budget {
			return appendWarpPromptBudgetNote(promptText, false, summarizedMessages), working
		}
	}

	keepRecent := warpPromptSummaryKeepRecent
	if keepRecent > len(working) {
		keepRecent = len(working)
	}
	for total > budget && len(working) > 2 {
		if keepRecent < 2 {
			keepRecent = 2
		}
		next, merged, changed := summarizeOlderWarpPromptHistory(working, keepRecent, warpPromptSummaryMaxChars)
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
		total, itemTokens = estimateWarpPromptHistoryTokens(promptTokens, overhead, working)
		if total <= budget {
			return appendWarpPromptBudgetNote(promptText, false, summarizedMessages), working
		}
		if keepRecent > 2 {
			keepRecent--
		}
	}

	if total > budget {
		if compressed, changed := compressWarpPromptMessages(working, warpPromptMessageHardLimit); changed {
			working = compressed
			compressionApplied = true
			total, itemTokens = estimateWarpPromptHistoryTokens(promptTokens, overhead, working)
			if total <= budget {
				return appendWarpPromptBudgetNote(promptText, false, summarizedMessages), working
			}
		}
	}

	kept := make([]map[string]string, 0, len(working))
	keptTokens := 0
	for i := len(working) - 1; i >= 0; i-- {
		if promptTokens+keptTokens+itemTokens[i]+overhead > budget {
			continue
		}
		keptTokens += itemTokens[i]
		kept = append(kept, working[i])
	}
	for i, j := 0, len(kept)-1; i < j; i, j = i+1, j-1 {
		kept[i], kept[j] = kept[j], kept[i]
	}

	if len(kept) == 0 && len(working) > 0 {
		last := working[len(working)-1]
		last["content"] = compactWarpPromptContent(last["content"], warpPromptMessageHardLimit)
		kept = append(kept, last)
	}

	if len(kept) < len(working) || compressionApplied {
		promptText = appendWarpPromptBudgetNote(promptText, true, summarizedMessages)
	}
	return promptText, kept
}

func estimateWarpPromptHistoryTokens(promptTokens int, overhead int, history []map[string]string) (int, []int) {
	historyTokens := 0
	itemTokens := make([]int, len(history))
	for i, it := range history {
		c := strings.TrimSpace(it["content"])
		t := tiktoken.EstimateTextTokens(c) + 15
		itemTokens[i] = t
		historyTokens += t
	}
	return promptTokens + historyTokens + overhead, itemTokens
}

func normalizeWarpPromptHistory(history []map[string]string) []map[string]string {
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
		out = append(out, map[string]string{"role": role, "content": content})
	}
	return out
}

func compressWarpPromptMessages(history []map[string]string, targetChars int) ([]map[string]string, bool) {
	if len(history) == 0 || targetChars <= 0 {
		return history, false
	}
	out := make([]map[string]string, 0, len(history))
	changed := false
	for _, item := range history {
		role := item["role"]
		before := strings.TrimSpace(item["content"])
		after := compactWarpPromptContent(before, targetChars)
		if after != before {
			changed = true
		}
		out = append(out, map[string]string{"role": role, "content": after})
	}
	return out, changed
}

func summarizeOlderWarpPromptHistory(history []map[string]string, keepRecent int, maxChars int) ([]map[string]string, int, bool) {
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

	summary := buildWarpPromptHistorySummary(older, maxChars)
	if summary == "" {
		return history, 0, false
	}

	out := make([]map[string]string, 0, 1+len(recent))
	out = append(out, map[string]string{"role": "assistant", "content": summary})
	out = append(out, recent...)
	return out, len(older), true
}

func buildWarpPromptHistorySummary(history []map[string]string, maxChars int) string {
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
		snippet := compactWarpPromptContent(item["content"], warpPromptSummaryItemChars)
		if snippet != "" {
			lines = append(lines, fmt.Sprintf("%s: %s", roleTag, snippet))
		}
	}
	if len(lines) == 1 {
		return ""
	}
	return recursivelyCompactHistorySummary(strings.Join(lines, "\n"), maxChars, 0)
}

func recursivelyCompactHistorySummary(summary string, maxChars int, depth int) string {
	if maxChars <= 0 {
		return ""
	}
	if runeLen(summary) <= maxChars {
		return summary
	}
	if depth >= warpPromptSummaryMaxDepth {
		return truncateTextWithEllipsis(summary, maxChars)
	}
	rawLines := strings.Split(summary, "\n")
	lines := make([]string, 0, len(rawLines))
	for _, line := range rawLines {
		line = strings.TrimSpace(line)
		if line != "" {
			lines = append(lines, line)
		}
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
		compacted = append(compacted, compactWarpPromptContent(chunk, warpPromptSummaryItemChars))
	}
	return recursivelyCompactHistorySummary(strings.Join(compacted, "\n"), maxChars, depth+1)
}

func compactWarpPromptContent(text string, targetChars int) string {
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
		line = truncateTextWithEllipsis(line, warpPromptSummaryItemChars)
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

func appendWarpPromptBudgetNote(promptText string, fallbackWindow bool, summarizedMessages int) string {
	var sb strings.Builder
	sb.WriteString(promptText)
	sb.WriteString("\n\n<context_budget_note>\n")
	sb.WriteString("chatHistory compressed")
	if summarizedMessages > 0 {
		sb.WriteString(fmt.Sprintf(" (%d summarized)", summarizedMessages))
	}
	if fallbackWindow {
		sb.WriteString("; recent window kept")
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
