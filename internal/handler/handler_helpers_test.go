package handler

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"

	"orchids-api/internal/loadbalancer"
	"orchids-api/internal/store"
	"orchids-api/internal/warp"
)

func TestResolveWorkdir_NoSessionFallbackWithoutExplicitConversation(t *testing.T) {
	ss := NewMemorySessionStore(30*time.Minute, 100)
	ss.SetWorkdir(context.TODO(), "k1", "/stale/workdir")

	h := &Handler{
		sessionStore: ss,
	}
	r := httptest.NewRequest(http.MethodPost, "http://example.com/warp/v1/messages", nil)
	req := ClaudeRequest{}

	got, prev, changed := h.resolveWorkdir(r, req, "k1")
	if got != "" {
		t.Fatalf("expected empty workdir, got %q", got)
	}
	if prev != "/stale/workdir" {
		t.Fatalf("expected prev workdir retained, got %q", prev)
	}
	if changed {
		t.Fatalf("expected changed=false when no new workdir")
	}
}

func setupModelValidationHandler(t *testing.T) (*Handler, *store.Store, *miniredis.Miniredis) {
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

	lb := loadbalancer.NewWithCacheTTL(s, time.Second)
	h := NewWithLoadBalancer(nil, lb)
	return h, s, mini
}

func TestValidateModelAvailability_PuterUsesChannelSpecificModel(t *testing.T) {
	h, s, mini := setupModelValidationHandler(t)
	defer func() {
		_ = s.Close()
		mini.Close()
	}()

	ctx := context.Background()

	got, err := h.validateModelAvailability(ctx, "claude-opus-5", "puter")
	if err != nil {
		t.Fatalf("validateModelAvailability() error = %v", err)
	}
	if got == nil {
		t.Fatal("validateModelAvailability() returned nil model")
	}
	if got.Channel != "Puter" {
		t.Fatalf("validateModelAvailability() channel = %q, want %q", got.Channel, "Puter")
	}
	if got.ModelID != "claude-opus-5" {
		t.Fatalf("validateModelAvailability() model = %q, want %q", got.ModelID, "claude-opus-5")
	}
}

func TestSelectAccountRecord_WarpRejectsModelOutsideCurrentPool(t *testing.T) {
	h, s, mini := setupModelValidationHandler(t)
	defer func() {
		_ = s.Close()
		mini.Close()
	}()

	ctx := context.Background()
	if err := s.CreateAccount(ctx, &store.Account{
		AccountType:  "warp",
		RefreshToken: "warp-free-token",
		Enabled:      true,
	}); err != nil {
		t.Fatalf("CreateAccount() error = %v", err)
	}
	if err := warp.SaveAccountModelChoices(ctx, s, &warp.AccountModelChoices{Accounts: map[string][]string{"1": {"auto-open"}}}); err != nil {
		t.Fatalf("SaveAccountModelChoices() error = %v", err)
	}

	_, err := h.selectAccountRecordWithOptions(ctx, "warp", nil, accountSelectionOptions{ModelID: "gpt-5-2-medium"})
	if err == nil {
		t.Fatal("selectAccountRecord() error = nil, want unavailable model error")
	}
	if !strings.Contains(err.Error(), "not available in the current Warp account pool") {
		t.Fatalf("selectAccountRecord() error = %q", err.Error())
	}
}

func TestSelectAccountRecord_WarpContinuationPinsIssuingAccount(t *testing.T) {
	h, s, mini := setupModelValidationHandler(t)
	defer func() {
		_ = s.Close()
		mini.Close()
	}()

	ctx := context.Background()
	for _, name := range []string{"warp-one", "warp-two"} {
		if err := s.CreateAccount(ctx, &store.Account{
			Name:                 name,
			AccountType:          "warp",
			RefreshToken:         name + "-token",
			Subscription:         "build/business",
			WarpMonthlyLimit:     1500,
			WarpMonthlyRemaining: 100,
			Enabled:              true,
			Weight:               1,
		}); err != nil {
			t.Fatalf("CreateAccount(%s) error = %v", name, err)
		}
	}

	account, err := h.selectAccountRecordWithOptions(ctx, "warp", nil, accountSelectionOptions{
		ModelID:            "auto-open",
		PreferredAccountID: 2,
	})
	if err != nil {
		t.Fatalf("select pinned account error = %v", err)
	}
	if account == nil || account.ID != 2 {
		t.Fatalf("selected account=%v want id=2", account)
	}
}

// mustCreateModel inserts a model directly (avoiding reliance on seed data).
func mustCreateModel(t *testing.T, s *store.Store, id string, channel, modelID string, status store.ModelStatus) *store.Model {
	t.Helper()
	m := &store.Model{
		ID:        id,
		Channel:   channel,
		ModelID:   modelID,
		Name:      modelID,
		Status:    status,
		IsDefault: false,
		SortOrder: 0,
	}
	if err := s.UpdateModel(context.Background(), m); err != nil {
		t.Fatalf("UpdateModel(%s) error = %v", modelID, err)
	}
	return m
}

func TestValidateModelAvailability_RejectsOfflineExactMatchEvenWhenAliasExists(t *testing.T) {
	h, s, mini := setupModelValidationHandler(t)
	defer func() {
		_ = s.Close()
		mini.Close()
	}()

	ctx := context.Background()

	mustCreateModel(t, s, "199", "Puter", "claude-opus-4-6", store.ModelStatusOffline)

	mustCreateModel(t, s, "200", "Puter", "claude-opus-4.6", store.ModelStatusAvailable)

	got, err := h.validateModelAvailability(ctx, "claude-opus-4-6", "puter")
	if err == nil {
		t.Fatalf("validateModelAvailability() error = nil, got model=%v", got)
	}
	if err.Error() != "model not available" {
		t.Fatalf("validateModelAvailability() error = %q, want %q", err.Error(), "model not available")
	}
}

func TestValidateModelAvailability_ReturnsOfflineExactMatch(t *testing.T) {
	h, s, mini := setupModelValidationHandler(t)
	defer func() {
		_ = s.Close()
		mini.Close()
	}()

	ctx := context.Background()

	mustCreateModel(t, s, "199", "Puter", "claude-opus-4-6", store.ModelStatusOffline)

	mustCreateModel(t, s, "201", "Puter", "claude-opus-4.6", store.ModelStatusOffline)

	_, err := h.validateModelAvailability(ctx, "claude-opus-4-6", "puter")
	if err == nil {
		t.Fatal("validateModelAvailability() error = nil, want model not available")
	}
	if err.Error() != "model not available" {
		t.Fatalf("validateModelAvailability() error = %q, want %q", err.Error(), "model not available")
	}
}
