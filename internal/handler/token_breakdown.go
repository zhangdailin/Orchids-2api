package handler

import (
	"strings"

	"orchids-api/internal/prompt"
	"orchids-api/internal/tiktoken"
	"orchids-api/internal/warp"
)

type inputTokenBreakdown struct {
	BasePromptTokens    int
	SystemContextTokens int
	HistoryTokens       int
	ToolsTokens         int
	Total               int
}

func estimateInputTokenBreakdown(promptText string, history []map[string]string, tools []interface{}) inputTokenBreakdown {
	var bd inputTokenBreakdown
	promptTokens := tiktoken.EstimateTextTokens(promptText)
	sysText := extractTaggedContent(promptText, "sys")
	if sysText == "" {
		sysText = extractTaggedContent(promptText, "system_context")
	}
	sysTokens := tiktoken.EstimateTextTokens(sysText)
	if sysTokens > promptTokens {
		sysTokens = promptTokens
	}

	bd.SystemContextTokens = sysTokens
	bd.BasePromptTokens = promptTokens - sysTokens

	for _, item := range history {
		content := strings.TrimSpace(item["content"])
		if content == "" {
			continue
		}
		bd.HistoryTokens += tiktoken.EstimateTextTokens(content) + 15
	}

	bd.ToolsTokens = estimateCompactedToolsTokens(tools)

	bd.Total = bd.BasePromptTokens + bd.SystemContextTokens + bd.HistoryTokens + bd.ToolsTokens
	return bd
}

func estimateOrchidsInputTokenBreakdown(promptText string, history []map[string]string) inputTokenBreakdown {
	return estimateInputTokenBreakdown(promptText, history, nil)
}

func estimateWarpInputTokenBreakdown(promptText, model string, messages []prompt.Message, tools []interface{}, disableWarpTools bool) (inputTokenBreakdown, string, error) {
	estimate, err := warp.EstimateInputTokens(promptText, model, messages, tools, disableWarpTools)
	if err != nil {
		return inputTokenBreakdown{}, "", err
	}

	return inputTokenBreakdown{
		BasePromptTokens:    estimate.BasePromptTokens,
		SystemContextTokens: 0,
		HistoryTokens:       estimate.HistoryTokens + estimate.ToolResultTokens,
		ToolsTokens:         estimate.ToolSchemaTokens,
		Total:               estimate.Total,
	}, estimate.Profile, nil
}

func extractTaggedContent(text string, tag string) string {
	if text == "" || tag == "" {
		return ""
	}
	startTag := "<" + tag + ">"
	endTag := "</" + tag + ">"

	start := strings.Index(text, startTag)
	if start == -1 {
		return ""
	}
	start += len(startTag)
	end := strings.Index(text[start:], endTag)
	if end == -1 {
		return ""
	}
	return strings.TrimSpace(text[start : start+end])
}
