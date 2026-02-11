package handler

import (
	"net/http/httptest"
	"strings"
	"testing"

	"orchids-api/internal/adapter"
	"orchids-api/internal/config"
	"orchids-api/internal/debug"
	"orchids-api/internal/upstream"
)

func TestWriteChunkSuppressThinkingFallsBackToTextDelta(t *testing.T) {
	t.Parallel()

	rec := httptest.NewRecorder()
	h := newStreamHandler(
		&config.Config{OutputTokenMode: "final"},
		rec,
		debug.New(false, false),
		true, // suppress thinking
		true, // stream mode
		adapter.FormatAnthropic,
		"",
	)
	defer h.release()

	h.handleMessage(upstream.SSEMessage{
		Type: "coding_agent.Write.content.chunk",
		Event: map[string]interface{}{
			"data": map[string]interface{}{
				"file_path": "/tmp/calculator.py",
				"text":      "print('hello')",
			},
		},
	})
	h.finishResponse("end_turn")

	body := rec.Body.String()
	if !strings.Contains(body, `event: content_block_start`) {
		t.Fatalf("expected content_block_start in stream, got: %s", body)
	}
	if !strings.Contains(body, `"type":"text_delta"`) {
		t.Fatalf("expected text_delta fallback, got: %s", body)
	}
	if !strings.Contains(body, `"text":"print('hello')"`) {
		t.Fatalf("expected chunk text in stream, got: %s", body)
	}
	if strings.Contains(body, `event: coding_agent.Write.content.chunk`) {
		t.Fatalf("did not expect raw coding_agent chunk event when thinking is suppressed, got: %s", body)
	}
}

func TestWriteChunkNormalModeKeepsThinkingAndRawEventAndAddsFallbackText(t *testing.T) {
	t.Parallel()

	rec := httptest.NewRecorder()
	h := newStreamHandler(
		&config.Config{OutputTokenMode: "final"},
		rec,
		debug.New(false, false),
		false, // suppress thinking disabled
		true,  // stream mode
		adapter.FormatAnthropic,
		"",
	)
	defer h.release()

	h.handleMessage(upstream.SSEMessage{
		Type: "coding_agent.Write.content.chunk",
		Event: map[string]interface{}{
			"data": map[string]interface{}{
				"file_path": "/tmp/calculator.py",
				"text":      "print('hello')",
			},
		},
	})
	h.finishResponse("end_turn")

	body := rec.Body.String()
	if !strings.Contains(body, `"type":"thinking_delta"`) {
		t.Fatalf("expected thinking_delta in normal mode, got: %s", body)
	}
	if !strings.Contains(body, `event: coding_agent.Write.content.chunk`) {
		t.Fatalf("expected raw coding_agent chunk passthrough in normal mode, got: %s", body)
	}
	if !strings.Contains(body, `"type":"text_delta"`) || !strings.Contains(body, `"text":"print('hello')"`) {
		t.Fatalf("expected fallback text_delta for clients that do not render thinking, got: %s", body)
	}
}

func TestWriteChunkNormalModeSkipsFallbackWhenTextAlreadyExists(t *testing.T) {
	t.Parallel()

	rec := httptest.NewRecorder()
	h := newStreamHandler(
		&config.Config{OutputTokenMode: "final"},
		rec,
		debug.New(false, false),
		false, // suppress thinking disabled
		true,  // stream mode
		adapter.FormatAnthropic,
		"",
	)
	defer h.release()

	h.handleMessage(upstream.SSEMessage{
		Type: "model",
		Event: map[string]interface{}{
			"type":  "text-delta",
			"delta": "done",
		},
	})
	h.handleMessage(upstream.SSEMessage{
		Type: "coding_agent.Write.content.chunk",
		Event: map[string]interface{}{
			"data": map[string]interface{}{
				"file_path": "/tmp/calculator.py",
				"text":      "print('hello')",
			},
		},
	})
	h.finishResponse("end_turn")

	body := rec.Body.String()
	if !strings.Contains(body, `"type":"text_delta"`) || !strings.Contains(body, `"text":"done"`) {
		t.Fatalf("expected original text delta, got: %s", body)
	}
	if strings.Contains(body, `"type":"text_delta","text":"print('hello')"`) {
		t.Fatalf("did not expect fallback text when model text already exists, got: %s", body)
	}
}

func TestCreditsExhaustedEmitsVisibleError(t *testing.T) {
	t.Parallel()

	rec := httptest.NewRecorder()
	h := newStreamHandler(
		&config.Config{OutputTokenMode: "final"},
		rec,
		debug.New(false, false),
		true, // suppress thinking
		true, // stream mode
		adapter.FormatAnthropic,
		"",
	)
	defer h.release()

	h.handleMessage(upstream.SSEMessage{
		Type: "coding_agent.credits_exhausted",
		Event: map[string]interface{}{
			"type": "coding_agent.credits_exhausted",
			"data": map[string]interface{}{
				"message": "You have run out of credits. Please upgrade your plan to continue.",
			},
		},
	})

	body := rec.Body.String()
	if !strings.Contains(body, `"type":"text_delta"`) {
		t.Fatalf("expected text_delta for credits exhausted, got: %s", body)
	}
	if !strings.Contains(body, `"text":"You have run out of credits. Please upgrade your plan to continue."`) {
		t.Fatalf("expected credits exhausted message in stream, got: %s", body)
	}
	if !strings.Contains(body, `event: content_block_stop`) {
		t.Fatalf("expected content_block_stop for completed text block, got: %s", body)
	}
	if !strings.Contains(body, `"stop_reason":"end_turn"`) {
		t.Fatalf("expected end_turn stop reason, got: %s", body)
	}
}

func TestModelToolCallAcceptedWithOpenInputBuffer(t *testing.T) {
	t.Parallel()

	rec := httptest.NewRecorder()
	h := newStreamHandler(
		&config.Config{OutputTokenMode: "final"},
		rec,
		debug.New(false, false),
		false, // suppress thinking disabled
		true,  // stream mode
		adapter.FormatAnthropic,
		"",
	)
	defer h.release()

	toolID := "toolu_abc123"
	h.handleMessage(upstream.SSEMessage{
		Type: "model",
		Event: map[string]interface{}{
			"type":     "tool-input-start",
			"id":       toolID,
			"toolName": "Write",
		},
	})
	h.handleMessage(upstream.SSEMessage{
		Type: "model",
		Event: map[string]interface{}{
			"type":  "tool-input-delta",
			"id":    toolID,
			"delta": "{\"file_path\":\"/tmp/calculator.py\"",
		},
	})

	h.handleMessage(upstream.SSEMessage{
		Type: "model",
		Event: map[string]interface{}{
			"type":       "tool-call",
			"toolCallId": toolID,
			"toolName":   "Write",
			"input":      "{\"file_path\":\"/tmp/calculator.py\",\"content\":\"print('ok')\\n\"}",
		},
	})
	h.finishResponse("tool_use")

	body := rec.Body.String()
	if !strings.Contains(body, `"type":"tool_use"`) {
		t.Fatalf("expected tool_use block, got: %s", body)
	}
	if !strings.Contains(body, `"name":"Write"`) {
		t.Fatalf("expected Write tool name, got: %s", body)
	}
	if !strings.Contains(body, `"partial_json":"{\"file_path\":\"/tmp/calculator.py\",\"content\":\"print('ok')\\n\"}"`) {
		t.Fatalf("expected tool input json delta, got: %s", body)
	}
	if !strings.Contains(body, `"stop_reason":"tool_use"`) {
		t.Fatalf("expected stop_reason tool_use, got: %s", body)
	}
}

func TestNoWriteChunkFallbackWhenStopReasonIsToolUse(t *testing.T) {
	t.Parallel()

	rec := httptest.NewRecorder()
	h := newStreamHandler(
		&config.Config{OutputTokenMode: "final"},
		rec,
		debug.New(false, false),
		false, // suppress thinking disabled
		true,  // stream mode
		adapter.FormatAnthropic,
		"",
	)
	defer h.release()

	h.handleMessage(upstream.SSEMessage{
		Type: "coding_agent.Write.content.chunk",
		Event: map[string]interface{}{
			"data": map[string]interface{}{
				"file_path": "/tmp/calculator.py",
				"text":      "print('hello')",
			},
		},
	})
	h.handleMessage(upstream.SSEMessage{
		Type: "model",
		Event: map[string]interface{}{
			"type":       "tool-call",
			"toolCallId": "toolu_write_1",
			"toolName":   "Write",
			"input":      "{\"file_path\":\"/tmp/calculator.py\",\"content\":\"print('hello')\\n\"}",
		},
	})
	h.finishResponse("tool_use")

	body := rec.Body.String()
	if strings.Contains(body, `"type":"text_delta","text":"print('hello')"`) {
		t.Fatalf("did not expect fallback text block when stop_reason is tool_use, got: %s", body)
	}
	if !strings.Contains(body, `"type":"tool_use"`) {
		t.Fatalf("expected tool_use output, got: %s", body)
	}
}

func TestModelToolCallDifferentIDNotDropped(t *testing.T) {
	t.Parallel()

	rec := httptest.NewRecorder()
	h := newStreamHandler(
		&config.Config{OutputTokenMode: "final"},
		rec,
		debug.New(false, false),
		false,
		true,
		adapter.FormatAnthropic,
		"",
	)
	defer h.release()

	// Keep one tool-input buffer open.
	h.handleMessage(upstream.SSEMessage{
		Type: "model",
		Event: map[string]interface{}{
			"type":     "tool-input-start",
			"id":       "toolu_open_input",
			"toolName": "Write",
		},
	})

	// A different tool-call id should still be accepted.
	h.handleMessage(upstream.SSEMessage{
		Type: "model",
		Event: map[string]interface{}{
			"type":       "tool-call",
			"toolCallId": "toolu_bash_2",
			"toolName":   "Bash",
			"input":      "{\"command\":\"echo ok\"}",
		},
	})
	h.finishResponse("tool_use")

	body := rec.Body.String()
	if !strings.Contains(body, `"type":"tool_use"`) || !strings.Contains(body, `"name":"Bash"`) {
		t.Fatalf("expected Bash tool_use not to be dropped by different currentToolInputID, got: %s", body)
	}
}

func TestToolInputAliasesAccepted(t *testing.T) {
	t.Parallel()

	if !hasRequiredToolInput("Bash", `{"cmd":"echo ok"}`) {
		t.Fatal("expected bash cmd alias to be accepted")
	}
	if !hasRequiredToolInput("Read", `{"path":"/tmp/a.txt"}`) {
		t.Fatal("expected read path alias to be accepted")
	}
	if !hasRequiredToolInput("Edit", `{"path":"/tmp/a.txt","old_string":"a","new_string":"b"}`) {
		t.Fatal("expected edit path alias to be accepted")
	}
	if key := sideEffectToolDedupKey("bash", `{"cmd":"echo ok"}`); key == "" {
		t.Fatal("expected dedup key for bash cmd alias")
	}
}

func TestWriteToolInputSanitizesOverwrite(t *testing.T) {
	t.Parallel()

	rec := httptest.NewRecorder()
	h := newStreamHandler(
		&config.Config{OutputTokenMode: "final"},
		rec,
		debug.New(false, false),
		false,
		true,
		adapter.FormatAnthropic,
		"",
	)
	defer h.release()

	h.handleMessage(upstream.SSEMessage{
		Type: "model",
		Event: map[string]interface{}{
			"type":       "tool-call",
			"toolCallId": "toolu_write_overwrite",
			"toolName":   "Write",
			"input":      `{"file_path":"/tmp/a.py","content":"print('ok')\n","overwrite":true}`,
		},
	})
	h.finishResponse("tool_use")

	body := rec.Body.String()
	if !strings.Contains(body, `"name":"Write"`) {
		t.Fatalf("expected Write tool_use, got: %s", body)
	}
	if strings.Contains(body, `\"overwrite\"`) || strings.Contains(body, `"overwrite"`) {
		t.Fatalf("did not expect overwrite field to be forwarded, got: %s", body)
	}
	if !strings.Contains(body, `\"file_path\":\"/tmp/a.py\"`) || !strings.Contains(body, `\"content\":\"print('ok')\\n\"`) {
		t.Fatalf("expected sanitized write input to keep file_path/content, got: %s", body)
	}
}
