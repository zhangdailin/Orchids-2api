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
	"orchids-api/internal/debug"
	"orchids-api/internal/loadbalancer"
	"orchids-api/internal/store"
	"orchids-api/internal/upstream"
)

type recordingWarpAccountUpstream struct {
	accountID int64
	seen      *[]int64
}

func (m *recordingWarpAccountUpstream) SendRequestWithPayload(ctx context.Context, req upstream.UpstreamRequest, onMessage func(upstream.SSEMessage), logger *debug.Logger) error {
	*m.seen = append(*m.seen, m.accountID)
	onMessage(upstream.SSEMessage{Type: "model", Event: map[string]interface{}{"type": "text-start"}})
	onMessage(upstream.SSEMessage{Type: "model", Event: map[string]interface{}{"type": "text-delta", "delta": "ok"}})
	onMessage(upstream.SSEMessage{Type: "model", Event: map[string]interface{}{"type": "finish", "finishReason": "stop"}})
	return nil
}

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

	lb := loadbalancer.NewWithCacheTTL(s, 0)
	h := NewWithLoadBalancer(&config.Config{
		DebugEnabled:            false,
		RequestTimeout:          10,
		MaxRetries:              0,
		ContextMaxTokens:        1024,
		ContextSummaryMaxTokens: 256,
		ContextKeepTurns:        2,
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
	if rec2.Code != http.StatusServiceUnavailable {
		t.Fatalf("second status=%d want 503 body=%s", rec2.Code, rec2.Body.String())
	}
	if !strings.Contains(rec2.Body.String(), "no enabled accounts available for channel: warp") {
		t.Fatalf("second body=%q want no available warp account", rec2.Body.String())
	}
	if upstreamCalls != 1 {
		t.Fatalf("upstreamCalls=%d want still 1 after cached 403", upstreamCalls)
	}
}

func TestHandleMessages_WarpCodingRequestUsesCloudAgentAccount(t *testing.T) {
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

	free := &store.Account{
		Name:                 "warp-free",
		AccountType:          "warp",
		RefreshToken:         "free-rt",
		Subscription:         "free",
		WarpMonthlyLimit:     60,
		WarpMonthlyRemaining: 50,
		Enabled:              true,
		Weight:               1,
	}
	if err := s.CreateAccount(context.Background(), free); err != nil {
		t.Fatalf("CreateAccount(free) error = %v", err)
	}
	paid := &store.Account{
		Name:                 "warp-build",
		AccountType:          "warp",
		RefreshToken:         "paid-rt",
		Subscription:         "build/business",
		WarpMonthlyLimit:     1500,
		WarpMonthlyRemaining: 100,
		Enabled:              true,
		Weight:               1,
	}
	if err := s.CreateAccount(context.Background(), paid); err != nil {
		t.Fatalf("CreateAccount(paid) error = %v", err)
	}

	lb := loadbalancer.NewWithCacheTTL(s, 0)
	h := NewWithLoadBalancer(&config.Config{
		DebugEnabled:            false,
		RequestTimeout:          10,
		MaxRetries:              0,
		ContextMaxTokens:        1024,
		ContextSummaryMaxTokens: 256,
		ContextKeepTurns:        2,
	}, lb)
	seen := []int64{}
	h.SetClientFactory(func(acc *store.Account, cfg *config.Config) UpstreamClient {
		return &recordingWarpAccountUpstream{accountID: acc.ID, seen: &seen}
	})

	payload := map[string]any{
		"model":    "auto-open",
		"messages": []map[string]any{{"role": "user", "content": "帮我用python写一个计算器"}},
		"system":   []any{},
		"stream":   false,
	}
	body, _ := json.Marshal(payload)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "http://x/warp/v1/messages", bytes.NewReader(body))
	h.HandleMessages(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d want 200 body=%s", rec.Code, rec.Body.String())
	}
	if len(seen) != 1 {
		t.Fatalf("selected account calls=%v want exactly one upstream call", seen)
	}
	if seen[0] != paid.ID {
		t.Fatalf("selected account=%d want paid cloud-agent account %d", seen[0], paid.ID)
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
		DebugEnabled:            false,
		RequestTimeout:          10,
		MaxRetries:              0,
		ContextMaxTokens:        1024,
		ContextSummaryMaxTokens: 256,
		ContextKeepTurns:        2,
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
