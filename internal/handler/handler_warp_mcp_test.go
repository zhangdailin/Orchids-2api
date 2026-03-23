package handler

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"orchids-api/internal/upstream"
)

func TestHandleMessages_Warp_CustomMCPToolCall_RemainsAllowed(t *testing.T) {
	t.Parallel()

	client := &fakePayloadClient{
		eventsByOp: [][]upstream.SSEMessage{{
			{
				Type: "model.tool-call",
				Event: map[string]any{
					"toolCallId": "call_workspace_search_1",
					"toolName":   "workspace_search",
					"input":      `{"query":"router","top_k":5}`,
				},
			},
			{
				Type:  "model.finish",
				Event: map[string]any{"finishReason": "tool_use"},
			},
		}},
	}
	h := newTestHandler(client)

	body := []byte(`{
		"model":"claude-sonnet-4-6",
		"stream":false,
		"conversation_id":"warp_custom_mcp",
		"messages":[
			{"role":"user","content":"find router handlers"}
		],
		"tools":[
			{"type":"function","function":{
				"name":"workspace_search",
				"description":"Search the project for matching code symbols",
				"parameters":{
					"type":"object",
					"properties":{
						"query":{"type":"string"},
						"top_k":{"type":"integer"}
					},
					"required":["query"]
				}
			}}
		]
	}`)

	req := httptest.NewRequest(http.MethodPost, "/warp/v1/messages", bytes.NewReader(body))
	rec := httptest.NewRecorder()

	h.HandleMessages(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	out := rec.Body.String()
	if !strings.Contains(out, `"type":"tool_use"`) {
		t.Fatalf("expected tool_use block, got: %s", out)
	}
	if !strings.Contains(out, `"name":"workspace_search"`) {
		t.Fatalf("expected custom MCP tool call to pass through, got: %s", out)
	}

	calls := client.snapshotCalls()
	if len(calls) != 1 {
		t.Fatalf("expected 1 upstream call, got %d", len(calls))
	}
	if len(calls[0].Tools) != 1 {
		t.Fatalf("expected 1 declared tool, got %#v", calls[0].Tools)
	}

	declared, ok := calls[0].Tools[0].(map[string]interface{})
	if !ok {
		t.Fatalf("declared tool type=%T want map[string]interface{}", calls[0].Tools[0])
	}
	function, ok := declared["function"].(map[string]interface{})
	if !ok {
		t.Fatalf("declared tool function=%T want map[string]interface{}", declared["function"])
	}
	if got, _ := function["name"].(string); got != "workspace_search" {
		t.Fatalf("declared tool name=%q want workspace_search", got)
	}
}
