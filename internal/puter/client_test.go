package puter

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"orchids-api/internal/prompt"
	"orchids-api/internal/store"
	"orchids-api/internal/upstream"
)

func TestSendRequestWithPayload_EmitsAnthropicEvents(t *testing.T) {
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

	want := []string{"message_start", "content_block_start", "content_block_delta", "content_block_stop", "message_delta", "message_stop"}
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
