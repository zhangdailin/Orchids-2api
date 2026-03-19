package handler

import (
	"fmt"
	"github.com/goccy/go-json"
	"strings"

	"orchids-api/internal/prompt"
	warpclient "orchids-api/internal/warp"
)

const (
	warpSummaryItemChars = 220
	warpSummaryMaxDepth  = 2
)

// enforceWarpBudget is now a passthrough for Warp requests.
// Warp models support much larger contexts, so we keep the original
// messages intact instead of compressing, summarizing, or trimming them.
type warpTokenBreakdown struct {
	PromptTokens   int
	MessagesTokens int
	ToolTokens     int
	Total          int
}

func enforceWarpBudget(model string, messages []prompt.Message, tools []interface{}, disableWarpTools bool, maxTokens int) (trimmed []prompt.Message, before warpTokenBreakdown, after warpTokenBreakdown, compressedBlocks int, summarizedMessages int, droppedMessages int) {
	if len(messages) == 0 {
		empty := estimateWarpTokensBreakdown(model, nil, tools, disableWarpTools)
		return nil, empty, empty, 0, 0, 0
	}

	trimmed = cloneMessages(messages)
	before = estimateWarpTokensBreakdown(model, trimmed, tools, disableWarpTools)
	return trimmed, before, before, 0, 0, 0
}

func estimateWarpTokensBreakdown(model string, messages []prompt.Message, tools []interface{}, disableWarpTools bool) warpTokenBreakdown {
	estimate, err := warpclient.EstimateInputTokens("", model, messages, tools, disableWarpTools)
	if err != nil {
		return warpTokenBreakdown{}
	}
	return warpTokenBreakdown{
		PromptTokens:   estimate.BasePromptTokens,
		MessagesTokens: estimate.HistoryTokens + estimate.ToolResultTokens,
		ToolTokens:     estimate.ToolSchemaTokens,
		Total:          estimate.Total,
	}
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
