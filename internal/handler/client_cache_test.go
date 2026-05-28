package handler

import (
	"context"
	"testing"
	"time"

	"orchids-api/internal/config"
	"orchids-api/internal/debug"
	"orchids-api/internal/store"
	"orchids-api/internal/upstream"
)

type testCachedClient struct {
	id int
}

func (c *testCachedClient) SendRequest(ctx context.Context, prompt string, chatHistory []interface{}, model string, onMessage func(upstream.SSEMessage), logger *debug.Logger) error {
	return nil
}

func TestGetOrCreateAccountClient_ReusesClientAcrossStatsOnlyAccountUpdates(t *testing.T) {
	t.Parallel()

	cfg := &config.Config{RequestTimeout: 30}
	h := &Handler{
		config:       cfg,
		clientCache:  newAccountClientCache(),
		sessionStore: NewMemorySessionStore(30*time.Minute, 1024),
		dedupStore:   NewMemoryDedupStore(duplicateWindow, duplicateCleanupWindow),
	}

	created := 0
	h.SetClientFactory(func(acc *store.Account, cfg *config.Config) UpstreamClient {
		created++
		return &testCachedClient{id: created}
	})

	base := &store.Account{
		ID:            6,
		AccountType:   "puter",
		SessionCookie: "session-a",
		UpdatedAt:     time.Unix(100, 0),
	}

	first := h.getOrCreateAccountClient(base)
	if first == nil {
		t.Fatal("expected first client")
	}
	if created != 1 {
		t.Fatalf("created=%d want 1", created)
	}

	statsOnly := *base
	statsOnly.UpdatedAt = base.UpdatedAt.Add(5 * time.Minute)
	statsOnly.LastUsedAt = time.Unix(200, 0)
	statsOnly.RequestCount = 99
	statsOnly.UsageTotal = 12345
	statsOnly.UsageCurrent = 678

	second := h.getOrCreateAccountClient(&statsOnly)
	if second == nil {
		t.Fatal("expected second client")
	}
	if second != first {
		t.Fatal("expected stats-only update to reuse cached client")
	}
	if created != 1 {
		t.Fatalf("created=%d want 1 after stats-only update", created)
	}
}

func TestGetOrCreateAccountClient_RebuildsWhenCredentialsChange(t *testing.T) {
	t.Parallel()

	cfg := &config.Config{RequestTimeout: 30}
	h := &Handler{
		config:       cfg,
		clientCache:  newAccountClientCache(),
		sessionStore: NewMemorySessionStore(30*time.Minute, 1024),
		dedupStore:   NewMemoryDedupStore(duplicateWindow, duplicateCleanupWindow),
	}

	created := 0
	h.SetClientFactory(func(acc *store.Account, cfg *config.Config) UpstreamClient {
		created++
		return &testCachedClient{id: created}
	})

	base := &store.Account{
		ID:            6,
		AccountType:   "puter",
		SessionCookie: "session-a",
	}

	first := h.getOrCreateAccountClient(base)
	if first == nil {
		t.Fatal("expected first client")
	}

	changed := *base
	changed.SessionCookie = "session-b"

	second := h.getOrCreateAccountClient(&changed)
	if second == nil {
		t.Fatal("expected second client")
	}
	if second == first {
		t.Fatal("expected credential change to rebuild cached client")
	}
	if created != 2 {
		t.Fatalf("created=%d want 2 after credential change", created)
	}
}
