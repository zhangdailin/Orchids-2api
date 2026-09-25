package cline

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"orchids-api/internal/prompt"
	"orchids-api/internal/store"
	"orchids-api/internal/upstream"
)

func TestChatDoesNotLocallyRetryNonAuthFailure(t *testing.T) {
	var attempts atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts.Add(1)
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = io.WriteString(w, `{"error":"busy"}`)
	}))
	defer server.Close()
	client := &Client{apiBase: server.URL, stream: server.Client(), creds: Credentials{AccessToken: "token"}, account: &store.Account{ClineModelIDs: []string{"model-a"}}}
	err := client.SendRequestWithPayload(context.Background(), upstream.UpstreamRequest{Model: "model-a", RequestID: "logical-1", Messages: []prompt.Message{{Role: "user", Content: prompt.MessageContent{Text: "hi"}}}}, nil, nil)
	if err == nil {
		t.Fatal("expected upstream error")
	}
	if got := attempts.Load(); got != 1 {
		t.Fatalf("attempts=%d want 1; shared handler owns non-auth retries", got)
	}
}

func TestChatKeepsLogicalTaskIDAcross401Refresh(t *testing.T) {
	var attempts atomic.Int32
	var taskIDs []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/auth/refresh":
			_, _ = io.WriteString(w, `{"data":{"accessToken":"new","refreshToken":"refresh","expiresAt":"2099-01-01T00:00:00Z"}}`)
		case "/chat/completions":
			taskIDs = append(taskIDs, r.Header.Get("X-Task-ID"))
			if attempts.Add(1) == 1 {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"ok\"},\"finish_reason\":\"stop\"}]}\n\n")
		}
	}))
	defer server.Close()
	client := &Client{apiBase: server.URL, stream: server.Client(), control: server.Client(), creds: Credentials{AccessToken: "old", RefreshToken: "refresh", ExpiresAt: time.Now().Add(time.Hour)}, account: &store.Account{ClineModelIDs: []string{"model-a"}}}
	err := client.SendRequestWithPayload(context.Background(), upstream.UpstreamRequest{Model: "model-a", RequestID: "logical-1", Messages: []prompt.Message{{Role: "user", Content: prompt.MessageContent{Text: "hi"}}}}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(taskIDs) != 2 || taskIDs[0] != "sess_logical-1" || taskIDs[1] != taskIDs[0] {
		t.Fatalf("task ids=%v", taskIDs)
	}
}

func TestResolveModelEnforcesAccountCatalog(t *testing.T) {
	client := &Client{account: &store.Account{ClineModelIDs: CatalogSnapshot([]Model{{ID: "allowed"}})}}
	if _, err := client.resolveModel(upstream.UpstreamRequest{Model: "denied"}); err == nil {
		t.Fatal("model absent from this account catalog was accepted")
	}
	if got, err := client.resolveModel(upstream.UpstreamRequest{Model: "ALLOWED"}); err != nil || got != "allowed" {
		t.Fatalf("resolve allowed=%q err=%v", got, err)
	}
}

func TestConsumeStreamPreservesFinishReasonAndReasoningUsage(t *testing.T) {
	body := `data: {"choices":[{"delta":{"content":"partial"},"finish_reason":"length"}],"usage":{"prompt_tokens":3,"completion_tokens":5,"completion_tokens_details":{"reasoning_tokens":4}}}` + "\n\n"
	result, err := consumeStream(strings.NewReader(body), false, nil)
	if err != nil {
		t.Fatal(err)
	}
	if result.FinishReason() != "max_tokens" {
		t.Fatalf("finish=%q", result.FinishReason())
	}
	if result.Usage["reasoningTokens"] != 4 || result.Usage["reasoning_tokens"] != 4 {
		t.Fatalf("usage=%v", result.Usage)
	}
}
