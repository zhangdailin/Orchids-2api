package warp

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/goccy/go-json"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/structpb"

	"orchids-api/internal/prompt"
	"orchids-api/internal/upstream"
)

func TestSendRequestWithPayload_CustomMCPTool_RoundTripsThroughWarpClient(t *testing.T) {
	t.Parallel()

	const toolName = "workspace_search"

	client := &Client{
		httpClient: &http.Client{
			Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
				if got := req.Header.Get("Accept"); got != "text/event-stream" {
					t.Fatalf("Accept=%q want text/event-stream", got)
				}

				payload, err := io.ReadAll(req.Body)
				if err != nil {
					t.Fatalf("read request body: %v", err)
				}
				if !bytes.Contains(payload, []byte(toolName)) {
					t.Fatalf("request payload missing custom MCP tool name %q", toolName)
				}
				if !bytes.Contains(payload, []byte("query")) {
					t.Fatalf("request payload missing custom MCP tool schema property")
				}

				messagePayload := appendBytesField(4, buildNestedMCPToolCallFrame(t, "call_workspace_search_1", toolName, map[string]interface{}{
					"query": "router",
					"top_k": 5,
				}))
				frame := wrapFrame(appendBytesField(2,
					appendBytesField(1,
						appendBytesField(1,
							appendBytesField(1,
								appendBytesField(5, messagePayload),
							),
						),
					),
				))
				finish := wrapFrame(appendBytesField(3, appendBytesField(8, appendVarintField(2, 1))))

				return &http.Response{
					StatusCode: http.StatusOK,
					Header:     make(http.Header),
					Body:       io.NopCloser(bytes.NewReader(append(frame, finish...))),
					Request:    req,
				}, nil
			}),
		},
		session: &session{
			jwt:       "test-jwt",
			expiresAt: time.Now().Add(time.Hour),
			loggedIn:  true,
			lastLogin: time.Now(),
		},
	}

	upstreamReq := upstream.UpstreamRequest{
		Model: "claude-4-5-sonnet",
		Messages: []prompt.Message{
			{
				Role: "user",
				Content: prompt.MessageContent{
					Text: "find router handlers",
				},
			},
		},
		Tools: []interface{}{
			map[string]interface{}{
				"type": "function",
				"function": map[string]interface{}{
					"name":        toolName,
					"description": strings.Repeat("Search the current workspace for code symbols. ", 8),
					"parameters": map[string]interface{}{
						"type": "object",
						"properties": map[string]interface{}{
							"query": map[string]interface{}{
								"type": "string",
							},
							"top_k": map[string]interface{}{
								"type": "integer",
							},
						},
						"required": []interface{}{"query"},
					},
				},
			},
		},
	}

	var events []upstream.SSEMessage
	err := client.SendRequestWithPayload(context.Background(), upstreamReq, func(msg upstream.SSEMessage) {
		events = append(events, msg)
	}, nil)
	if err != nil {
		t.Fatalf("SendRequestWithPayload error: %v", err)
	}

	if len(events) != 2 {
		t.Fatalf("len(events)=%d want 2", len(events))
	}
	if events[0].Type != "model.tool-call" {
		t.Fatalf("tool event=%#v want model.tool-call", events[0])
	}
	if got, _ := events[0].Event["toolName"].(string); got != toolName {
		t.Fatalf("toolName=%q want %q", got, toolName)
	}
	if got, _ := events[0].Event["toolCallId"].(string); got != "call_workspace_search_1" {
		t.Fatalf("toolCallId=%q want call_workspace_search_1", got)
	}

	inputJSON, _ := events[0].Event["input"].(string)
	var input map[string]interface{}
	if err := json.Unmarshal([]byte(inputJSON), &input); err != nil {
		t.Fatalf("unmarshal tool input: %v", err)
	}
	if input["query"] != "router" || input["top_k"] != float64(5) {
		t.Fatalf("tool input=%#v want query=router top_k=5", input)
	}
	if events[1].Type != "model.finish" || events[1].Event["finishReason"] != "tool_use" {
		t.Fatalf("finish event=%#v want tool_use", events[1])
	}
}

func buildNestedMCPToolCallFrame(t *testing.T, callID, toolName string, input map[string]interface{}) []byte {
	t.Helper()

	st, err := structpb.NewStruct(input)
	if err != nil {
		t.Fatalf("structpb.NewStruct error: %v", err)
	}
	stPayload, err := proto.Marshal(st)
	if err != nil {
		t.Fatalf("marshal structpb: %v", err)
	}

	callPayload := appendBytesField(1, []byte(toolName))
	callPayload = append(callPayload, appendBytesField(2, stPayload)...)

	toolPayload := appendBytesField(1, []byte(callID))
	toolPayload = append(toolPayload, appendBytesField(12, callPayload)...)
	return toolPayload
}
