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

func TestValidateModelAvailability_RejectsOfflineExactMatchEvenWhenAliasExists(t *testing.T) {
	h, s, mini := setupModelValidationHandler(t)
	defer func() {
		_ = s.Close()
		mini.Close()
	}()

	ctx := context.Background()

	exact, err := s.GetModelByChannelAndModelID(ctx, "orchids", "claude-opus-4-6")
	if err != nil {
		t.Fatalf("GetModelByModelID(exact) error = %v", err)
	}
	exact.Status = store.ModelStatusOffline
	if err := s.UpdateModel(ctx, exact); err != nil {
		t.Fatalf("UpdateModel(exact) error = %v", err)
	}

	alias := &store.Model{
		ID:        "200",
		Channel:   "Orchids",
		ModelID:   "claude-opus-4.6",
		Name:      "Claude Opus 4.6",
		Status:    store.ModelStatusAvailable,
		IsDefault: false,
		SortOrder: 0,
	}
	if err := s.UpdateModel(ctx, alias); err != nil {
		t.Fatalf("UpdateModel(alias) error = %v", err)
	}

	got, err := h.validateModelAvailability(ctx, "claude-opus-4-6", "orchids")
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

	exact, err := s.GetModelByChannelAndModelID(ctx, "orchids", "claude-opus-4-6")
	if err != nil {
		t.Fatalf("GetModelByModelID(exact) error = %v", err)
	}
	exact.Status = store.ModelStatusOffline
	if err := s.UpdateModel(ctx, exact); err != nil {
		t.Fatalf("UpdateModel(exact) error = %v", err)
	}

	alias := &store.Model{
		ID:        "201",
		Channel:   "Orchids",
		ModelID:   "claude-opus-4.6",
		Name:      "Claude Opus 4.6",
		Status:    store.ModelStatusOffline,
		IsDefault: false,
		SortOrder: 0,
	}
	if err := s.UpdateModel(ctx, alias); err != nil {
		t.Fatalf("UpdateModel(alias) error = %v", err)
	}

	_, err = h.validateModelAvailability(ctx, "claude-opus-4-6", "orchids")
	if err == nil {
		t.Fatal("validateModelAvailability() error = nil, want model not available")
	}
	if err.Error() != "model not available" {
		t.Fatalf("validateModelAvailability() error = %q, want %q", err.Error(), "model not available")
	}
}

func TestValidateModelAvailability_PuterUsesChannelSpecificModel(t *testing.T) {
	h, s, mini := setupModelValidationHandler(t)
	defer func() {
		_ = s.Close()
		mini.Close()
	}()

	ctx := context.Background()

	got, err := h.validateModelAvailability(ctx, "claude-opus-4-5", "puter")
	if err != nil {
		t.Fatalf("validateModelAvailability() error = %v", err)
	}
	if got == nil {
		t.Fatal("validateModelAvailability() returned nil model")
	}
	if got.Channel != "Puter" {
		t.Fatalf("validateModelAvailability() channel = %q, want %q", got.Channel, "Puter")
	}
	if got.ModelID != "claude-opus-4-5" {
		t.Fatalf("validateModelAvailability() model = %q, want %q", got.ModelID, "claude-opus-4-5")
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
	if err := warp.SaveAccountModelChoicesForAccount(ctx, s, 1, []string{"auto-open"}); err != nil {
		t.Fatalf("SaveAccountModelChoicesForAccount() error = %v", err)
	}

	_, err := h.selectAccountRecord(ctx, "warp", nil, "gpt-5-2-medium")
	if err == nil {
		t.Fatal("selectAccountRecord() error = nil, want unavailable model error")
	}
	if !strings.Contains(err.Error(), "not available in the current Warp account pool") {
		t.Fatalf("selectAccountRecord() error = %q", err.Error())
	}
}

func TestSelectAccountRecord_WarpExhaustedPaidAccountIsFreeOnly(t *testing.T) {
	h, s, mini := setupModelValidationHandler(t)
	defer func() {
		_ = s.Close()
		mini.Close()
	}()

	ctx := context.Background()
	if err := s.CreateAccount(ctx, &store.Account{
		AccountType:          "warp",
		RefreshToken:         "warp-paid-token",
		Subscription:         "build/business",
		UsageLimit:           1500,
		UsageCurrent:         100,
		WarpMonthlyLimit:     1500,
		WarpMonthlyRemaining: 0,
		WarpBonusRemaining:   0,
		Enabled:              true,
	}); err != nil {
		t.Fatalf("CreateAccount() error = %v", err)
	}
	if err := warp.SaveAccountModelChoicesForAccount(ctx, s, 1, []string{"auto-open", "gpt-5-2-medium"}); err != nil {
		t.Fatalf("SaveAccountModelChoicesForAccount() error = %v", err)
	}

	_, err := h.selectAccountRecord(ctx, "warp", nil, "gpt-5-2-medium")
	if err == nil {
		t.Fatal("selectAccountRecord() error = nil, want unavailable model error")
	}
	if !strings.Contains(err.Error(), "not available in the current Warp account pool") {
		t.Fatalf("selectAccountRecord() error = %q", err.Error())
	}

	account, err := h.selectAccountRecord(ctx, "warp", nil, "auto-open")
	if err != nil {
		t.Fatalf("selectAccountRecord(default) error = %v", err)
	}
	if account == nil || account.ID != 1 {
		t.Fatalf("selectAccountRecord(default) account=%v want id=1", account)
	}
}
