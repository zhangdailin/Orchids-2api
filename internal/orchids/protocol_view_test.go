package orchids

import (
	"strings"
	"testing"

	"orchids-api/internal/prompt"
)

func TestBuildCodeFreeMaxPromptAndHistoryWithMeta_UsesProtocolProfile(t *testing.T) {
	t.Parallel()

	messages := []prompt.Message{
		{Role: "user", Content: prompt.MessageContent{Text: "first request"}},
		{
			Role: "assistant",
			Content: prompt.MessageContent{
				Blocks: []prompt.ContentBlock{
					{Type: "tool_use", ID: "tool_1", Name: "Read", Input: map[string]interface{}{"file_path": "/tmp/demo.txt"}},
				},
			},
		},
		{
			Role: "user",
			Content: prompt.MessageContent{
				Blocks: []prompt.ContentBlock{
					{Type: "tool_result", ToolUseID: "tool_1", Content: "demo result"},
				},
			},
		},
		{Role: "assistant", Content: prompt.MessageContent{Text: "done"}},
		{Role: "user", Content: prompt.MessageContent{Text: "second request"}},
	}
	system := []prompt.SystemItem{{Type: "text", Text: "system rules"}}

	promptText, history, meta := BuildCodeFreeMaxPromptAndHistoryWithMeta(messages, system, false)

	if meta.Profile != promptProfileOrchidsProtocol {
		t.Fatalf("profile=%q want %q", meta.Profile, promptProfileOrchidsProtocol)
	}
	if meta.NoThinking {
		t.Fatalf("NoThinking=%v want false", meta.NoThinking)
	}
	if !strings.Contains(promptText, "<sys>") || !strings.Contains(promptText, "system rules") {
		t.Fatalf("promptText=%q want system context", promptText)
	}
	if !strings.Contains(promptText, "<user>") || !strings.Contains(promptText, "second request") {
		t.Fatalf("promptText=%q want latest user text", promptText)
	}
	if len(history) < 3 {
		t.Fatalf("history len=%d want at least 3", len(history))
	}
	if !strings.Contains(history[1]["content"], `<tool_use id="tool_1" name="Read"`) {
		t.Fatalf("assistant history=%q want tool_use marker", history[1]["content"])
	}
}

func TestBuildCodeFreeMaxPromptAndHistoryWithMeta_ToolResultOnlyTurnStaysRaw(t *testing.T) {
	t.Parallel()

	messages := []prompt.Message{
		{Role: "user", Content: prompt.MessageContent{Text: "analyze this file"}},
		{
			Role: "assistant",
			Content: prompt.MessageContent{
				Blocks: []prompt.ContentBlock{
					{Type: "tool_use", ID: "tool_1", Name: "Read", Input: map[string]interface{}{"file_path": "/tmp/demo.txt"}},
				},
			},
		},
		{
			Role: "user",
			Content: prompt.MessageContent{
				Blocks: []prompt.ContentBlock{
					{Type: "tool_result", ToolUseID: "tool_1", Content: "package main\nfunc main() {}"},
				},
			},
		},
	}

	promptText, _, meta := BuildCodeFreeMaxPromptAndHistoryWithMeta(messages, nil, true)

	if meta.Profile != promptProfileOrchidsProtocol {
		t.Fatalf("profile=%q want %q", meta.Profile, promptProfileOrchidsProtocol)
	}
	if !meta.NoThinking {
		t.Fatalf("NoThinking=%v want true", meta.NoThinking)
	}
	if strings.Contains(promptText, "Original user request:") {
		t.Fatalf("promptText=%q unexpectedly contains local follow-up prompt", promptText)
	}
	if strings.Contains(promptText, "Tool result:") {
		t.Fatalf("promptText=%q unexpectedly contains tool result section", promptText)
	}
	if strings.TrimSpace(promptText) != "" {
		t.Fatalf("promptText=%q want empty raw prompt for tool-result-only current turn", promptText)
	}
}
