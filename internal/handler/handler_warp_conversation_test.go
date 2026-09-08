package handler

import (
	"bytes"
	"context"
	"github.com/goccy/go-json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"orchids-api/internal/audit"
	"orchids-api/internal/config"
	"orchids-api/internal/debug"
	"orchids-api/internal/prompt"
	"orchids-api/internal/upstream"
)

type fakePayloadClient struct {
	mu                  sync.Mutex
	calls               []upstream.UpstreamRequest
	conversationIDsByOp []string
	eventsByOp          [][]upstream.SSEMessage
}

func (f *fakePayloadClient) SendRequestWithPayload(ctx context.Context, req upstream.UpstreamRequest, onMessage func(upstream.SSEMessage), logger *debug.Logger) error {
	f.mu.Lock()
	f.calls = append(f.calls, req)
	idx := len(f.calls) - 1
	var convID string
	if idx >= 0 && idx < len(f.conversationIDsByOp) {
		convID = f.conversationIDsByOp[idx]
	}
	var events []upstream.SSEMessage
	if idx >= 0 && idx < len(f.eventsByOp) {
		events = f.eventsByOp[idx]
	}
	f.mu.Unlock()

	if len(events) > 0 {
		for _, event := range events {
			onMessage(event)
		}
		return nil
	}

	if convID != "" {
		onMessage(upstream.SSEMessage{
			Type:  "model.conversation_id",
			Event: map[string]interface{}{"id": convID},
		})
	}
	onMessage(upstream.SSEMessage{
		Type:  "model.finish",
		Event: map[string]interface{}{"finishReason": "end_turn"},
	})
	return nil
}

func (f *fakePayloadClient) snapshotCalls() []upstream.UpstreamRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]upstream.UpstreamRequest, len(f.calls))
	copy(out, f.calls)
	return out
}

func makeWarpRequestBody(t *testing.T, text, conversationID string) []byte {
	t.Helper()
	req := ClaudeRequest{
		Model:          "claude-opus-4-6",
		ConversationID: conversationID,
		Messages: []prompt.Message{
			{
				Role:    "user",
				Content: prompt.MessageContent{Text: text},
			},
		},
		Stream: false,
		Tools:  []interface{}{},
	}
	body, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}
	return body
}

func newTestHandler(client UpstreamClient) *Handler {
	return &Handler{
		config:       &config.Config{DebugEnabled: false},
		client:       client,
		sessionStore: NewMemorySessionStore(30*time.Minute, 1024),
		auditLogger:  audit.NewNopLogger(),
	}
}

func TestWarpConversationID_NotPersistedWithoutConversationKey(t *testing.T) {
	t.Parallel()

	client := &fakePayloadClient{
		conversationIDsByOp: []string{"warp_upstream_conv_1", "warp_upstream_conv_2"},
	}
	h := newTestHandler(client)

	req1 := httptest.NewRequest(http.MethodPost, "/warp/v1/messages", bytes.NewReader(makeWarpRequestBody(t, "first", "")))
	rec1 := httptest.NewRecorder()
	h.HandleMessages(rec1, req1)
	if rec1.Code != http.StatusOK {
		t.Fatalf("first request status = %d, want %d", rec1.Code, http.StatusOK)
	}

	req2 := httptest.NewRequest(http.MethodPost, "/warp/v1/messages", bytes.NewReader(makeWarpRequestBody(t, "second", "")))
	rec2 := httptest.NewRecorder()
	h.HandleMessages(rec2, req2)
	if rec2.Code != http.StatusOK {
		t.Fatalf("second request status = %d, want %d", rec2.Code, http.StatusOK)
	}

	calls := client.snapshotCalls()
	if len(calls) != 2 {
		t.Fatalf("expected 2 upstream calls, got %d", len(calls))
	}

	if calls[0].ChatSessionID != "" {
		t.Fatalf("first ChatSessionID = %q, want empty for a new official Warp conversation", calls[0].ChatSessionID)
	}
	if calls[1].ChatSessionID != "" {
		t.Fatalf("second unrelated ChatSessionID = %q, want empty", calls[1].ChatSessionID)
	}
	if calls[1].ChatSessionID == "warp_upstream_conv_1" {
		t.Fatalf("second request unexpectedly reused upstream conversation id: %q", calls[1].ChatSessionID)
	}
	// Verify empty conversation key does not store convID
	if _, ok := h.sessionStore.GetConvID(context.Background(), ""); ok {
		t.Fatalf("unexpected cached conversation id for empty conversation key")
	}
}

func TestWarpToolResultFollowup_RequiresStableClientSessionID(t *testing.T) {
	t.Parallel()

	client := &fakePayloadClient{
		eventsByOp: [][]upstream.SSEMessage{{
			{Type: "model.conversation_id", Event: map[string]interface{}{"id": "warp_upstream_tool_conv"}},
			{Type: "model.tool-call", Event: map[string]interface{}{
				"toolCallId":   "tool_write_1",
				"toolName":     "Write",
				"input":        `{"file_path":"calculator.py","content":"print(1)"}`,
				"warpToolType": "call_mcp_tool",
			}},
			{Type: "model.finish", Event: map[string]interface{}{"finishReason": "tool_use"}},
		}},
	}
	h := newTestHandler(client)

	first := []byte(`{
		"model":"claude-opus-4-6",
		"stream":false,
		"messages":[{"role":"user","content":"create calculator.py"}],
		"tools":[{"name":"Write","input_schema":{"type":"object","properties":{"file_path":{"type":"string"},"content":{"type":"string"}},"required":["file_path","content"]}}]
	}`)
	rec1 := httptest.NewRecorder()
	h.HandleMessages(rec1, httptest.NewRequest(http.MethodPost, "/warp/v1/messages", bytes.NewReader(first)))
	if rec1.Code != http.StatusOK {
		t.Fatalf("first status=%d body=%s", rec1.Code, rec1.Body.String())
	}

	second := []byte(`{
		"model":"claude-opus-4-6",
		"stream":false,
		"messages":[
			{"role":"user","content":"create calculator.py"},
			{"role":"assistant","content":[{"type":"tool_use","id":"tool_write_1","name":"Write","input":{"file_path":"calculator.py","content":"print(1)"}}]},
			{"role":"user","content":[{"type":"tool_result","tool_use_id":"tool_write_1","content":"File created successfully"}]}
		],
		"tools":[{"name":"Write","input_schema":{"type":"object","properties":{"file_path":{"type":"string"},"content":{"type":"string"}},"required":["file_path","content"]}}]
	}`)
	rec2 := httptest.NewRecorder()
	h.HandleMessages(rec2, httptest.NewRequest(http.MethodPost, "/warp/v1/messages", bytes.NewReader(second)))
	if rec2.Code != http.StatusConflict {
		t.Fatalf("second status=%d body=%s", rec2.Code, rec2.Body.String())
	}

	calls := client.snapshotCalls()
	if len(calls) != 1 {
		t.Fatalf("calls=%d want 1", len(calls))
	}
	if calls[0].ChatSessionID != "" {
		t.Fatalf("new conversation id=%q want empty", calls[0].ChatSessionID)
	}
	if !strings.Contains(rec2.Body.String(), "stable conversation_id") {
		t.Fatalf("missing stable session diagnostic: %s", rec2.Body.String())
	}
}

func TestWarpNoToolsWriteIsReturnedAsText(t *testing.T) {
	t.Parallel()

	client := &fakePayloadClient{eventsByOp: [][]upstream.SSEMessage{{
		{Type: "model.tool-call", Event: map[string]interface{}{
			"toolCallId": "tool_write_unavailable",
			"toolName":   "Write",
			"input":      `{"file_path":"index.html","content":"<!doctype html><h1>Cherry ready</h1>"}`,
		}},
		{Type: "model.finish", Event: map[string]interface{}{"finishReason": "tool_use"}},
	}}}
	h := newTestHandler(client)
	body := []byte(`{
		"model":"claude-opus-4-6",
		"stream":false,
		"messages":[{"role":"user","content":"Create an index.html landing page"}]
	}`)
	rec := httptest.NewRecorder()
	h.HandleMessages(rec, httptest.NewRequest(http.MethodPost, "/warp/v1/chat/completions", bytes.NewReader(body)))
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}

	calls := client.snapshotCalls()
	if len(calls) != 1 || !calls[0].NoTools || len(calls[0].Tools) != 0 {
		t.Fatalf("upstream request did not enforce no-tools: %#v", calls)
	}
	if !strings.Contains(calls[0].Prompt, "<tool_gate>") {
		t.Fatalf("tool gate was not included in finalized prompt: %q", calls[0].Prompt)
	}
	out := rec.Body.String()
	if strings.Contains(out, `"tool_calls"`) || strings.Contains(out, `"name":"Write"`) {
		t.Fatalf("undeclared Write leaked to OpenAI client: %s", out)
	}
	if !strings.Contains(out, "Cherry ready") {
		t.Fatalf("generated file content was lost: %s", out)
	}
}

func TestWarpSuccessfulWriteResultGetsVisibleConfirmation(t *testing.T) {
	t.Parallel()

	client := &fakePayloadClient{eventsByOp: [][]upstream.SSEMessage{{
		{Type: "model.finish", Event: map[string]interface{}{
			"finishReason": "end_turn",
			"usage":        map[string]interface{}{"inputTokens": 50, "outputTokens": 0},
		}},
	}}}
	h := newTestHandler(client)
	h.sessionStore.SetWarpToolBinding(context.Background(), "test-conversation", "tool_write_done", WarpToolBinding{ConversationID: "warp_conv_write_done", ToolType: "call_mcp_tool"})
	body := []byte(`{
		"model":"claude-opus-4-6",
		"stream":false,"conversation_id":"test-conversation",
		"messages":[
			{"role":"user","content":"Create notes.txt"},
			{"role":"assistant","content":[{"type":"tool_use","id":"tool_write_done","name":"Write","input":{"file_path":"notes.txt","content":"done"}}]},
			{"role":"user","content":[{"type":"tool_result","tool_use_id":"tool_write_done","content":"File created successfully"}]}
		],
		"tools":[{"name":"Write","input_schema":{"type":"object","properties":{"file_path":{"type":"string"},"content":{"type":"string"}}}}]
	}`)
	rec := httptest.NewRecorder()
	h.HandleMessages(rec, httptest.NewRequest(http.MethodPost, "/warp/v1/messages", bytes.NewReader(body)))
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "File operation completed successfully.") {
		t.Fatalf("zero-output write result remained invisible: %s", rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), `"output_tokens":0`) {
		t.Fatalf("visible confirmation reported zero output tokens: %s", rec.Body.String())
	}
	if calls := client.snapshotCalls(); len(calls) != 1 {
		t.Fatalf("write result was replayed through extra upstream calls: %d", len(calls))
	}
}

func TestWarpEmptyOutputRecoveryMarkerDoesNotBecomeUserQuestion(t *testing.T) {
	t.Parallel()

	client := &fakePayloadClient{}
	h := newTestHandler(client)
	body := []byte(`{
		"model":"claude-opus-4-6",
		"stream":false,
		"messages":[
			{"role":"user","content":"Create notes.txt"},
			{"role":"assistant","content":[{"type":"tool_use","id":"tool_write_done","name":"Write","input":{"file_path":"notes.txt","content":"done"}}]},
			{"role":"user","content":[{"type":"tool_result","tool_use_id":"tool_write_done","content":"File created successfully"}]},
			{"role":"assistant","content":[]},
			{"role":"user","content":"[Your previous response had no visible output. Please continue and produce a user-visible response.]"}
		],
		"tools":[{"name":"Write","input_schema":{"type":"object","properties":{"file_path":{"type":"string"},"content":{"type":"string"}}}}]
	}`)
	rec := httptest.NewRecorder()
	h.HandleMessages(rec, httptest.NewRequest(http.MethodPost, "/warp/v1/messages", bytes.NewReader(body)))
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	calls := client.snapshotCalls()
	if len(calls) != 1 || !calls[0].NoTools {
		t.Fatalf("recovery request did not prevent duplicate tools: %#v", calls)
	}
	if strings.Contains(calls[0].Prompt, clientEmptyOutputRecoveryMarker) {
		t.Fatalf("client recovery marker was forwarded as the user's question: %q", calls[0].Prompt)
	}
	if !strings.Contains(calls[0].Prompt, "completed successfully") {
		t.Fatalf("recovery prompt lost operation context: %q", calls[0].Prompt)
	}
	if !strings.Contains(rec.Body.String(), "File operation completed successfully.") {
		t.Fatalf("recovery request remained empty: %s", rec.Body.String())
	}
}

func TestResolveWarpContinuationRejectsMixedConversations(t *testing.T) {
	t.Parallel()
	h := newTestHandler(&fakePayloadClient{})
	h.sessionStore.SetWarpToolBinding(context.Background(), "test-conversation", "tool_a", WarpToolBinding{ConversationID: "conv_a", AccountID: 1})
	h.sessionStore.SetWarpToolBinding(context.Background(), "test-conversation", "tool_b", WarpToolBinding{ConversationID: "conv_b", AccountID: 1})
	messages := []prompt.Message{{Role: "user", Content: prompt.MessageContent{Blocks: []prompt.ContentBlock{
		{Type: "tool_result", ToolUseID: "tool_a", Content: "a"},
		{Type: "tool_result", ToolUseID: "tool_b", Content: "b"},
	}}}}
	if _, err := h.resolveWarpContinuation(context.Background(), "test-conversation", messages); err == nil {
		t.Fatal("expected mixed-conversation tool results to be rejected")
	}
}

func TestResolveWarpContinuationRejectsExpiredToolBinding(t *testing.T) {
	t.Parallel()
	h := newTestHandler(&fakePayloadClient{})
	messages := []prompt.Message{{Role: "user", Content: prompt.MessageContent{Blocks: []prompt.ContentBlock{{
		Type: "tool_result", ToolUseID: "expired_tool", Content: "done",
	}}}}}
	if _, err := h.resolveWarpContinuation(context.Background(), "test-conversation", messages); err == nil || !strings.Contains(err.Error(), "expired or is unavailable") {
		t.Fatalf("error=%v want explicit expired continuation error", err)
	}
}

func TestWarpBindingInput_DoesNotPersistMCPPayloads(t *testing.T) {
	if got := warpBindingInput("call_mcp_tool", `{"content":"large source file"}`); got != "" {
		t.Fatalf("MCP input was persisted: %q", got)
	}
	if got := warpBindingInput("run_shell_command", `{"command":"pwd"}`); got != `{"command":"pwd"}` {
		t.Fatalf("native shell input=%q", got)
	}
}

func TestPuterCurrentWorkdirAfterToolTurn_ReturnsLocalResponse(t *testing.T) {
	t.Parallel()

	client := &fakePayloadClient{}
	h := newTestHandler(client)
	body := []byte(`{
		"model":"claude-opus-5",
		"stream":false,
		"conversation_id":"puter_fresh_reset",
		"messages":[
			{"role":"user","content":[{"type":"text","text":"帮我用python写一个计算器"}]},
			{"role":"assistant","content":[
				{"type":"tool_use","id":"tool_write","name":"Write","input":{"file_path":"calculator.py","content":"print(1)"}}
			]},
			{"role":"user","content":[
				{"type":"tool_result","tool_use_id":"tool_write","content":"File created successfully at: calculator.py"}
			]},
			{"role":"user","content":[{"type":"text","text":"当前运行的目录"}]}
		],
		"tools":[
			{"name":"Read","input_schema":{"type":"object","properties":{"file_path":{"type":"string"}},"required":["file_path"]}},
			{"name":"Write","input_schema":{"type":"object","properties":{"file_path":{"type":"string"},"content":{"type":"string"}},"required":["file_path","content"]}}
		]
	}`)

	req := httptest.NewRequest(http.MethodPost, "/puter/v1/messages", bytes.NewReader(body))
	req.Header.Set("X-Workdir", `C:\Users\zhangdailin\Desktop\新建文件夹`)
	rec := httptest.NewRecorder()

	h.HandleMessages(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("request status = %d, want %d", rec.Code, http.StatusOK)
	}

	calls := client.snapshotCalls()
	if len(calls) != 0 {
		t.Fatalf("expected 0 upstream calls for local workdir response, got %d", len(calls))
	}
	if out := rec.Body.String(); !strings.Contains(out, `C:\\Users\\zhangdailin\\Desktop\\新建文件夹`) {
		t.Fatalf("expected local response to include exact workdir, got: %s", out)
	}
}

func TestPuterToolResultFollowup_RecoversSandboxPathFailureWithoutNoToolsGate(t *testing.T) {
	t.Parallel()

	client := &fakePayloadClient{}
	h := newTestHandler(client)

	body := []byte(`{
		"model":"claude-opus-5",
		"stream":false,
		"conversation_id":"puter_followup_recover",
		"messages":[
			{"role":"user","content":[{"type":"text","text":"这个项目是干什么的"}]},
			{"role":"assistant","content":[
				{"type":"tool_use","id":"tool_ls","name":"Bash","input":{"command":"ls /tmp/cc-agent/sb1-fxjxbmvk/project","description":"List project files"}}
			]},
			{"role":"user","content":[
				{"type":"tool_result","tool_use_id":"tool_ls","content":"Exit code 2\nls: cannot access '/tmp/cc-agent/sb1-fxjxbmvk/project': No such file or directory"},
				{"type":"text","text":"这个项目是干什么的"}
			]}
		],
		"tools":[
			{"name":"Read","input_schema":{"type":"object","properties":{"file_path":{"type":"string"}},"required":["file_path"]}},
			{"name":"Bash","input_schema":{"type":"object","properties":{"command":{"type":"string"}},"required":["command"]}}
		]
	}`)

	req := httptest.NewRequest(http.MethodPost, "/puter/v1/messages", bytes.NewReader(body))
	rec := httptest.NewRecorder()

	h.HandleMessages(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("request status = %d, want %d", rec.Code, http.StatusOK)
	}

	calls := client.snapshotCalls()
	if len(calls) != 1 {
		t.Fatalf("expected 1 upstream call, got %d", len(calls))
	}
	if calls[0].NoTools {
		t.Fatalf("expected puter follow-up after sandbox path miss to keep tools enabled")
	}
}

func TestPuterOpenAIChatCompletionsToolFollowup_NormalizesToolMessages(t *testing.T) {
	t.Parallel()

	client := &fakePayloadClient{}
	h := newTestHandler(client)

	body := []byte(`{
		"model":"claude-opus-5",
		"stream":false,
		"conversation_id":"puter_openai_tool_followup",
		"messages":[
			{"role":"user","content":"Create note.txt with hello world"},
			{"role":"assistant","content":null,"tool_calls":[
				{"id":"call_write_1","type":"function","function":{"name":"Write","arguments":"{\"file_path\":\"note.txt\",\"content\":\"hello world\"}"}}
			]},
			{"role":"tool","tool_call_id":"call_write_1","content":"Write succeeded: note.txt created with hello world"}
		],
		"tools":[
			{"type":"function","function":{"name":"Write","description":"Write content to a file","parameters":{"type":"object","properties":{"file_path":{"type":"string"},"content":{"type":"string"}},"required":["file_path","content"]}}}
		]
	}`)

	req := httptest.NewRequest(http.MethodPost, "/puter/v1/chat/completions", bytes.NewReader(body))
	rec := httptest.NewRecorder()

	h.HandleMessages(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("request status = %d, want %d: %s", rec.Code, http.StatusOK, rec.Body.String())
	}

	calls := client.snapshotCalls()
	if len(calls) != 1 {
		t.Fatalf("expected 1 upstream call, got %d", len(calls))
	}
	if calls[0].NoTools {
		t.Fatalf("expected openai puter tool follow-up to keep tools enabled")
	}

	if len(calls[0].Messages) != 3 {
		t.Fatalf("expected 3 normalized messages, got %d", len(calls[0].Messages))
	}

	assistantMsg := calls[0].Messages[1]
	if assistantMsg.Role != "assistant" {
		t.Fatalf("assistant role = %q, want assistant", assistantMsg.Role)
	}
	if assistantMsg.Content.IsString() {
		t.Fatal("expected assistant tool call message to normalize into content blocks")
	}
	assistantBlocks := assistantMsg.Content.GetBlocks()
	if len(assistantBlocks) != 1 {
		t.Fatalf("assistant blocks len = %d, want 1", len(assistantBlocks))
	}
	if assistantBlocks[0].Type != "tool_use" || assistantBlocks[0].Name != "Write" || assistantBlocks[0].ID != "call_write_1" {
		t.Fatalf("unexpected assistant tool_use block: %#v", assistantBlocks[0])
	}
	input, ok := assistantBlocks[0].Input.(map[string]interface{})
	if !ok {
		t.Fatalf("assistant tool input type = %T, want map[string]interface{}", assistantBlocks[0].Input)
	}
	if input["file_path"] != "note.txt" || input["content"] != "hello world" {
		t.Fatalf("assistant tool input = %#v", input)
	}

	toolResultMsg := calls[0].Messages[2]
	if toolResultMsg.Role != "user" {
		t.Fatalf("tool result role = %q, want user", toolResultMsg.Role)
	}
	if toolResultMsg.Content.IsString() {
		t.Fatal("expected tool result follow-up to normalize into content blocks")
	}
	resultBlocks := toolResultMsg.Content.GetBlocks()
	if len(resultBlocks) != 1 {
		t.Fatalf("tool result blocks len = %d, want 1", len(resultBlocks))
	}
	if resultBlocks[0].Type != "tool_result" || resultBlocks[0].ToolUseID != "call_write_1" {
		t.Fatalf("unexpected tool_result block: %#v", resultBlocks[0])
	}
	if got, ok := resultBlocks[0].Content.(string); !ok || !strings.Contains(got, "Write succeeded") {
		t.Fatalf("tool_result content = %#v", resultBlocks[0].Content)
	}
}

func TestPuterToolResultFollowup_PassesThroughUpstreamInsteadOfLocalFallback(t *testing.T) {
	t.Parallel()

	client := &fakePayloadClient{
		eventsByOp: [][]upstream.SSEMessage{{
			{Type: "model", Event: map[string]any{"type": "text-start"}},
			{Type: "model", Event: map[string]any{"type": "text-delta", "delta": "Let me first understand the project structure and code."}},
			{Type: "model", Event: map[string]any{"type": "finish", "finishReason": "stop"}},
		}},
	}
	h := newTestHandler(client)
	body := []byte(`{
		"model":"claude-opus-5",
		"stream":false,
		"conversation_id":"puter_followup_local_fallback",
		"messages":[
			{"role":"user","content":[{"type":"text","text":"这个项目是干什么的"}]},
			{"role":"assistant","content":[
				{"type":"tool_use","id":"tool_ls","name":"Bash","input":{"command":"ls -la","description":"List project files"}}
			]},
			{"role":"user","content":[
				{"type":"tool_result","tool_use_id":"tool_ls","content":"README.md\napi.py\ndashboard.py\nweb-ui/\nweb-ui/package.json\nweb-ui/src/\nrequirements.txt"}
			]}
		],
		"tools":[
			{"name":"Read","input_schema":{"type":"object","properties":{"file_path":{"type":"string"}},"required":["file_path"]}},
			{"name":"Bash","input_schema":{"type":"object","properties":{"command":{"type":"string"}},"required":["command"]}}
		]
	}`)

	req := httptest.NewRequest(http.MethodPost, "/puter/v1/messages", bytes.NewReader(body))
	rec := httptest.NewRecorder()

	h.HandleMessages(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("request status = %d, want %d", rec.Code, http.StatusOK)
	}

	calls := client.snapshotCalls()
	if len(calls) != 1 {
		t.Fatalf("expected upstream passthrough call, got %d", len(calls))
	}

	out := rec.Body.String()
	if !strings.Contains(out, "Let me first understand the project structure and code.") {
		t.Fatalf("expected upstream text to be preserved, got: %s", out)
	}
	for _, unwanted := range []string{"前端", "后端", "脚本层", "当前只拿到目录概览", "基于当前已读取内容"} {
		if strings.Contains(out, unwanted) {
			t.Fatalf("did not expect local fallback text %q in %s", unwanted, out)
		}
	}
}

func TestPuterMultiTurnEditFollowup_PreservesHistory(t *testing.T) {
	t.Parallel()

	client := &fakePayloadClient{}
	h := newTestHandler(client)
	body := []byte(`{
		"model":"claude-opus-5",
		"stream":false,
		"conversation_id":"puter_multiturn_scientific_notation",
		"messages":[
			{"role":"user","content":[{"type":"text","text":"帮我用python写一个计算器"}]},
			{"role":"assistant","content":[
				{"type":"tool_use","id":"tool_write","name":"Write","input":{"file_path":"calculator.py","content":"print(1)"}}
			]},
			{"role":"user","content":[
				{"type":"tool_result","tool_use_id":"tool_write","content":"File created successfully at: calculator.py"}
			]},
			{"role":"assistant","content":[
				{"type":"text","text":"完成！计算器已创建在项目目录中。"}
			]},
			{"role":"user","content":[{"type":"text","text":"帮我添加科学计数法"}]}
		],
		"tools":[
			{"name":"Write","input_schema":{"type":"object","properties":{"file_path":{"type":"string"},"content":{"type":"string"}},"required":["file_path","content"]}},
			{"name":"Edit","input_schema":{"type":"object","properties":{"file_path":{"type":"string"},"old_string":{"type":"string"},"new_string":{"type":"string"}},"required":["file_path","old_string","new_string"]}}
		]
	}`)

	req := httptest.NewRequest(http.MethodPost, "/puter/v1/messages", bytes.NewReader(body))
	rec := httptest.NewRecorder()

	h.HandleMessages(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("request status = %d, want %d", rec.Code, http.StatusOK)
	}

	calls := client.snapshotCalls()
	if len(calls) != 1 {
		t.Fatalf("expected 1 upstream call, got %d", len(calls))
	}
	if len(calls[0].Messages) != 5 {
		t.Fatalf("messages len=%d want 5", len(calls[0].Messages))
	}
	if got := calls[0].Messages[0].ExtractText(); got != "帮我用python写一个计算器" {
		t.Fatalf("first user text=%q want original create request", got)
	}
	if got := calls[0].Messages[3].ExtractText(); got != "完成！计算器已创建在项目目录中。" {
		t.Fatalf("assistant completion=%q want preserved assistant summary", got)
	}
	if got := calls[0].Messages[4].ExtractText(); got != "帮我添加科学计数法" {
		t.Fatalf("latest user text=%q want edit follow-up", got)
	}
}

func TestWarpConversationID_PersistedWithConversationKey(t *testing.T) {
	t.Parallel()

	client := &fakePayloadClient{
		conversationIDsByOp: []string{"warp_upstream_conv_persist"},
	}
	h := newTestHandler(client)
	const conversationID = "local_conversation_key_1"

	req1 := httptest.NewRequest(http.MethodPost, "/warp/v1/messages", bytes.NewReader(makeWarpRequestBody(t, "first", conversationID)))
	rec1 := httptest.NewRecorder()
	h.HandleMessages(rec1, req1)
	if rec1.Code != http.StatusOK {
		t.Fatalf("first request status = %d, want %d", rec1.Code, http.StatusOK)
	}

	req2 := httptest.NewRequest(http.MethodPost, "/warp/v1/messages", bytes.NewReader(makeWarpRequestBody(t, "second", conversationID)))
	rec2 := httptest.NewRecorder()
	h.HandleMessages(rec2, req2)
	if rec2.Code != http.StatusOK {
		t.Fatalf("second request status = %d, want %d", rec2.Code, http.StatusOK)
	}

	calls := client.snapshotCalls()
	if len(calls) != 2 {
		t.Fatalf("expected 2 upstream calls, got %d", len(calls))
	}

	if calls[0].ChatSessionID != "" {
		t.Fatalf("first ChatSessionID = %q, want empty for a new official Warp conversation", calls[0].ChatSessionID)
	}
	if calls[1].ChatSessionID != "warp_upstream_conv_persist" {
		t.Fatalf("second ChatSessionID = %q, want %q", calls[1].ChatSessionID, "warp_upstream_conv_persist")
	}
}

func TestWarpPassthrough_DoesNotTrimMessagesOrSanitizeSystem(t *testing.T) {
	t.Parallel()

	client := &fakePayloadClient{}
	h := &Handler{
		config: &config.Config{
			DebugEnabled:           false,
			WarpMaxHistoryMessages: 1,
			WarpMaxToolResults:     1,
		},
		client:       client,
		sessionStore: NewMemorySessionStore(30*time.Minute, 1024),
		auditLogger:  audit.NewNopLogger(),
	}

	reqPayload := ClaudeRequest{
		Model: "claude-opus-4-6",
		Messages: []prompt.Message{
			{Role: "user", Content: prompt.MessageContent{Text: "m1"}},
			{Role: "assistant", Content: prompt.MessageContent{Text: "m2"}},
			{Role: "user", Content: prompt.MessageContent{Text: "m3"}},
		},
		System: []prompt.SystemItem{
			{Type: "text", Text: "You are Claude Code, Anthropic's official CLI for Claude."},
			{Type: "text", Text: "cc_entrypoint=claude-code; keep=this"},
		},
		Stream: false,
		Tools:  []interface{}{},
	}

	body, err := json.Marshal(reqPayload)
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/warp/v1/messages", bytes.NewReader(body))
	rec := httptest.NewRecorder()

	h.HandleMessages(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("request status = %d, want %d", rec.Code, http.StatusOK)
	}

	calls := client.snapshotCalls()
	if len(calls) != 1 {
		t.Fatalf("expected 1 upstream call, got %d", len(calls))
	}

	if len(calls[0].Messages) != len(reqPayload.Messages) {
		t.Fatalf("messages len = %d, want %d", len(calls[0].Messages), len(reqPayload.Messages))
	}
	if len(calls[0].System) != len(reqPayload.System) {
		t.Fatalf("system len = %d, want %d", len(calls[0].System), len(reqPayload.System))
	}
	if !strings.Contains(calls[0].System[0].Text, "Claude Code") {
		t.Fatalf("expected warp system prompt to be unchanged, got %q", calls[0].System[0].Text)
	}
	if !strings.Contains(calls[0].System[1].Text, "cc_entrypoint=claude-code") {
		t.Fatalf("expected cc_entrypoint to be preserved for warp, got %q", calls[0].System[1].Text)
	}
}

func TestWarpToolResultFollowupWithText_HonorsEmptyTools(t *testing.T) {
	t.Parallel()

	client := &fakePayloadClient{}
	h := newTestHandler(client)
	h.sessionStore.SetWarpToolBinding(context.Background(), "test-conversation", "tool_1", WarpToolBinding{ConversationID: "warp_conv_tool_1", ToolType: "read_files"})

	body := []byte(`{
		"model":"claude-opus-4-6",
		"stream":false,"conversation_id":"test-conversation",
		"messages":[
			{
				"role":"user",
				"content":[
					{"type":"text","text":"You are an interactive agent that helps users with software engineering tasks.\n# Environment\nPrimary working directory: /Users/dailin/Documents/GitHub/truth_social_scraper\n# auto memory\ngitStatus: dirty\nRecent commits: abcdef"}
				]
			},
			{
				"role":"assistant",
				"content":[
					{"type":"tool_use","id":"tool_1","name":"Read","input":{"file_path":"utils.py"}}
				]
			},
			{
				"role":"user",
				"content":[
					{"type":"tool_result","tool_use_id":"tool_1","content":"1→import json\n2→import os\n3→from urllib.request import Request\n4→import socks\n5→from flask import Flask\n6→def load_media_mapping():\n7→    with open(MEDIA_MAPPING_FILE, \"r\") as f:\n8→        return json.load(f)\n9→ALERTS_FILE = os.path.join(PROJECT_ROOT, \"market_alerts.json\")"},
					{"type":"text","text":"这个项目使用了哪些技术架构"}
				]
			}
		],
		"tools":[]
	}`)

	req := httptest.NewRequest(http.MethodPost, "/warp/v1/messages", bytes.NewReader(body))
	rec := httptest.NewRecorder()

	h.HandleMessages(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("request status = %d, want %d", rec.Code, http.StatusOK)
	}

	calls := client.snapshotCalls()
	if len(calls) != 1 {
		t.Fatalf("expected 1 upstream call, got %d", len(calls))
	}
	if !calls[0].NoTools {
		t.Fatalf("expected explicit tools:[] to disable tools on the follow-up")
	}
}

func TestWarpToolResultFollowup_RepeatedWriteIsForwarded(t *testing.T) {
	t.Parallel()

	client := &fakePayloadClient{
		eventsByOp: [][]upstream.SSEMessage{
			{
				{
					Type: "model.tool-call",
					Event: map[string]interface{}{
						"toolCallId": "tool_new_1",
						"toolName":   "Write",
						"input":      `{"file_path":"scratch.txt","content":"alpha\nbeta\n"}`,
					},
				},
				{Type: "model.finish", Event: map[string]interface{}{"finishReason": "tool_use"}},
			},
		},
	}
	h := newTestHandler(client)
	h.sessionStore.SetWarpToolBinding(context.Background(), "test-conversation", "tool_old_1", WarpToolBinding{ConversationID: "warp_conv_tool_old", ToolType: "call_mcp_tool"})

	body := []byte(`{
		"model":"claude-opus-4-6",
		"stream":false,"conversation_id":"test-conversation",
		"messages":[
			{
				"role":"user",
				"content":[
					{"type":"text","text":"Create scratch.txt with alpha and beta"}
				]
			},
			{
				"role":"assistant",
				"content":[
					{"type":"tool_use","id":"tool_old_1","name":"Write","input":{"file_path":"scratch.txt","content":"alpha\nbeta\n"}}
				]
			},
			{
				"role":"user",
				"content":[
					{"type":"tool_result","tool_use_id":"tool_old_1","content":"Done"}
				]
			}
		],
		"tools":[
			{
				"name":"Write",
				"description":"Write a file",
				"input_schema":{
					"type":"object",
					"properties":{
						"file_path":{"type":"string"},
						"content":{"type":"string"}
					},
					"required":["file_path","content"]
				}
			}
		]
	}`)

	req := httptest.NewRequest(http.MethodPost, "/warp/v1/messages", bytes.NewReader(body))
	rec := httptest.NewRecorder()

	h.HandleMessages(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("request status = %d, want %d", rec.Code, http.StatusOK)
	}

	out := rec.Body.String()
	if strings.Contains(out, "No output was presented to the user") {
		t.Fatalf("did not expect generic empty fallback in response, got: %s", out)
	}
	if strings.Contains(out, "duplicate mutating tool call was suppressed") {
		t.Fatalf("did not expect duplicate-tool-result fallback in response, got: %s", out)
	}
	if !strings.Contains(out, "tool_new_1") || !strings.Contains(out, `"name":"Write"`) {
		t.Fatalf("repeat tool call was suppressed: %s", out)
	}

}

func TestWarpToolResultFollowup_SendsAllCurrentTurnResultsInOneRequest(t *testing.T) {
	t.Parallel()

	client := &fakePayloadClient{}
	h := newTestHandler(client)
	h.sessionStore.SetConvID(context.Background(), "local_conversation_key_split", "warp_conv_existing")
	for _, id := range []string{"tool_ls", "tool_api", "tool_utils"} {
		h.sessionStore.SetWarpToolBinding(context.Background(), "local_conversation_key_split", id, WarpToolBinding{ConversationID: "warp_conv_existing", ToolType: "call_mcp_tool"})
	}

	body := []byte(`{
		"model":"claude-opus-4-6",
		"stream":false,
		"conversation_id":"local_conversation_key_split",
		"messages":[
			{"role":"user","content":[{"type":"text","text":"帮我优化一下这个项目"}]},
			{"role":"assistant","content":[
				{"type":"tool_use","id":"tool_ls","name":"Bash","input":{"command":"ls -la /Users/dailin/Documents/GitHub/truth_social_scraper"}},
				{"type":"tool_use","id":"tool_api","name":"Read","input":{"file_path":"/Users/dailin/Documents/GitHub/truth_social_scraper/api.py"}},
				{"type":"tool_use","id":"tool_utils","name":"Read","input":{"file_path":"/Users/dailin/Documents/GitHub/truth_social_scraper/utils.py"}}
			]},
			{"role":"user","content":[
				{"type":"tool_result","tool_use_id":"tool_ls","content":"README.md\napi.py\nutils.py"},
				{"type":"tool_result","tool_use_id":"tool_api","content":"from fastapi import FastAPI\napp = FastAPI()"},
				{"type":"tool_result","tool_use_id":"tool_utils","content":"import json\nALERTS_FILE='alerts.json'"},
				{"type":"text","text":"帮我优化一下这个项目"}
			]}
		],
		"tools":[]
	}`)

	req := httptest.NewRequest(http.MethodPost, "/warp/v1/messages", bytes.NewReader(body))
	rec := httptest.NewRecorder()

	h.HandleMessages(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("request status = %d, want %d", rec.Code, http.StatusOK)
	}

	calls := client.snapshotCalls()
	if len(calls) != 1 {
		t.Fatalf("expected one official UserInputs request, got %d", len(calls))
	}

	countToolResults := func(msgs []prompt.Message) int {
		total := 0
		for _, msg := range msgs {
			for _, block := range msg.Content.Blocks {
				if block.Type == "tool_result" {
					total++
				}
			}
		}
		return total
	}

	if got := countToolResults(calls[0].Messages); got != 3 {
		t.Fatalf("tool_results = %d, want %d", got, 3)
	}
	if got := strings.TrimSpace(calls[0].Messages[len(calls[0].Messages)-1].ExtractText()); got != "帮我优化一下这个项目" {
		t.Fatalf("user text = %q, want final user request", got)
	}
}

func TestWarpToolResultFollowup_StreamsSingleBatchedResponse(t *testing.T) {
	t.Parallel()

	client := &fakePayloadClient{
		eventsByOp: [][]upstream.SSEMessage{
			{
				{Type: "model.conversation_id", Event: map[string]interface{}{"id": "warp_conv_batch"}},
				{Type: "model.text-delta", Event: map[string]interface{}{"delta": "Let me dig into the rest of the codebase first."}},
				{
					Type: "model.tool-call",
					Event: map[string]interface{}{
						"toolCallId": "tool_visible",
						"toolName":   "Read",
						"input":      `{"file_path":"/Users/dailin/Documents/GitHub/truth_social_scraper/monitor_trump.py"}`,
					},
				},
				{Type: "model.finish", Event: map[string]interface{}{"finishReason": "tool_use"}},
			},
		},
	}
	h := newTestHandler(client)
	h.sessionStore.SetConvID(context.Background(), "local_conversation_key_intermediate", "warp_conv_existing")
	for _, id := range []string{"tool_ls", "tool_api", "tool_utils"} {
		h.sessionStore.SetWarpToolBinding(context.Background(), "local_conversation_key_intermediate", id, WarpToolBinding{ConversationID: "warp_conv_existing", ToolType: "call_mcp_tool"})
	}

	body := []byte(`{
		"model":"claude-opus-4-6",
		"stream":true,
		"conversation_id":"local_conversation_key_intermediate",
		"messages":[
			{"role":"user","content":[{"type":"text","text":"帮我优化一下这个项目"}]},
			{"role":"assistant","content":[
				{"type":"tool_use","id":"tool_ls","name":"Bash","input":{"command":"ls -la /Users/dailin/Documents/GitHub/truth_social_scraper"}},
				{"type":"tool_use","id":"tool_api","name":"Read","input":{"file_path":"/Users/dailin/Documents/GitHub/truth_social_scraper/api.py"}},
				{"type":"tool_use","id":"tool_utils","name":"Read","input":{"file_path":"/Users/dailin/Documents/GitHub/truth_social_scraper/utils.py"}}
			]},
			{"role":"user","content":[
				{"type":"tool_result","tool_use_id":"tool_ls","content":"README.md\napi.py\nutils.py"},
				{"type":"tool_result","tool_use_id":"tool_api","content":"from fastapi import FastAPI\napp = FastAPI()"},
				{"type":"tool_result","tool_use_id":"tool_utils","content":"import json\nALERTS_FILE='alerts.json'"},
				{"type":"text","text":"帮我优化一下这个项目"}
			]}
		],
		"tools":[{"name":"Read","input_schema":{"type":"object","properties":{"file_path":{"type":"string"}},"required":["file_path"]}}]
	}`)

	req := httptest.NewRequest(http.MethodPost, "/warp/v1/messages", bytes.NewReader(body))
	rec := httptest.NewRecorder()

	h.HandleMessages(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("request status = %d, want %d", rec.Code, http.StatusOK)
	}

	calls := client.snapshotCalls()
	if len(calls) != 1 {
		t.Fatalf("expected one batched request, got %d calls", len(calls))
	}

	out := rec.Body.String()
	if !strings.Contains(out, "Let me dig into the rest of the codebase first.") {
		t.Fatalf("expected intermediate text to be replayed, got: %s", out)
	}
	if !strings.Contains(out, "monitor_trump.py") {
		t.Fatalf("expected intermediate tool call to be replayed, got: %s", out)
	}
}
