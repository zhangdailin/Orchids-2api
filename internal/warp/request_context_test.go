package warp

import (
	"strings"
	"testing"

	"orchids-api/internal/prompt"
)

func TestBuildRequestBytes_UsesProvidedWorkdir(t *testing.T) {
	t.Parallel()

	reqBytes, err := buildRequestBytes("check cwd", "auto", nil, nil, false, "/Users/dailin/Documents/GitHub/Orchids-2api", "")
	if err != nil {
		t.Fatalf("buildRequestBytes: %v", err)
	}

	s := string(reqBytes)
	if !strings.Contains(s, "/Users/dailin/Documents/GitHub/Orchids-2api") {
		t.Fatalf("expected dynamic workdir in request bytes")
	}
	if strings.Contains(s, "/Users/lofyer") {
		t.Fatalf("unexpected hardcoded /Users/lofyer in request bytes")
	}
}

func TestBuildWarpQuery_IncludesSingleResultPrompt(t *testing.T) {
	t.Parallel()

	query, isNew := buildWarpQuery("帮我用python写一个计算器", nil, nil, false)
	if isNew != true {
		t.Fatalf("expected new conversation when no history/tool results")
	}
	if !strings.Contains(query, singleResultPrompt) {
		t.Fatalf("expected singleResultPrompt to be included")
	}
	if !strings.Contains(query, "帮我用python写一个计算器") {
		t.Fatalf("expected user query to be included")
	}
}

func TestBuildWarpQuery_DisableToolsStillIncludesStylePrompt(t *testing.T) {
	t.Parallel()

	query, _ := buildWarpQuery("hello", nil, nil, true)
	if !strings.Contains(query, singleResultPrompt) {
		t.Fatalf("expected singleResultPrompt to be included")
	}
	if !strings.Contains(query, noWarpToolsPrompt) {
		t.Fatalf("expected noWarpToolsPrompt when disableWarpTools=true")
	}
}

func TestSingleResultPrompt_ContainsNoopDeleteGuidance(t *testing.T) {
	t.Parallel()

	if !strings.Contains(singleResultPrompt, "no matches found") {
		t.Fatalf("expected delete no-op guidance for no matches found")
	}
	if !strings.Contains(singleResultPrompt, "No such file or directory") {
		t.Fatalf("expected delete no-op guidance for missing file")
	}
	if !strings.Contains(singleResultPrompt, "EOFError: EOF when reading a line") {
		t.Fatalf("expected interactive stdin guidance for EOFError")
	}
}

func TestIsNoiseToolResult_BenignNoopDeleteErrors(t *testing.T) {
	t.Parallel()

	cases := []string{
		"Error: Exit code 1\n(eval):1: no matches found: /Users/dailin/Documents/GitHub/TEST/*",
		"rm: /Users/dailin/Documents/GitHub/TEST/calculator.py: No such file or directory",
		"cannot remove '/tmp/a.txt': No such file or directory",
	}
	for _, tc := range cases {
		if !isNoiseToolResult(tc) {
			t.Fatalf("expected benign delete error to be treated as noise: %q", tc)
		}
	}
}

func TestIsNoiseToolResult_RealErrorsNotSuppressed(t *testing.T) {
	t.Parallel()

	cases := []string{
		"rm: /tmp/a.txt: Permission denied",
		"bash: syntax error near unexpected token",
	}
	for _, tc := range cases {
		if isNoiseToolResult(tc) {
			t.Fatalf("unexpected suppression of real error: %q", tc)
		}
	}
}

func TestExtractWarpConversation_DedupsRepeatedEOFErrorToolResultsAcrossIDs(t *testing.T) {
	t.Parallel()

	eof := "Traceback ...\nEOFError: EOF when reading a line"
	messages := []prompt.Message{
		{
			Role: "assistant",
			Content: prompt.MessageContent{
				Blocks: []prompt.ContentBlock{
					{Type: "tool_use", ID: "tool_1", Name: "Bash", Input: map[string]interface{}{"command": "python3 calculator.py"}},
				},
			},
		},
		{
			Role: "user",
			Content: prompt.MessageContent{
				Blocks: []prompt.ContentBlock{
					{Type: "tool_result", ToolUseID: "tool_1", Content: eof},
				},
			},
		},
		{
			Role: "assistant",
			Content: prompt.MessageContent{
				Blocks: []prompt.ContentBlock{
					{Type: "tool_use", ID: "tool_2", Name: "Bash", Input: map[string]interface{}{"command": "python3 calculator.py"}},
				},
			},
		},
		{
			Role: "user",
			Content: prompt.MessageContent{
				Blocks: []prompt.ContentBlock{
					{Type: "tool_result", ToolUseID: "tool_2", Content: eof},
				},
			},
		},
	}

	_, _, toolResults, err := extractWarpConversation(messages, "")
	if err != nil {
		t.Fatalf("extractWarpConversation: %v", err)
	}
	if len(toolResults) != 1 {
		t.Fatalf("expected 1 deduped tool result, got %d", len(toolResults))
	}
}

func TestExtractWarpConversation_KeepDifferentIDSameContentForNonInteractiveErrors(t *testing.T) {
	t.Parallel()

	errText := "rm: /tmp/a.txt: Permission denied"
	messages := []prompt.Message{
		{
			Role: "assistant",
			Content: prompt.MessageContent{
				Blocks: []prompt.ContentBlock{
					{Type: "tool_use", ID: "tool_1", Name: "Bash", Input: map[string]interface{}{"command": "rm /tmp/a.txt"}},
				},
			},
		},
		{
			Role: "user",
			Content: prompt.MessageContent{
				Blocks: []prompt.ContentBlock{
					{Type: "tool_result", ToolUseID: "tool_1", Content: errText},
				},
			},
		},
		{
			Role: "assistant",
			Content: prompt.MessageContent{
				Blocks: []prompt.ContentBlock{
					{Type: "tool_use", ID: "tool_2", Name: "Bash", Input: map[string]interface{}{"command": "rm /tmp/a.txt"}},
				},
			},
		},
		{
			Role: "user",
			Content: prompt.MessageContent{
				Blocks: []prompt.ContentBlock{
					{Type: "tool_result", ToolUseID: "tool_2", Content: errText},
				},
			},
		},
	}

	_, _, toolResults, err := extractWarpConversation(messages, "")
	if err != nil {
		t.Fatalf("extractWarpConversation: %v", err)
	}
	if len(toolResults) != 2 {
		t.Fatalf("expected 2 tool results for non-interactive error, got %d", len(toolResults))
	}
}
