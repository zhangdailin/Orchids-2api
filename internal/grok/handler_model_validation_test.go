package grok

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
)

func setupValidationHandler(t *testing.T) (*Handler, *store.Store, *miniredis.Miniredis) {
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
	return NewHandler(nil, lb), s, mini
}

func TestEnsureModelEnabled_RejectsHiddenGrokModel(t *testing.T) {
	h, s, mini := setupValidationHandler(t)
	defer func() {
		_ = s.Close()
		mini.Close()
	}()

	err := h.ensureModelEnabled(context.Background(), "grok-4.1")
	if err == nil {
		t.Fatal("expected error")
	}
	if err.Error() != "model not found" {
		t.Fatalf("error=%q want %q", err.Error(), "model not found")
	}
}

func TestHandleChatCompletions_DoesNotAutoRegisterUnknownModel(t *testing.T) {
	h, s, mini := setupValidationHandler(t)
	defer func() {
		_ = s.Close()
		mini.Close()
	}()

	body := `{"model":"grok-5","messages":[{"role":"user","content":"hello"}],"stream":false}`
	req := httptest.NewRequest(http.MethodPost, "/grok/v1/chat/completions", strings.NewReader(body))
	rec := httptest.NewRecorder()

	h.HandleChatCompletions(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status=%d want=%d body=%s", rec.Code, http.StatusBadRequest, rec.Body.String())
	}
	if _, err := s.GetModelByModelID(context.Background(), "grok-5"); err == nil {
		t.Fatal("unexpected auto-registered model grok-5")
	}
}

func TestEnsureModelEnabled_AllowsVerifiedDynamicGrokModel(t *testing.T) {
	h, s, mini := setupValidationHandler(t)
	defer func() {
		_ = s.Close()
		mini.Close()
	}()

	if err := s.CreateModel(context.Background(), &store.Model{
		Channel:  "Grok",
		ModelID:  "grok-5",
		Name:     "grok-5",
		Status:   store.ModelStatusAvailable,
		Verified: true,
	}); err != nil {
		t.Fatalf("CreateModel() error = %v", err)
	}

	if err := h.ensureModelEnabled(context.Background(), "grok-5"); err != nil {
		t.Fatalf("ensureModelEnabled() error = %v", err)
	}
}

func TestResolveModel_AcceptsLegacyGrok43Alias(t *testing.T) {
	spec, ok := ResolveModel("grok-4.3")
	if !ok {
		t.Fatal("ResolveModel(grok-4.3) = false, want true")
	}
	if spec.ID != "grok-4.3-beta" {
		t.Fatalf("spec.ID=%q want grok-4.3-beta", spec.ID)
	}
	if spec.ConsoleModel != "grok-4.3" {
		t.Fatalf("ConsoleModel=%q want grok-4.3", spec.ConsoleModel)
	}
}

func TestEnsureModelEnabled_AcceptsLegacyGrok43StoreRecord(t *testing.T) {
	h, s, mini := setupValidationHandler(t)
	defer func() {
		_ = s.Close()
		mini.Close()
	}()

	if err := s.CreateModel(context.Background(), &store.Model{
		Channel:  "Grok",
		ModelID:  "grok-4.3",
		Name:     "Grok 4.3",
		Status:   store.ModelStatusAvailable,
		Verified: true,
	}); err != nil {
		t.Fatalf("CreateModel() error = %v", err)
	}

	if err := h.ensureModelEnabled(context.Background(), "grok-4.3"); err != nil {
		t.Fatalf("ensureModelEnabled(grok-4.3) error = %v", err)
	}
}

func TestOpenChatAccountSessionForModel_UsesGrok2APIPoolCandidates(t *testing.T) {
	h, s, mini := setupValidationHandler(t)
	defer func() {
		_ = s.Close()
		mini.Close()
	}()

	for _, acc := range []*store.Account{
		{AccountType: "grok", Enabled: true, ClientCookie: "sso=basic-token", Subscription: "basic", Weight: 1},
		{AccountType: "grok", Enabled: true, ClientCookie: "sso=super-token", Subscription: "super", Weight: 1},
		{AccountType: "grok", Enabled: true, ClientCookie: "sso=heavy-token", Subscription: "heavy", Weight: 1},
	} {
		if err := s.CreateAccount(context.Background(), acc); err != nil {
			t.Fatalf("CreateAccount() error = %v", err)
		}
	}

	superSpec, ok := ResolveModel("grok-4.20-0309")
	if !ok {
		t.Fatal("missing grok-4.20-0309 spec")
	}
	superSess, err := h.openChatAccountSessionForModel(context.Background(), superSpec)
	if err != nil {
		t.Fatalf("open super session error=%v", err)
	}
	if superSess.token != "super-token" {
		t.Fatalf("super token=%q want super-token", superSess.token)
	}
	superSess.Close()

	heavySpec, ok := ResolveModel("grok-4.20-heavy")
	if !ok {
		t.Fatal("missing grok-4.20-heavy spec")
	}
	heavySess, err := h.openChatAccountSessionForModel(context.Background(), heavySpec)
	if err != nil {
		t.Fatalf("open heavy session error=%v", err)
	}
	if heavySess.token != "heavy-token" {
		t.Fatalf("heavy token=%q want heavy-token", heavySess.token)
	}
	heavySess.Close()

	fastSpec, ok := ResolveModel("grok-4.20-fast")
	if !ok {
		t.Fatal("missing grok-4.20-fast spec")
	}
	fastSess, err := h.openChatAccountSessionForModel(context.Background(), fastSpec)
	if err != nil {
		t.Fatalf("open fast session error=%v", err)
	}
	if fastSess.token != "heavy-token" {
		t.Fatalf("prefer-best fast token=%q want heavy-token", fastSess.token)
	}
	fastSess.Close()
}

func TestTryAutoRegisterModel_VerifiesBeforeCreate(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != defaultRateLimitsPath {
			t.Fatalf("unexpected path: %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"remainingTokens":80,"totalTokens":80}`))
	}))
	defer upstream.Close()

	h, s, mini := setupValidationHandler(t)
	defer func() {
		_ = s.Close()
		mini.Close()
	}()

	h.client = New(&config.Config{GrokAPIBaseURL: upstream.URL})
	acc := &store.Account{
		AccountType:  "grok",
		Enabled:      true,
		ClientCookie: "sso=test-token",
	}
	if err := s.CreateAccount(context.Background(), acc); err != nil {
		t.Fatalf("CreateAccount() error = %v", err)
	}

	if ok := h.tryAutoRegisterModel(context.Background(), "grok-5"); !ok {
		t.Fatal("tryAutoRegisterModel() = false, want true")
	}

	m, err := s.GetModelByModelID(context.Background(), "grok-5")
	if err != nil {
		t.Fatalf("GetModelByModelID() error = %v", err)
	}
	if !m.Verified {
		t.Fatalf("verified=%v want true", m.Verified)
	}
	if !m.Status.Enabled() {
		t.Fatalf("status=%q want available", m.Status)
	}
}

func TestTryAutoRegisterModel_RejectsUnverifiedModel(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"error":{"message":"Model is not found"}}`, http.StatusBadRequest)
	}))
	defer upstream.Close()

	h, s, mini := setupValidationHandler(t)
	defer func() {
		_ = s.Close()
		mini.Close()
	}()

	h.client = New(&config.Config{GrokAPIBaseURL: upstream.URL})
	acc := &store.Account{
		AccountType:  "grok",
		Enabled:      true,
		ClientCookie: "sso=test-token",
	}
	if err := s.CreateAccount(context.Background(), acc); err != nil {
		t.Fatalf("CreateAccount() error = %v", err)
	}

	if ok := h.tryAutoRegisterModel(context.Background(), "grok-5"); ok {
		t.Fatal("tryAutoRegisterModel() = true, want false")
	}
	if _, err := s.GetModelByModelID(context.Background(), "grok-5"); err == nil {
		t.Fatal("unexpected created model grok-5")
	} else if err.Error() == "" {
		t.Fatalf("unexpected error: %v", err)
	}
}
