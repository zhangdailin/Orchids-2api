package orchids

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"orchids-api/internal/config"
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
