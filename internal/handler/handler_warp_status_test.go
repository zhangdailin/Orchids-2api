package handler

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
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
}
