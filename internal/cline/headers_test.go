package cline

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"orchids-api/internal/prompt"
	"orchids-api/internal/upstream"
)

// TestChatRequestCarriesTheClineProductIdentity pins the identity block the
// upstream gates the free catalog on.
//
// Without it the API answers 403 with "<model> is only available via Cline
// product surfaces": the refusal names a client, not a credential, so no amount
// of re-login repairs it. The header set is therefore part of the wire contract
// and is asserted like one.
func TestChatRequestCarriesTheClineProductIdentity(t *testing.T) {
	t.Parallel()

	var seen http.Header
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = r.Header.Clone()
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"ok\"}}]}\n\ndata: [DONE]\n\n"))
	}))
	defer server.Close()

	client := &Client{
		apiBase: server.URL,
		stream:  server.Client(),
		creds:   Credentials{AccessToken: "access-1"},
	}
	if err := client.SendRequestWithPayload(context.Background(), upstream.UpstreamRequest{
		Model:    "cline-free/deepseek-v4.1-flash",
		Messages: []prompt.Message{{Role: "user", Content: prompt.MessageContent{Text: "ping"}}},
	}, nil, nil); err != nil {
		t.Fatalf("SendRequestWithPayload() error = %v", err)
	}

	for name, want := range defaultClientHeaders {
		if got := seen.Get(name); got != want {
			t.Errorf("header %s = %q, want %q", name, got, want)
		}
	}
	if got := seen.Get("X-Task-ID"); got == "" {
		t.Error("X-Task-ID is empty; the upstream correlates a turn by it")
	}
	if got := seen.Get("Authorization"); got != "Bearer workos:access-1" {
		t.Errorf("Authorization = %q, want %q", got, "Bearer workos:access-1")
	}
}

// TestCatalogRequestCarriesTheClineProductIdentity keeps the model feed on the
// same identity: it is read with the same account token and is served by the
// same product gate.
func TestCatalogRequestCarriesTheClineProductIdentity(t *testing.T) {
	t.Parallel()

	var seen http.Header
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = r.Header.Clone()
		_, _ = w.Write([]byte(`{"free":[{"id":"cline-free/deepseek-v4.1-flash","name":"DeepSeek V4.1 Flash"}]}`))
	}))
	defer server.Close()

	client := &Client{
		apiBase: server.URL,
		control: server.Client(),
		creds:   Credentials{AccessToken: "access-1"},
	}
	if _, err := client.FetchUpstreamModels(context.Background()); err != nil {
		t.Fatalf("FetchUpstreamModels() error = %v", err)
	}
	for name, want := range defaultClientHeaders {
		if got := seen.Get(name); got != want {
			t.Errorf("header %s = %q, want %q", name, got, want)
		}
	}
}

// TestApplyClientHeadersKeepsAnExplicitValue lets a caller-supplied header win,
// so an override or a deliberately different Accept is not silently replaced.
func TestApplyClientHeadersKeepsAnExplicitValue(t *testing.T) {
	t.Parallel()

	h := http.Header{}
	h.Set("Accept", "application/json")
	applyClientHeaders(h)
	if got := h.Get("Accept"); got != "application/json" {
		t.Errorf("Accept = %q, want it to survive", got)
	}
	if got := h.Get("X-CLIENT-TYPE"); got != "cline-cli" {
		t.Errorf("X-CLIENT-TYPE = %q, want cline-cli", got)
	}
}

// TestClassifyStatusKeepsAForbiddenOutOfTheCredentialPath guards the other half
// of the failure: a 403 used to be reported as a missing credential, which took
// a healthy account out of rotation and demanded a re-login that could not fix
// an entitlement refusal.
func TestClassifyStatusKeepsAForbiddenOutOfTheCredentialPath(t *testing.T) {
	t.Parallel()

	forbidden := classifyStatus(http.StatusForbidden, []byte(`{"error":{"message":"cline-free/x is only available via Cline product surfaces"}}`))
	if isUnauthorized(forbidden) {
		t.Error("403 must not be treated as unauthorized: a refresh cannot repair it")
	}
	if isRetryable(forbidden) {
		t.Error("403 must not be retried blindly")
	}

	unauthorized := classifyStatus(http.StatusUnauthorized, []byte(`{"error":{"message":"token expired"}}`))
	if !isUnauthorized(unauthorized) {
		t.Error("401 must stay unauthorized so one refresh is attempted")
	}
}
