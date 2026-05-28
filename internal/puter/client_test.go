package puter

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
	prevURL := puterAPIURL
	t.Cleanup(func() { puterAPIURL = prevURL })

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if !strings.Contains(string(body), `"auth_token":"puter-token"`) {
			t.Fatalf("request body missing auth token: %s", string(body))
		}
		if !strings.Contains(string(body), `"service":"claude"`) {
			t.Fatalf("request body missing claude service: %s", string(body))
		}
		if strings.Contains(string(body), `"driver"`) {
			t.Fatalf("request body should use service instead of deprecated driver: %s", string(body))
		}
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "{\"type\":\"text\",\"text\":\"hello from puter\"}\n")
	}))
	defer srv.Close()
	puterAPIURL = srv.URL

	client := NewFromAccount(&store.Account{AccountType: "puter", ClientCookie: "puter-token"}, nil)
	var events []string
	err := client.SendRequestWithPayload(context.Background(), upstream.UpstreamRequest{
		Model: "claude-opus-4-5",
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

func TestVerifyModel_UsesTestModeAndRequestedModel(t *testing.T) {
	prevURL := puterAPIURL
	t.Cleanup(func() { puterAPIURL = prevURL })

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		text := string(body)
		if !strings.Contains(text, `"test_mode":true`) {
			t.Fatalf("verify request missing test_mode=true: %s", text)
		}
		if !strings.Contains(text, `"model":"claude-sonnet-4-5"`) {
			t.Fatalf("verify request missing requested model: %s", text)
		}
		if !strings.Contains(text, `"service":"claude"`) {
			t.Fatalf("verify request missing service: %s", text)
		}
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "{\"type\":\"text\",\"text\":\"pong\"}\n")
	}))
	defer srv.Close()
	puterAPIURL = srv.URL

	client := NewFromAccount(&store.Account{AccountType: "puter", ClientCookie: "puter-token"}, nil)
	if err := client.VerifyModel(context.Background(), "claude-sonnet-4-5"); err != nil {
		t.Fatalf("VerifyModel() error = %v", err)
	}
}

func TestSendRequestWithPayload_PropagatesAPIError(t *testing.T) {
	prevURL := puterAPIURL
	t.Cleanup(func() { puterAPIURL = prevURL })

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "{\"success\":false,\"error\":{\"iface\":\"puter-chat-completion\",\"code\":\"no_implementation_available\",\"message\":\"No implementation available for interface `puter-chat-completion`.\",\"status\":502}}\n")
	}))
	defer srv.Close()
	puterAPIURL = srv.URL

	client := NewFromAccount(&store.Account{AccountType: "puter", ClientCookie: "puter-token"}, nil)
	err := client.SendRequestWithPayload(context.Background(), upstream.UpstreamRequest{
		Model: "gpt-5.4",
		Messages: []prompt.Message{
			{Role: "user", Content: prompt.MessageContent{Text: "hello"}},
		},
	}, nil, nil)
	if err == nil {
		t.Fatal("expected SendRequestWithPayload() to fail")
	}
	if !strings.Contains(err.Error(), "no_implementation_available") {
		t.Fatalf("expected no_implementation_available error, got %v", err)
	}
}

func TestSendRequestWithPayload_PropagatesStringAPIError(t *testing.T) {
	prevURL := puterAPIURL
	t.Cleanup(func() { puterAPIURL = prevURL })

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "{\"success\":false,\"error\":\"Model not found, please try one of the following models listed here: https://developer.puter.com/ai/models/\"}\n")
	}))
	defer srv.Close()
	puterAPIURL = srv.URL

	client := NewFromAccount(&store.Account{AccountType: "puter", ClientCookie: "puter-token"}, nil)
	err := client.SendRequestWithPayload(context.Background(), upstream.UpstreamRequest{
		Model: "claude-3-5-sonnet",
		Messages: []prompt.Message{
			{Role: "user", Content: prompt.MessageContent{Text: "hello"}},
		},
	}, nil, nil)
	if err == nil {
		t.Fatal("expected SendRequestWithPayload() to fail")
	}
	if !strings.Contains(strings.ToLower(err.Error()), "model not found") {
		t.Fatalf("expected model not found error, got %v", err)
	}
}

func TestReadStreamText_StripsDataPrefixAndUsesDelta(t *testing.T) {
	text, err := readStreamText(strings.NewReader("data: {\"type\":\"text\",\"delta\":\"hello\"}\n\ndata: [DONE]\n"))
	if err != nil {
		t.Fatalf("readStreamText() error = %v", err)
	}
	if text != "hello" {
		t.Fatalf("text = %q, want hello", text)
	}
}

func TestAsString_NilLikeValuesReturnEmpty(t *testing.T) {
	tests := []struct {
		name  string
		value any
	}{
		{name: "nil", value: nil},
		{name: "literal-nil-string", value: "<nil>"},
	}

	for _, tt := range tests {
		if got := asString(tt.value); got != "" {
			t.Fatalf("%s: asString() = %q, want empty", tt.name, got)
		}
	}
}

func TestParseToolCalls_StripsToolCallMarkup(t *testing.T) {
	toolCalls, text := parseToolCalls("before <tool_call>{\"name\":\"Read\",\"input\":{\"path\":\"/tmp/a\"}}</tool_call> after")
	if len(toolCalls) != 1 {
		t.Fatalf("toolCalls len = %d, want 1", len(toolCalls))
	}
	if toolCalls[0].Name != "Read" {
		t.Fatalf("toolCalls[0].Name = %q, want Read", toolCalls[0].Name)
	}
	if text != "before  after" && text != "before after" {
		t.Fatalf("text = %q", text)
	}
}

func TestParseToolCalls_AcceptsArgumentsAlias(t *testing.T) {
	toolCalls, text := parseToolCalls(`<tool_call>{"name":"Write","arguments":{"file_path":"note.txt","content":"alpha beta"}}</tool_call>`)
	if len(toolCalls) != 1 {
		t.Fatalf("toolCalls len = %d, want 1", len(toolCalls))
	}
	if toolCalls[0].Name != "Write" {
		t.Fatalf("toolCalls[0].Name = %q, want Write", toolCalls[0].Name)
	}
	var input map[string]any
	if err := json.Unmarshal(toolCalls[0].Input, &input); err != nil {
		t.Fatalf("unmarshal input: %v", err)
	}
	if input["file_path"] != "note.txt" || input["content"] != "alpha beta" {
		t.Fatalf("toolCalls[0].Input = %#v", input)
	}
	if text != "" {
		t.Fatalf("text = %q, want empty", text)
	}
}

func TestParseToolCalls_AcceptsWholeToolUseJSON(t *testing.T) {
	toolCalls, text := parseToolCalls("```json\n{\"type\":\"tool_use\",\"name\":\"Write\",\"input\":{\"file_path\":\"note.txt\",\"content\":\"alpha beta\"}}\n```")
	if len(toolCalls) != 1 {
		t.Fatalf("toolCalls len = %d, want 1", len(toolCalls))
	}
	if toolCalls[0].Name != "Write" {
		t.Fatalf("toolCalls[0].Name = %q, want Write", toolCalls[0].Name)
	}
	var input map[string]any
	if err := json.Unmarshal(toolCalls[0].Input, &input); err != nil {
		t.Fatalf("unmarshal input: %v", err)
	}
	if input["file_path"] != "note.txt" || input["content"] != "alpha beta" {
		t.Fatalf("toolCalls[0].Input = %#v", input)
	}
	if text != "" {
		t.Fatalf("text = %q, want empty", text)
	}
}

func TestParseToolCalls_AcceptsMixedProseAndFencedToolJSON(t *testing.T) {
	raw := "I will use a tool now.\n```json\n{\"type\":\"tool_use\",\"name\":\"Write\",\"input\":{\"file_path\":\"calculator.py\",\"content\":\"print(1)\"}}\n```\nI will wait for the result."
	toolCalls, text := parseToolCalls(raw)
	if len(toolCalls) != 1 {
		t.Fatalf("toolCalls len = %d, want 1", len(toolCalls))
	}
	if toolCalls[0].Name != "Write" {
		t.Fatalf("toolCalls[0].Name = %q, want Write", toolCalls[0].Name)
	}
	var input map[string]any
	if err := json.Unmarshal(toolCalls[0].Input, &input); err != nil {
		t.Fatalf("unmarshal input: %v", err)
	}
	if input["file_path"] != "calculator.py" {
		t.Fatalf("toolCalls[0].Input = %#v", input)
	}
	if !strings.Contains(text, "I will use a tool now.") || !strings.Contains(text, "I will wait for the result.") {
		t.Fatalf("remaining text = %q", text)
	}
	if strings.Contains(text, "tool_use") {
		t.Fatalf("expected fenced tool JSON to be removed from text, got %q", text)
	}
}

func TestSanitizeAssistantText_StripsProceduralTextWhenToolCallExists(t *testing.T) {
	raw := strings.Join([]string{
		"我来帮你在 test.txt 中添加内容。首先让我读取一下这个文件的当前内容。",
		"",
		`{"content":"     1\tHello World\n"}tool_call_result>`,
		"",
		"现在我将添加 \"我是大帅比\" 到文件中：",
		"",
		`{"content":"     1\tHello World\n     2\t我是大帅比\n"}tool_call_result>`,
		"",
		"完成！✅ 我已经成功在 test.txt 中添加了 \"我是大帅比\"。",
	}, "\n")

	out := sanitizeAssistantText(raw, []ParsedToolCall{{
		Name: "Update",
		ID:   "tool_update_1",
		Input: json.RawMessage(
			`{"file_path":"test.txt","old_string":"Hello World","new_string":"Hello World\n我是大帅比"}`,
		),
	}})
	if out != "" {
		t.Fatalf("sanitizeAssistantText() = %q, want empty", out)
	}
}

func TestSanitizeAssistantText_KeepsSubstantiveTextWithToolCall(t *testing.T) {
	raw := "我先检查配置文件，然后修复代理设置。\n\n代理认证字段的格式有误，我会继续修正。"
	out := sanitizeAssistantText(raw, []ParsedToolCall{{
		Name:  "Read",
		ID:    "tool_read_1",
		Input: json.RawMessage(`{"file_path":"config.json"}`),
	}})
	if !strings.Contains(out, "代理认证字段的格式有误") {
		t.Fatalf("sanitizeAssistantText() = %q, want substantive summary kept", out)
	}
	if strings.Contains(out, "我先检查配置文件") {
		t.Fatalf("sanitizeAssistantText() should drop procedural lead-in, got %q", out)
	}
}

func TestParseToolCalls_GeneratesToolCallIDWhenMissingOrNil(t *testing.T) {
	tests := []struct {
		name string
		raw  string
	}{
		{
			name: "missing-id",
			raw:  `<tool_call>{"name":"Write","input":{"file_path":"note.txt","content":"alpha beta"}}</tool_call>`,
		},
		{
			name: "null-id",
			raw:  `<tool_call>{"id":null,"name":"Write","input":{"file_path":"note.txt","content":"alpha beta"}}</tool_call>`,
		},
		{
			name: "literal-nil-id",
			raw:  `<tool_call>{"id":"<nil>","name":"Write","input":{"file_path":"note.txt","content":"alpha beta"}}</tool_call>`,
		},
	}

	for _, tt := range tests {
		toolCalls, text := parseToolCalls(tt.raw)
		if len(toolCalls) != 1 {
			t.Fatalf("%s: toolCalls len = %d, want 1", tt.name, len(toolCalls))
		}
		if !strings.HasPrefix(toolCalls[0].ID, "toolu_") {
			t.Fatalf("%s: toolCalls[0].ID = %q, want generated toolu_* id", tt.name, toolCalls[0].ID)
		}
		if text != "" {
			t.Fatalf("%s: text = %q, want empty", tt.name, text)
		}
	}
}

func TestSendRequestWithPayload_ParsesArgumentsAliasToolCall(t *testing.T) {
	prevURL := puterAPIURL
	t.Cleanup(func() { puterAPIURL = prevURL })

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "{\"type\":\"text\",\"text\":\"<tool_call>{\\\"name\\\":\\\"Write\\\",\\\"arguments\\\":{\\\"file_path\\\":\\\"note.txt\\\",\\\"content\\\":\\\"alpha beta\\\"}}</tool_call>\"}\n")
	}))
	defer srv.Close()
	puterAPIURL = srv.URL

	client := NewFromAccount(&store.Account{AccountType: "puter", ClientCookie: "puter-token"}, nil)
	var events []upstream.SSEMessage
	err := client.SendRequestWithPayload(context.Background(), upstream.UpstreamRequest{
		Model: "claude-opus-4-5",
		Messages: []prompt.Message{
			{Role: "user", Content: prompt.MessageContent{Text: "use Write"}},
		},
	}, func(msg upstream.SSEMessage) {
		events = append(events, msg)
	}, nil)
	if err != nil {
		t.Fatalf("SendRequestWithPayload() error = %v", err)
	}

	if len(events) != 2 {
		t.Fatalf("events len = %d, want 2", len(events))
	}
	if events[0].Type != "model.tool-call" {
		t.Fatalf("first event type = %q, want model.tool-call", events[0].Type)
	}
	if got := events[0].Event["toolName"]; got != "Write" {
		t.Fatalf("tool name = %v, want Write", got)
	}
	partialJSON, _ := events[0].Event["input"].(string)
	var input map[string]any
	if err := json.Unmarshal([]byte(partialJSON), &input); err != nil {
		t.Fatalf("unmarshal partial_json: %v", err)
	}
	if input["file_path"] != "note.txt" || input["content"] != "alpha beta" {
		t.Fatalf("partial_json = %#v", input)
	}
	if events[1].Type != "model.finish" {
		t.Fatalf("second event type = %q, want model.finish", events[1].Type)
	}
	if got := events[1].Event["finishReason"]; got != "tool_use" {
		t.Fatalf("finishReason = %v, want tool_use", got)
	}
}

func TestSendRequestWithPayload_GeneratesToolCallIDWhenUpstreamOmitsIt(t *testing.T) {
	prevURL := puterAPIURL
	t.Cleanup(func() { puterAPIURL = prevURL })

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "{\"type\":\"text\",\"text\":\"<tool_call>{\\\"id\\\":null,\\\"name\\\":\\\"Write\\\",\\\"input\\\":{\\\"file_path\\\":\\\"note.txt\\\",\\\"content\\\":\\\"alpha beta\\\"}}</tool_call>\"}\n")
	}))
	defer srv.Close()
	puterAPIURL = srv.URL

	client := NewFromAccount(&store.Account{AccountType: "puter", ClientCookie: "puter-token"}, nil)
	var events []upstream.SSEMessage
	err := client.SendRequestWithPayload(context.Background(), upstream.UpstreamRequest{
		Model: "claude-opus-4-5",
		Messages: []prompt.Message{
			{Role: "user", Content: prompt.MessageContent{Text: "use Write"}},
		},
	}, func(msg upstream.SSEMessage) {
		events = append(events, msg)
	}, nil)
	if err != nil {
		t.Fatalf("SendRequestWithPayload() error = %v", err)
	}

	if len(events) != 2 {
		t.Fatalf("events len = %d, want 2", len(events))
	}
	if events[0].Type != "model.tool-call" {
		t.Fatalf("first event type = %q, want model.tool-call", events[0].Type)
	}
	toolCallID, _ := events[0].Event["toolCallId"].(string)
	if !strings.HasPrefix(toolCallID, "toolu_") {
		t.Fatalf("toolCallId = %q, want generated toolu_* id", toolCallID)
	}
	if toolCallID == "<nil>" {
		t.Fatal("toolCallId should not be <nil>")
	}
}

func TestServiceForModel(t *testing.T) {
	tests := []struct {
		model string
		want  string
	}{
		{model: "claude-opus-4-5", want: "claude"},
		{model: "gpt-5.1", want: "openai"},
		{model: "gemini-2.5-pro", want: "google"},
		{model: "grok-3", want: "x-ai"},
		{model: "deepseek-chat", want: "deepseek"},
		{model: "mistral-large-latest", want: "mistral"},
		{model: "devstral-small-2507", want: "mistral"},
		{model: "openrouter:openai/gpt-5.1", want: "openrouter"},
		{model: "togetherai:meta-llama/Llama-3.3-70B-Instruct-Turbo", want: "togetherai"},
	}

	for _, tt := range tests {
		if got := serviceForModel(tt.model); got != tt.want {
			t.Fatalf("serviceForModel(%q)=%q want %q", tt.model, got, tt.want)
		}
	}
}

func TestExtractAuthToken_AcceptsCookieOrRawToken(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want string
	}{
		{name: "raw", raw: "puter-token", want: "puter-token"},
		{name: "quoted", raw: `"puter-token"`, want: "puter-token"},
		{name: "cookie-auth-token", raw: "foo=bar; auth_token=puter-token; theme=dark", want: "puter-token"},
		{name: "cookie-puter-auth-token", raw: "puter_auth_token=puter-token", want: "puter-token"},
	}

	for _, tt := range tests {
		if got := extractAuthToken(tt.raw); got != tt.want {
			t.Fatalf("%s: extractAuthToken()=%q want %q", tt.name, got, tt.want)
		}
	}
}

func TestBuildRequest_IncludesWorkdirToolPrompt(t *testing.T) {
	client := NewFromAccount(&store.Account{AccountType: "puter", ClientCookie: "puter-token"}, nil)
	req := upstream.UpstreamRequest{
		Model:   "claude-sonnet-4-6",
		Workdir: `d:\Code\Orchids-2api`,
		Tools: []interface{}{
			map[string]interface{}{
				"name":        "Read",
				"description": "Read a file",
				"input_schema": map[string]interface{}{
					"type": "object",
				},
			},
		},
		Messages: []prompt.Message{
			{Role: "user", Content: prompt.MessageContent{Text: "这个项目是干什么的"}},
		},
	}

	built := client.buildRequest(req, false)
	if len(built.Args.Messages) == 0 || built.Args.Messages[0].Role != "system" {
		t.Fatalf("expected leading system prompt, got %#v", built.Args.Messages)
	}
	systemPrompt := built.Args.Messages[0].Content
	for _, want := range []string{
		"The real local project working directory is `d:\\Code\\Orchids-2api`.",
		"Treat the project root as `.`",
		"Never emit Windows absolute paths like `C:\\...`",
		"# Tools",
		"<tool_call>",
		"Never claim that a file was created, updated, or deleted unless you emitted the corresponding tool call.",
	} {
		if !strings.Contains(systemPrompt, want) {
			t.Fatalf("system prompt missing %q:\n%s", want, systemPrompt)
		}
	}
}

func TestBuildRequest_NoToolsPromptDisablesToolCalls(t *testing.T) {
	client := NewFromAccount(&store.Account{AccountType: "puter", ClientCookie: "puter-token"}, nil)
	req := upstream.UpstreamRequest{
		Model:   "claude-sonnet-4-6",
		Workdir: `d:\Code\Orchids-2api`,
		NoTools: true,
		Tools: []interface{}{
			map[string]interface{}{
				"name": "Read",
			},
		},
		Messages: []prompt.Message{
			{Role: "user", Content: prompt.MessageContent{Text: "总结刚才的工具结果"}},
		},
	}

	built := client.buildRequest(req, false)
	systemPrompt := built.Args.Messages[0].Content
	if !strings.Contains(systemPrompt, "This turn must not make any tool calls.") {
		t.Fatalf("expected no-tools instruction, got:\n%s", systemPrompt)
	}
	if strings.Contains(systemPrompt, "# Tools") {
		t.Fatalf("did not expect tool catalog when no-tools gate is enabled, got:\n%s", systemPrompt)
	}
}

func TestBuildRequest_TestModeEnabledForVerification(t *testing.T) {
	client := NewFromAccount(&store.Account{AccountType: "puter", ClientCookie: "puter-token"}, nil)
	req := upstream.UpstreamRequest{
		Model: "claude-sonnet-4-6",
		Messages: []prompt.Message{
			{Role: "user", Content: prompt.MessageContent{Text: "ping"}},
		},
	}

	built := client.buildRequest(req, true)
	if !built.TestMode {
		t.Fatal("expected puter verify request to enable test_mode")
	}
	if built.Args.Model != "claude-sonnet-4-6" {
		t.Fatalf("Args.Model=%q want claude-sonnet-4-6", built.Args.Model)
	}
}

func TestSanitizeParsedToolCalls_RelativizesWorkspaceAbsolutePaths(t *testing.T) {
	calls := []ParsedToolCall{
		{
			Name: "Write",
			ID:   "toolu_1",
			Input: json.RawMessage(`{
				"file_path":"C:\\Users\\zhangdailin\\Desktop\\11112\\output.txt",
				"content":"123123"
			}`),
		},
	}

	sanitized := sanitizeParsedToolCalls(calls, `C:\Users\zhangdailin\Desktop\11112`)
	var input map[string]any
	if err := json.Unmarshal(sanitized[0].Input, &input); err != nil {
		t.Fatalf("unmarshal sanitized input: %v", err)
	}
	if got := input["file_path"]; got != "output.txt" {
		t.Fatalf("file_path = %v, want output.txt", got)
	}
}

func TestRelativizeWorkspacePath_WindowsStyleOnNonWindowsHost(t *testing.T) {
	got, ok := relativizeWorkspacePath(`C:\Users\zhangdailin\Desktop\11112\folder\output.txt`, `C:\Users\zhangdailin\Desktop\11112`)
	if !ok {
		t.Fatal("expected Windows-style path to be relativized")
	}
	if got != "folder/output.txt" {
		t.Fatalf("rel = %q, want %q", got, "folder/output.txt")
	}
}

func TestSanitizeParsedToolCalls_LeavesExternalAbsolutePathsUnchanged(t *testing.T) {
	calls := []ParsedToolCall{
		{
			Name: "Write",
			ID:   "toolu_1",
			Input: json.RawMessage(`{
				"file_path":"D:\\other\\place\\output.txt",
				"content":"123123"
			}`),
		},
	}

	sanitized := sanitizeParsedToolCalls(calls, `C:\Users\zhangdailin\Desktop\11112`)
	var input map[string]any
	if err := json.Unmarshal(sanitized[0].Input, &input); err != nil {
		t.Fatalf("unmarshal sanitized input: %v", err)
	}
	if got := input["file_path"]; got != `D:\other\place\output.txt` {
		t.Fatalf("file_path = %v, want external path to remain unchanged", got)
	}
}

func TestNewFromAccount_ReusesSharedHTTPClient(t *testing.T) {
	cfg := &config.Config{
		RequestTimeout: 30,
		ProxyHTTP:      "http://proxy.local:3128",
		ProxyUser:      "user",
		ProxyPass:      "pass",
	}

	clientA := NewFromAccount(&store.Account{AccountType: "puter", ClientCookie: "token-a"}, cfg)
	clientB := NewFromAccount(&store.Account{AccountType: "puter", ClientCookie: "token-b"}, cfg)

	if clientA.httpClient != clientB.httpClient {
		t.Fatal("expected puter clients with same transport config to reuse shared http client")
	}
	if !clientA.sharedHTTPClient || !clientB.sharedHTTPClient {
		t.Fatal("expected sharedHTTPClient flag to be set")
	}
}
