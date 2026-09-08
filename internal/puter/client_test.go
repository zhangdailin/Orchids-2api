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

func TestSendRequestWithPayloadEmitsNativeStreamEvents(t *testing.T) {
	prevURL := puterAPIURL
	t.Cleanup(func() { puterAPIURL = prevURL })

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		text := string(body)
		for _, want := range []string{`"auth_token":"puter-token"`, `"service":"deepseek"`, `"model":"deepseek-v4-flash"`} {
			if !strings.Contains(text, want) {
				t.Fatalf("request body missing %s: %s", want, text)
			}
		}
		_, _ = io.WriteString(w, strings.Join([]string{
			`{"type":"reasoning","reasoning":"thinking"}`,
			`{"type":"text","text":"I will search."}`,
			`{"type":"tool_use","id":"call_search","name":"web_search","input":{"query":"SpaceX latest news"}}`,
			`{"type":"usage","usage":{"input_tokens":12,"output_tokens":7}}`,
		}, "\n"))
	}))
	defer srv.Close()
	puterAPIURL = srv.URL

	client := NewFromAccount(&store.Account{AccountType: "puter", ClientCookie: "puter-token"}, nil)
	var events []upstream.SSEMessage
	err := client.SendRequestWithPayload(context.Background(), upstream.UpstreamRequest{
		Model:    "deepseek-v4-flash",
		Messages: []prompt.Message{{Role: "user", Content: prompt.MessageContent{Text: "latest news"}}},
	}, func(msg upstream.SSEMessage) { events = append(events, msg) }, nil)
	if err != nil {
		t.Fatalf("SendRequestWithPayload() error = %v", err)
	}

	wantTypes := []string{"model.reasoning-delta", "model.text-delta", "model.tool-call", "model.tokens-used", "model.finish"}
	if len(events) != len(wantTypes) {
		t.Fatalf("event count=%d want=%d: %#v", len(events), len(wantTypes), events)
	}
	for i, want := range wantTypes {
		if events[i].Type != want {
			t.Fatalf("events[%d].Type=%q want=%q", i, events[i].Type, want)
		}
	}
	if signature, _ := events[0].Event["signature"].(string); !strings.HasPrefix(signature, "puter-v1:") {
		t.Fatalf("reasoning event signature=%q want generated puter signature", signature)
	}
	if events[2].Event["toolName"] != "web_search" || events[2].Event["input"] != `{"query":"SpaceX latest news"}` {
		t.Fatalf("tool event=%#v", events[2].Event)
	}
	if events[3].Event["inputTokens"] != 12 || events[3].Event["outputTokens"] != 7 {
		t.Fatalf("usage event=%#v", events[3].Event)
	}
	if events[4].Event["finishReason"] != "tool_use" {
		t.Fatalf("finish event=%#v", events[4].Event)
	}
}

func TestBuildRequestUsesNativeToolsAndToolHistory(t *testing.T) {
	client := NewFromAccount(&store.Account{AccountType: "puter", ClientCookie: "puter-token"}, nil)
	req := upstream.UpstreamRequest{
		Model:   "claude-opus-5",
		Workdir: `C:\Code\Orchids-2api`,
		Tools: []interface{}{map[string]interface{}{
			"name": "Read", "description": "Read a file",
			"input_schema": map[string]interface{}{"type": "object", "properties": map[string]interface{}{"file_path": map[string]interface{}{"type": "string"}}},
		}},
		Messages: []prompt.Message{
			{Role: "user", Content: prompt.MessageContent{Text: "read README"}},
			{Role: "assistant", Content: prompt.MessageContent{Blocks: []prompt.ContentBlock{
				{Type: "text", Text: "I will inspect it."},
				{Type: "tool_use", ID: "call_read", Name: "Read", Input: map[string]interface{}{"file_path": "README.md"}},
			}}},
			{Role: "user", Content: prompt.MessageContent{Blocks: []prompt.ContentBlock{
				{Type: "tool_result", ToolUseID: "call_read", Content: "project readme"},
				{Type: "text", Text: "summarize it"},
			}}},
		},
	}

	built, err := client.buildRequest(req, false)
	if err != nil {
		t.Fatalf("buildRequest() error=%v", err)
	}
	if len(built.Args.Tools) != 1 {
		t.Fatalf("native tools=%#v", built.Args.Tools)
	}
	rawTool, _ := json.Marshal(built.Args.Tools[0])
	if !strings.Contains(string(rawTool), `"type":"function"`) || !strings.Contains(string(rawTool), `"parameters"`) {
		t.Fatalf("normalized tool=%s", rawTool)
	}
	if len(built.Args.Messages) != 4 {
		t.Fatalf("messages=%#v", built.Args.Messages)
	}
	if built.Args.Messages[0].Role != "user" || built.Args.Messages[0].Content != "read README" {
		t.Fatalf("first message=%#v want verbatim user", built.Args.Messages[0])
	}
	assistant := built.Args.Messages[1]
	if assistant.Role != "assistant" || len(assistant.ToolCalls) != 1 || assistant.ToolCalls[0].Function.Name != "Read" {
		t.Fatalf("assistant native tool history=%#v", assistant)
	}
	if assistant.ToolCalls[0].Function.Arguments != `{"file_path":"README.md"}` {
		t.Fatalf("assistant arguments=%q", assistant.ToolCalls[0].Function.Arguments)
	}
	if built.Args.Messages[2].Role != "tool" || built.Args.Messages[2].ToolCallID != "call_read" {
		t.Fatalf("tool result message=%#v", built.Args.Messages[2])
	}
	if built.Args.Messages[3].Role != "user" || built.Args.Messages[3].Content != "summarize it" {
		t.Fatalf("follow-up message=%#v", built.Args.Messages[3])
	}
}

func TestBuildRequestNoToolsOmitsTools(t *testing.T) {
	// NoTools is protocol framing: tools are omitted, but no instruction text is
	// injected — the client's content passes through verbatim.
	client := NewFromAccount(&store.Account{ClientCookie: "puter-token"}, nil)
	built, err := client.buildRequest(upstream.UpstreamRequest{
		Model:    "claude-opus-5",
		NoTools:  true,
		Tools:    []interface{}{map[string]interface{}{"name": "Read"}},
		Messages: []prompt.Message{{Role: "user", Content: prompt.MessageContent{Text: "summarize"}}},
	}, false)
	if err != nil {
		t.Fatalf("buildRequest() error=%v", err)
	}
	if len(built.Args.Tools) != 0 {
		t.Fatalf("tools=%#v want omitted", built.Args.Tools)
	}
	if len(built.Args.Messages) != 1 || built.Args.Messages[0].Role != "user" {
		t.Fatalf("messages=%#v want only the user message", built.Args.Messages)
	}
	if strings.Contains(built.Args.Messages[0].Content, "must not make any tool calls") {
		t.Fatalf("unexpected injected instruction: %q", built.Args.Messages[0].Content)
	}
}

func TestBuildRequestPreservesContentVerbatimByDefault(t *testing.T) {
	// Fidelity default (nil config): system items pass through verbatim as
	// individual system messages — no current-date/workdir/noTools injection.
	client := NewFromAccount(&store.Account{ClientCookie: "puter-token"}, nil)
	req := upstream.UpstreamRequest{
		Model:   "claude-opus-5",
		Workdir: `C:\Code\Orchids-2api`,
		NoTools: false,
		System: []prompt.SystemItem{
			{Type: "text", Text: "x-anthropic-billing-header: cc_version=2.1.85.351; cc_entrypoint=cli; cch=5e896;"},
			{Type: "text", Text: "You are Claude Code, Anthropic's official CLI for Claude."},
			{Type: "text", Text: "cc_entrypoint=claude-code; keep=this"},
		},
		Messages: []prompt.Message{
			{Role: "user", Content: prompt.MessageContent{Text: "<system-reminder>\nToday's date is 2026-03-27.\n</system-reminder> 帮我添加"}},
			{Role: "assistant", Content: prompt.MessageContent{Text: "好的。"}},
		},
	}

	built, err := client.buildRequest(req, false)
	if err != nil {
		t.Fatalf("buildRequest() error=%v", err)
	}

	if len(built.Args.Messages) != 5 {
		t.Fatalf("messages=%#v want 3 system + 2 chat", built.Args.Messages)
	}
	wantSystem := []string{
		"x-anthropic-billing-header: cc_version=2.1.85.351; cc_entrypoint=cli; cch=5e896;",
		"You are Claude Code, Anthropic's official CLI for Claude.",
		"cc_entrypoint=claude-code; keep=this",
	}
	for i, want := range wantSystem {
		if built.Args.Messages[i].Role != "system" || built.Args.Messages[i].Content != want {
			t.Fatalf("system[%d]=%#v want verbatim %q", i, built.Args.Messages[i], want)
		}
	}
	for _, msg := range built.Args.Messages {
		if strings.Contains(msg.Content, "Current date:") ||
			strings.Contains(msg.Content, "working directory is") ||
			strings.Contains(msg.Content, "must not make any tool calls") {
			t.Fatalf("unexpected injected content: %q", msg.Content)
		}
	}
	if built.Args.Messages[3].Role != "user" || built.Args.Messages[3].Content != "<system-reminder>\nToday's date is 2026-03-27.\n</system-reminder> 帮我添加" {
		t.Fatalf("user message not preserved verbatim: %#v", built.Args.Messages[3])
	}
	if built.Args.Messages[4].Role != "assistant" || built.Args.Messages[4].Content != "好的。" {
		t.Fatalf("assistant message not preserved verbatim: %#v", built.Args.Messages[4])
	}
}

func TestNormalizeToolDefinitionsPreservesBuiltinTools(t *testing.T) {
	got := normalizeToolDefinitions([]interface{}{map[string]interface{}{"type": "web_search"}})
	if len(got) != 1 {
		t.Fatalf("builtin tools=%#v", got)
	}
	raw, _ := json.Marshal(got[0])
	if string(raw) != `{"type":"web_search"}` {
		t.Fatalf("builtin tool=%s", raw)
	}
}

func TestVerifyModelRequiresUsableEvent(t *testing.T) {
	for _, tt := range []struct {
		name    string
		body    string
		wantErr string
	}{
		{name: "valid", body: `{"type":"usage","usage":{"input_tokens":1,"output_tokens":1}}`},
		{name: "empty", body: "", wantErr: "no usable stream events"},
		{name: "stream-error", body: `{"type":"error","message":"model unavailable"}`, wantErr: "model unavailable"},
		{name: "malformed-only", body: `not-json`, wantErr: "no usable stream events"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			prevURL := puterAPIURL
			t.Cleanup(func() { puterAPIURL = prevURL })
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				body, _ := io.ReadAll(r.Body)
				if !strings.Contains(string(body), `"test_mode":true`) || !strings.Contains(string(body), `"model":"claude-opus-5"`) {
					t.Fatalf("invalid verify request: %s", body)
				}
				_, _ = io.WriteString(w, tt.body)
			}))
			defer srv.Close()
			puterAPIURL = srv.URL

			client := NewFromAccount(&store.Account{ClientCookie: "puter-token"}, nil)
			err := client.VerifyModel(context.Background(), "claude-opus-5")
			if tt.wantErr == "" && err != nil {
				t.Fatalf("VerifyModel() error=%v", err)
			}
			if tt.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tt.wantErr)) {
				t.Fatalf("VerifyModel() error=%v want containing %q", err, tt.wantErr)
			}
		})
	}
}

func TestSendRequestWithPayloadPropagatesEnvelopeErrors(t *testing.T) {
	for _, body := range []string{
		`{"success":false,"error":{"iface":"puter-chat-completion","code":"no_implementation_available","message":"No implementation available","status":502}}`,
		`{"success":false,"error":"Model not found"}`,
	} {
		prevURL := puterAPIURL
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = io.WriteString(w, body) }))
		puterAPIURL = srv.URL
		client := NewFromAccount(&store.Account{ClientCookie: "puter-token"}, nil)
		err := client.SendRequestWithPayload(context.Background(), upstream.UpstreamRequest{
			Model:    "claude-opus-5",
			Messages: []prompt.Message{{Role: "user", Content: prompt.MessageContent{Text: "hello"}}},
		}, nil, nil)
		srv.Close()
		puterAPIURL = prevURL
		if err == nil {
			t.Fatalf("expected envelope error for %s", body)
		}
	}
}

func TestStreamPreservesWhitespaceTextChunks(t *testing.T) {
	raw := strings.Join([]string{
		"data: {\"type\":\"text\",\"text\":\"```go\"}",
		`data: {"type":"text","text":"\n"}`,
		`data: {"type":"text","text":"func main() {}"}`,
		`data: [DONE]`,
	}, "\n")
	var text strings.Builder
	result, err := consumePuterStream(strings.NewReader(raw), func(msg upstream.SSEMessage) {
		if msg.Type == "model.text-delta" {
			text.WriteString(msg.Event["delta"].(string))
		}
	})
	if err != nil {
		t.Fatalf("consumePuterStream() error=%v", err)
	}
	if !result.SawMeaningfulEvent || text.String() != "```go\nfunc main() {}" {
		t.Fatalf("result=%#v text=%q", result, text.String())
	}
}

func TestFetchMonthlyUsageDecodesOnlyAllowance(t *testing.T) {
	prevURL := puterMeteringUsageURL
	t.Cleanup(func() { puterMeteringUsageURL = prevURL })
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer puter-token" {
			t.Fatalf("Authorization=%q", r.Header.Get("Authorization"))
		}
		_, _ = io.WriteString(w, `{"usage":{"ignored":true},"appTotals":{"ignored":true},"allowanceInfo":{"remaining":13.5,"monthUsageAllowance":25,"addons":{}}}`)
	}))
	defer srv.Close()
	puterMeteringUsageURL = srv.URL

	usage, err := NewFromAccount(&store.Account{ClientCookie: "puter-token"}, nil).FetchMonthlyUsage(context.Background())
	if err != nil {
		t.Fatalf("FetchMonthlyUsage() error=%v", err)
	}
	if usage.AllowanceInfo.Remaining != 13.5 || usage.AllowanceInfo.MonthUsageAllowance != 25 {
		t.Fatalf("usage=%#v", usage)
	}
}

func TestChatHeadersAreMinimal(t *testing.T) {
	prevURL := puterAPIURL
	t.Cleanup(func() { puterAPIURL = prevURL })
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Content-Type") != "text/plain;actually=json" {
			t.Fatalf("Content-Type=%q", r.Header.Get("Content-Type"))
		}
		if r.Header.Get("Origin") != "" || r.Header.Get("Sec-Fetch-Site") != "" || r.Header.Get("sec-ch-ua") != "" {
			t.Fatalf("legacy browser headers leaked: %#v", r.Header)
		}
		_, _ = io.WriteString(w, `{"type":"text","text":"ok"}`)
	}))
	defer srv.Close()
	puterAPIURL = srv.URL
	client := NewFromAccount(&store.Account{ClientCookie: "puter-token"}, nil)
	if err := client.SendRequestWithPayload(context.Background(), upstream.UpstreamRequest{
		Model: "claude-opus-5", Messages: []prompt.Message{{Role: "user", Content: prompt.MessageContent{Text: "hi"}}},
	}, nil, nil); err != nil {
		t.Fatalf("SendRequestWithPayload() error=%v", err)
	}
}

func TestServiceForCurrentModelsAndRejectsLegacyRoutes(t *testing.T) {
	tests := []struct{ model, want string }{
		{"claude-opus-5", "claude"},
		{"gpt-5.6-sol", "openai"},
		{"gemini-3.5-flash", "google"},
		{"grok-4.5", "x-ai"},
		{"deepseek-v4-flash", "deepseek"},
		{"mistral-small-2603", "mistral"},
	}
	for _, tt := range tests {
		got, err := serviceForModel(tt.model)
		if err != nil || got != tt.want {
			t.Fatalf("serviceForModel(%q)=(%q,%v) want %q", tt.model, got, err, tt.want)
		}
	}
	for _, legacy := range []string{"claude-opus-4-6", "openrouter:openai/gpt-5.6", "togetherai:qwen/model", "o3", "unknown"} {
		if _, err := serviceForModel(legacy); err == nil {
			t.Fatalf("serviceForModel(%q) unexpectedly succeeded", legacy)
		}
	}
}

func TestExtractAuthTokenAcceptsCookieOrRawToken(t *testing.T) {
	for _, tt := range []struct{ raw, want string }{
		{"puter-token", "puter-token"},
		{`"puter-token"`, "puter-token"},
		{"foo=bar; auth_token=puter-token; theme=dark", "puter-token"},
		{"puter_auth_token=puter-token", "puter-token"},
	} {
		if got := extractAuthToken(tt.raw); got != tt.want {
			t.Fatalf("extractAuthToken(%q)=%q want=%q", tt.raw, got, tt.want)
		}
	}
}

func TestNewFromAccountReusesSharedHTTPClient(t *testing.T) {
	cfg := &config.Config{RequestTimeout: 30, ProxyHTTP: "http://proxy.local:3128", ProxyUser: "user", ProxyPass: "pass"}
	clientA := NewFromAccount(&store.Account{ClientCookie: "token-a"}, cfg)
	clientB := NewFromAccount(&store.Account{ClientCookie: "token-b"}, cfg)
	if clientA.httpClient != clientB.httpClient {
		t.Fatal("expected shared HTTP client")
	}
}
