package orchids

import (
	"strings"
	"testing"

	"orchids-api/internal/prompt"
)

func TestBuildLocalAssistantPrompt_ContainsSingleResultGuideline(t *testing.T) {
	t.Parallel()

	prompt := buildLocalAssistantPrompt("", "hello", "", "")
	if !strings.Contains(prompt, "工具执行成功后只输出一次简短结果") {
		t.Fatalf("expected Chinese single-result guideline to be present")
	}
	if !strings.Contains(prompt, "After tool success, emit one concise completion message only") {
		t.Fatalf("expected English single-result guideline to be present")
	}
	if !strings.Contains(prompt, "no matches found / No such file or directory") {
		t.Fatalf("expected delete no-op guideline to be present")
	}
	if !strings.Contains(prompt, "treat as idempotent no-op and do not rerun the same delete command") {
		t.Fatalf("expected English no-op delete guideline to be present")
	}
	if !strings.Contains(prompt, "EOFError: EOF when reading a line") {
		t.Fatalf("expected EOFError guideline to be present")
	}
	if !strings.Contains(prompt, "If a command fails with interactive stdin errors") {
		t.Fatalf("expected English EOFError no-rerun guideline to be present")
	}
}

func TestBuildLocalAssistantPrompt_IsCompact(t *testing.T) {
	t.Parallel()

	promptText := buildLocalAssistantPrompt("", "hello", "claude-opus-4-6", "/tmp/project")
	if got := len([]rune(promptText)); got > 3500 {
		t.Fatalf("expected compact local assistant prompt <= 3500 runes, got %d", got)
	}
}

func TestBuildAIClientPromptAndHistory_NoLargeCarryoverOnToolResultTurn(t *testing.T) {
	t.Parallel()

	const marker = "LONG_PREVIOUS_USER_TEXT_MARKER"
	messages := []prompt.Message{
		{
			Role: "user",
			Content: prompt.MessageContent{
				Text: marker + strings.Repeat("x", 2000),
			},
		},
		{
			Role: "assistant",
			Content: prompt.MessageContent{
				Blocks: []prompt.ContentBlock{
					{Type: "tool_use", Name: "Read", Input: map[string]interface{}{"file_path": "a.txt"}},
				},
			},
		},
		{
			Role: "user",
			Content: prompt.MessageContent{
				Blocks: []prompt.ContentBlock{
					{Type: "tool_result", ToolUseID: "tool-1", Content: strings.Repeat("tool output ", 200)},
				},
			},
		},
	}

	builtPrompt, _ := BuildAIClientPromptAndHistory(messages, nil, "claude-opus-4-6", false, "/tmp/project", 12000)
	if strings.Contains(builtPrompt, marker) {
		t.Fatalf("expected previous large user text not to be carried over in tool-result-only turn")
	}
}
