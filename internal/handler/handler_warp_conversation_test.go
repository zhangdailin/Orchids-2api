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

func (f *fakePayloadClient) SendRequest(ctx context.Context, prompt string, chatHistory []interface{}, model string, onMessage func(upstream.SSEMessage), logger *debug.Logger) error {
	return nil
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
		dedupStore:   NewMemoryDedupStore(duplicateWindow, duplicateCleanupWindow),
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

	if !strings.HasPrefix(calls[0].ChatSessionID, "chat_") {
		t.Fatalf("first ChatSessionID = %q, expected chat_*", calls[0].ChatSessionID)
	}
	if !strings.HasPrefix(calls[1].ChatSessionID, "chat_") {
		t.Fatalf("second ChatSessionID = %q, expected chat_*", calls[1].ChatSessionID)
	}
	if calls[1].ChatSessionID == "warp_upstream_conv_1" {
		t.Fatalf("second request unexpectedly reused upstream conversation id: %q", calls[1].ChatSessionID)
	}
	// Verify empty conversation key does not store convID
	if _, ok := h.sessionStore.GetConvID(context.Background(), ""); ok {
		t.Fatalf("unexpected cached conversation id for empty conversation key")
	}
}

func TestBoltToolResultFollowup_RecoversSandboxPathFailureWithoutNoToolsGate(t *testing.T) {
	t.Parallel()

	client := &fakePayloadClient{}
	h := newTestHandler(client)

	body := []byte(`{
		"model":"claude-sonnet-4-6",
		"stream":false,
		"conversation_id":"bolt_followup_recover",
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

	req := httptest.NewRequest(http.MethodPost, "/bolt/v1/messages", bytes.NewReader(body))
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
		t.Fatalf("expected bolt follow-up after sandbox path miss to keep tools enabled")
	}
}

func TestPuterToolResultFollowup_RecoversSandboxPathFailureWithoutNoToolsGate(t *testing.T) {
	t.Parallel()

	client := &fakePayloadClient{}
	h := newTestHandler(client)

	body := []byte(`{
		"model":"claude-sonnet-4-6",
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

func TestBoltToolResultFollowup_PassesThroughUpstreamInsteadOfLocalFallback(t *testing.T) {
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
		"model":"claude-sonnet-4-6",
		"stream":false,
		"conversation_id":"bolt_followup_local_fallback",
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

	req := httptest.NewRequest(http.MethodPost, "/bolt/v1/messages", bytes.NewReader(body))
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

func TestOrchidsPayload_UsesProtocolPromptView(t *testing.T) {
	t.Parallel()

	client := &fakePayloadClient{}
	h := newTestHandler(client)

	reqPayload := ClaudeRequest{
		Model: "claude-sonnet-4-6",
		Messages: []prompt.Message{
			{Role: "user", Content: prompt.MessageContent{Text: "hi"}},
		},
		System: []prompt.SystemItem{
			{Type: "text", Text: "system rules"},
		},
		Stream: false,
		Tools:  []interface{}{},
	}

	body, err := json.Marshal(reqPayload)
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/orchids/v1/messages", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	h.HandleMessages(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d want %d: %s", rec.Code, http.StatusOK, rec.Body.String())
	}

	calls := client.snapshotCalls()
	if len(calls) != 1 {
		t.Fatalf("calls len=%d want 1", len(calls))
	}
	if strings.Contains(calls[0].Prompt, "<env>") || strings.Contains(calls[0].Prompt, "<rules>") {
		t.Fatalf("prompt=%q want protocol prompt without legacy wrapper", calls[0].Prompt)
	}
	if !strings.Contains(calls[0].Prompt, "<user>") || !strings.Contains(calls[0].Prompt, "hi") {
		t.Fatalf("prompt=%q want protocol user block", calls[0].Prompt)
	}
	if len(calls[0].ChatHistory) != 0 {
		t.Fatalf("chatHistory=%#v want empty for orchids protocol mode", calls[0].ChatHistory)
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

	if !strings.HasPrefix(calls[0].ChatSessionID, "chat_") {
		t.Fatalf("first ChatSessionID = %q, expected chat_*", calls[0].ChatSessionID)
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
			DebugEnabled:            false,
			WarpMaxHistoryMessages:  1,
			WarpMaxToolResults:      1,
			OrchidsCCEntrypointMode: "strip",
		},
		client:       client,
		sessionStore: NewMemorySessionStore(30*time.Minute, 1024),
		dedupStore:   NewMemoryDedupStore(duplicateWindow, duplicateCleanupWindow),
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

func TestWarpToolResultFollowupWithText_DisablesTools(t *testing.T) {
	t.Parallel()

	client := &fakePayloadClient{}
	h := newTestHandler(client)

	body := []byte(`{
		"model":"claude-opus-4-6",
		"stream":false,
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
	if calls[0].NoTools {
		t.Fatalf("expected warp follow-up with tool_result+text to keep passthrough tools enabled")
	}
}

func TestWarpToolResultFollowup_DuplicateWriteFallsBackToPriorToolResult(t *testing.T) {
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

	body := []byte(`{
		"model":"claude-opus-4-6",
		"stream":false,
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
	if !strings.Contains(out, duplicateToolResultFallbackText) {
		t.Fatalf("expected duplicate-tool-result fallback in response, got: %s", out)
	}
	if strings.Contains(out, genericEmptyOutputFallbackText) {
		t.Fatalf("did not expect generic empty fallback in response, got: %s", out)
	}
}

func TestWarpToolResultFollowup_SplitsCurrentTurnAndChainsConversationIDs(t *testing.T) {
	t.Parallel()

	client := &fakePayloadClient{
		conversationIDsByOp: []string{"warp_conv_batch_1", "warp_conv_batch_2"},
	}
	h := newTestHandler(client)

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
	if len(calls) != 3 {
		t.Fatalf("expected 3 upstream calls, got %d", len(calls))
	}
	if calls[1].ChatSessionID != "warp_conv_batch_1" {
		t.Fatalf("second batch ChatSessionID = %q, want %q", calls[1].ChatSessionID, "warp_conv_batch_1")
	}
	if calls[2].ChatSessionID != "warp_conv_batch_2" {
		t.Fatalf("third batch ChatSessionID = %q, want %q", calls[2].ChatSessionID, "warp_conv_batch_2")
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

	if got := countToolResults(calls[0].Messages); got != 1 {
		t.Fatalf("first batch tool_results = %d, want %d", got, 1)
	}
	if got := countToolResults(calls[1].Messages); got != 2 {
		t.Fatalf("second batch tool_results = %d, want %d", got, 2)
	}
	if got := countToolResults(calls[2].Messages); got != 3 {
		t.Fatalf("third batch tool_results = %d, want %d", got, 3)
	}

	for i := 0; i < 2; i++ {
		lastMsg := calls[i].Messages[len(calls[i].Messages)-1]
		if got := strings.TrimSpace(lastMsg.ExtractText()); got != "" {
			t.Fatalf("batch %d unexpectedly kept current-turn user text: %q", i+1, got)
		}
	}
	if got := strings.TrimSpace(calls[2].Messages[len(calls[2].Messages)-1].ExtractText()); got != "帮我优化一下这个项目" {
		t.Fatalf("last batch user text = %q, want final user request", got)
	}
}

func TestWarpToolResultFollowup_ReplaysVisibleIntermediateBatch(t *testing.T) {
	t.Parallel()

	client := &fakePayloadClient{
		eventsByOp: [][]upstream.SSEMessage{
			{
				{Type: "model.conversation_id", Event: map[string]interface{}{"id": "warp_conv_batch_1"}},
				{Type: "model.finish", Event: map[string]interface{}{"finishReason": "end_turn"}},
			},
			{
				{Type: "model.conversation_id", Event: map[string]interface{}{"id": "warp_conv_batch_2"}},
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
		"tools":[]
	}`)

	req := httptest.NewRequest(http.MethodPost, "/warp/v1/messages", bytes.NewReader(body))
	rec := httptest.NewRecorder()

	h.HandleMessages(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("request status = %d, want %d", rec.Code, http.StatusOK)
	}

	calls := client.snapshotCalls()
	if len(calls) != 2 {
		t.Fatalf("expected batching to stop after visible intermediate output, got %d calls", len(calls))
	}

	out := rec.Body.String()
	if !strings.Contains(out, "Let me dig into the rest of the codebase first.") {
		t.Fatalf("expected intermediate text to be replayed, got: %s", out)
	}
	if !strings.Contains(out, "monitor_trump.py") {
		t.Fatalf("expected intermediate tool call to be replayed, got: %s", out)
	}
}
