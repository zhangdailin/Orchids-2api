package orchids

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"orchids-api/internal/config"
	"orchids-api/internal/prompt"
	"orchids-api/internal/upstream"
)

func TestFetchUpstreamModels_DecodeObjectEnvelope(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer test-token" {
			t.Fatalf("authorization=%q want Bearer test-token", got)
		}
		if r.URL.Path != "/v1/models" {
			t.Fatalf("path=%q want /v1/models", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"object":"list","data":[{"id":"gpt-5.4","object":"model","owned_by":"orchids"}]}`))
	}))
	defer srv.Close()

	c := New(&config.Config{
		UpstreamToken:     "test-token",
		OrchidsAPIBaseURL: srv.URL,
		RequestTimeout:    5,
	})

	models, err := c.FetchUpstreamModels(context.Background())
	if err != nil {
		t.Fatalf("FetchUpstreamModels() error = %v", err)
	}
	if len(models) != 1 || models[0].ID != "gpt-5.4" {
		t.Fatalf("models=%+v want single gpt-5.4", models)
	}
}

func TestFetchUpstreamModels_DecodeArrayEnvelope(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[{"id":"claude-sonnet-4-6","object":"model","owned_by":"orchids"}]`))
	}))
	defer srv.Close()

	c := New(&config.Config{
		UpstreamToken:     "test-token",
		OrchidsAPIBaseURL: srv.URL,
		RequestTimeout:    5,
	})

	models, err := c.FetchUpstreamModels(context.Background())
	if err != nil {
		t.Fatalf("FetchUpstreamModels() error = %v", err)
	}
	if len(models) != 1 || models[0].ID != "claude-sonnet-4-6" {
		t.Fatalf("models=%+v want single claude-sonnet-4-6", models)
	}
}

func TestSendRequestWithPayload_AllowsModelVerificationFlow(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Path; got != "/agent/coding-agent" {
			t.Fatalf("path=%q want /agent/coding-agent", got)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer test-token" {
			t.Fatalf("authorization=%q want Bearer test-token", got)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"type\":\"output_text_delta\",\"text\":\"OK\"}\n\n"))
		_, _ = w.Write([]byte("data: {\"type\":\"model\",\"event\":{\"data\":{\"type\":\"finish\",\"stop_reason\":\"stop\",\"usage\":{\"input_tokens\":1,\"output_tokens\":1}}}}\n\n"))
	}))
	defer srv.Close()

	c := New(&config.Config{
		UpstreamToken:  "test-token",
		UpstreamURL:    srv.URL + "/agent/coding-agent",
		RequestTimeout: 5,
	})

	err := c.SendRequestWithPayload(context.Background(), upstream.UpstreamRequest{
		Model: "claude-sonnet-4-6",
		Messages: []prompt.Message{
			{Role: "user", Content: prompt.MessageContent{Text: "Reply with exactly OK."}},
		},
		NoTools: true,
	}, nil, nil)
	if err != nil {
		t.Fatalf("SendRequestWithPayload() error = %v", err)
	}
}
