package warp

import (
	"strings"
	"testing"

	"orchids-api/internal/prompt"
)

func TestEstimateInputTokens_TracksWarpToolCost(t *testing.T) {
	t.Parallel()

	messages := []prompt.Message{
		{
			Role: "user",
			Content: prompt.MessageContent{
				Text: "当前目录",
			},
		},
	}

	noToolEstimate, err := EstimateInputTokens("", "auto-efficient", messages, nil, true)
	if err != nil {
		t.Fatalf("EstimateInputTokens without tools: %v", err)
	}
	if noToolEstimate.Total <= 0 {
		t.Fatalf("expected positive total without tools, got %+v", noToolEstimate)
	}
	if noToolEstimate.Profile != "warp-no-tools" {
		t.Fatalf("unexpected no-tool profile: %s", noToolEstimate.Profile)
	}

	withToolEstimate, err := EstimateInputTokens("", "auto-efficient", messages, []interface{}{
		map[string]interface{}{
			"type": "function",
			"function": map[string]interface{}{
				"name":        "Bash",
				"description": strings.Repeat("Run shell command. ", 20),
				"parameters": map[string]interface{}{
					"type": "object",
					"properties": map[string]interface{}{
						"command":     map[string]interface{}{"type": "string", "description": "command"},
						"description": map[string]interface{}{"type": "string", "description": "reason"},
						"ignored":     map[string]interface{}{"type": "string", "description": "should be dropped"},
					},
					"required": []interface{}{"command", "ignored"},
				},
			},
		},
	}, false)
	if err != nil {
		t.Fatalf("EstimateInputTokens with tools: %v", err)
	}
	if withToolEstimate.Profile != "warp-tools" {
		t.Fatalf("unexpected tool profile: %s", withToolEstimate.Profile)
	}
	if withToolEstimate.ToolSchemaTokens <= 0 {
		t.Fatalf("expected tool schema tokens > 0, got %+v", withToolEstimate)
	}
	if withToolEstimate.Total <= noToolEstimate.Total {
		t.Fatalf("expected tools to increase total tokens: no_tools=%d with_tools=%d", noToolEstimate.Total, withToolEstimate.Total)
	}
}

func TestConvertTools_FiltersUnsupportedAndMinimizesSchema(t *testing.T) {
	t.Parallel()

	defs := convertTools([]interface{}{
		map[string]interface{}{
			"type": "function",
			"function": map[string]interface{}{
				"name":        "Bash",
				"description": strings.Repeat("desc ", 100),
				"parameters": map[string]interface{}{
					"type": "object",
					"properties": map[string]interface{}{
						"command":     map[string]interface{}{"type": "string"},
						"description": map[string]interface{}{"type": "string"},
						"ignored":     map[string]interface{}{"type": "string"},
					},
					"required": []interface{}{"command", "ignored"},
				},
			},
		},
		map[string]interface{}{
			"type": "function",
			"function": map[string]interface{}{
				"name":        "Agent",
				"description": "unsupported",
				"parameters": map[string]interface{}{
					"type": "object",
				},
			},
		},
	})

	if len(defs) != 1 {
		t.Fatalf("expected only supported tool to remain, got %d defs", len(defs))
	}
	if defs[0].Name != "Bash" {
		t.Fatalf("expected Bash, got %s", defs[0].Name)
	}
	if len([]rune(defs[0].Description)) > maxWarpToolDescLen {
		t.Fatalf("description not compacted: len=%d", len([]rune(defs[0].Description)))
	}
	props, ok := defs[0].Schema["properties"].(map[string]interface{})
	if !ok {
		t.Fatalf("expected schema properties, got %#v", defs[0].Schema["properties"])
	}
	if _, ok := props["command"]; !ok {
		t.Fatalf("expected command property to remain")
	}
	if _, ok := props["ignored"]; ok {
		t.Fatalf("expected ignored property to be dropped")
	}
	req, ok := defs[0].Schema["required"].([]interface{})
	if !ok || len(req) != 1 || req[0] != "command" {
		t.Fatalf("expected required to keep only command, got %#v", defs[0].Schema["required"])
	}
}

func TestBuildWarpQuery_CompactsDirectoryToolResults(t *testing.T) {
	t.Parallel()

	listing := strings.Join([]string{
		"/Users/dailin/Documents/GitHub/TEST/.DS_Store",
		"/Users/dailin/Documents/GitHub/TEST/.claude/settings.local.json",
		"/Users/dailin/Documents/GitHub/TEST/everything-claude-code/.git/config",
		"/Users/dailin/Documents/GitHub/TEST/everything-claude-code/README.md",
		"/Users/dailin/Documents/GitHub/TEST/everything-claude-code/src/main.ts",
		"/Users/dailin/Documents/GitHub/TEST/everything-claude-code/src/commands/build.ts",
		"/Users/dailin/Documents/GitHub/TEST/everything-claude-code/src/commands/review.ts",
		"/Users/dailin/Documents/GitHub/TEST/orchids_accounts.txt",
		"/Users/dailin/Documents/GitHub/TEST/test.py",
		"/Users/dailin/Documents/GitHub/TEST/tmp/cache/data.json",
		"/Users/dailin/Documents/GitHub/TEST/tmp/cache/index.json",
		"/Users/dailin/Documents/GitHub/TEST/tmp/cache/log.txt",
	}, "\n")

	query, isNew := buildWarpQuery("这个项目是干什么的", nil, []warpToolResult{{
		ToolCallID: "tool_1",
		Content:    listing,
	}}, true)
	if isNew {
		t.Fatalf("expected tool-result follow-up to reuse conversation context")
	}
	if strings.Contains(query, ".DS_Store") {
		t.Fatalf("directory summary unexpectedly kept .DS_Store: %q", query)
	}
	if strings.Contains(query, "/.git/") {
		t.Fatalf("directory summary unexpectedly kept git metadata: %q", query)
	}
	if !strings.Contains(query, "[directory listing summarized:") && !strings.Contains(query, "[directory listing trimmed:") {
		t.Fatalf("expected directory summary marker in %q", query)
	}
	if !strings.Contains(query, "./everything-claude-code/") {
		t.Fatalf("expected top-level directory summary in %q", query)
	}
	if !strings.Contains(query, "RESPONSE RULES:") {
		t.Fatalf("expected base response rules in %q", query)
	}
	if !strings.Contains(query, "TOOL RULES:") {
		t.Fatalf("expected tool rules in %q", query)
	}
}

func TestBuildWarpQuery_FollowupStripsHistoricalToolCalls(t *testing.T) {
	t.Parallel()

	query, isNew := buildWarpQuery(
		"这个项目是干什么的",
		[]warpHistoryMessage{
			{Role: "user", Content: "这个项目是干什么的"},
			{
				Role:    "assistant",
				Content: "",
				ToolCalls: []warpToolCall{{
					ID:        "call_1",
					Name:      "Read",
					Arguments: `{"file_path":"README.md"}`,
				}},
			},
			{Role: "tool", ToolCallID: "call_1", Content: "README content"},
		},
		[]warpToolResult{{
			ToolCallID: "call_2",
			Content:    "./README.md\n./main.go",
		}},
		true,
	)
	if isNew {
		t.Fatalf("expected follow-up query, got new conversation")
	}
	if strings.Contains(query, "<tool_use") {
		t.Fatalf("expected historical tool_use to be stripped from follow-up query: %q", query)
	}
	if strings.Contains(query, "<tool_result tool_call_id=\"call_1\">") {
		t.Fatalf("expected historical tool_result to be stripped from follow-up query: %q", query)
	}
	if !strings.Contains(query, "User: 这个项目是干什么的") {
		t.Fatalf("expected original user intent to remain in follow-up query: %q", query)
	}
	if !strings.Contains(query, "<tool_result id=\"call_2\">") {
		t.Fatalf("expected current tool_result to remain in follow-up query: %q", query)
	}
}

func TestExtractWarpConversation_LastUserTextAndToolResultRemainCurrent(t *testing.T) {
	t.Parallel()

	userText, history, toolResults, err := extractWarpConversation([]prompt.Message{
		{
			Role: "assistant",
			Content: prompt.MessageContent{
				Blocks: []prompt.ContentBlock{{
					Type:  "tool_use",
					ID:    "tool_1",
					Name:  "Glob",
					Input: map[string]interface{}{"path": "/tmp", "pattern": "*"},
				}},
			},
		},
		{
			Role: "user",
			Content: prompt.MessageContent{
				Blocks: []prompt.ContentBlock{
					{Type: "tool_result", ToolUseID: "tool_1", Content: "./README.md\n./main.go"},
					{Type: "text", Text: "这个项目是干什么的"},
				},
			},
		},
	}, "")
	if err != nil {
		t.Fatalf("extractWarpConversation: %v", err)
	}
	if userText != "这个项目是干什么的" {
		t.Fatalf("unexpected userText: %q", userText)
	}
	if len(toolResults) != 1 {
		t.Fatalf("expected current tool_result, got %d", len(toolResults))
	}
	if len(history) != 1 || history[0].Role != "assistant" {
		t.Fatalf("expected only assistant tool_use in history, got %#v", history)
	}
}

func TestEstimateInputTokens_TreatsLastUserToolResultAsFollowup(t *testing.T) {
	t.Parallel()

	estimate, err := EstimateInputTokens("", "auto-efficient", []prompt.Message{
		{
			Role: "assistant",
			Content: prompt.MessageContent{
				Blocks: []prompt.ContentBlock{{
					Type:  "tool_use",
					ID:    "tool_1",
					Name:  "Glob",
					Input: map[string]interface{}{"path": "/tmp", "pattern": "*"},
				}},
			},
		},
		{
			Role: "user",
			Content: prompt.MessageContent{
				Blocks: []prompt.ContentBlock{
					{Type: "tool_result", ToolUseID: "tool_1", Content: "./README.md\n./main.go"},
					{Type: "text", Text: "这个项目是干什么的"},
				},
			},
		},
	}, nil, true)
	if err != nil {
		t.Fatalf("EstimateInputTokens: %v", err)
	}
	if estimate.Profile != "warp-tool-result" {
		t.Fatalf("expected warp-tool-result profile, got %+v", estimate)
	}
}

func TestFormatWarpHistory_CompactsHistoricalToolResults(t *testing.T) {
	t.Parallel()

	var lines []string
	for i := 0; i < 18; i++ {
		lines = append(lines, "line "+strings.Repeat("x", 30))
	}
	history := []warpHistoryMessage{{
		Role:       "tool",
		ToolCallID: "tool_2",
		Content:    strings.Join(lines, "\n"),
	}}

	parts := formatWarpHistory(history)
	if len(parts) != 1 {
		t.Fatalf("expected one history entry, got %d", len(parts))
	}
	if !strings.Contains(parts[0], "[tool_result summary: omitted") {
		t.Fatalf("expected historical tool_result summary marker in %q", parts[0])
	}
	if strings.Count(parts[0], "line ") >= len(lines) {
		t.Fatalf("expected historical tool_result to be compacted, got %q", parts[0])
	}
}
