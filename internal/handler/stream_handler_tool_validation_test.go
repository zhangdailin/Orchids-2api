package handler

import (
	"net/http/httptest"
	"strings"
	"testing"

	"orchids-api/internal/adapter"
	"orchids-api/internal/config"
	"orchids-api/internal/debug"
	"orchids-api/internal/prompt"
	"orchids-api/internal/upstream"
)

func TestHasRequiredToolInput(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name     string
		tool     string
		input    string
		expected bool
	}{
		{name: "edit empty json", tool: "Edit", input: `{}`, expected: false},
		{name: "edit missing old/new", tool: "Edit", input: `{"file_path":"/tmp/a"}`, expected: false},
		{name: "edit valid", tool: "Edit", input: `{"file_path":"/tmp/a","old_string":"a","new_string":"b"}`, expected: true},
		{name: "write empty json", tool: "Write", input: `{}`, expected: false},
		{name: "write valid", tool: "Write", input: `{"file_path":"/tmp/a","content":"x"}`, expected: true},
		{name: "bash empty", tool: "Bash", input: `{}`, expected: false},
		{name: "bash valid", tool: "Bash", input: `{"command":"ls"}`, expected: true},
		{name: "unknown tool malformed json", tool: "Unknown", input: `{`, expected: true},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := hasRequiredToolInput(tc.tool, tc.input)
			if got != tc.expected {
				t.Fatalf("hasRequiredToolInput(%q, %q) = %v, want %v", tc.tool, tc.input, got, tc.expected)
			}
		})
	}
}

func TestToolCallSameIDInvalidThenValid_UsesValidOne(t *testing.T) {
	t.Parallel()

	h := newStreamHandler(
		&config.Config{OutputTokenMode: "final"},
		httptest.NewRecorder(),
		debug.New(false, false),
		false,
		false, // non-stream mode for easier assertions
		adapter.FormatAnthropic,
		"",
	)
	defer h.release()

	// First frame is incomplete and should be rejected.
	h.handleMessage(upstream.SSEMessage{
		Type: "model.tool-call",
		Event: map[string]interface{}{
			"toolCallId": "tool_same_id",
			"toolName":   "Edit",
			"input":      "{}",
		},
	})

	// Second frame (same toolCallId) is valid and should be accepted.
	h.handleMessage(upstream.SSEMessage{
		Type: "model.tool-call",
		Event: map[string]interface{}{
			"toolCallId": "tool_same_id",
			"toolName":   "Write",
			"input":      `{"file_path":"/tmp/a.txt","content":"x"}`,
		},
	})

	h.handleMessage(upstream.SSEMessage{
		Type:  "model.finish",
		Event: map[string]interface{}{"finishReason": "tool_use"},
	})

	if len(h.contentBlocks) != 1 {
		t.Fatalf("expected 1 content block, got %d", len(h.contentBlocks))
	}

	block := h.contentBlocks[0]
	if got, _ := block["type"].(string); got != "tool_use" {
		t.Fatalf("expected tool_use block, got %q", got)
	}
	if got, _ := block["name"].(string); got != "Write" {
		t.Fatalf("expected Write tool call, got %q", got)
	}
}

func TestWriteToolCallDifferentIDsSameInput_Deduped(t *testing.T) {
	t.Parallel()

	h := newStreamHandler(
		&config.Config{OutputTokenMode: "final"},
		httptest.NewRecorder(),
		debug.New(false, false),
		false,
		false, // non-stream mode for easier assertions
		adapter.FormatAnthropic,
		"",
	)
	defer h.release()

	input := `{"file_path":"/tmp/a.txt","content":"x"}`
	h.handleMessage(upstream.SSEMessage{
		Type: "model.tool-call",
		Event: map[string]interface{}{
			"toolCallId": "tool_id_1",
			"toolName":   "Write",
			"input":      input,
		},
	})
	h.handleMessage(upstream.SSEMessage{
		Type: "model.tool-call",
		Event: map[string]interface{}{
			"toolCallId": "tool_id_2",
			"toolName":   "Write",
			"input":      input,
		},
	})

	h.handleMessage(upstream.SSEMessage{
		Type:  "model.finish",
		Event: map[string]interface{}{"finishReason": "tool_use"},
	})

	if len(h.contentBlocks) != 1 {
		t.Fatalf("expected 1 content block, got %d", len(h.contentBlocks))
	}
	block := h.contentBlocks[0]
	if got, _ := block["type"].(string); got != "tool_use" {
		t.Fatalf("expected tool_use block, got %q", got)
	}
	if got, _ := block["name"].(string); got != "Write" {
		t.Fatalf("expected Write tool call, got %q", got)
	}
}

func TestReadToolCallDifferentIDsSameInput_BothAccepted(t *testing.T) {
	t.Parallel()

	h := newStreamHandler(
		&config.Config{OutputTokenMode: "final"},
		httptest.NewRecorder(),
		debug.New(false, false),
		false,
		false, // non-stream mode for easier assertions
		adapter.FormatAnthropic,
		"",
	)
	defer h.release()

	input := `{"file_path":"/tmp/a.txt"}`
	h.handleMessage(upstream.SSEMessage{
		Type: "model.tool-call",
		Event: map[string]interface{}{
			"toolCallId": "read_id_1",
			"toolName":   "Read",
			"input":      input,
		},
	})
	h.handleMessage(upstream.SSEMessage{
		Type: "model.tool-call",
		Event: map[string]interface{}{
			"toolCallId": "read_id_2",
			"toolName":   "Read",
			"input":      input,
		},
	})

	h.handleMessage(upstream.SSEMessage{
		Type:  "model.finish",
		Event: map[string]interface{}{"finishReason": "tool_use"},
	})

	if len(h.contentBlocks) != 2 {
		t.Fatalf("expected 2 content blocks, got %d", len(h.contentBlocks))
	}
}

func TestWriteToolCallDifferentIDsDifferentContent_BothAccepted(t *testing.T) {
	t.Parallel()

	h := newStreamHandler(
		&config.Config{OutputTokenMode: "final"},
		httptest.NewRecorder(),
		debug.New(false, false),
		false,
		false, // non-stream mode for easier assertions
		adapter.FormatAnthropic,
		"",
	)
	defer h.release()

	h.handleMessage(upstream.SSEMessage{
		Type: "model.tool-call",
		Event: map[string]interface{}{
			"toolCallId": "write_id_1",
			"toolName":   "Write",
			"input":      `{"file_path":"/tmp/a.txt","content":"x"}`,
		},
	})
	h.handleMessage(upstream.SSEMessage{
		Type: "model.tool-call",
		Event: map[string]interface{}{
			"toolCallId": "write_id_2",
			"toolName":   "Write",
			"input":      `{"file_path":"/tmp/a.txt","content":"y"}`,
		},
	})

	h.handleMessage(upstream.SSEMessage{
		Type:  "model.finish",
		Event: map[string]interface{}{"finishReason": "tool_use"},
	})

	if len(h.contentBlocks) != 2 {
		t.Fatalf("expected 2 content blocks, got %d", len(h.contentBlocks))
	}
}

func TestBashToolCallDifferentIDsSameCommand_Deduped(t *testing.T) {
	t.Parallel()

	h := newStreamHandler(
		&config.Config{OutputTokenMode: "final"},
		httptest.NewRecorder(),
		debug.New(false, false),
		false,
		false, // non-stream mode for easier assertions
		adapter.FormatAnthropic,
		"",
	)
	defer h.release()

	input := `{"command":"rm /Users/dailin/Documents/GitHub/TEST/calculator.py"}`
	h.handleMessage(upstream.SSEMessage{
		Type: "model.tool-call",
		Event: map[string]interface{}{
			"toolCallId": "bash_id_1",
			"toolName":   "Bash",
			"input":      input,
		},
	})
	h.handleMessage(upstream.SSEMessage{
		Type: "model.tool-call",
		Event: map[string]interface{}{
			"toolCallId": "bash_id_2",
			"toolName":   "Bash",
			"input":      input,
		},
	})

	h.handleMessage(upstream.SSEMessage{
		Type:  "model.finish",
		Event: map[string]interface{}{"finishReason": "tool_use"},
	})

	if len(h.contentBlocks) != 1 {
		t.Fatalf("expected 1 content block, got %d", len(h.contentBlocks))
	}
	if got, _ := h.contentBlocks[0]["name"].(string); got != "Bash" {
		t.Fatalf("expected Bash tool call, got %q", got)
	}
}

func TestBashToolCallDifferentIDsDifferentCommands_BothAccepted(t *testing.T) {
	t.Parallel()

	h := newStreamHandler(
		&config.Config{OutputTokenMode: "final"},
		httptest.NewRecorder(),
		debug.New(false, false),
		false,
		false, // non-stream mode for easier assertions
		adapter.FormatAnthropic,
		"",
	)
	defer h.release()

	h.handleMessage(upstream.SSEMessage{
		Type: "model.tool-call",
		Event: map[string]interface{}{
			"toolCallId": "bash_id_1",
			"toolName":   "Bash",
			"input":      `{"command":"pwd"}`,
		},
	})
	h.handleMessage(upstream.SSEMessage{
		Type: "model.tool-call",
		Event: map[string]interface{}{
			"toolCallId": "bash_id_2",
			"toolName":   "Bash",
			"input":      `{"command":"ls -la"}`,
		},
	})

	h.handleMessage(upstream.SSEMessage{
		Type:  "model.finish",
		Event: map[string]interface{}{"finishReason": "tool_use"},
	})

	if len(h.contentBlocks) != 2 {
		t.Fatalf("expected 2 content blocks, got %d", len(h.contentBlocks))
	}
}

func TestToolCallMissingID_UsesFallbackAndIsAccepted(t *testing.T) {
	t.Parallel()

	h := newStreamHandler(
		&config.Config{OutputTokenMode: "final"},
		httptest.NewRecorder(),
		debug.New(false, false),
		false,
		false, // non-stream mode for easier assertions
		adapter.FormatAnthropic,
		"",
	)
	defer h.release()

	h.handleMessage(upstream.SSEMessage{
		Type: "model.tool-call",
		Event: map[string]interface{}{
			"toolName": "Bash",
			"input":    `{"command":"pwd"}`,
		},
	})

	h.handleMessage(upstream.SSEMessage{
		Type:  "model.finish",
		Event: map[string]interface{}{"finishReason": "tool_use"},
	})

	if len(h.contentBlocks) != 1 {
		t.Fatalf("expected 1 content block, got %d", len(h.contentBlocks))
	}
	if got, _ := h.contentBlocks[0]["name"].(string); got != "Bash" {
		t.Fatalf("expected Bash tool call, got %q", got)
	}
}

func TestMaskDedupKey_DoesNotLeakRawCommand(t *testing.T) {
	t.Parallel()

	raw := "bash:rm /Users/dailin/Documents/GitHub/TEST/calculator.py"
	masked := maskDedupKey(raw)
	if strings.Contains(masked, "rm ") || strings.Contains(masked, "calculator.py") {
		t.Fatalf("masked key leaks raw command/path: %q", masked)
	}
	if !strings.HasPrefix(masked, "bash#") {
		t.Fatalf("unexpected masked key prefix: %q", masked)
	}
}

func TestSeedSideEffectDedupFromMessages_SuppressRepeatDeleteAcrossTurns(t *testing.T) {
	t.Parallel()

	h := newStreamHandler(
		&config.Config{OutputTokenMode: "final"},
		httptest.NewRecorder(),
		debug.New(false, false),
		false,
		false,
		adapter.FormatAnthropic,
		"",
	)
	defer h.release()

	history := []prompt.Message{
		{Role: "user", Content: prompt.MessageContent{Text: "删除这个文件"}},
		{
			Role: "assistant",
			Content: prompt.MessageContent{
				Blocks: []prompt.ContentBlock{
					{
						Type:  "tool_use",
						ID:    "tool_old_1",
						Name:  "Bash",
						Input: map[string]interface{}{"command": "rm /Users/dailin/Documents/GitHub/TEST/calculator.py"},
					},
				},
			},
		},
		{
			Role: "user",
			Content: prompt.MessageContent{
				Blocks: []prompt.ContentBlock{
					{
						Type:      "tool_result",
						ToolUseID: "tool_old_1",
						Content:   "Done",
					},
				},
			},
		},
	}
	h.seedSideEffectDedupFromMessages(history)

	h.handleMessage(upstream.SSEMessage{
		Type: "model.tool-call",
		Event: map[string]interface{}{
			"toolCallId": "tool_new_1",
			"toolName":   "Bash",
			"input":      `{"command":"rm /Users/dailin/Documents/GitHub/TEST/calculator.py"}`,
		},
	})
	h.handleMessage(upstream.SSEMessage{
		Type:  "model.finish",
		Event: map[string]interface{}{"finishReason": "tool_use"},
	})

	if len(h.contentBlocks) != 0 {
		t.Fatalf("expected repeated delete tool call to be suppressed, got %d blocks", len(h.contentBlocks))
	}
	if h.toolDedupCount != 1 {
		t.Fatalf("expected dedup count 1, got %d", h.toolDedupCount)
	}
}

func TestSeedSideEffectDedupFromMessages_DoesNotUseOlderTurnBeforeLatestUserText(t *testing.T) {
	t.Parallel()

	h := newStreamHandler(
		&config.Config{OutputTokenMode: "final"},
		httptest.NewRecorder(),
		debug.New(false, false),
		false,
		false,
		adapter.FormatAnthropic,
		"",
	)
	defer h.release()

	history := []prompt.Message{
		{Role: "user", Content: prompt.MessageContent{Text: "先删除A"}},
		{
			Role: "assistant",
			Content: prompt.MessageContent{
				Blocks: []prompt.ContentBlock{
					{
						Type:  "tool_use",
						ID:    "tool_old_1",
						Name:  "Bash",
						Input: map[string]interface{}{"command": "rm /tmp/a.txt"},
					},
				},
			},
		},
		{Role: "user", Content: prompt.MessageContent{Text: "现在处理B"}},
	}
	h.seedSideEffectDedupFromMessages(history)

	h.handleMessage(upstream.SSEMessage{
		Type: "model.tool-call",
		Event: map[string]interface{}{
			"toolCallId": "tool_new_1",
			"toolName":   "Bash",
			"input":      `{"command":"rm /tmp/a.txt"}`,
		},
	})
	h.handleMessage(upstream.SSEMessage{
		Type:  "model.finish",
		Event: map[string]interface{}{"finishReason": "tool_use"},
	})

	if len(h.contentBlocks) != 1 {
		t.Fatalf("expected old-turn command not pre-deduped, got %d blocks", len(h.contentBlocks))
	}
	if got, _ := h.contentBlocks[0]["name"].(string); got != "Bash" {
		t.Fatalf("expected Bash tool call, got %q", got)
	}
}
