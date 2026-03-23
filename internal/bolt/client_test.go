package bolt

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/goccy/go-json"

	"orchids-api/internal/config"
	"orchids-api/internal/prompt"
	"orchids-api/internal/store"
	"orchids-api/internal/upstream"
)

func TestSendRequestWithPayload_EmitsModelEvents(t *testing.T) {
	prevURL := boltAPIURL
	prevRootURL := boltRootDataURL
	t.Cleanup(func() {
		boltAPIURL = prevURL
		boltRootDataURL = prevRootURL
	})

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Cookie"); !strings.Contains(got, "__session=session-token") {
			t.Fatalf("cookie=%q", got)
		}
		body, _ := io.ReadAll(r.Body)
		if !strings.Contains(string(body), `"projectId":"sb1-demo"`) {
			t.Fatalf("request body missing projectId: %s", string(body))
		}
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "0:\"hello\"\n")
		_, _ = io.WriteString(w, "e:{\"finishReason\":\"stop\",\"usage\":{\"promptTokens\":5,\"completionTokens\":7}}\n")
	}))
	defer srv.Close()
	boltAPIURL = srv.URL

	client := NewFromAccount(&store.Account{
		AccountType:   "bolt",
		SessionCookie: "session-token",
		ProjectID:     "sb1-demo",
	}, nil)

	var events []string
	err := client.SendRequestWithPayload(context.Background(), upstream.UpstreamRequest{
		Model: "claude-opus-4-6",
		Messages: []prompt.Message{
			{Role: "user", Content: prompt.MessageContent{Text: "hello"}},
		},
	}, func(msg upstream.SSEMessage) {
		events = append(events, msg.Type)
	}, nil)
	if err != nil {
		t.Fatalf("SendRequestWithPayload() error = %v", err)
	}

	want := []string{"model.text-delta", "model.finish"}
	if strings.Join(events, ",") != strings.Join(want, ",") {
		t.Fatalf("events=%v want %v", events, want)
	}
}

func TestSendRequestWithPayload_ConvertsJSONToolCallTextToModelToolCall(t *testing.T) {
	prevURL := boltAPIURL
	t.Cleanup(func() { boltAPIURL = prevURL })

	toolJSON, err := json.Marshal(`{"tool":"Read","parameters":{"file_path":"README.md"}}`)
	if err != nil {
		t.Fatalf("marshal tool json: %v", err)
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "0:"+string(toolJSON)+"\n")
		_, _ = io.WriteString(w, "e:{\"finishReason\":\"stop\",\"usage\":{\"promptTokens\":5,\"completionTokens\":3}}\n")
	}))
	defer srv.Close()
	boltAPIURL = srv.URL

	client := NewFromAccount(&store.Account{
		AccountType:   "bolt",
		SessionCookie: "session-token",
		ProjectID:     "sb1-demo",
	}, nil)

	var events []upstream.SSEMessage
	err = client.SendRequestWithPayload(context.Background(), upstream.UpstreamRequest{
		Model: "claude-opus-4-6",
		Messages: []prompt.Message{
			{Role: "user", Content: prompt.MessageContent{Text: "read readme"}},
		},
	}, func(msg upstream.SSEMessage) {
		events = append(events, msg)
	}, nil)
	if err != nil {
		t.Fatalf("SendRequestWithPayload() error = %v", err)
	}

	if len(events) != 2 {
		t.Fatalf("events len=%d want 2", len(events))
	}
	if events[0].Type != "model.tool-call" {
		t.Fatalf("first event type=%q want model.tool-call", events[0].Type)
	}
	if got := events[0].Event["toolName"]; got != "Read" {
		t.Fatalf("toolName=%v want Read", got)
	}
	if got := events[0].Event["input"]; got != `{"file_path":"README.md"}` {
		t.Fatalf("input=%v want read input json", got)
	}
	if events[1].Type != "model.finish" {
		t.Fatalf("second event type=%q want model.finish", events[1].Type)
	}
}

func TestSendRequestWithPayload_FlushesUnclosedJSONCodeFenceAsToolCall(t *testing.T) {
	prevURL := boltAPIURL
	t.Cleanup(func() { boltAPIURL = prevURL })

	chunk, err := json.Marshal("```json\n{\"tool_calls\":[{\"function\":\"Read\",\"parameters\":{\"file_path\":\"README.md\"}}]}")
	if err != nil {
		t.Fatalf("marshal chunk: %v", err)
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "0:"+string(chunk)+"\n")
		_, _ = io.WriteString(w, "e:{\"finishReason\":\"stop\",\"usage\":{\"promptTokens\":5,\"completionTokens\":3}}\n")
	}))
	defer srv.Close()
	boltAPIURL = srv.URL

	client := NewFromAccount(&store.Account{
		AccountType:   "bolt",
		SessionCookie: "session-token",
		ProjectID:     "sb1-demo",
	}, nil)

	var events []upstream.SSEMessage
	err = client.SendRequestWithPayload(context.Background(), upstream.UpstreamRequest{
		Model: "claude-opus-4-6",
		Messages: []prompt.Message{
			{Role: "user", Content: prompt.MessageContent{Text: "inspect readme"}},
		},
	}, func(msg upstream.SSEMessage) {
		events = append(events, msg)
	}, nil)
	if err != nil {
		t.Fatalf("SendRequestWithPayload() error = %v", err)
	}

	if len(events) != 2 {
		t.Fatalf("events len=%d want 2", len(events))
	}
	if events[0].Type != "model.tool-call" {
		t.Fatalf("first event type=%q want model.tool-call", events[0].Type)
	}
	if got := events[0].Event["toolName"]; got != "Read" {
		t.Fatalf("toolName=%v want Read", got)
	}
}

func TestSendRequestWithPayload_PreservesMarkdownCodeFenceLanguageAcrossChunks(t *testing.T) {
	prevURL := boltAPIURL
	t.Cleanup(func() { boltAPIURL = prevURL })

	chunk1, err := json.Marshal("计算器已创建，运行方式：\n```")
	if err != nil {
		t.Fatalf("marshal chunk1: %v", err)
	}
	chunk2, err := json.Marshal("bash\npython calculator.py\n```")
	if err != nil {
		t.Fatalf("marshal chunk2: %v", err)
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "0:"+string(chunk1)+"\n")
		_, _ = io.WriteString(w, "0:"+string(chunk2)+"\n")
		_, _ = io.WriteString(w, "e:{\"finishReason\":\"stop\",\"usage\":{\"promptTokens\":5,\"completionTokens\":9}}\n")
	}))
	defer srv.Close()
	boltAPIURL = srv.URL

	client := NewFromAccount(&store.Account{
		AccountType:   "bolt",
		SessionCookie: "session-token",
		ProjectID:     "sb1-demo",
	}, nil)

	var events []upstream.SSEMessage
	err = client.SendRequestWithPayload(context.Background(), upstream.UpstreamRequest{
		Model: "claude-opus-4-6",
		Messages: []prompt.Message{
			{Role: "user", Content: prompt.MessageContent{Text: "写一个计算器"}},
		},
	}, func(msg upstream.SSEMessage) {
		events = append(events, msg)
	}, nil)
	if err != nil {
		t.Fatalf("SendRequestWithPayload() error = %v", err)
	}

	if len(events) != 3 {
		t.Fatalf("events len=%d want 3", len(events))
	}
	if events[0].Type != "model.text-delta" || events[1].Type != "model.text-delta" {
		t.Fatalf("unexpected event types: %v, %v", events[0].Type, events[1].Type)
	}
	if events[2].Type != "model.finish" {
		t.Fatalf("third event type=%q want model.finish", events[2].Type)
	}
	got := events[0].Event["delta"].(string) + events[1].Event["delta"].(string)
	want := "计算器已创建，运行方式：\n```bash\npython calculator.py\n```"
	if got != want {
		t.Fatalf("delta=%q want %q", got, want)
	}
}

func TestSendRequestWithPayload_StripsBoltToolTranscriptFromFinalText(t *testing.T) {
	prevURL := boltAPIURL
	t.Cleanup(func() { boltAPIURL = prevURL })

	chunk, err := json.Marshal("Read 1 file (ctrl+o to expand)\n\n● Write(calculator.py)\n  ⎿  Wrote 42 lines to calculator.py\n       1 def add(a, b): return a + b\n\n● 计算器已创建，运行方式：\n```bash\npython calculator.py\n```")
	if err != nil {
		t.Fatalf("marshal chunk: %v", err)
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "0:"+string(chunk)+"\n")
		_, _ = io.WriteString(w, "e:{\"finishReason\":\"stop\",\"usage\":{\"promptTokens\":5,\"completionTokens\":9}}\n")
	}))
	defer srv.Close()
	boltAPIURL = srv.URL

	client := NewFromAccount(&store.Account{
		AccountType:   "bolt",
		SessionCookie: "session-token",
		ProjectID:     "sb1-demo",
	}, nil)

	var events []upstream.SSEMessage
	err = client.SendRequestWithPayload(context.Background(), upstream.UpstreamRequest{
		Model: "claude-opus-4-6",
		Messages: []prompt.Message{
			{Role: "user", Content: prompt.MessageContent{Text: "写一个计算器"}},
		},
	}, func(msg upstream.SSEMessage) {
		events = append(events, msg)
	}, nil)
	if err != nil {
		t.Fatalf("SendRequestWithPayload() error = %v", err)
	}

	if len(events) != 3 {
		t.Fatalf("events len=%d want 3", len(events))
	}
	if events[0].Type != "model.text-delta" || events[1].Type != "model.text-delta" {
		t.Fatalf("unexpected event types: %v, %v", events[0].Type, events[1].Type)
	}
	if events[2].Type != "model.finish" {
		t.Fatalf("third event type=%q want model.finish", events[2].Type)
	}
	got := events[0].Event["delta"].(string) + events[1].Event["delta"].(string)
	if strings.Contains(got, "ctrl+o to expand") || strings.Contains(got, "Write(calculator.py)") || strings.Contains(got, "Wrote 42 lines") {
		t.Fatalf("delta still contains tool transcript: %q", got)
	}
	want := "计算器已创建，运行方式：\n```bash\npython calculator.py\n```"
	if got != want {
		t.Fatalf("delta=%q want %q", got, want)
	}
}

func TestSendRequestWithPayload_HandlesBoltToolInvocationFramesAndFinalDMarker(t *testing.T) {
	prevURL := boltAPIURL
	t.Cleanup(func() { boltAPIURL = prevURL })

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "9:{\"toolCallId\":\"toolu_1\",\"toolName\":\"Bash\",\"args\":{\"command\":\"ls /tmp/cc-agent/sb1-demo/project/\",\"description\":\"List project files\"}}\n")
		_, _ = io.WriteString(w, "a:{\"toolCallId\":\"toolu_1\",\"toolName\":\"Bash\",\"args\":{\"command\":\"ls /tmp/cc-agent/sb1-demo/project/\",\"description\":\"List project files\"},\"result\":\"README.md\"}\n")
		_, _ = io.WriteString(w, "e:{\"finishReason\":\"unknown\",\"isContinued\":false,\"usage\":{\"promptTokens\":3,\"completionTokens\":27}}\n")
		_, _ = io.WriteString(w, "0:\"沙箱中\"\n")
		_, _ = io.WriteString(w, "e:{\"finishReason\":\"stop\",\"isContinued\":false,\"usage\":{\"promptTokens\":5,\"completionTokens\":386}}\n")
		_, _ = io.WriteString(w, "d:{\"finishReason\":\"stop\",\"usage\":{\"promptTokens\":5,\"completionTokens\":386}}\n")
	}))
	defer srv.Close()
	boltAPIURL = srv.URL

	client := NewFromAccount(&store.Account{
		AccountType:   "bolt",
		SessionCookie: "session-token",
		ProjectID:     "sb1-demo",
	}, nil)

	var events []upstream.SSEMessage
	err := client.SendRequestWithPayload(context.Background(), upstream.UpstreamRequest{
		Model: "claude-opus-4-6",
		Messages: []prompt.Message{
			{Role: "user", Content: prompt.MessageContent{Text: "inspect project"}},
		},
	}, func(msg upstream.SSEMessage) {
		events = append(events, msg)
	}, nil)
	if err != nil {
		t.Fatalf("SendRequestWithPayload() error = %v", err)
	}

	if len(events) != 2 {
		t.Fatalf("events len=%d want 2, events=%v", len(events), events)
	}
	if events[0].Type != "model.tool-call" {
		t.Fatalf("first event type=%q want model.text-delta", events[0].Type)
	}
	if got := events[0].Event["toolName"]; got != "Bash" {
		t.Fatalf("toolName=%v want Bash", got)
	}
	if events[1].Type != "model.finish" {
		t.Fatalf("second event type=%q want model.finish", events[1].Type)
	}
	if got := events[1].Event["finishReason"]; got != "tool_use" {
		t.Fatalf("finishReason=%v want tool_use", got)
	}
}

func TestSendRequestWithPayload_UsesPreparedInputEstimateInFinishUsage(t *testing.T) {
	prevURL := boltAPIURL
	t.Cleanup(func() { boltAPIURL = prevURL })

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "0:\"hello\"\n")
		_, _ = io.WriteString(w, "e:{\"finishReason\":\"stop\",\"usage\":{\"promptTokens\":5,\"completionTokens\":7}}\n")
	}))
	defer srv.Close()
	boltAPIURL = srv.URL

	req := upstream.UpstreamRequest{
		Model:   "claude-opus-4-6",
		Workdir: "d:\\Code\\Orchids-2api",
		Tools: []interface{}{
			map[string]interface{}{"name": "Read"},
			map[string]interface{}{"name": "Bash"},
		},
		System: []prompt.SystemItem{
			{Type: "text", Text: "keep this custom instruction"},
		},
		Messages: []prompt.Message{
			{Role: "user", Content: prompt.MessageContent{Text: "inspect project"}},
		},
	}

	client := NewFromAccount(&store.Account{
		AccountType:   "bolt",
		SessionCookie: "session-token",
		ProjectID:     "sb1-demo",
	}, nil)

	want := EstimateInputTokens(req)
	var events []upstream.SSEMessage
	if err := client.SendRequestWithPayload(context.Background(), req, func(msg upstream.SSEMessage) {
		events = append(events, msg)
	}, nil); err != nil {
		t.Fatalf("SendRequestWithPayload() error = %v", err)
	}

	if len(events) != 2 {
		t.Fatalf("events len=%d want 2", len(events))
	}
	usage, ok := events[1].Event["usage"].(map[string]interface{})
	if !ok {
		t.Fatalf("finish usage=%#v", events[1].Event["usage"])
	}
	if got := usage["inputTokens"]; got != want.Total {
		t.Fatalf("inputTokens=%v want %d", got, want.Total)
	}
}

func TestSendRequestWithPayload_ParsesStructuredToolCallEnvelope(t *testing.T) {
	prevURL := boltAPIURL
	t.Cleanup(func() { boltAPIURL = prevURL })

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "9:{\"tool_calls\":[{\"function\":\"Read\",\"parameters\":{\"file_path\":\"README.md\"}}]}\n")
		_, _ = io.WriteString(w, "e:{\"finishReason\":\"stop\",\"usage\":{\"promptTokens\":5,\"completionTokens\":3}}\n")
	}))
	defer srv.Close()
	boltAPIURL = srv.URL

	client := NewFromAccount(&store.Account{
		AccountType:   "bolt",
		SessionCookie: "session-token",
		ProjectID:     "sb1-demo",
	}, nil)

	var events []upstream.SSEMessage
	err := client.SendRequestWithPayload(context.Background(), upstream.UpstreamRequest{
		Model: "claude-opus-4-6",
		Messages: []prompt.Message{
			{Role: "user", Content: prompt.MessageContent{Text: "inspect readme"}},
		},
	}, func(msg upstream.SSEMessage) {
		events = append(events, msg)
	}, nil)
	if err != nil {
		t.Fatalf("SendRequestWithPayload() error = %v", err)
	}

	if len(events) != 2 {
		t.Fatalf("events len=%d want 2", len(events))
	}
	if events[0].Type != "model.tool-call" {
		t.Fatalf("first event type=%q want model.tool-call", events[0].Type)
	}
	if got := events[0].Event["toolName"]; got != "Read" {
		t.Fatalf("toolName=%v want Read", got)
	}
	if got := events[0].Event["input"]; got != `{"file_path":"README.md"}` {
		t.Fatalf("input=%v want read input json", got)
	}
}

func TestEstimateInputTokens_SplitsPromptBuckets(t *testing.T) {
	req := upstream.UpstreamRequest{
		Model:   "claude-opus-4-6",
		Workdir: "d:\\Code\\Orchids-2api",
		Tools: []interface{}{
			map[string]interface{}{"name": "Read"},
			map[string]interface{}{"name": "Bash"},
		},
		System: []prompt.SystemItem{
			{Type: "text", Text: "keep this custom instruction"},
		},
		Messages: []prompt.Message{
			{Role: "user", Content: prompt.MessageContent{Text: "inspect project"}},
			{Role: "assistant", Content: prompt.MessageContent{Text: "I will inspect the repository."}},
		},
	}

	got := EstimateInputTokens(req)
	if got.BasePromptTokens <= 0 {
		t.Fatalf("BasePromptTokens=%d want >0", got.BasePromptTokens)
	}
	if got.ToolsTokens <= 0 {
		t.Fatalf("ToolsTokens=%d want >0", got.ToolsTokens)
	}
	if got.SystemContextTokens <= 0 {
		t.Fatalf("SystemContextTokens=%d want >0", got.SystemContextTokens)
	}
	if got.HistoryTokens <= 0 {
		t.Fatalf("HistoryTokens=%d want >0", got.HistoryTokens)
	}
	if got.Total != got.BasePromptTokens+got.SystemContextTokens+got.ToolsTokens+got.HistoryTokens {
		t.Fatalf("Total=%d does not match bucket sum", got.Total)
	}
}

func TestPrepareRequest_AddsWorkspaceAndToolInstructions(t *testing.T) {
	req := upstream.UpstreamRequest{
		Model:   "claude-opus-4-6",
		Workdir: "d:\\Code\\Orchids-2api",
		Tools: []interface{}{
			map[string]interface{}{"name": "Read"},
			map[string]interface{}{"function": map[string]interface{}{"name": "Bash"}},
		},
		System: []prompt.SystemItem{
			{Type: "text", Text: "You are Claude Code, Anthropic's official CLI for Claude."},
			{Type: "text", Text: "keep this custom instruction"},
		},
	}

	boltReq, _ := prepareRequest(req, "sb1-demo")
	if !strings.Contains(boltReq.GlobalSystemPrompt, "当前项目目录名: Orchids-2api") {
		t.Fatalf("system prompt missing project name hint: %s", boltReq.GlobalSystemPrompt)
	}
	if !strings.Contains(boltReq.GlobalSystemPrompt, "当前项目真实工作目录") || !strings.Contains(boltReq.GlobalSystemPrompt, "`d:\\Code\\Orchids-2api`") {
		t.Fatalf("system prompt missing explicit workdir hint: %s", boltReq.GlobalSystemPrompt)
	}
	if !strings.Contains(boltReq.GlobalSystemPrompt, "不要回答 `/tmp/cc-agent/...`") {
		t.Fatalf("system prompt missing sandbox-path answer guard: %s", boltReq.GlobalSystemPrompt)
	}
	if !strings.Contains(boltReq.GlobalSystemPrompt, "把项目根目录视为 `.`") {
		t.Fatalf("system prompt missing relative root instruction: %s", boltReq.GlobalSystemPrompt)
	}
	if !strings.Contains(boltReq.GlobalSystemPrompt, "Read(file_path, limit?, offset?)") {
		t.Fatalf("system prompt missing Read tool hint: %s", boltReq.GlobalSystemPrompt)
	}
	if !strings.Contains(boltReq.GlobalSystemPrompt, "不要先解释计划") {
		t.Fatalf("system prompt missing direct tool-call instruction: %s", boltReq.GlobalSystemPrompt)
	}
	if !strings.Contains(boltReq.GlobalSystemPrompt, "不要解释当前运行在什么系统或沙箱") {
		t.Fatalf("system prompt missing no-sandbox-explanation instruction: %s", boltReq.GlobalSystemPrompt)
	}
	if !strings.Contains(boltReq.GlobalSystemPrompt, "如果某次工具结果提示路径不存在，不要据此断言项目为空") {
		t.Fatalf("system prompt missing path-miss recovery instruction: %s", boltReq.GlobalSystemPrompt)
	}
	if strings.Contains(strings.ToLower(boltReq.GlobalSystemPrompt), "anthropic's official cli for claude") {
		t.Fatalf("system prompt should strip claude code system boilerplate: %s", boltReq.GlobalSystemPrompt)
	}
	if strings.Contains(boltReq.GlobalSystemPrompt, "Primary working directory") {
		t.Fatalf("system prompt should strip raw environment workdir block: %s", boltReq.GlobalSystemPrompt)
	}
	if !strings.Contains(boltReq.GlobalSystemPrompt, "keep this custom instruction") {
		t.Fatalf("system prompt dropped custom instruction: %s", boltReq.GlobalSystemPrompt)
	}
}

func TestPrepareRequest_PreservesMCPSystemContext(t *testing.T) {
	req := upstream.UpstreamRequest{
		Model: "claude-opus-4-6",
		System: []prompt.SystemItem{
			{Type: "text", Text: "You are Claude Code, Anthropic's official CLI for Claude."},
			{Type: "text", Text: "# MCP Server\n- filesystem\n# VSCode Extension Context\nkeep this custom instruction"},
		},
	}

	boltReq, _ := prepareRequest(req, "sb1-demo")
	if strings.Contains(strings.ToLower(boltReq.GlobalSystemPrompt), "anthropic's official cli for claude") {
		t.Fatalf("system prompt should strip claude code boilerplate: %s", boltReq.GlobalSystemPrompt)
	}
	if !strings.Contains(boltReq.GlobalSystemPrompt, "# MCP Server") {
		t.Fatalf("system prompt should preserve MCP context: %s", boltReq.GlobalSystemPrompt)
	}
	if !strings.Contains(boltReq.GlobalSystemPrompt, "# VSCode Extension Context") {
		t.Fatalf("system prompt should preserve VSCode context: %s", boltReq.GlobalSystemPrompt)
	}
	if !strings.Contains(boltReq.GlobalSystemPrompt, "keep this custom instruction") {
		t.Fatalf("system prompt should preserve custom instruction: %s", boltReq.GlobalSystemPrompt)
	}
}

func TestPrepareRequest_AddsHistoryAwarePathRecoveryInstructions(t *testing.T) {
	req := upstream.UpstreamRequest{
		Model:   "claude-opus-4-6",
		Workdir: "d:\\Code\\Orchids-2api",
		Tools: []interface{}{
			map[string]interface{}{"name": "Read"},
			map[string]interface{}{"name": "Bash"},
			map[string]interface{}{"name": "Glob"},
		},
		Messages: []prompt.Message{
			{Role: "user", Content: prompt.MessageContent{Text: "这个项目是干什么的"}},
			{
				Role: "assistant",
				Content: prompt.MessageContent{
					Blocks: []prompt.ContentBlock{
						{
							Type:  "tool_use",
							ID:    "tool_ls",
							Name:  "Bash",
							Input: map[string]interface{}{"command": "ls /tmp/cc-agent/sb1-demo/project", "description": "List project files"},
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
							ToolUseID: "tool_ls",
							Content:   "Exit code 2\nls: cannot access '/tmp/cc-agent/sb1-demo/project': No such file or directory",
						},
						{Type: "text", Text: "这个项目是干什么的"},
					},
				},
			},
		},
	}

	boltReq, _ := prepareRequest(req, "sb1-demo")
	if !strings.Contains(boltReq.GlobalSystemPrompt, "`/tmp/cc-agent/sb1-demo/project`") {
		t.Fatalf("system prompt missing invalid-history path hint: %s", boltReq.GlobalSystemPrompt)
	}
	if !strings.Contains(boltReq.GlobalSystemPrompt, "真实项目目录是 `d:\\Code\\Orchids-2api`") {
		t.Fatalf("system prompt missing explicit real-workdir recovery hint: %s", boltReq.GlobalSystemPrompt)
	}
	if !strings.Contains(boltReq.GlobalSystemPrompt, "不要复用这个路径") {
		t.Fatalf("system prompt missing do-not-reuse instruction: %s", boltReq.GlobalSystemPrompt)
	}
	if !strings.Contains(boltReq.GlobalSystemPrompt, "在至少成功查看一次 `.`、README.md、go.mod、package.json 等项目内路径之前") {
		t.Fatalf("system prompt missing must-check-project instruction: %s", boltReq.GlobalSystemPrompt)
	}
}

func TestPrepareRequest_EncodesToolResultsAsUserContentAndDropsAssistantToolInvocations(t *testing.T) {
	req := upstream.UpstreamRequest{
		Model: "claude-opus-4-6",
		Messages: []prompt.Message{
			{Role: "user", Content: prompt.MessageContent{Text: "这个项目是干什么的"}},
			{
				Role: "assistant",
				Content: prompt.MessageContent{
					Blocks: []prompt.ContentBlock{
						{
							Type:  "tool_use",
							ID:    "tool_read",
							Name:  "Read",
							Input: map[string]interface{}{"file_path": "README.md"},
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
							ToolUseID: "tool_read",
							Content:   "# Orchids-2api\n一个基于 Go 的多通道代理服务。",
						},
					},
				},
			},
		},
	}

	boltReq, _ := prepareRequest(req, "sb1-demo")
	if len(boltReq.Messages) != 2 {
		t.Fatalf("messages len=%d want 2", len(boltReq.Messages))
	}
	if got := boltReq.Messages[1].Role; got != "user" {
		t.Fatalf("last role=%q want user", got)
	}
	if !strings.Contains(boltReq.Messages[1].Content, "Tool result:") {
		t.Fatalf("expected tool result to be serialized into user content, got: %q", boltReq.Messages[1].Content)
	}
	if strings.Contains(boltReq.Messages[1].Content, "toolInvocation") {
		t.Fatalf("did not expect tool invocation metadata in user content, got: %q", boltReq.Messages[1].Content)
	}
	if len(boltReq.Messages[0].Parts) != 0 {
		t.Fatalf("expected first user message to have no parts, got: %#v", boltReq.Messages[0].Parts)
	}
	if len(boltReq.Messages[1].Parts) != 0 {
		t.Fatalf("expected tool-result follow-up user message to have no parts, got: %#v", boltReq.Messages[1].Parts)
	}
}

func TestPrepareRequest_SkipsToolRoleMessages(t *testing.T) {
	req := upstream.UpstreamRequest{
		Model: "claude-opus-4-6",
		Messages: []prompt.Message{
			{Role: "user", Content: prompt.MessageContent{Text: "hi"}},
			{Role: "assistant", Content: prompt.MessageContent{Text: "hello"}},
			{
				Role: "tool",
				Content: prompt.MessageContent{
					Text: "Model tried to call unavailable tool 'WebSearch'. Available tools: builtin_web_search.",
				},
			},
			{Role: "user", Content: prompt.MessageContent{Text: "现在上海的天气怎么样"}},
		},
	}

	boltReq, _ := prepareRequest(req, "sb1-demo")
	if len(boltReq.Messages) != 3 {
		t.Fatalf("messages len=%d want 3", len(boltReq.Messages))
	}
	for _, msg := range boltReq.Messages {
		if msg.Role == "tool" {
			t.Fatalf("unexpected tool role message in bolt request: %#v", msg)
		}
		if strings.Contains(msg.Content, "unavailable tool") {
			t.Fatalf("unexpected tool error content in bolt request: %#v", msg)
		}
	}
}

func TestFetchRootData_UsesSessionCookie(t *testing.T) {
	prevRootURL := boltRootDataURL
	t.Cleanup(func() { boltRootDataURL = prevRootURL })

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.RawQuery != "_data=root" {
			t.Fatalf("unexpected query: %q", r.URL.RawQuery)
		}
		if got := r.Header.Get("Cookie"); !strings.Contains(got, "__session=session-token") {
			t.Fatalf("cookie=%q", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"token":"root-token","user":{"id":"user_1","email":"bolt@example.com","totalBoltTokenPurchases":1000000}}`)
	}))
	defer srv.Close()
	boltRootDataURL = srv.URL + "?_data=root"

	client := NewFromAccount(&store.Account{
		AccountType:   "bolt",
		SessionCookie: "session-token",
		ProjectID:     "sb1-demo",
	}, nil)

	data, err := client.FetchRootData(context.Background())
	if err != nil {
		t.Fatalf("FetchRootData() error = %v", err)
	}
	if data.Token != "root-token" || data.User == nil || data.User.ID != "user_1" {
		t.Fatalf("unexpected root data: %+v", data)
	}
	if data.User.TotalBoltTokenPurchases != 1_000_000 {
		t.Fatalf("totalBoltTokenPurchases=%v want 1000000", data.User.TotalBoltTokenPurchases)
	}
}

func TestFetchRateLimits_UsesSessionCookieAndUserPath(t *testing.T) {
	prevRateURL := boltRateLimitsURL
	prevTeamsRateURL := boltTeamsRateLimitsURL
	t.Cleanup(func() {
		boltRateLimitsURL = prevRateURL
		boltTeamsRateLimitsURL = prevTeamsRateURL
	})

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/rate-limits/user" {
			t.Fatalf("unexpected path: %q", r.URL.Path)
		}
		if got := r.Header.Get("Cookie"); !strings.Contains(got, "__session=session-token") {
			t.Fatalf("cookie=%q", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"maxPerMonth":10000000,"regularTokens":{"available":10000000,"used":255061},"purchased":{"available":1000000,"used":0},"rewardTokens":{"available":0,"used":0},"specialTokens":{"available":0,"used":0},"referralTokens":{"free":{"available":0,"used":0},"paid":{"available":0,"used":0}},"totalThisMonth":255061,"totalToday":255061}`)
	}))
	defer srv.Close()

	boltRateLimitsURL = srv.URL + "/api/rate-limits/user"
	boltTeamsRateLimitsURL = srv.URL + "/api/rate-limits/teams"

	client := NewFromAccount(&store.Account{
		AccountType:   "bolt",
		SessionCookie: "session-token",
		ProjectID:     "sb1-demo",
	}, nil)

	data, err := client.FetchRateLimits(context.Background(), 0)
	if err != nil {
		t.Fatalf("FetchRateLimits() error = %v", err)
	}
	if data.MaxPerMonth != 10_000_000 {
		t.Fatalf("maxPerMonth=%v want 10000000", data.MaxPerMonth)
	}
	if data.RegularTokens == nil || data.RegularTokens.Used != 255061 {
		t.Fatalf("regularTokens=%+v", data.RegularTokens)
	}
}

func TestFetchRateLimits_UsesTeamPathWhenOrganizationSelected(t *testing.T) {
	prevRateURL := boltRateLimitsURL
	prevTeamsRateURL := boltTeamsRateLimitsURL
	t.Cleanup(func() {
		boltRateLimitsURL = prevRateURL
		boltTeamsRateLimitsURL = prevTeamsRateURL
	})

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/rate-limits/teams/42" {
			t.Fatalf("unexpected path: %q", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"maxPerMonth":26000000,"regularTokens":{"available":26000000,"used":0},"purchased":{"available":0,"used":0},"rewardTokens":{"available":0,"used":0},"specialTokens":{"available":0,"used":0},"referralTokens":{"free":{"available":0,"used":0},"paid":{"available":0,"used":0}},"totalThisMonth":0,"totalToday":0}`)
	}))
	defer srv.Close()

	boltRateLimitsURL = srv.URL + "/api/rate-limits/user"
	boltTeamsRateLimitsURL = srv.URL + "/api/rate-limits/teams"

	client := NewFromAccount(&store.Account{
		AccountType:   "bolt",
		SessionCookie: "session-token",
		ProjectID:     "sb1-demo",
	}, nil)

	data, err := client.FetchRateLimits(context.Background(), 42)
	if err != nil {
		t.Fatalf("FetchRateLimits() error = %v", err)
	}
	if data.MaxPerMonth != 26_000_000 {
		t.Fatalf("maxPerMonth=%v want 26000000", data.MaxPerMonth)
	}
}

func TestNewFromAccount_ReusesSharedHTTPClient(t *testing.T) {
	cfg := &config.Config{
		RequestTimeout: 30,
		ProxyHTTP:      "http://proxy.local:3128",
		ProxyUser:      "user",
		ProxyPass:      "pass",
	}

	clientA := NewFromAccount(&store.Account{
		AccountType:   "bolt",
		SessionCookie: "session-a",
		ProjectID:     "sb1-a",
	}, cfg)
	clientB := NewFromAccount(&store.Account{
		AccountType:   "bolt",
		SessionCookie: "session-b",
		ProjectID:     "sb1-b",
	}, cfg)

	if clientA.httpClient != clientB.httpClient {
		t.Fatal("expected bolt clients with same transport config to reuse shared http client")
	}
	if !clientA.sharedHTTPClient || !clientB.sharedHTTPClient {
		t.Fatal("expected sharedHTTPClient flag to be set")
	}
}
