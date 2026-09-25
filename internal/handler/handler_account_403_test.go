package handler

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/alicebob/miniredis/v2"
	"github.com/goccy/go-json"

	"orchids-api/internal/config"
	"orchids-api/internal/loadbalancer"
	"orchids-api/internal/store"
)

func TestHandleMessages_403MarksAccountBlocked(t *testing.T) {
	mini := miniredis.RunT(t)
	s, err := store.New(store.Options{
		StoreMode:   "redis",
		RedisAddr:   mini.Addr(),
		RedisDB:     0,
		RedisPrefix: "test:",
	})
	if err != nil {
		t.Fatalf("store.New() error = %v", err)
	}
	defer func() {
		_ = s.Close()
		mini.Close()
	}()

	acc := &store.Account{
		Name:         "workbuddy-1",
		AccountType:  "workbuddy",
		RefreshToken: "rt",
		Enabled:      true,
		Weight:       1,
	}
	if err := s.CreateAccount(context.Background(), acc); err != nil {
		t.Fatalf("CreateAccount() error = %v", err)
	}

	publishModel(t, s, &store.Model{Channel: "WorkBuddy", ModelID: "claude-opus-5"})

	lb := loadbalancer.NewWithCacheTTL(s, 0)
	h := NewWithLoadBalancer(&config.Config{
		DebugEnabled:   false,
		RequestTimeout: 10,
		MaxRetries:     0,
	}, lb)
	upstreamCalls := 0
	h.SetClientFactory(func(acc *store.Account, cfg *config.Config) UpstreamClient {
		upstreamCalls++
		return &errorUpstreamEdge{err: errors.New("workbuddy stream request failed: HTTP 403")}
	})

	payload := map[string]any{
		"model":    "claude-opus-5",
		"messages": []map[string]any{{"role": "user", "content": "hi"}},
		"system":   []any{},
		"stream":   false,
	}
	body, _ := json.Marshal(payload)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "http://x/workbuddy/v1/messages", bytes.NewReader(body))
	h.HandleMessages(rec, req)

	updated, err := s.GetAccount(context.Background(), acc.ID)
	if err != nil {
		t.Fatalf("GetAccount() error = %v", err)
	}
	if updated.StatusCode != "403" {
		t.Fatalf("status_code=%q want 403", updated.StatusCode)
	}
	if updated.LastAttempt.IsZero() {
		t.Fatal("expected last_attempt to be set")
	}
	if upstreamCalls != 1 {
		t.Fatalf("upstreamCalls=%d want 1 after first request", upstreamCalls)
	}

	payload["messages"] = []map[string]any{{"role": "user", "content": "hi again"}}
	body2, _ := json.Marshal(payload)
	rec2 := httptest.NewRecorder()
	req2 := httptest.NewRequest(http.MethodPost, "http://x/workbuddy/v1/messages", bytes.NewReader(body2))
	h.HandleMessages(rec2, req2)
	// The pool's own note ("no enabled accounts available for channel: workbuddy") is a
	// diagnostic and stays in the log: the client is told the pool cannot serve the
	// request, in words that do not read like the channel is empty or the caller
	// sent something wrong.
	if rec2.Code != http.StatusServiceUnavailable {
		t.Fatalf("second status=%d want 503 body=%s", rec2.Code, rec2.Body.String())
	}
	secondBody := rec2.Body.String()
	if !strings.Contains(secondBody, "no account in this channel can serve the request") {
		t.Fatalf("second body=%q want the pool-unavailable answer", secondBody)
	}
	if strings.Contains(secondBody, "no enabled accounts available for channel") {
		t.Fatalf("selector detail leaked into the response: %s", secondBody)
	}
	if upstreamCalls != 1 {
		t.Fatalf("upstreamCalls=%d want still 1 after cached 403", upstreamCalls)
	}
}
