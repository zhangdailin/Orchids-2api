package handler

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/goccy/go-json"

	"orchids-api/internal/config"
	"orchids-api/internal/upstream"
)

func newPuterRegressionHandler(events []upstream.SSEMessage) *Handler {
	cfg := &config.Config{
		DebugEnabled:   false,
		RequestTimeout: 10,
	}
	h := NewWithLoadBalancer(cfg, nil)
	h.client = &mockUpstream{events: events}
	return h
}

func mustMarshalTestJSON(t *testing.T, payload any) []byte {
	t.Helper()
	body, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}
	return body
}

func decodeMessageResponse(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var resp map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v; body=%s", err, rec.Body.String())
	}
	return resp
}

func firstMessageContentBlock(t *testing.T, resp map[string]any) map[string]any {
	t.Helper()
	content, ok := resp["content"].([]any)
	if !ok || len(content) == 0 {
		t.Fatalf("expected content blocks, got %#v", resp["content"])
	}
	block, ok := content[0].(map[string]any)
	if !ok {
		t.Fatalf("expected content block object, got %#v", content[0])
	}
	return block
}

func puterDirectToolUseEvents(msgID, model, toolID, toolName, partialJSON string, outputTokens int) []upstream.SSEMessage {
	return []upstream.SSEMessage{
		{Type: "model.tool-call", Event: map[string]any{
			"toolCallId": toolID,
			"toolName":   toolName,
			"input":      partialJSON,
		}},
		{Type: "model.finish", Event: map[string]any{
			"finishReason": "tool_use",
			"usage": map[string]any{
				"outputTokens": outputTokens,
			},
		}},
	}
}

func puterDirectTextEvents(msgID, model, text string, outputTokens int) []upstream.SSEMessage {
	return []upstream.SSEMessage{
		{Type: "model.text-delta", Event: map[string]any{
			"delta": text,
		}},
		{Type: "model.finish", Event: map[string]any{
			"finishReason": "end_turn",
			"usage": map[string]any{
				"outputTokens": outputTokens,
			},
		}},
	}
}

func puterToolSchema(name string, properties map[string]any, required ...string) map[string]any {
	return map[string]any{
		"name": name,
		"input_schema": map[string]any{
			"type":       "object",
			"properties": properties,
			"required":   required,
		},
	}
}

func TestHandleMessages_PuterMapsToolNameToClientDeclaredCase(t *testing.T) {
	h := newPuterRegressionHandler(puterDirectToolUseEvents(
		"msg_case_mapping",
		"deepseek-v4-flash",
		"tool_grep_1",
		"Grep",
		`{"pattern":"signup","path":"bundle.js"}`,
		8,
	))
	body := mustMarshalTestJSON(t, map[string]any{
		"model":    "deepseek-v4-flash",
		"messages": []map[string]any{{"role": "user", "content": "Search the bundle."}},
		"tools": []map[string]any{puterToolSchema("grep", map[string]any{
			"pattern": map[string]any{"type": "string"},
			"path":    map[string]any{"type": "string"},
		}, "pattern")},
		"stream": false,
	})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "http://x/puter/v1/messages", bytes.NewReader(body))
	h.HandleMessages(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d want=%d: %s", rec.Code, http.StatusOK, rec.Body.String())
	}
	block := firstMessageContentBlock(t, decodeMessageResponse(t, rec))
	if block["type"] != "tool_use" || block["name"] != "grep" {
		t.Fatalf("tool block=%#v want client-declared lowercase grep", block)
	}
}

func TestHandleMessages_Puter_EditRoundTripRegression(t *testing.T) {
	model := "claude-opus-5"
	editTool := puterToolSchema("Edit", map[string]any{
		"file_path":  map[string]any{"type": "string"},
		"old_string": map[string]any{"type": "string"},
		"new_string": map[string]any{"type": "string"},
	}, "file_path", "old_string", "new_string")

	initialHandler := newPuterRegressionHandler(puterDirectToolUseEvents(
		"msg_edit_roundtrip",
		model,
		"tool_edit_1",
		"Edit",
		`{"file_path":"data/tmp/puter-edit-case.txt","old_string":"alpha beta","new_string":"gamma delta"}`,
		9,
	))
	initialBody := mustMarshalTestJSON(t, map[string]any{
		"model": model,
		"messages": []map[string]any{
			{"role": "user", "content": "Replace alpha beta with gamma delta using Edit."},
		},
		"tools":  []map[string]any{editTool},
		"stream": false,
	})
	initialRec := httptest.NewRecorder()
	initialReq := httptest.NewRequest(http.MethodPost, "http://x/puter/v1/messages", bytes.NewReader(initialBody))
	initialHandler.HandleMessages(initialRec, initialReq)
	if initialRec.Code != http.StatusOK {
		t.Fatalf("initial status=%d want=%d: %s", initialRec.Code, http.StatusOK, initialRec.Body.String())
	}
	initialResp := decodeMessageResponse(t, initialRec)
	initialBlock := firstMessageContentBlock(t, initialResp)
	if initialResp["stop_reason"] != "tool_use" {
		t.Fatalf("initial stop_reason=%#v want tool_use", initialResp["stop_reason"])
	}
	if initialBlock["type"] != "tool_use" || initialBlock["name"] != "Edit" {
		t.Fatalf("expected Edit tool_use block, got %#v", initialBlock)
	}

	followupHandler := newPuterRegressionHandler(puterDirectTextEvents(
		"msg_edit_followup",
		model,
		"The edit successfully replaced alpha beta with gamma delta.",
		12,
	))
	followupBody := mustMarshalTestJSON(t, map[string]any{
		"model": model,
		"messages": []map[string]any{
			{"role": "user", "content": "Replace alpha beta with gamma delta using Edit."},
			{"role": "assistant", "content": []map[string]any{
				{
					"type":  "tool_use",
					"id":    "tool_edit_1",
					"name":  "Edit",
					"input": map[string]any{"file_path": "data/tmp/puter-edit-case.txt", "old_string": "alpha beta", "new_string": "gamma delta"},
				},
			}},
			{"role": "user", "content": []map[string]any{
				{"type": "tool_result", "tool_use_id": "tool_edit_1", "content": "Done"},
				{"type": "text", "text": "The edit succeeded. What changed?"},
			}},
		},
		"tools":  []map[string]any{editTool},
		"stream": false,
	})
	followupRec := httptest.NewRecorder()
	followupReq := httptest.NewRequest(http.MethodPost, "http://x/puter/v1/messages", bytes.NewReader(followupBody))
	followupHandler.HandleMessages(followupRec, followupReq)
	if followupRec.Code != http.StatusOK {
		t.Fatalf("followup status=%d want=%d: %s", followupRec.Code, http.StatusOK, followupRec.Body.String())
	}
	followupResp := decodeMessageResponse(t, followupRec)
	followupBlock := firstMessageContentBlock(t, followupResp)
	if followupResp["stop_reason"] != "end_turn" {
		t.Fatalf("followup stop_reason=%#v want end_turn", followupResp["stop_reason"])
	}
	if followupBlock["type"] != "text" {
		t.Fatalf("expected text follow-up block, got %#v", followupBlock)
	}
	if !strings.Contains(followupBlock["text"].(string), "gamma delta") {
		t.Fatalf("expected edit summary text, got %#v", followupBlock["text"])
	}
}

func TestHandleMessages_Puter_ReadRoundTripRegression(t *testing.T) {
	model := "claude-opus-5"
	readTool := puterToolSchema("Read", map[string]any{
		"file_path": map[string]any{"type": "string"},
	}, "file_path")

	initialHandler := newPuterRegressionHandler(puterDirectToolUseEvents(
		"msg_read_roundtrip",
		model,
		"tool_read_1",
		"Read",
		`{"file_path":"README.md"}`,
		5,
	))
	initialBody := mustMarshalTestJSON(t, map[string]any{
		"model": model,
		"messages": []map[string]any{
			{"role": "user", "content": "Read README.md using Read."},
		},
		"tools":  []map[string]any{readTool},
		"stream": false,
	})
	initialRec := httptest.NewRecorder()
	initialReq := httptest.NewRequest(http.MethodPost, "http://x/puter/v1/messages", bytes.NewReader(initialBody))
	initialHandler.HandleMessages(initialRec, initialReq)
	if initialRec.Code != http.StatusOK {
		t.Fatalf("initial status=%d want=%d: %s", initialRec.Code, http.StatusOK, initialRec.Body.String())
	}
	initialResp := decodeMessageResponse(t, initialRec)
	initialBlock := firstMessageContentBlock(t, initialResp)
	if initialResp["stop_reason"] != "tool_use" {
		t.Fatalf("initial stop_reason=%#v want tool_use", initialResp["stop_reason"])
	}
	if initialBlock["type"] != "tool_use" || initialBlock["name"] != "Read" {
		t.Fatalf("expected Read tool_use block, got %#v", initialBlock)
	}

	followupHandler := newPuterRegressionHandler(puterDirectTextEvents(
		"msg_read_followup",
		model,
		"README.md is the project overview and quickstart document.",
		10,
	))
	followupBody := mustMarshalTestJSON(t, map[string]any{
		"model": model,
		"messages": []map[string]any{
			{"role": "user", "content": "Read README.md using Read."},
			{"role": "assistant", "content": []map[string]any{
				{
					"type":  "tool_use",
					"id":    "tool_read_1",
					"name":  "Read",
					"input": map[string]any{"file_path": "README.md"},
				},
			}},
			{"role": "user", "content": []map[string]any{
				{"type": "tool_result", "tool_use_id": "tool_read_1", "content": "# Orchids-2api\nOverview"},
				{"type": "text", "text": "Summarize the file in one sentence."},
			}},
		},
		"tools":  []map[string]any{readTool},
		"stream": false,
	})
	followupRec := httptest.NewRecorder()
	followupReq := httptest.NewRequest(http.MethodPost, "http://x/puter/v1/messages", bytes.NewReader(followupBody))
	followupHandler.HandleMessages(followupRec, followupReq)
	if followupRec.Code != http.StatusOK {
		t.Fatalf("followup status=%d want=%d: %s", followupRec.Code, http.StatusOK, followupRec.Body.String())
	}
	followupResp := decodeMessageResponse(t, followupRec)
	followupBlock := firstMessageContentBlock(t, followupResp)
	if followupResp["stop_reason"] != "end_turn" {
		t.Fatalf("followup stop_reason=%#v want end_turn", followupResp["stop_reason"])
	}
	if followupBlock["type"] != "text" {
		t.Fatalf("expected text follow-up block, got %#v", followupBlock)
	}
	if !strings.Contains(strings.ToLower(followupBlock["text"].(string)), "readme") {
		t.Fatalf("expected read summary text, got %#v", followupBlock["text"])
	}
}

func TestHandleMessages_Puter_DeleteRoundTripRegression(t *testing.T) {
	model := "claude-opus-5"
	deleteTool := puterToolSchema("Delete", map[string]any{
		"file_path": map[string]any{"type": "string"},
	}, "file_path")

	initialHandler := newPuterRegressionHandler(puterDirectToolUseEvents(
		"msg_delete_roundtrip",
		model,
		"tool_delete_1",
		"Delete",
		`{"file_path":"data/tmp/puter-delete-case.txt"}`,
		4,
	))
	initialBody := mustMarshalTestJSON(t, map[string]any{
		"model": model,
		"messages": []map[string]any{
			{"role": "user", "content": "Delete data/tmp/puter-delete-case.txt using Delete."},
		},
		"tools":  []map[string]any{deleteTool},
		"stream": false,
	})
	initialRec := httptest.NewRecorder()
	initialReq := httptest.NewRequest(http.MethodPost, "http://x/puter/v1/messages", bytes.NewReader(initialBody))
	initialHandler.HandleMessages(initialRec, initialReq)
	if initialRec.Code != http.StatusOK {
		t.Fatalf("initial status=%d want=%d: %s", initialRec.Code, http.StatusOK, initialRec.Body.String())
	}
	initialResp := decodeMessageResponse(t, initialRec)
	initialBlock := firstMessageContentBlock(t, initialResp)
	if initialResp["stop_reason"] != "tool_use" {
		t.Fatalf("initial stop_reason=%#v want tool_use", initialResp["stop_reason"])
	}
	if initialBlock["type"] != "tool_use" || initialBlock["name"] != "Delete" {
		t.Fatalf("expected Delete tool_use block, got %#v", initialBlock)
	}

	followupHandler := newPuterRegressionHandler(puterDirectTextEvents(
		"msg_delete_followup",
		model,
		"The file data/tmp/puter-delete-case.txt was deleted successfully.",
		10,
	))
	followupBody := mustMarshalTestJSON(t, map[string]any{
		"model": model,
		"messages": []map[string]any{
			{"role": "user", "content": "Delete data/tmp/puter-delete-case.txt using Delete."},
			{"role": "assistant", "content": []map[string]any{
				{
					"type":  "tool_use",
					"id":    "tool_delete_1",
					"name":  "Delete",
					"input": map[string]any{"file_path": "data/tmp/puter-delete-case.txt"},
				},
			}},
			{"role": "user", "content": []map[string]any{
				{"type": "tool_result", "tool_use_id": "tool_delete_1", "content": "Done"},
				{"type": "text", "text": "The delete succeeded. What did you do?"},
			}},
		},
		"tools":  []map[string]any{deleteTool},
		"stream": false,
	})
	followupRec := httptest.NewRecorder()
	followupReq := httptest.NewRequest(http.MethodPost, "http://x/puter/v1/messages", bytes.NewReader(followupBody))
	followupHandler.HandleMessages(followupRec, followupReq)
	if followupRec.Code != http.StatusOK {
		t.Fatalf("followup status=%d want=%d: %s", followupRec.Code, http.StatusOK, followupRec.Body.String())
	}
	followupResp := decodeMessageResponse(t, followupRec)
	followupBlock := firstMessageContentBlock(t, followupResp)
	if followupResp["stop_reason"] != "end_turn" {
		t.Fatalf("followup stop_reason=%#v want end_turn", followupResp["stop_reason"])
	}
	if followupBlock["type"] != "text" {
		t.Fatalf("expected text follow-up block, got %#v", followupBlock)
	}
	if !strings.Contains(strings.ToLower(followupBlock["text"].(string)), "deleted") {
		t.Fatalf("expected delete summary text, got %#v", followupBlock["text"])
	}
}

func TestHandleMessages_Puter_LongContextRegression(t *testing.T) {
	const sentinel = "LONGCTX_PUTER_REGRESSION_SENTINEL"

	client := &fakePayloadClient{
		eventsByOp: [][]upstream.SSEMessage{
			puterDirectTextEvents("msg_long_context", "claude-opus-5", sentinel, 8),
		},
	}
	h := newTestHandler(client)

	var b strings.Builder
	b.WriteString("You will receive a long context block. Return exactly the sentinel value and nothing else.\n")
	for i := 0; i < 1200; i++ {
		b.WriteString("Context line ")
		b.WriteString("repeated filler for long context validation.\n")
	}
	b.WriteString("Near the end, the sentinel appears below.\nSENTINEL: ")
	b.WriteString(sentinel)
	longPrompt := b.String()

	body := mustMarshalTestJSON(t, map[string]any{
		"model":    "claude-opus-5",
		"messages": []map[string]any{{"role": "user", "content": longPrompt}},
		"stream":   false,
	})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "http://x/puter/v1/messages", bytes.NewReader(body))
	h.HandleMessages(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d want=%d: %s", rec.Code, http.StatusOK, rec.Body.String())
	}

	resp := decodeMessageResponse(t, rec)
	block := firstMessageContentBlock(t, resp)
	if block["type"] != "text" || block["text"] != sentinel {
		t.Fatalf("expected exact sentinel response, got %#v", block)
	}

	calls := client.snapshotCalls()
	if len(calls) != 1 {
		t.Fatalf("expected 1 upstream call, got %d", len(calls))
	}
	if len(calls[0].Messages) != 1 {
		t.Fatalf("expected 1 upstream message, got %d", len(calls[0].Messages))
	}
	if got := calls[0].Messages[0].ExtractText(); got != longPrompt {
		t.Fatalf("forwarded long prompt mismatch: len(got)=%d len(want)=%d", len(got), len(longPrompt))
	}
}

func TestHandleMessages_Puter_MultiRoundToolResultChainRegression(t *testing.T) {
	readTool := puterToolSchema("Read", map[string]any{
		"file_path": map[string]any{"type": "string"},
	}, "file_path")

	client := &fakePayloadClient{
		eventsByOp: [][]upstream.SSEMessage{
			puterDirectToolUseEvents("msg_chain_1", "claude-opus-5", "tool_readme_1", "Read", `{"file_path":"README.md"}`, 4),
			puterDirectToolUseEvents("msg_chain_2", "claude-opus-5", "tool_gomod_1", "Read", `{"file_path":"go.mod"}`, 4),
			puterDirectTextEvents("msg_chain_3", "claude-opus-5", "This project is a Go-based API gateway that normalizes multiple AI providers.", 16),
		},
	}
	h := newTestHandler(client)

	req1Body := mustMarshalTestJSON(t, map[string]any{
		"model":           "claude-opus-5",
		"conversation_id": "puter_chain_regression",
		"messages": []map[string]any{
			{"role": "user", "content": "Understand this project in two inspection steps."},
		},
		"tools":  []map[string]any{readTool},
		"stream": false,
	})
	rec1 := httptest.NewRecorder()
	req1 := httptest.NewRequest(http.MethodPost, "http://x/puter/v1/messages", bytes.NewReader(req1Body))
	h.HandleMessages(rec1, req1)
	if rec1.Code != http.StatusOK {
		t.Fatalf("turn1 status=%d want=%d: %s", rec1.Code, http.StatusOK, rec1.Body.String())
	}
	resp1 := decodeMessageResponse(t, rec1)
	block1 := firstMessageContentBlock(t, resp1)
	if resp1["stop_reason"] != "tool_use" || block1["name"] != "Read" {
		t.Fatalf("expected first turn Read tool_use, got resp=%#v block=%#v", resp1["stop_reason"], block1)
	}

	req2Body := mustMarshalTestJSON(t, map[string]any{
		"model":           "claude-opus-5",
		"conversation_id": "puter_chain_regression",
		"messages": []map[string]any{
			{"role": "user", "content": "Understand this project in two inspection steps."},
			{"role": "assistant", "content": []map[string]any{
				{"type": "tool_use", "id": "tool_readme_1", "name": "Read", "input": map[string]any{"file_path": "README.md"}},
			}},
			{"role": "user", "content": []map[string]any{
				{"type": "tool_result", "tool_use_id": "tool_readme_1", "content": "# Orchids-2api\nOverview"},
			}},
		},
		"tools":  []map[string]any{readTool},
		"stream": false,
	})
	rec2 := httptest.NewRecorder()
	req2 := httptest.NewRequest(http.MethodPost, "http://x/puter/v1/messages", bytes.NewReader(req2Body))
	h.HandleMessages(rec2, req2)
	if rec2.Code != http.StatusOK {
		t.Fatalf("turn2 status=%d want=%d: %s", rec2.Code, http.StatusOK, rec2.Body.String())
	}
	resp2 := decodeMessageResponse(t, rec2)
	block2 := firstMessageContentBlock(t, resp2)
	if resp2["stop_reason"] != "tool_use" || block2["name"] != "Read" {
		t.Fatalf("expected second turn Read tool_use, got resp=%#v block=%#v", resp2["stop_reason"], block2)
	}

	req3Body := mustMarshalTestJSON(t, map[string]any{
		"model":           "claude-opus-5",
		"conversation_id": "puter_chain_regression",
		"messages": []map[string]any{
			{"role": "user", "content": "Understand this project in two inspection steps."},
			{"role": "assistant", "content": []map[string]any{
				{"type": "tool_use", "id": "tool_readme_1", "name": "Read", "input": map[string]any{"file_path": "README.md"}},
			}},
			{"role": "user", "content": []map[string]any{
				{"type": "tool_result", "tool_use_id": "tool_readme_1", "content": "# Orchids-2api\nOverview"},
			}},
			{"role": "assistant", "content": []map[string]any{
				{"type": "tool_use", "id": "tool_gomod_1", "name": "Read", "input": map[string]any{"file_path": "go.mod"}},
			}},
			{"role": "user", "content": []map[string]any{
				{"type": "tool_result", "tool_use_id": "tool_gomod_1", "content": "module orchids-api\n\ngo 1.24.0"},
				{"type": "text", "text": "Now answer in one sentence."},
			}},
		},
		"tools":  []map[string]any{readTool},
		"stream": false,
	})
	rec3 := httptest.NewRecorder()
	req3 := httptest.NewRequest(http.MethodPost, "http://x/puter/v1/messages", bytes.NewReader(req3Body))
	h.HandleMessages(rec3, req3)
	if rec3.Code != http.StatusOK {
		t.Fatalf("turn3 status=%d want=%d: %s", rec3.Code, http.StatusOK, rec3.Body.String())
	}
	resp3 := decodeMessageResponse(t, rec3)
	block3 := firstMessageContentBlock(t, resp3)
	if resp3["stop_reason"] != "end_turn" || block3["type"] != "text" {
		t.Fatalf("expected third turn final text, got resp=%#v block=%#v", resp3["stop_reason"], block3)
	}
	if !strings.Contains(strings.ToLower(block3["text"].(string)), "go-based") {
		t.Fatalf("expected final summary text, got %#v", block3["text"])
	}

	calls := client.snapshotCalls()
	if len(calls) != 3 {
		t.Fatalf("expected 3 upstream calls, got %d", len(calls))
	}
	if calls[0].NoTools {
		t.Fatalf("expected turn1 tools enabled")
	}
	if calls[1].NoTools {
		t.Fatalf("expected turn2 tool_result follow-up to keep tools enabled")
	}
}
