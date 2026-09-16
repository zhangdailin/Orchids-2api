package handler

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"

	"orchids-api/internal/config"
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

// Warp publishes models as "<family>-<effort>"; a client that asks for the
// family name plus reasoning_effort must land on the matching catalog entry
// instead of a "model not found" rejection.
func TestResolveEffortModelVariant(t *testing.T) {
	h, s, mini := setupModelValidationHandler(t)
	defer func() {
		_ = s.Close()
		mini.Close()
	}()

	ctx := context.Background()
	mustCreateModel(t, s, "301", "Warp", "gpt-5-6-sol-low", store.ModelStatusAvailable)
	mustCreateModel(t, s, "302", "Warp", "gpt-5-6-sol-medium", store.ModelStatusAvailable)
	mustCreateModel(t, s, "303", "Warp", "gpt-5-6-sol-high", store.ModelStatusAvailable)

	cases := []struct {
		name     string
		model    string
		effort   string
		channel  string
		expected string
	}{
		{"requested effort wins", "gpt-5-6-sol", "low", "warp", "gpt-5-6-sol-low"},
		{"defaults to medium", "gpt-5-6-sol", "", "warp", "gpt-5-6-sol-medium"},
		{"unknown effort falls back", "gpt-5-6-sol", "turbo", "warp", "gpt-5-6-sol-medium"},
		{"exact hit wins", "gpt-5-6-sol-high", "low", "warp", "gpt-5-6-sol-high"},
		{"unknown family is untouched", "gpt-9-unknown", "low", "warp", "gpt-9-unknown"},
		{"empty model is untouched", "", "low", "warp", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := h.resolveEffortModelVariant(ctx, tc.model, tc.effort, tc.channel); got != tc.expected {
				t.Fatalf("resolveEffortModelVariant(%q, %q, %q) = %q, want %q", tc.model, tc.effort, tc.channel, got, tc.expected)
			}
		})
	}
}

func TestResolveEffortModelVariant_CaseInsensitiveRequest(t *testing.T) {
	h, s, mini := setupModelValidationHandler(t)
	defer func() {
		_ = s.Close()
		mini.Close()
	}()

	mustCreateModel(t, s, "310", "Warp", "gpt-5-6-sol-low", store.ModelStatusAvailable)

	got := h.resolveEffortModelVariant(context.Background(), "GPT-5-6-SOL", "LOW", "warp")
	if got != "gpt-5-6-sol-low" {
		t.Fatalf("resolved = %q, want the lower-case catalog id", got)
	}
}

func TestHandleMessages_WarpResolvesBareModelToEffortVariant(t *testing.T) {
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

	ctx := context.Background()
	if err := s.CreateAccount(ctx, &store.Account{
		Name:         "warp-1",
		AccountType:  "warp",
		RefreshToken: "rt",
		Enabled:      true,
		Weight:       1,
	}); err != nil {
		t.Fatalf("CreateAccount() error = %v", err)
	}
	mustCreateModel(t, s, "401", "Warp", "gpt-5-6-sol-low", store.ModelStatusAvailable)
	mustCreateModel(t, s, "402", "Warp", "gpt-5-6-sol-medium", store.ModelStatusAvailable)

	lb := loadbalancer.NewWithCacheTTL(s, 0)
	h := NewWithLoadBalancer(&config.Config{DebugEnabled: false, RequestTimeout: 10, MaxRetries: 0}, lb)
	client := &fakePayloadClient{}
	h.SetClientFactory(func(acc *store.Account, cfg *config.Config) UpstreamClient { return client })

	body := `{"model":"gpt-5-6-sol","messages":[{"role":"user","content":"hi"}],"stream":false,"reasoning_effort":"low"}`
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "http://x/warp/v1/chat/completions", strings.NewReader(body))
	h.HandleMessages(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	client.mu.Lock()
	defer client.mu.Unlock()
	if len(client.calls) != 1 {
		t.Fatalf("upstream calls = %d, want 1", len(client.calls))
	}
	if client.calls[0].Model != "gpt-5-6-sol-low" {
		t.Fatalf("upstream model = %q, want the effort variant gpt-5-6-sol-low", client.calls[0].Model)
	}
}
