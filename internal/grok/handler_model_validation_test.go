package grok

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

func TestEnsureModelEnabled_PrefersGrokChannelWhenModelIDExistsInOtherProvider(t *testing.T) {
	h, s, mini := setupValidationHandler(t)
	defer func() {
		_ = s.Close()
		mini.Close()
	}()

	if err := s.CreateModel(context.Background(), &store.Model{
		Channel:  "Puter",
		ModelID:  "grok-shared-id",
		Name:     "Puter shared",
		Status:   store.ModelStatusAvailable,
		Verified: true,
	}); err != nil {
		t.Fatalf("CreateModel(puter) error = %v", err)
	}
	if err := s.CreateModel(context.Background(), &store.Model{
		Channel:  "Grok",
		ModelID:  "grok-shared-id",
		Name:     "Grok shared",
		Status:   store.ModelStatusAvailable,
		Verified: true,
	}); err != nil {
		t.Fatalf("CreateModel(grok) error = %v", err)
	}

	if err := h.ensureModelEnabled(context.Background(), "grok-shared-id"); err != nil {
		t.Fatalf("ensureModelEnabled() error = %v", err)
	}
}

func TestResolveModel_AcceptsOfficialGrok43(t *testing.T) {
	spec, ok := ResolveModel("grok-4.3")
	if !ok {
		t.Fatal("ResolveModel(grok-4.3) = false, want true")
	}
	if spec.ID != "grok-4.3" {
		t.Fatalf("spec.ID=%q want grok-4.3", spec.ID)
	}
	if spec.ConsoleModel != "grok-4.3" {
		t.Fatalf("ConsoleModel=%q want grok-4.3", spec.ConsoleModel)
	}
}

func TestResolveModel_RejectsRemovedGrok43Beta(t *testing.T) {
	if _, ok := ResolveModel("grok-4.3-beta"); ok {
		t.Fatal("ResolveModel(grok-4.3-beta) = true, want false")
	}
	if _, ok := ResolveModelOrDynamic("grok-4.3-beta"); ok {
		t.Fatal("ResolveModelOrDynamic(grok-4.3-beta) = true, want false")
	}
	if !IsDeprecatedModelID("grok-4.3-beta") {
		t.Fatal("grok-4.3-beta should be deprecated")
	}
}

func TestEnsureModelEnabled_AcceptsOfficialGrok43StoreRecord(t *testing.T) {
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

func TestEnsureModelEnabled_RejectsRemovedGrok43BetaEvenWhenStored(t *testing.T) {
	h, s, mini := setupValidationHandler(t)
	defer func() {
		_ = s.Close()
		mini.Close()
	}()

	if err := s.CreateModel(context.Background(), &store.Model{
		Channel:  "Grok",
		ModelID:  "grok-4.3-beta",
		Name:     "Grok 4.3 Beta",
		Status:   store.ModelStatusAvailable,
		Verified: true,
	}); err != nil {
		t.Fatalf("CreateModel(beta) error = %v", err)
	}

	if err := h.ensureModelEnabled(context.Background(), "grok-4.3-beta"); err == nil {
		t.Fatal("ensureModelEnabled(grok-4.3-beta) succeeded, want error")
	}
}

func TestHandleChatCompletions_Grok43NeverFallsBackToAppChat(t *testing.T) {
	h, s, mini := setupValidationHandler(t)
	defer func() {
		_ = s.Close()
		mini.Close()
	}()

	if err := s.CreateAccount(context.Background(), &store.Account{
		AccountType:  "grok",
		ClientCookie: "sso=super-token",
		Subscription: "super",
		Enabled:      true,
	}); err != nil {
		t.Fatalf("CreateAccount() error = %v", err)
	}

	body := `{"model":"grok-4.3","messages":[{"role":"user","content":[{"type":"text","text":"hello"},{"type":"image_url","image_url":{"url":"data:image/png;base64,aGVsbG8="}}]}],"stream":false}`
	req := httptest.NewRequest(http.MethodPost, "/grok/v1/chat/completions", strings.NewReader(body))
	rec := httptest.NewRecorder()

	h.HandleChatCompletions(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status=%d want=%d body=%s", rec.Code, http.StatusBadRequest, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "console.x.ai") {
		t.Fatalf("body=%q want console.x.ai guidance", rec.Body.String())
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
		{AccountType: "grok", Enabled: true, ClientCookie: "sso=lite-token", Subscription: "lite", Weight: 1},
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
	if NormalizeSSOToken(superSess.token) != "super-token" {
		t.Fatalf("super token=%q want sso super-token", superSess.token)
	}
	superSess.Close()

	liteSpec := ModelSpec{ID: "grok-lite-test", Tier: grokTierLite}
	liteSess, err := h.openChatAccountSessionForModel(context.Background(), liteSpec)
	if err != nil {
		t.Fatalf("open lite session error=%v", err)
	}
	if NormalizeSSOToken(liteSess.token) != "lite-token" {
		t.Fatalf("lite token=%q want sso lite-token", liteSess.token)
	}
	liteSess.Close()

	heavySpec, ok := ResolveModel("grok-4.20-heavy")
	if !ok {
		t.Fatal("missing grok-4.20-heavy spec")
	}
	heavySess, err := h.openChatAccountSessionForModel(context.Background(), heavySpec)
	if err != nil {
		t.Fatalf("open heavy session error=%v", err)
	}
	if NormalizeSSOToken(heavySess.token) != "heavy-token" {
		t.Fatalf("heavy token=%q want sso heavy-token", heavySess.token)
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
	if NormalizeSSOToken(fastSess.token) != "heavy-token" {
		t.Fatalf("prefer-best fast token=%q want sso heavy-token", fastSess.token)
	}
	fastSess.Close()
}

func TestOpenChatAccountSessionForImageLitePrefersBasicPool(t *testing.T) {
	h, s, mini := setupValidationHandler(t)
	defer func() {
		_ = s.Close()
		mini.Close()
	}()

	for _, acc := range []*store.Account{
		{AccountType: "grok", Enabled: true, ClientCookie: "sso=basic-token", Subscription: "basic", Weight: 1},
		{AccountType: "grok", Enabled: true, ClientCookie: "sso=lite-token", Subscription: "lite", Weight: 1},
	} {
		if err := s.CreateAccount(context.Background(), acc); err != nil {
			t.Fatalf("CreateAccount() error = %v", err)
		}
	}

	spec, ok := ResolveModel("grok-imagine-image-lite")
	if !ok {
		t.Fatal("missing grok-imagine-image-lite spec")
	}
	sess, err := h.openChatAccountSessionForModel(context.Background(), spec)
	if err != nil {
		t.Fatalf("open image lite session error=%v", err)
	}
	if NormalizeSSOToken(sess.token) != "basic-token" {
		t.Fatalf("token=%q want sso basic-token", sess.token)
	}
	basicID := sess.acc.ID
	sess.Close()

	next, err := h.openChatAccountSessionForModelExcluding(context.Background(), []int64{basicID}, spec)
	if err != nil {
		t.Fatalf("open non-basic image lite session error=%v", err)
	}
	defer next.Close()
	if NormalizeSSOToken(next.token) != "lite-token" {
		t.Fatalf("fallback token=%q want sso lite-token", next.token)
	}
}

func TestOpenAppChatImageAccountSessionRequiresFullBrowserCookie(t *testing.T) {
	h, s, mini := setupValidationHandler(t)
	defer func() {
		_ = s.Close()
		mini.Close()
	}()

	for _, acc := range []*store.Account{
		{AccountType: "grok", Enabled: true, ClientCookie: "basic-bare-token", Subscription: "basic", Weight: 1},
		{AccountType: "grok", Enabled: true, ClientCookie: "sso=basic-full-token; sso-rw=basic-full-token; x-userid=user-1; cf_clearance=cf-1; __cf_bm=bm-1", Subscription: "basic", Weight: 1},
	} {
		if err := s.CreateAccount(context.Background(), acc); err != nil {
			t.Fatalf("CreateAccount() error = %v", err)
		}
	}

	spec, ok := ResolveModel("grok-imagine-image-lite")
	if !ok {
		t.Fatal("missing grok-imagine-image-lite spec")
	}
	sess, err := h.openAppChatImageAccountSessionForModelExcluding(context.Background(), nil, spec)
	if err != nil {
		t.Fatalf("open app-chat image session error=%v", err)
	}
	defer sess.Close()
	if NormalizeSSOToken(sess.token) != "basic-full-token" {
		t.Fatalf("token=%q want sso basic-full-token", sess.token)
	}
}

func TestOpenChatAccountSessionForImageLiteSkipsCoolingLitePool(t *testing.T) {
	h, s, mini := setupValidationHandler(t)
	defer func() {
		_ = s.Close()
		mini.Close()
	}()

	for _, acc := range []*store.Account{
		{AccountType: "grok", Enabled: true, ClientCookie: "sso=lite-token", Subscription: "lite", StatusCode: "429", LastAttempt: time.Now(), Weight: 1},
		{AccountType: "grok", Enabled: true, ClientCookie: "sso=basic-token", Subscription: "basic", Weight: 1},
	} {
		if err := s.CreateAccount(context.Background(), acc); err != nil {
			t.Fatalf("CreateAccount() error = %v", err)
		}
	}

	spec, ok := ResolveModel("grok-imagine-image-lite")
	if !ok {
		t.Fatal("missing grok-imagine-image-lite spec")
	}
	sess, err := h.openChatAccountSessionForModel(context.Background(), spec)
	if err != nil {
		t.Fatalf("open image lite session error=%v", err)
	}
	defer sess.Close()
	if NormalizeSSOToken(sess.token) != "basic-token" {
		t.Fatalf("token=%q want sso basic-token", sess.token)
	}
}

func TestOpenChatAccountSessionForModel_FallsBackWhenPoolMetadataMissing(t *testing.T) {
	h, s, mini := setupValidationHandler(t)
	defer func() {
		_ = s.Close()
		mini.Close()
	}()

	if err := s.CreateAccount(context.Background(), &store.Account{
		AccountType:  "grok",
		Enabled:      true,
		ClientCookie: "sso=unknown-tier-token",
		Weight:       1,
	}); err != nil {
		t.Fatalf("CreateAccount() error = %v", err)
	}

	spec, ok := ResolveModel("grok-4.3")
	if !ok {
		t.Fatal("missing grok-4.3 spec")
	}
	sess, err := h.openChatAccountSessionForModel(context.Background(), spec)
	if err != nil {
		t.Fatalf("open session error=%v", err)
	}
	defer sess.Close()
	if NormalizeSSOToken(sess.token) != "unknown-tier-token" {
		t.Fatalf("token=%q want sso unknown-tier-token", sess.token)
	}
}

func TestHandleChatCompletions_DoesNotProbeMissingModel(t *testing.T) {
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
		t.Fatal("unexpected created model grok-5")
	} else if err.Error() == "" {
		t.Fatalf("unexpected error: %v", err)
	}
}
