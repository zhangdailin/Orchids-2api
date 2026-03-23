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
		if !strings.Contains(string(body), `"driver":"claude"`) {
			t.Fatalf("request body missing claude driver: %s", string(body))
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

func TestDriverForModel(t *testing.T) {
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
		if got := driverForModel(tt.model); got != tt.want {
			t.Fatalf("driverForModel(%q)=%q want %q", tt.model, got, tt.want)
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

	built := client.buildRequest(req)
	if len(built.Args.Messages) == 0 || built.Args.Messages[0].Role != "system" {
		t.Fatalf("expected leading system prompt, got %#v", built.Args.Messages)
	}
	systemPrompt := built.Args.Messages[0].Content
	for _, want := range []string{
		"The real local project working directory is `d:\\Code\\Orchids-2api`.",
		"Treat the project root as `.`",
		"# Tools",
		"<tool_call>",
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

	built := client.buildRequest(req)
	systemPrompt := built.Args.Messages[0].Content
	if !strings.Contains(systemPrompt, "This turn must not make any tool calls.") {
		t.Fatalf("expected no-tools instruction, got:\n%s", systemPrompt)
	}
	if strings.Contains(systemPrompt, "# Tools") {
		t.Fatalf("did not expect tool catalog when no-tools gate is enabled, got:\n%s", systemPrompt)
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
