package handler

import (
	"encoding/json"
	"fmt"
	"strings"

	"orchids-api/internal/prompt"
	"orchids-api/internal/tiktoken"
)

const (
	warpMessageSoftLimit  = 2200
	warpMessageHardLimit  = 900
	warpSummaryKeepRecent = 8
	warpSummaryMaxChars   = 2600
	warpSummaryItemChars  = 220
	warpSummaryMaxDepth   = 2
)

// enforceWarpBudget trims Warp messages to keep total prompt+messages within a hard token budget.
// Strategy: compress first, trim last.
// 1) Compress tool_result blocks and oversized text blocks.
// 2) Summarize older messages while keeping recent raw turns.
// 3) Drop oldest messages only as a hard fallback.
type warpTokenBreakdown struct {
	PromptTokens   int
	MessagesTokens int
	ToolTokens     int
	Total          int
}

func enforceWarpBudget(builtPrompt string, messages []prompt.Message, maxTokens int) (trimmed []prompt.Message, before warpTokenBreakdown, after warpTokenBreakdown, compressedBlocks int, summarizedMessages int, droppedMessages int) {
	budget := maxTokens
	if budget <= 0 {
		budget = 12000
	}
	if budget > 12000 {
		budget = 12000
	}
	if len(messages) == 0 {
		empty := estimateWarpTokensBreakdown(builtPrompt, nil)
		return nil, empty, empty, 0, 0, 0
	}

	// Stage 1: tool_result compression.
	compressed, compressedCount := compressToolResults(messages, 1800, "warp")
	working := compressed

	// Stage 2: compress long text blocks/messages.
	if textCompressed, count := compressWarpMessages(working, warpMessageSoftLimit); count > 0 {
		working = textCompressed
		compressedCount += count
	}

	beforeBD := estimateWarpTokensBreakdown(builtPrompt, working)
	if beforeBD.Total <= budget {
		return working, beforeBD, beforeBD, compressedCount, 0, 0
	}

	// Stage 3: summarize older history, keep recent raw turns.
	keepRecent := warpSummaryKeepRecent
	if keepRecent > len(working) {
		keepRecent = len(working)
	}
	for beforeBD.Total > budget && len(working) > 2 {
		if keepRecent < 2 {
			keepRecent = 2
		}
		next, merged, changed := summarizeOlderWarpMessages(working, keepRecent, warpSummaryMaxChars)
		if !changed {
			if keepRecent > 2 {
				keepRecent--
				continue
			}
			break
		}
		working = next
		summarizedMessages += merged
		beforeBD = estimateWarpTokensBreakdown(builtPrompt, working)
		if beforeBD.Total <= budget {
			return working, beforeBD, beforeBD, compressedCount, summarizedMessages, 0
		}
		if keepRecent > 2 {
			keepRecent--
		}
	}

	// Stage 4: harder per-message compression.
	if harder, count := compressWarpMessages(working, warpMessageHardLimit); count > 0 {
		working = harder
		compressedCount += count
		beforeBD = estimateWarpTokensBreakdown(builtPrompt, working)
		if beforeBD.Total <= budget {
			return working, beforeBD, beforeBD, compressedCount, summarizedMessages, 0
		}
	}

	// Stage 5 hard fallback: drop oldest messages until within budget.
	work := cloneMessages(working)
	beforeTokens := beforeBD

	// Find last user message index.
	lastUser := -1
	for i := len(work) - 1; i >= 0; i-- {
		if work[i].Role == "user" {
			lastUser = i
			break
		}
	}
	if lastUser == -1 {
		lastUser = len(work) - 1
	}

	start := 0
	for start < lastUser && len(work[start:]) > 1 {
		testMsgs := work[start+1:]
		bd := estimateWarpTokensBreakdown(builtPrompt, testMsgs)
		if bd.Total <= budget {
			start++
			break
		}
		start++
	}
	trimmed = work[start:]
	if len(trimmed) == 0 {
		trimmed = work[len(work)-1:]
	}
	afterTokens := estimateWarpTokensBreakdown(builtPrompt, trimmed)
	return trimmed, beforeTokens, afterTokens, compressedCount, summarizedMessages, start
}

func estimateWarpTokensBreakdown(builtPrompt string, messages []prompt.Message) warpTokenBreakdown {
	bd := warpTokenBreakdown{}
	bd.PromptTokens = tiktoken.EstimateTextTokens(builtPrompt)
	// Conservative wrapper overhead.
	overhead := 200

	for _, m := range messages {
		if m.Content.IsString() {
			bd.MessagesTokens += tiktoken.EstimateTextTokens(strings.TrimSpace(m.Content.GetText())) + 15
			continue
		}
		for _, b := range m.Content.GetBlocks() {
			switch b.Type {
			case "text":
				bd.MessagesTokens += tiktoken.EstimateTextTokens(strings.TrimSpace(b.Text)) + 10
			case "tool_result":
				if s, ok := b.Content.(string); ok {
					bd.ToolTokens += tiktoken.EstimateTextTokens(s) + 10
				} else {
					bd.ToolTokens += 200
				}
			default:
				bd.ToolTokens += 50
			}
		}
	}
	bd.Total = bd.PromptTokens + bd.MessagesTokens + bd.ToolTokens + overhead
	return bd
}

func compressWarpMessages(messages []prompt.Message, targetChars int) ([]prompt.Message, int) {
	if targetChars <= 0 || len(messages) == 0 {
		return messages, 0
	}
	out := cloneMessages(messages)
	changed := 0
	for i := range out {
		msg := &out[i]
		if msg.Content.IsString() {
			before := strings.TrimSpace(msg.Content.GetText())
			after := compactWarpText(before, targetChars)
			if after != before {
				msg.Content.Text = after
				changed++
			}
			continue
		}
		blocks := msg.Content.GetBlocks()
		if len(blocks) == 0 {
			continue
		}
		for j := range blocks {
			block := &blocks[j]
			switch block.Type {
			case "text":
				before := strings.TrimSpace(block.Text)
				after := compactWarpText(before, targetChars)
				if after != before {
					block.Text = after
					changed++
				}
			case "tool_result":
				if s, ok := block.Content.(string); ok {
					after := compactWarpText(strings.TrimSpace(s), targetChars)
					if after != s {
						block.Content = after
						changed++
					}
				}
			}
		}
		msg.Content.Blocks = blocks
	}
	return out, changed
}

func summarizeOlderWarpMessages(messages []prompt.Message, keepRecent int, maxChars int) ([]prompt.Message, int, bool) {
	if len(messages) <= keepRecent+1 {
		return messages, 0, false
	}
	if keepRecent < 1 {
		keepRecent = 1
	}
	if keepRecent >= len(messages) {
		return messages, 0, false
	}
	pivot := len(messages) - keepRecent
	older := messages[:pivot]
	recent := messages[pivot:]
	if len(older) == 0 {
		return messages, 0, false
	}

	summary := buildWarpHistorySummary(older, maxChars)
	if summary == "" {
		return messages, 0, false
	}

	out := make([]prompt.Message, 0, 1+len(recent))
	out = append(out, prompt.Message{
		Role: "assistant",
		Content: prompt.MessageContent{
			Text: summary,
		},
	})
	out = append(out, recent...)
	return out, len(older), true
}

func buildWarpHistorySummary(messages []prompt.Message, maxChars int) string {
	if len(messages) == 0 {
		return ""
	}
	lines := make([]string, 0, len(messages)+1)
	lines = append(lines, fmt.Sprintf("[history_summary] compressed %d earlier messages.", len(messages)))
	for _, msg := range messages {
		role := strings.ToUpper(strings.TrimSpace(msg.Role))
		if role == "" {
			role = "MSG"
		}
		snippet := summarizeWarpMessage(msg, warpSummaryItemChars)
		if snippet == "" {
			continue
		}
		lines = append(lines, fmt.Sprintf("%s: %s", role, snippet))
	}
	if len(lines) <= 1 {
		return ""
	}
	return recursivelyCompactWarpSummary(strings.Join(lines, "\n"), maxChars, 0)
}

func recursivelyCompactWarpSummary(text string, maxChars int, depth int) string {
	if maxChars <= 0 {
		return ""
	}
	if warpRuneLen(text) <= maxChars {
		return text
	}
	if depth >= warpSummaryMaxDepth {
		return truncateWarpTextWithEllipsis(text, maxChars)
	}

	rawLines := strings.Split(text, "\n")
	lines := make([]string, 0, len(rawLines))
	for _, line := range rawLines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		lines = append(lines, line)
	}
	if len(lines) <= 2 {
		return truncateWarpTextWithEllipsis(text, maxChars)
	}

	compacted := make([]string, 0, len(lines)/3+2)
	compacted = append(compacted, lines[0])
	for i := 1; i < len(lines); i += 3 {
		end := i + 3
		if end > len(lines) {
			end = len(lines)
		}
		chunk := strings.Join(lines[i:end], " | ")
		compacted = append(compacted, compactWarpText(chunk, warpSummaryItemChars))
	}
	return recursivelyCompactWarpSummary(strings.Join(compacted, "\n"), maxChars, depth+1)
}

func summarizeWarpMessage(msg prompt.Message, targetChars int) string {
	if targetChars <= 0 {
		targetChars = warpSummaryItemChars
	}
	if msg.Content.IsString() {
		return compactWarpText(strings.TrimSpace(msg.Content.GetText()), targetChars)
	}
	parts := make([]string, 0, 6)
	for _, block := range msg.Content.GetBlocks() {
		switch block.Type {
		case "text":
			if text := strings.TrimSpace(block.Text); text != "" {
				parts = append(parts, compactWarpText(text, targetChars))
			}
		case "tool_use":
			toolName := strings.TrimSpace(block.Name)
			if toolName == "" {
				toolName = "unknown_tool"
			}
			parts = append(parts, "[tool_use "+toolName+"]")
		case "tool_result":
			switch v := block.Content.(type) {
			case string:
				parts = append(parts, "[tool_result "+compactWarpText(v, targetChars)+"]")
			default:
				raw, _ := json.Marshal(v)
				parts = append(parts, "[tool_result "+compactWarpText(string(raw), targetChars)+"]")
			}
		case "image":
			parts = append(parts, "[image]")
		case "document":
			parts = append(parts, "[document]")
		}
		if len(parts) >= 6 {
			break
		}
	}
	return compactWarpText(strings.Join(parts, " | "), targetChars)
}

func compactWarpText(text string, targetChars int) string {
	text = strings.TrimSpace(text)
	if text == "" {
		return ""
	}
	if targetChars <= 0 || warpRuneLen(text) <= targetChars {
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
		line = strings.Join(strings.Fields(line), " ")
		line = truncateWarpTextWithEllipsis(line, warpSummaryItemChars)
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
		return truncateWarpTextWithEllipsis(text, targetChars)
	}
	joined := strings.Join(selected, " | ")
	joined = truncateWarpTextWithEllipsis(joined, targetChars-32)
	return fmt.Sprintf("[compressed %d chars] %s", warpRuneLen(text), joined)
}

func warpRuneLen(text string) int {
	return len([]rune(text))
}

func truncateWarpTextWithEllipsis(text string, maxLen int) string {
	if maxLen <= 0 {
		return ""
	}
	runes := []rune(text)
	if len(runes) <= maxLen {
		return text
	}
	return string(runes[:maxLen]) + "…[truncated]"
}
