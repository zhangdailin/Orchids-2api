package handler

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/goccy/go-json"

	"orchids-api/internal/config"
	"orchids-api/internal/debug"
	"orchids-api/internal/loadbalancer"
	"orchids-api/internal/store"
	"orchids-api/internal/upstream"
)

type spyConnTracker struct {
	mu             sync.Mutex
	counts         map[int64]int64
	acquireCalls   int
	releaseCalls   int
	getCountsCalls int
}

func newSpyConnTracker(counts map[int64]int64) *spyConnTracker {
	cloned := make(map[int64]int64, len(counts))
	for id, count := range counts {
		cloned[id] = count
	}
	return &spyConnTracker{counts: cloned}
}

func (t *spyConnTracker) Acquire(accountID int64) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.acquireCalls++
	t.counts[accountID]++
}

func (t *spyConnTracker) Release(accountID int64) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.releaseCalls++
	if current := t.counts[accountID]; current > 0 {
		t.counts[accountID] = current - 1
	}
}

func (t *spyConnTracker) GetCount(accountID int64) int64 {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.counts[accountID]
}

func (t *spyConnTracker) GetCounts(accountIDs []int64) map[int64]int64 {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.getCountsCalls++
	counts := make(map[int64]int64, len(accountIDs))
	for _, id := range accountIDs {
		counts[id] = t.counts[id]
	}
	return counts
}

type trackerTestUpstream struct {
	err    error
	events []upstream.SSEMessage
}

func (m *trackerTestUpstream) SendRequestWithPayload(ctx context.Context, req upstream.UpstreamRequest, onMessage func(upstream.SSEMessage), logger *debug.Logger) error {
	if m.err != nil {
		return m.err
	}
	for _, e := range m.events {
		onMessage(e)
	}
	return nil
}

func setupConnTrackerHandlerTest(t *testing.T) (*store.Store, *miniredis.Miniredis) {
	t.Helper()

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

	return s, mini
}

func createEnabledTestAccount(t *testing.T, s *store.Store, name, accountType string) *store.Account {
	t.Helper()

	acc := &store.Account{
		Name:        name,
		AccountType: accountType,
		SessionID:   name + "-session",
		Enabled:     true,
		Weight:      1,
	}
	if err := s.CreateAccount(context.Background(), acc); err != nil {
		t.Fatalf("CreateAccount(%s) error = %v", name, err)
	}
	return acc
}

func TestSelectAccount_UsesHandlerConnTracker(t *testing.T) {
	s, mini := setupConnTrackerHandlerTest(t)
	defer func() {
		_ = s.Close()
		mini.Close()
	}()

	acc1 := createEnabledTestAccount(t, s, "acc-1", "workbuddy")
	acc2 := createEnabledTestAccount(t, s, "acc-2", "workbuddy")

	lb := loadbalancer.NewWithCacheTTL(s, time.Second)
	globalTracker := newSpyConnTracker(map[int64]int64{
		acc1.ID: 0,
		acc2.ID: 9,
	})
	lb.SetConnTracker(globalTracker)

	h := NewWithLoadBalancer(&config.Config{RequestTimeout: 10}, lb)
	localTracker := newSpyConnTracker(map[int64]int64{
		acc1.ID: 8,
		acc2.ID: 0,
	})
	h.connTracker = localTracker
	h.SetClientFactory(func(acc *store.Account, cfg *config.Config) UpstreamClient {
		return &trackerTestUpstream{}
	})

	_, selected, release, err := h.acquireAccountSelection(context.Background(), "workbuddy", true, nil, accountSelectionOptions{})
	defer release()
	if err != nil {
		t.Fatalf("selectAccount() error = %v", err)
	}
	if selected == nil {
		t.Fatal("selectAccount() returned nil account")
	}
	if selected.ID != acc2.ID {
		t.Fatalf("selectAccount() picked account %d, want %d", selected.ID, acc2.ID)
	}
	if localTracker.getCountsCalls == 0 {
		t.Fatal("expected handler-local tracker to be consulted")
	}
	if globalTracker.getCountsCalls != 0 {
		t.Fatalf("expected global tracker to be bypassed, got %d GetCounts calls", globalTracker.getCountsCalls)
	}
}

func TestAcquireReservedAccountSelection_WaitsForShortLease(t *testing.T) {
	s, mini := setupConnTrackerHandlerTest(t)
	defer func() {
		_ = s.Close()
		mini.Close()
	}()

	acc := createEnabledTestAccount(t, s, "busy-workbuddy", "workbuddy")
	lb := loadbalancer.NewWithCacheTTL(s, time.Second)
	h := NewWithLoadBalancer(&config.Config{RequestTimeout: 10}, lb)
	tracker := newSpyConnTracker(map[int64]int64{acc.ID: 1})
	h.connTracker = tracker
	h.SetClientFactory(func(acc *store.Account, cfg *config.Config) UpstreamClient {
		return &trackerTestUpstream{}
	})

	// A request finishing on the only account must be visible to the selector;
	// the old 225ms retry window could return 503 before this release landed.
	go func() {
		time.Sleep(350 * time.Millisecond)
		tracker.mu.Lock()
		tracker.counts[acc.ID] = 0
		tracker.mu.Unlock()
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_, selected, release, trackedID, err := h.acquireReservedAccountSelection(ctx, "workbuddy", true, nil, accountSelectionOptions{ModelID: "deepseek-v4-flash"})
	defer release()
	if err != nil {
		t.Fatalf("acquireReservedAccountSelection() error = %v", err)
	}
	if selected == nil || selected.ID != acc.ID {
		t.Fatalf("selected account = %#v, want account %d", selected, acc.ID)
	}
	if trackedID != acc.ID {
		t.Fatalf("tracked account id = %d, want %d", trackedID, acc.ID)
	}
}

func TestAcquireReservedAccountSelection_WaitsForBusyAccountLease(t *testing.T) {
	s, mini := setupConnTrackerHandlerTest(t)
	defer func() {
		_ = s.Close()
		mini.Close()
	}()

	acc := createEnabledTestAccount(t, s, "busy-workbuddy", "workbuddy")
	if err := s.UpdateAccount(context.Background(), acc); err != nil {
		t.Fatalf("UpdateAccount() error = %v", err)
	}

	lb := loadbalancer.NewWithCacheTTL(s, time.Second)
	h := NewWithLoadBalancer(&config.Config{RequestTimeout: 10}, lb)
	tracker := newSpyConnTracker(map[int64]int64{acc.ID: 1})
	h.connTracker = tracker
	h.SetClientFactory(func(acc *store.Account, cfg *config.Config) UpstreamClient {
		return &trackerTestUpstream{}
	})

	go func() {
		time.Sleep(350 * time.Millisecond)
		tracker.mu.Lock()
		tracker.counts[acc.ID] = 0
		tracker.mu.Unlock()
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_, selected, release, trackedID, err := h.acquireReservedAccountSelection(ctx, "workbuddy", true, nil, accountSelectionOptions{
		ModelID: "claude-opus-5",
	})
	defer release()
	if err != nil {
		t.Fatalf("acquireReservedAccountSelection() error = %v", err)
	}
	if selected == nil || selected.ID != acc.ID {
		t.Fatalf("selected account = %#v, want account %d", selected, acc.ID)
	}
	if trackedID != acc.ID {
		t.Fatalf("tracked account id = %d, want %d", trackedID, acc.ID)
	}
}

func TestHandleMessages_AccountSwitchUsesHandlerConnTracker(t *testing.T) {
	s, mini := setupConnTrackerHandlerTest(t)
	defer func() {
		_ = s.Close()
		mini.Close()
	}()

	publishModel(t, s, &store.Model{Channel: "WorkBuddy", ModelID: "claude-opus-5"})
	acc1 := createEnabledTestAccount(t, s, "acc-1", "workbuddy")
	acc2 := createEnabledTestAccount(t, s, "acc-2", "workbuddy")
	acc2.MaxConcurrent = 2
	if err := s.UpdateAccount(context.Background(), acc2); err != nil {
		t.Fatalf("UpdateAccount(acc-2) error = %v", err)
	}

	lb := loadbalancer.NewWithCacheTTL(s, time.Second)
	globalTracker := newSpyConnTracker(nil)
	lb.SetConnTracker(globalTracker)

	cfg := &config.Config{
		DebugEnabled:   false,
		RequestTimeout: 10,
		MaxRetries:     1,
		RetryDelay:     0,
	}
	h := NewWithLoadBalancer(cfg, lb)
	localTracker := newSpyConnTracker(map[int64]int64{
		acc1.ID: 0,
		acc2.ID: 1,
	})
	h.connTracker = localTracker
	h.SetClientFactory(func(acc *store.Account, cfg *config.Config) UpstreamClient {
		if acc != nil && acc.ID == acc1.ID {
			return &trackerTestUpstream{err: errors.New("HTTP 429 Too Many Requests")}
		}
		if acc != nil && acc.ID == acc2.ID {
			return &trackerTestUpstream{events: []upstream.SSEMessage{
				{Type: "model", Event: map[string]any{"type": "text-start"}},
				{Type: "model", Event: map[string]any{"type": "text-delta", "delta": "ok"}},
				{Type: "model", Event: map[string]any{"type": "finish", "finishReason": "stop"}},
			}}
		}
		return &trackerTestUpstream{}
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

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	if globalTracker.acquireCalls != 0 || globalTracker.releaseCalls != 0 {
		t.Fatalf("expected global tracker to stay idle, got acquire=%d release=%d", globalTracker.acquireCalls, globalTracker.releaseCalls)
	}
	if localTracker.acquireCalls != 2 {
		t.Fatalf("expected local tracker acquire twice across account switch, got %d", localTracker.acquireCalls)
	}
	if localTracker.releaseCalls != 2 {
		t.Fatalf("expected both account leases released, got %d releases", localTracker.releaseCalls)
	}
	if got := localTracker.GetCount(acc1.ID); got != 0 {
		t.Fatalf("failed account retained %d connection leases", got)
	}
	if got := localTracker.GetCount(acc2.ID); got != 1 {
		t.Fatalf("successful account count = %d, want original pre-existing lease only", got)
	}
}

func TestDefaultAccountConcurrencyLimitIsTen(t *testing.T) {
	t.Parallel()

	// Every provider shares one default. The per-channel values this replaced
	// (WorkBuddy 3, Grok 1, Qoder unlimited) made a channel's capacity depend on
	// which switch arm it happened to fall into.
	for _, accountType := range []string{"grok", "workbuddy", "qoder", "cline"} {
		if got := effectiveAccountConcurrencyLimit(&store.Account{AccountType: accountType}); got != 10 {
			t.Fatalf("unconfigured %s limit = %d, want 10", accountType, got)
		}
	}
	if got := effectiveAccountConcurrencyLimit(&store.Account{AccountType: "workbuddy", MaxConcurrent: 7}); got != 7 {
		t.Fatalf("configured WorkBuddy limit = %d, want 7", got)
	}
	if got := effectiveAccountConcurrencyLimit(&store.Account{AccountType: "qoder", MaxConcurrent: 4}); got != 4 {
		t.Fatalf("configured Qoder limit = %d, want 4", got)
	}
}

func TestTryAcquireTrackedAccount_DoesNotAdmitRequestPastLimit(t *testing.T) {
	t.Parallel()

	const limit = 10
	tracker := loadbalancer.NewMemoryConnTracker()
	h := &Handler{connTracker: tracker}
	acc := &store.Account{ID: 42, AccountType: "workbuddy"}
	for i := 0; i < limit; i++ {
		if _, ok := h.tryAcquireTrackedAccount(acc); !ok {
			t.Fatalf("acquire %d unexpectedly rejected", i+1)
		}
	}
	if _, ok := h.tryAcquireTrackedAccount(acc); ok {
		t.Fatalf("request %d was admitted past the %d-slot limit", limit+1, limit)
	}
	if got := tracker.GetCount(acc.ID); got != limit {
		t.Fatalf("tracked count = %d, want %d", got, limit)
	}
	for range limit {
		h.releaseTrackedAccount(acc.ID)
	}
	if got := tracker.GetCount(acc.ID); got != 0 {
		t.Fatalf("tracked count after release = %d, want 0", got)
	}
}
