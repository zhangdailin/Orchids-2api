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

func TestHandleMessages_Warp403MarksAccountBlocked(t *testing.T) {
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
		Name:         "warp-1",
		AccountType:  "warp",
		RefreshToken: "rt",
		Enabled:      true,
		Weight:       1,
	}
	if err := s.CreateAccount(context.Background(), acc); err != nil {
		t.Fatalf("CreateAccount() error = %v", err)
	}

	publishModel(t, s, &store.Model{Channel: "Warp", ModelID: "auto-open"})

	lb := loadbalancer.NewWithCacheTTL(s, 0)
	h := NewWithLoadBalancer(&config.Config{
		DebugEnabled:   false,
		RequestTimeout: 10,
		MaxRetries:     0,
	}, lb)
	upstreamCalls := 0
	h.SetClientFactory(func(acc *store.Account, cfg *config.Config) UpstreamClient {
		upstreamCalls++
		return &errorUpstreamEdge{err: errors.New("warp stream request failed: HTTP 403")}
	})

	payload := map[string]any{
		"model":    "auto-open",
		"messages": []map[string]any{{"role": "user", "content": "hi"}},
		"system":   []any{},
		"stream":   false,
	}
	body, _ := json.Marshal(payload)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "http://x/warp/v1/messages", bytes.NewReader(body))
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
	req2 := httptest.NewRequest(http.MethodPost, "http://x/warp/v1/messages", bytes.NewReader(body2))
	h.HandleMessages(rec2, req2)
	// The pool's own note ("no enabled accounts available for channel: warp") is a
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

func TestHandleMessages_WarpCloudAgent403DoesNotMarkAccountBlocked(t *testing.T) {
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
		Name:                 "warp-free",
		AccountType:          "warp",
		RefreshToken:         "rt",
		Subscription:         "free",
		WarpMonthlyLimit:     60,
		WarpMonthlyRemaining: 50,
		Enabled:              true,
		Weight:               1,
	}
	if err := s.CreateAccount(context.Background(), acc); err != nil {
		t.Fatalf("CreateAccount() error = %v", err)
	}

	lb := loadbalancer.NewWithCacheTTL(s, 0)
	h := NewWithLoadBalancer(&config.Config{
		DebugEnabled:   false,
		RequestTimeout: 10,
		MaxRetries:     0,
	}, lb)
	h.SetClientFactory(func(acc *store.Account, cfg *config.Config) UpstreamClient {
		return &errorUpstreamEdge{err: errors.New(`warp stream request failed: HTTP 403: {"error":"not allowed to use the provided cloud agent"}`)}
	})

	payload := map[string]any{
		"model":    "auto-open",
		"messages": []map[string]any{{"role": "user", "content": "hi"}},
		"system":   []any{},
		"stream":   false,
	}
	body, _ := json.Marshal(payload)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "http://x/warp/v1/messages", bytes.NewReader(body))
	h.HandleMessages(rec, req)

	updated, err := s.GetAccount(context.Background(), acc.ID)
	if err != nil {
		t.Fatalf("GetAccount() error = %v", err)
	}
	if updated.StatusCode != "" {
		t.Fatalf("status_code=%q want empty for cloud-agent-only 403", updated.StatusCode)
	}
	if !updated.LastAttempt.IsZero() {
		t.Fatalf("last_attempt=%v want zero for cloud-agent-only 403", updated.LastAttempt)
	}
}
