package handler

import (
	"net/http/httptest"
	"path/filepath"
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

func TestWriteToolCallDifferentIDsSameWorkdirTarget_Deduped(t *testing.T) {
	t.Parallel()

	workdir := t.TempDir()
	h := newStreamHandler(
		&config.Config{OutputTokenMode: "final"},
		httptest.NewRecorder(),
		debug.New(false, false),
		false,
		false,
		adapter.FormatAnthropic,
		workdir,
	)
	defer h.release()

	relativeInput := `{"file_path":"calculator.py","content":"x"}`
	absoluteInput := `{"file_path":"` + strings.ReplaceAll(filepath.Join(workdir, "calculator.py"), `\`, `\\`) + `","content":"x"}`

	h.handleMessage(upstream.SSEMessage{
		Type: "model.tool-call",
		Event: map[string]interface{}{
			"toolCallId": "tool_rel",
			"toolName":   "Write",
			"input":      relativeInput,
		},
	})
	h.handleMessage(upstream.SSEMessage{
		Type: "model.tool-call",
		Event: map[string]interface{}{
			"toolCallId": "tool_abs",
			"toolName":   "Write",
			"input":      absoluteInput,
		},
	})

	h.handleMessage(upstream.SSEMessage{
		Type:  "model.finish",
		Event: map[string]interface{}{"finishReason": "tool_use"},
	})

	if len(h.contentBlocks) != 1 {
		t.Fatalf("expected 1 deduped content block, got %d: %v", len(h.contentBlocks), h.contentBlocks)
	}
	if h.toolDedupCount != 1 {
		t.Fatalf("expected dedup count 1, got %d", h.toolDedupCount)
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
		t.Fatalf("expected 2 content blocks, got %d: %v", len(h.contentBlocks), h.contentBlocks)
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

func TestToolCallNotDeclaredInCurrentRequest_IsSuppressed(t *testing.T) {
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
	h.setAllowedToolNames([]string{"Read", "Bash"})

	h.handleMessage(upstream.SSEMessage{
		Type: "model.tool-call",
		Event: map[string]interface{}{
			"toolCallId": "readfolder_1",
			"toolName":   "ReadFolder",
			"input":      `{"path":"/tmp"}`,
		},
	})
	h.handleMessage(upstream.SSEMessage{
		Type:  "model.finish",
		Event: map[string]interface{}{"finishReason": "tool_use"},
	})

	if len(h.contentBlocks) != 1 {
		t.Fatalf("expected undeclared tool call to be suppressed and fallback text, got %#v", h.contentBlocks)
	}
	if got, _ := h.contentBlocks[0]["text"].(string); !strings.Contains(got, "No output was presented") {
		t.Fatalf("expected fallback text block, got %q", got)
	}
	if h.suppressedToolCalls != 1 {
		t.Fatalf("suppressedToolCalls=%d want=1", h.suppressedToolCalls)
	}
	if h.finalStopReason != "end_turn" {
		t.Fatalf("finalStopReason=%q want end_turn", h.finalStopReason)
	}
}

func TestWriteToolCallNotDeclaredInCurrentRequest_IsSuppressed(t *testing.T) {
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
	h.setAllowedToolNames([]string{"Read", "Glob", "Grep"})

	h.handleMessage(upstream.SSEMessage{
		Type: "model.tool-call",
		Event: map[string]interface{}{
			"toolCallId": "write_undeclared_1",
			"toolName":   "Write",
			"input":      `{"file_path":"calculator.py","content":"print(1)"}`,
		},
	})
	h.handleMessage(upstream.SSEMessage{
		Type:  "model.finish",
		Event: map[string]interface{}{"finishReason": "tool_use"},
	})

	if len(h.contentBlocks) != 1 {
		t.Fatalf("expected undeclared Write tool call to be suppressed and replaced with fallback text, got %#v", h.contentBlocks)
	}
	if got, _ := h.contentBlocks[0]["text"].(string); !strings.Contains(got, "No output was presented") {
		t.Fatalf("expected fallback text block, got %q", got)
	}
	if h.suppressedToolCalls != 1 {
		t.Fatalf("suppressedToolCalls=%d want=1", h.suppressedToolCalls)
	}
	if h.finalStopReason != "end_turn" {
		t.Fatalf("finalStopReason=%q want end_turn", h.finalStopReason)
	}
}

func TestSandboxMetadataReadToolCall_IsSuppressed(t *testing.T) {
	t.Parallel()

	h := newStreamHandler(
		&config.Config{OutputTokenMode: "final"},
		httptest.NewRecorder(),
		debug.New(false, false),
		false,
		false,
		adapter.FormatAnthropic,
		`D:\Code\Orchids-2api`,
	)
	defer h.release()
	h.setAllowedToolNames([]string{"Read", "Bash"})

	h.handleMessage(upstream.SSEMessage{
		Type: "model.tool-call",
		Event: map[string]interface{}{
			"toolCallId": "sandbox_meta_read",
			"toolName":   "Read",
			"input":      `{"file_path":"/tmp/cc-agent/sb1-demo/.claude/.claude.json"}`,
		},
	})
	h.handleMessage(upstream.SSEMessage{
		Type:  "model.finish",
		Event: map[string]interface{}{"finishReason": "tool_use"},
	})

	if len(h.contentBlocks) != 1 {
		t.Fatalf("expected suppressed tool call to fall back to a single text block, got %#v", h.contentBlocks)
	}
	if got, _ := h.contentBlocks[0]["text"].(string); !strings.Contains(got, "No output was presented") {
		t.Fatalf("expected fallback text block, got %q", got)
	}
	if h.suppressedToolCalls != 1 {
		t.Fatalf("suppressedToolCalls=%d want=1", h.suppressedToolCalls)
	}
	if h.finalStopReason != "end_turn" {
		t.Fatalf("finalStopReason=%q want end_turn", h.finalStopReason)
	}
}

func TestTodoWriteToolCall_IsSuppressedWhenNotDeclared(t *testing.T) {
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
	h.setAllowedToolNames([]string{"Read", "Write", "Edit", "Bash", "Glob", "Grep"})

	h.handleMessage(upstream.SSEMessage{
		Type: "model.tool-call",
		Event: map[string]interface{}{
			"toolCallId": "todo_1",
			"toolName":   "TodoWrite",
			"input":      `{"todos":[{"content":"Create calculator app with scientific notation support","status":"in_progress"}]}`,
		},
	})
	h.handleMessage(upstream.SSEMessage{
		Type:  "model.finish",
		Event: map[string]interface{}{"finishReason": "tool_use"},
	})

	if len(h.contentBlocks) != 1 {
		t.Fatalf("expected TodoWrite tool call to be suppressed and replaced with fallback text, got %#v", h.contentBlocks)
	}
	if got, _ := h.contentBlocks[0]["text"].(string); !strings.Contains(got, "No output was presented") {
		t.Fatalf("expected fallback text block, got %q", got)
	}
	if h.suppressedToolCalls != 1 {
		t.Fatalf("suppressedToolCalls=%d want=1", h.suppressedToolCalls)
	}
	if h.finalStopReason != "end_turn" {
		t.Fatalf("finalStopReason=%q want end_turn", h.finalStopReason)
	}
}

func TestTaskToolCall_IsAcceptedWhenClientDeclaredAgent(t *testing.T) {
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

	h.setAllowedToolNames(passthroughAllowedToolNames([]interface{}{
		map[string]interface{}{"name": "Agent"},
	}, true))

	h.handleMessage(upstream.SSEMessage{
		Type: "model.tool-call",
		Event: map[string]interface{}{
			"toolCallId": "task_1",
			"toolName":   "Task",
			"input":      `{"description":"Explore calculator codebase","prompt":"Find calculator files","subagent_type":"Explore"}`,
		},
	})
	h.handleMessage(upstream.SSEMessage{
		Type:  "model.finish",
		Event: map[string]interface{}{"finishReason": "tool_use"},
	})

	if len(h.contentBlocks) != 1 {
		t.Fatalf("expected Task tool call to pass through, got %#v", h.contentBlocks)
	}
	if got, _ := h.contentBlocks[0]["type"].(string); got != "tool_use" {
		t.Fatalf("expected tool_use block, got %q", got)
	}
	if got, _ := h.contentBlocks[0]["name"].(string); got != "Task" {
		t.Fatalf("expected Task tool call, got %q", got)
	}
	if h.suppressedToolCalls != 0 {
		t.Fatalf("suppressedToolCalls=%d want=0", h.suppressedToolCalls)
	}
}

func TestBoltCustomMCPWebSearchToolCall_MapsToDeclaredWebSearch(t *testing.T) {
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

	h.setAllowedToolNames([]string{"web_search"})

	h.handleMessage(upstream.SSEMessage{
		Type: "model.tool-call",
		Event: map[string]interface{}{
			"toolCallId": "ws_1",
			"toolName":   "mcp__tavily__web_search",
			"input":      `{"query":"Akron Ohio weather today March 29 2026 why so cold","timeRange":"day"}`,
		},
	})
	h.handleMessage(upstream.SSEMessage{
		Type:  "model.finish",
		Event: map[string]interface{}{"finishReason": "tool_use"},
	})

	if len(h.contentBlocks) != 1 {
		t.Fatalf("expected mapped web_search tool call to pass through, got %#v", h.contentBlocks)
	}
	if got, _ := h.contentBlocks[0]["type"].(string); got != "tool_use" {
		t.Fatalf("expected tool_use block, got %q", got)
	}
	if got, _ := h.contentBlocks[0]["name"].(string); got != "web_search" {
		t.Fatalf("expected mapped tool name web_search, got %q", got)
	}
	if h.suppressedToolCalls != 0 {
		t.Fatalf("suppressedToolCalls=%d want=0", h.suppressedToolCalls)
	}
}

func TestBoltCustomMCPFetchToolCall_MapsToDeclaredWebFetch(t *testing.T) {
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

	h.setAllowedToolNames([]string{"web_fetch"})

	h.handleMessage(upstream.SSEMessage{
		Type: "model.tool-call",
		Event: map[string]interface{}{
			"toolCallId": "wf_1",
			"toolName":   "mcp__fetch__fetch",
			"input":      `{"url":"https://example.com","max_length":4000}`,
		},
	})
	h.handleMessage(upstream.SSEMessage{
		Type:  "model.finish",
		Event: map[string]interface{}{"finishReason": "tool_use"},
	})

	if len(h.contentBlocks) != 1 {
		t.Fatalf("expected mapped web_fetch tool call to pass through, got %#v", h.contentBlocks)
	}
	if got, _ := h.contentBlocks[0]["name"].(string); got != "web_fetch" {
		t.Fatalf("expected mapped tool name web_fetch, got %q", got)
	}
	if h.suppressedToolCalls != 0 {
		t.Fatalf("suppressedToolCalls=%d want=0", h.suppressedToolCalls)
	}
}

func TestTaskToolCall_IsAcceptedWhenDelegatedToolsStayWithinAllowedSet(t *testing.T) {
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

	h.setAllowedToolNames([]string{"Read"})

	h.handleMessage(upstream.SSEMessage{
		Type: "model.tool-call",
		Event: map[string]interface{}{
			"toolCallId": "task_1",
			"toolName":   "Task",
			"input":      `{"description":"Get weather","prompt":"Read weather skill","allowed_tools":["Read"]}`,
		},
	})
	h.handleMessage(upstream.SSEMessage{
		Type:  "model.finish",
		Event: map[string]interface{}{"finishReason": "tool_use"},
	})

	if len(h.contentBlocks) != 1 {
		t.Fatalf("expected delegated Task tool call to pass through, got %#v", h.contentBlocks)
	}
	if got, _ := h.contentBlocks[0]["type"].(string); got != "tool_use" {
		t.Fatalf("expected tool_use block, got %q", got)
	}
	if got, _ := h.contentBlocks[0]["name"].(string); got != "Task" {
		t.Fatalf("expected Task tool call, got %q", got)
	}
	if h.suppressedToolCalls != 0 {
		t.Fatalf("suppressedToolCalls=%d want=0", h.suppressedToolCalls)
	}
}

func TestTaskToolCall_IsRejectedWhenDelegatedToolsExceedAllowedSet(t *testing.T) {
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

	h.setAllowedToolNames([]string{"Read"})

	h.handleMessage(upstream.SSEMessage{
		Type: "model.tool-call",
		Event: map[string]interface{}{
			"toolCallId": "task_1",
			"toolName":   "Task",
			"input":      `{"description":"Get weather","prompt":"Run shell","allowed_tools":["Bash"]}`,
		},
	})
	h.handleMessage(upstream.SSEMessage{
		Type:  "model.finish",
		Event: map[string]interface{}{"finishReason": "tool_use"},
	})

	if len(h.contentBlocks) != 1 {
		t.Fatalf("expected empty-output fallback text block after rejecting delegated Task, got %#v", h.contentBlocks)
	}
	if got, _ := h.contentBlocks[0]["type"].(string); got != "text" {
		t.Fatalf("expected text fallback block, got %q", got)
	}
	if h.suppressedToolCalls == 0 {
		t.Fatalf("expected suppressed tool call count to increase")
	}
}

func TestSkillToolCall_IsAcceptedWhenClientDeclaredSkill(t *testing.T) {
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

	h.setAllowedToolNames([]string{"Skill"})

	h.handleMessage(upstream.SSEMessage{
		Type: "model.tool-call",
		Event: map[string]interface{}{
			"toolCallId": "skill_1",
			"toolName":   "Skill",
			"input":      `{"skill":"weather","args":"Yangzhou, China"}`,
		},
	})
	h.handleMessage(upstream.SSEMessage{
		Type:  "model.finish",
		Event: map[string]interface{}{"finishReason": "tool_use"},
	})

	if len(h.contentBlocks) != 1 {
		t.Fatalf("expected Skill tool call to pass through, got %#v", h.contentBlocks)
	}
	if got, _ := h.contentBlocks[0]["type"].(string); got != "tool_use" {
		t.Fatalf("expected tool_use block, got %q", got)
	}
	if got, _ := h.contentBlocks[0]["name"].(string); got != "Skill" {
		t.Fatalf("expected Skill tool call, got %q", got)
	}
	if h.suppressedToolCalls != 0 {
		t.Fatalf("suppressedToolCalls=%d want=0", h.suppressedToolCalls)
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

	if len(h.contentBlocks) != 1 {
		t.Fatalf("expected repeated delete tool call to be suppressed and fallback text injected, got %d blocks: %v", len(h.contentBlocks), h.contentBlocks)
	}
	if got, _ := h.contentBlocks[0]["text"].(string); !strings.Contains(got, "No output was presented") {
		t.Fatalf("expected fallback text block, got %q", got)
	}
	if h.toolDedupCount != 1 {
		t.Fatalf("expected dedup count 1, got %d", h.toolDedupCount)
	}
}

func TestSeedSideEffectDedupFromMessages_DoesNotSuppressFailedEditRetryAfterRead(t *testing.T) {
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
		{Role: "user", Content: prompt.MessageContent{Text: "把第三行改掉"}},
		{
			Role: "assistant",
			Content: prompt.MessageContent{
				Blocks: []prompt.ContentBlock{
					{
						Type:  "tool_use",
						ID:    "tool_edit_1",
						Name:  "Edit",
						Input: map[string]interface{}{"file_path": "/tmp/demo.txt", "old_string": "three", "new_string": "LONG_SESSION_OK"},
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
						ToolUseID: "tool_edit_1",
						Content:   "File has not been read yet. Read it first before writing to it.",
					},
				},
			},
		},
		{
			Role: "assistant",
			Content: prompt.MessageContent{
				Blocks: []prompt.ContentBlock{
					{
						Type:  "tool_use",
						ID:    "tool_read_1",
						Name:  "Read",
						Input: map[string]interface{}{"file_path": "/tmp/demo.txt"},
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
						ToolUseID: "tool_read_1",
						Content:   "one\ntwo\nthree",
					},
				},
			},
		},
	}
	h.seedSideEffectDedupFromMessages(history)

	h.handleMessage(upstream.SSEMessage{
		Type: "model.tool-call",
		Event: map[string]interface{}{
			"toolCallId": "tool_edit_2",
			"toolName":   "Edit",
			"input":      `{"file_path":"/tmp/demo.txt","old_string":"three","new_string":"LONG_SESSION_OK"}`,
		},
	})
	h.handleMessage(upstream.SSEMessage{
		Type:  "model.finish",
		Event: map[string]interface{}{"finishReason": "tool_use"},
	})

	if h.toolDedupCount != 0 {
		t.Fatalf("expected failed edit retry not to be deduped, got %d", h.toolDedupCount)
	}
	if len(h.contentBlocks) != 1 {
		t.Fatalf("expected retry edit tool call to be emitted, got %d blocks: %v", len(h.contentBlocks), h.contentBlocks)
	}
	if got, _ := h.contentBlocks[0]["name"].(string); got != "Edit" {
		t.Fatalf("expected Edit tool call, got %q", got)
	}
}

func TestSeedSideEffectDedupFromMessages_SuppressesRepeatSuccessfulEditAcrossTurns(t *testing.T) {
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
		{Role: "user", Content: prompt.MessageContent{Text: "把第三行改掉"}},
		{
			Role: "assistant",
			Content: prompt.MessageContent{
				Blocks: []prompt.ContentBlock{
					{
						Type:  "tool_use",
						ID:    "tool_edit_1",
						Name:  "Edit",
						Input: map[string]interface{}{"file_path": "/tmp/demo.txt", "old_string": "three", "new_string": "LONG_SESSION_OK"},
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
						ToolUseID: "tool_edit_1",
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
			"toolCallId": "tool_edit_2",
			"toolName":   "Edit",
			"input":      `{"file_path":"/tmp/demo.txt","old_string":"three","new_string":"LONG_SESSION_OK"}`,
		},
	})
	h.handleMessage(upstream.SSEMessage{
		Type:  "model.finish",
		Event: map[string]interface{}{"finishReason": "tool_use"},
	})

	if h.toolDedupCount != 1 {
		t.Fatalf("expected successful edit retry to be deduped, got %d", h.toolDedupCount)
	}
	if len(h.contentBlocks) != 1 {
		t.Fatalf("expected fallback text block after deduped repeat edit, got %d blocks: %v", len(h.contentBlocks), h.contentBlocks)
	}
	if got, _ := h.contentBlocks[0]["text"].(string); !strings.Contains(got, "No output was presented") {
		t.Fatalf("expected fallback text block, got %q", got)
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

func TestSeedSideEffectDedupFromMessages_DoesNotSuppressRepeatGitBashAcrossTurns(t *testing.T) {
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
		{Role: "user", Content: prompt.MessageContent{Text: "上传到 git"}},
		{
			Role: "assistant",
			Content: prompt.MessageContent{
				Blocks: []prompt.ContentBlock{
					{
						Type:  "tool_use",
						ID:    "tool_git_1",
						Name:  "Bash",
						Input: map[string]interface{}{"command": "git add -A && git status --short"},
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
						ToolUseID: "tool_git_1",
						Content:   "M internal/bolt/client.go\nM internal/bolt/client_test.go",
					},
				},
			},
		},
	}
	h.seedSideEffectDedupFromMessages(history)

	h.handleMessage(upstream.SSEMessage{
		Type: "model.tool-call",
		Event: map[string]interface{}{
			"toolCallId": "tool_git_2",
			"toolName":   "Bash",
			"input":      `{"command":"git add -A && git status --short"}`,
		},
	})
	h.handleMessage(upstream.SSEMessage{
		Type:  "model.finish",
		Event: map[string]interface{}{"finishReason": "tool_use"},
	})

	if h.toolDedupCount != 0 {
		t.Fatalf("expected repeated git bash command not to be deduped, got %d", h.toolDedupCount)
	}
	if len(h.contentBlocks) != 1 {
		t.Fatalf("expected repeated git bash tool call to be emitted, got %d blocks", len(h.contentBlocks))
	}
	if got, _ := h.contentBlocks[0]["name"].(string); got != "Bash" {
		t.Fatalf("expected Bash tool call, got %q", got)
	}
}

func TestRepeatedReadOnlyBashToolCall_IsNotDeduped(t *testing.T) {
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
		{Role: "user", Content: prompt.MessageContent{Text: "优化这个项目"}},
		{
			Role: "assistant",
			Content: prompt.MessageContent{
				Blocks: []prompt.ContentBlock{
					{
						Type:  "tool_use",
						ID:    "tool_old_1",
						Name:  "Bash",
						Input: map[string]interface{}{"command": "find /Users/dailin/Documents/GitHub/truth_social_scraper -type f | sort"},
					},
				},
			},
		},
		{
			Role: "user",
			Content: prompt.MessageContent{
				Blocks: []prompt.ContentBlock{
					{Type: "tool_result", ToolUseID: "tool_old_1", Content: "./api.py"},
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
			"input":      `{"command":"find /Users/dailin/Documents/GitHub/truth_social_scraper -type f | sort"}`,
		},
	})
	h.handleMessage(upstream.SSEMessage{
		Type:  "model.finish",
		Event: map[string]interface{}{"finishReason": "tool_use"},
	})

	if h.toolDedupCount != 0 {
		t.Fatalf("expected read-only bash command not to be deduped, got %d", h.toolDedupCount)
	}
	if len(h.contentBlocks) != 1 {
		t.Fatalf("expected repeated read-only bash tool call to be emitted, got %d blocks", len(h.contentBlocks))
	}
	if got, _ := h.contentBlocks[0]["name"].(string); got != "Bash" {
		t.Fatalf("expected Bash tool call, got %q", got)
	}
}
