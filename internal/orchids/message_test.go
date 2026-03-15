package orchids

import (
	"strings"
	"testing"

	"orchids-api/internal/prompt"
)

func TestExtractOrchidsMessageContent_ToolResultOnly(t *testing.T) {
	t.Parallel()

	content := []interface{}{
		map[string]interface{}{
			"type":        "tool_result",
			"tool_use_id": "tool_1",
			"content":     "<tool_use_error>boom</tool_use_error>\ncontent line",
		},
	}

	text, toolResultOnly := extractOrchidsMessageContent(content, "slice")
	if !toolResultOnly {
		t.Fatal("toolResultOnly=false want true")
	}
	if strings.TrimSpace(text) != "" {
		t.Fatalf("text=%q want empty for raw tool-result-only extraction", text)
	}
}

func TestExtractOrchidsMessageContent_PreservesSystemReminderText(t *testing.T) {
	t.Parallel()

	text, toolResultOnly := extractOrchidsMessageContent("<system-reminder>keep me</system-reminder>", "string")
	if toolResultOnly {
		t.Fatal("toolResultOnly=true want false")
	}
	if text != "<system-reminder>keep me</system-reminder>" {
		t.Fatalf("text=%q want raw string content", text)
	}
}

func TestBuildOrchidsConversationHistory_ExcludesCurrentUserTurn(t *testing.T) {
	t.Parallel()

	messages := []prompt.Message{
		{Role: "user", Content: prompt.MessageContent{Text: "first"}},
		{Role: "assistant", Content: prompt.MessageContent{Text: "second"}},
		{Role: "user", Content: prompt.MessageContent{Text: "current"}},
	}

	conversation := buildOrchidsConversationMessages(messages)
	currentUserIdx := findCurrentOrchidsUserMessageIndex(conversation)
	history := buildOrchidsConversationHistory(conversation, currentUserIdx)

	if len(history) != 2 {
		t.Fatalf("len(history)=%d want 2", len(history))
	}
	if history[0]["content"] != "first" || history[1]["content"] != "second" {
		t.Fatalf("history=%#v want prior turns only", history)
	}
}

func TestExtractOrchidsAttachmentURLs_DedupesURLs(t *testing.T) {
	t.Parallel()

	messages := []prompt.Message{
		{
			Role: "user",
			Content: prompt.MessageContent{
				Blocks: []prompt.ContentBlock{
					{Type: "image", URL: "https://example.com/a.png"},
					{Type: "document", URL: "https://example.com/a.png"},
					{Type: "document", URL: "https://example.com/spec.pdf"},
				},
			},
		},
	}

	urls := extractOrchidsAttachmentURLs(messages)
	if len(urls) != 2 {
		t.Fatalf("len(urls)=%d want 2", len(urls))
	}
	if urls[0] != "https://example.com/a.png" || urls[1] != "https://example.com/spec.pdf" {
		t.Fatalf("urls=%#v want deduped stable order", urls)
	}
}
