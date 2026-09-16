package handler

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
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
	"orchids-api/internal/warp"
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

	acc1 := createEnabledTestAccount(t, s, "acc-1", "puter")
	acc2 := createEnabledTestAccount(t, s, "acc-2", "puter")

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

	_, selected, release, err := h.acquireAccountSelection(context.Background(), "puter", true, nil, accountSelectionOptions{})
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

	acc := createEnabledTestAccount(t, s, "busy-puter", "puter")
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
	_, selected, release, trackedID, err := h.acquireReservedAccountSelection(ctx, "puter", true, nil, accountSelectionOptions{ModelID: "deepseek-v4-flash"})
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

func TestAcquireReservedWarpCloudAgent_WaitsForPaidAccountLease(t *testing.T) {
	s, mini := setupConnTrackerHandlerTest(t)
	defer func() {
		_ = s.Close()
		mini.Close()
	}()

	acc := createEnabledTestAccount(t, s, "busy-warp-build", "warp")
	acc.Subscription = "build/business"
	acc.WarpMonthlyLimit = 1500
	acc.WarpMonthlyRemaining = 1152
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
	_, selected, release, trackedID, err := h.acquireReservedAccountSelection(ctx, "warp", true, nil, accountSelectionOptions{
		ModelID: "auto-open",
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

func TestSelectAccount_WarpUsesAccountModelChoices(t *testing.T) {
	s, mini := setupConnTrackerHandlerTest(t)
	defer func() {
		_ = s.Close()
		mini.Close()
	}()

	acc1 := createEnabledTestAccount(t, s, "warp-1", "warp")
	acc2 := createEnabledTestAccount(t, s, "warp-2", "warp")
	if err := warp.SaveAccountModelChoices(context.Background(), s, &warp.AccountModelChoices{
		Accounts: map[string][]string{
			strconv.FormatInt(acc1.ID, 10): {"auto-open", "gpt-5-2-low"},
			strconv.FormatInt(acc2.ID, 10): {"auto-open", "claude-4-6-opus-high"},
		},
	}); err != nil {
		t.Fatalf("SaveAccountModelChoices() error = %v", err)
	}

	lb := loadbalancer.NewWithCacheTTL(s, time.Second)
	h := NewWithLoadBalancer(&config.Config{}, lb)
	h.connTracker = newSpyConnTracker(map[int64]int64{
		acc1.ID: 0,
		acc2.ID: 0,
	})
	h.SetClientFactory(func(acc *store.Account, cfg *config.Config) UpstreamClient {
		return &trackerTestUpstream{}
	})

	_, selected, release, err := h.acquireAccountSelection(context.Background(), "warp", true, nil, accountSelectionOptions{ModelID: "claude-4-6-opus-high"})
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
}

func TestSelectAccount_WarpRejectsModelMissingFromCachedPool(t *testing.T) {
	s, mini := setupConnTrackerHandlerTest(t)
	defer func() {
		_ = s.Close()
		mini.Close()
	}()

	acc1 := createEnabledTestAccount(t, s, "warp-1", "warp")
	acc2 := createEnabledTestAccount(t, s, "warp-2", "warp")
	if err := warp.SaveAccountModelChoices(context.Background(), s, &warp.AccountModelChoices{
		Accounts: map[string][]string{
			strconv.FormatInt(acc1.ID, 10): {"auto-open", "gpt-5-2-low"},
			strconv.FormatInt(acc2.ID, 10): {"auto-open", "gpt-5-2-low"},
		},
	}); err != nil {
		t.Fatalf("SaveAccountModelChoices() error = %v", err)
	}
	lb := loadbalancer.NewWithCacheTTL(s, time.Second)
	h := NewWithLoadBalancer(&config.Config{}, lb)
	h.connTracker = newSpyConnTracker(map[int64]int64{
		acc1.ID: 0,
		acc2.ID: 1,
	})
	h.SetClientFactory(func(acc *store.Account, cfg *config.Config) UpstreamClient {
		return &trackerTestUpstream{}
	})

	_, selected, release, err := h.acquireAccountSelection(context.Background(), "warp", true, nil, accountSelectionOptions{ModelID: "claude-4-7-opus-xhigh-fast"})
	defer release()
	if err == nil {
		t.Fatal("selectAccount() error = nil, want unavailable model error")
	}
	if selected != nil {
		t.Fatalf("selectAccount() selected account %d, want nil", selected.ID)
	}
	if !strings.Contains(err.Error(), "not available in the current Warp account pool") {
		t.Fatalf("selectAccount() error = %q", err.Error())
	}
}

func TestHandleMessages_AccountSwitchUsesHandlerConnTracker(t *testing.T) {
	s, mini := setupConnTrackerHandlerTest(t)
	defer func() {
		_ = s.Close()
		mini.Close()
	}()

	acc1 := createEnabledTestAccount(t, s, "acc-1", "puter")
	acc2 := createEnabledTestAccount(t, s, "acc-2", "puter")
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
	req := httptest.NewRequest(http.MethodPost, "http://x/puter/v1/messages", bytes.NewReader(body))
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

func TestWorkBuddyDefaultConcurrencyLimitIsThree(t *testing.T) {
	t.Parallel()

	if got := effectiveAccountConcurrencyLimit(&store.Account{AccountType: "workbuddy"}); got != 3 {
		t.Fatalf("default WorkBuddy limit = %d, want 3", got)
	}
	if got := effectiveAccountConcurrencyLimit(&store.Account{AccountType: "workbuddy", MaxConcurrent: 7}); got != 7 {
		t.Fatalf("configured WorkBuddy limit = %d, want 7", got)
	}
	for _, accountType := range []string{"puter", "warp", "grok"} {
		if got := effectiveAccountConcurrencyLimit(&store.Account{AccountType: accountType}); got != 1 {
			t.Fatalf("unconfigured %s limit = %d, want 1", accountType, got)
		}
	}
	if got := effectiveAccountConcurrencyLimit(&store.Account{AccountType: "puter", MaxConcurrent: 4}); got != 4 {
		t.Fatalf("configured Puter limit = %d, want 4", got)
	}
}

func TestTryAcquireTrackedAccount_DoesNotAdmitFourthWorkBuddyRequest(t *testing.T) {
	t.Parallel()

	tracker := loadbalancer.NewMemoryConnTracker()
	h := &Handler{connTracker: tracker}
	acc := &store.Account{ID: 42, AccountType: "workbuddy"}
	for i := 0; i < 3; i++ {
		if _, ok := h.tryAcquireTrackedAccount(acc); !ok {
			t.Fatalf("acquire %d unexpectedly rejected", i+1)
		}
	}
	if _, ok := h.tryAcquireTrackedAccount(acc); ok {
		t.Fatal("fourth WorkBuddy request was admitted")
	}
	if got := tracker.GetCount(acc.ID); got != 3 {
		t.Fatalf("tracked count = %d, want 3", got)
	}
	for range 3 {
		h.releaseTrackedAccount(acc.ID)
	}
	if got := tracker.GetCount(acc.ID); got != 0 {
		t.Fatalf("tracked count after release = %d, want 0", got)
	}
}
