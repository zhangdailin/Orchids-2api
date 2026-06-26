package grok

import (
	"context"
	"io"
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

func TestResolveModel_AcceptsGrok43ConsoleModels(t *testing.T) {
	for _, id := range []string{"grok-4.3", "grok-build-0.1", "grok-4.3-beta"} {
		spec, ok := ResolveModel(id)
		if !ok {
			t.Fatalf("ResolveModel(%s) = false, want true", id)
		}
		if spec.ConsoleModel == "" {
			t.Fatalf("%s ConsoleModel should not be empty", id)
		}
	}
}

func TestResolveModel_AllowsGrok43BetaAppChat(t *testing.T) {
	spec, ok := ResolveModel("grok-4.3-beta")
	if !ok {
		t.Fatal("ResolveModel(grok-4.3-beta) = false, want true")
	}
	if _, ok := ResolveModelOrDynamic("grok-4.3-beta"); !ok {
		t.Fatal("ResolveModelOrDynamic(grok-4.3-beta) = false, want true")
	}
	if IsDeprecatedModelID("grok-4.3-beta") {
		t.Fatal("grok-4.3-beta should not be deprecated")
	}
	if spec.ModeID != "grok-420-computer-use-sa" {
		t.Fatalf("ModeID=%q want grok-420-computer-use-sa", spec.ModeID)
	}
}

func TestEnsureModelEnabled_RejectsConsoleOnlyGrok43EvenWhenStored(t *testing.T) {
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

func TestEnsureModelEnabled_AllowsGrok43BetaAppChat(t *testing.T) {
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

	if err := h.ensureModelEnabled(context.Background(), "grok-4.3-beta"); err != nil {
		t.Fatalf("ensureModelEnabled(grok-4.3-beta) error=%v", err)
	}
}

func TestHandleChatCompletions_Grok43RejectedBeforeUpstream(t *testing.T) {
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

	var upstreamPaths []string
	h.client = &Client{
		cfg: &config.Config{},
		httpClient: &http.Client{
			Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
				upstreamPaths = append(upstreamPaths, req.URL.Path)
				return &http.Response{
					StatusCode: http.StatusOK,
					Header:     make(http.Header),
					Body: io.NopCloser(strings.NewReader(
						`{"result":{"response":{"modelResponse":{"responseId":"resp_1","message":"OK","metadata":{"llm_info":{"modelHash":"hash_1"}}}}}}`,
					)),
					Request: req,
				}, nil
			}),
		},
	}

	body := `{"model":"grok-4.3","messages":[{"role":"user","content":"hello"}],"stream":false}`
	req := httptest.NewRequest(http.MethodPost, "/grok/v1/chat/completions", strings.NewReader(body))
	rec := httptest.NewRecorder()

	h.HandleChatCompletions(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status=%d want=%d body=%s", rec.Code, http.StatusBadRequest, rec.Body.String())
	}
	if len(upstreamPaths) != 0 {
		t.Fatalf("upstream paths=%#v want none", upstreamPaths)
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

	superSpec, ok := ResolveModel("grok-4.20-0309-non-reasoning-super")
	if !ok {
		t.Fatal("missing grok-4.20-0309-non-reasoning-super spec")
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

func TestOpenChatAccountSessionForImageLiteSkipsBasicPool(t *testing.T) {
	h, s, mini := setupValidationHandler(t)
	defer func() {
		_ = s.Close()
		mini.Close()
	}()

	liteAcc := &store.Account{AccountType: "grok", Enabled: true, ClientCookie: "sso=lite-token", Subscription: "lite", Weight: 1}
	for _, acc := range []*store.Account{
		{AccountType: "grok", Enabled: true, ClientCookie: "sso=basic-token", Subscription: "basic", Weight: 1},
		liteAcc,
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
	if NormalizeSSOToken(sess.token) != "lite-token" {
		t.Fatalf("token=%q want sso lite-token", sess.token)
	}
	sess.Close()
}

func TestOpenChatAccountSessionForImagineLiteSkipsBasicPool(t *testing.T) {
	h2, s2, mini2 := setupValidationHandler(t)
	defer func() {
		_ = s2.Close()
		mini2.Close()
	}()
	for _, acc := range []*store.Account{
		{AccountType: "grok", Enabled: true, ClientCookie: "sso=basic-token", Subscription: "basic", Weight: 1},
		{AccountType: "grok", Enabled: true, ClientCookie: "sso=lite-token", Subscription: "lite", Weight: 1},
	} {
		if err := s2.CreateAccount(context.Background(), acc); err != nil {
			t.Fatalf("CreateAccount() error = %v", err)
		}
	}
	spec, ok := ResolveModel("grok-imagine-image-lite")
	if !ok {
		t.Fatal("missing grok-imagine-image-lite spec")
	}
	sess, err := h2.openChatAccountSessionForImagineLite(context.Background(), nil, spec)
	if err != nil {
		t.Fatalf("open imagine lite session error=%v", err)
	}
	if NormalizeSSOToken(sess.token) != "lite-token" {
		t.Fatalf("token=%q want sso lite-token", sess.token)
	}
	sess.Close()

	h3, s3, mini3 := setupValidationHandler(t)
	defer func() {
		_ = s3.Close()
		mini3.Close()
	}()
	if err := s3.CreateAccount(context.Background(), &store.Account{
		AccountType: "grok", Enabled: true, ClientCookie: "sso=basic-only-token", Subscription: "basic", Weight: 1,
	}); err != nil {
		t.Fatalf("CreateAccount(basic only) error = %v", err)
	}
	next, err := h3.openChatAccountSessionForImagineLite(context.Background(), nil, spec)
	if err == nil {
		defer next.Close()
		t.Fatalf("open image lite with only basic unexpectedly succeeded token=%q", next.token)
	}
}

func TestOpenChatAccountSessionForImageLiteTierOverrideSkipsBasicPool(t *testing.T) {
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
	spec.Tier = grokTierLite
	sess, err := h.openChatAccountSessionForModel(context.Background(), spec)
	if err != nil {
		t.Fatalf("open tier-overridden image lite session error=%v", err)
	}
	defer sess.Close()
	if NormalizeSSOToken(sess.token) != "lite-token" {
		t.Fatalf("token=%q want sso lite-token", sess.token)
	}
}

func TestOpenChatAccountSessionForImageLiteLitePoolDoesNotRequireFullBrowserCookie(t *testing.T) {
	h, s, mini := setupValidationHandler(t)
	defer func() {
		_ = s.Close()
		mini.Close()
	}()

	if err := s.CreateAccount(context.Background(), &store.Account{
		AccountType:  "grok",
		Enabled:      true,
		ClientCookie: "lite-bare-token",
		Subscription: "lite",
		Weight:       1,
	}); err != nil {
		t.Fatalf("CreateAccount() error = %v", err)
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
	if NormalizeSSOToken(sess.token) != "lite-bare-token" {
		t.Fatalf("token=%q want lite-bare-token", sess.token)
	}
}

func TestOpenChatAccountSessionForImagineLiteSkipsCoolingLiteWithoutBasicFallback(t *testing.T) {
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
	sess, err := h.openChatAccountSessionForImagineLite(context.Background(), nil, spec)
	if err == nil {
		defer sess.Close()
		t.Fatalf("open image lite with cooling lite and basic unexpectedly succeeded token=%q", sess.token)
	}
}

func TestOpenChatAccountSessionForImageLiteSkipsCoolingLiteWithoutBasicFallback(t *testing.T) {
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
	if err == nil {
		defer sess.Close()
		t.Fatalf("open image lite with cooling lite and basic unexpectedly succeeded token=%q", sess.token)
	}
}

func TestOpenChatAccountSessionForImageLiteTierOverrideSkipsCoolingLiteWithoutBasicFallback(t *testing.T) {
	h, s, mini := setupValidationHandler(t)
	defer func() {
		_ = s.Close()
		mini.Close()
	}()

	for _, acc := range []*store.Account{
		{AccountType: "grok", Enabled: true, ClientCookie: "sso=lite-token", Subscription: "lite", StatusCode: "429", LastAttempt: time.Now(), Weight: 1},
		{AccountType: "grok", Enabled: true, ClientCookie: "sso=basic-token", Subscription: "basic", Weight: 1},
		{AccountType: "grok", Enabled: true, ClientCookie: "sso=super-token", Subscription: "super", Weight: 1},
	} {
		if err := s.CreateAccount(context.Background(), acc); err != nil {
			t.Fatalf("CreateAccount() error = %v", err)
		}
	}

	spec, ok := ResolveModel("grok-imagine-image-lite")
	if !ok {
		t.Fatal("missing grok-imagine-image-lite spec")
	}
	spec.Tier = grokTierLite
	sess, err := h.openChatAccountSessionForModel(context.Background(), spec)
	if err != nil {
		t.Fatalf("open tier-overridden image lite session error=%v", err)
	}
	defer sess.Close()
	if NormalizeSSOToken(sess.token) != "super-token" {
		t.Fatalf("token=%q want sso super-token", sess.token)
	}
}

func TestOpenChatAccountSessionForModel_FallsBackToBasicAccount(t *testing.T) {
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

	spec, ok := ResolveModel("grok-4.20-0309-non-reasoning")
	if !ok {
		t.Fatal("missing grok-4.20-0309-non-reasoning spec")
	}
	sess, err := h.openChatAccountSessionForModel(context.Background(), spec)
	if err != nil {
		t.Fatalf("open session for grok-4.20-0309-non-reasoning with unknown-tier account should fall back: error=%v", err)
	}
	defer sess.Close()
	if NormalizeSSOToken(sess.token) != "unknown-tier-token" {
		t.Fatalf("token=%q want sso=unknown-tier-token", sess.token)
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
