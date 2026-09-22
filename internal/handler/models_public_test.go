package handler

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"orchids-api/internal/middleware"
	"orchids-api/internal/store"
	"orchids-api/internal/warp"
)

func TestHandleModels_FiltersAPIKeyModelAllowlist(t *testing.T) {
	h, s, mini := setupModelValidationHandler(t)
	defer func() {
		_ = s.Close()
		mini.Close()
	}()

	// The catalog is published explicitly: the store starts empty now.
	publishModel(t, s,
		&store.Model{Channel: "Grok", ModelID: "grok-4.6"},
		&store.Model{Channel: "Grok", ModelID: "grok-4.5"},
		&store.Model{Channel: "Grok", ModelID: "grok-imagine-image"},
	)

	wrapper := middleware.APIKeyAuthWithRequest(
		func(*http.Request) bool { return true },
		func(context.Context, string) (*middleware.APIKeyPrincipal, error) {
			return &middleware.APIKeyPrincipal{AllowedModels: []string{"grok-4.6"}}, nil
		},
		h.HandleModels,
	)
	req := httptest.NewRequest(http.MethodGet, "http://example.com/grok/v1/models", nil)
	req.Header.Set("Authorization", "Bearer sk-test")
	rec := httptest.NewRecorder()
	wrapper(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, "grok-4.6") || strings.Contains(body, "grok-4.5") || strings.Contains(body, "grok-imagine-image") {
		t.Fatalf("unexpected filtered models: %s", body)
	}
}

func TestHandleModelByID_HidesOfflineModel(t *testing.T) {
	h, s, mini := setupModelValidationHandler(t)
	defer func() {
		_ = s.Close()
		mini.Close()
	}()

	if err := s.CreateModel(context.Background(), &store.Model{
		Channel: "Warp",
		ModelID: "offline-only-model",
		Name:    "Offline Only",
		Status:  store.ModelStatusOffline,
	}); err != nil {
		t.Fatalf("CreateModel() error = %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "http://example.com/warp/v1/models/offline-only-model", nil)
	rec := httptest.NewRecorder()

	h.HandleModelByID(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status=%d want=%d body=%s", rec.Code, http.StatusNotFound, rec.Body.String())
	}
}

func TestHandleModelByID_HidesUnsupportedGrokModel(t *testing.T) {
	h, s, mini := setupModelValidationHandler(t)
	defer func() {
		_ = s.Close()
		mini.Close()
	}()

	req := httptest.NewRequest(http.MethodGet, "http://example.com/grok/v1/models/grok-4.1", nil)
	rec := httptest.NewRecorder()

	h.HandleModelByID(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status=%d want=%d body=%s", rec.Code, http.StatusNotFound, rec.Body.String())
	}
}

func TestHandleModelByID_ReturnsVisibleModel(t *testing.T) {
	h, s, mini := setupModelValidationHandler(t)
	defer func() {
		_ = s.Close()
		mini.Close()
	}()

	publishModel(t, s, &store.Model{Channel: "Grok", ModelID: "grok-4.5"})
	if err := s.CreateAccount(context.Background(), &store.Account{
		AccountType:  "grok",
		ClientCookie: "sso=super-token",
		Subscription: "super",
		Enabled:      true,
	}); err != nil {
		t.Fatalf("CreateAccount() error = %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "http://example.com/grok/v1/models/grok-4.5", nil)
	rec := httptest.NewRecorder()

	h.HandleModelByID(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d want=%d body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}
}

func TestHandleModelByID_ReturnsVerifiedDynamicGrokModel(t *testing.T) {
	h, s, mini := setupModelValidationHandler(t)
	defer func() {
		_ = s.Close()
		mini.Close()
	}()

	if err := s.CreateModel(context.Background(), &store.Model{
		Channel:  "Grok",
		ModelID:  "grok-future-6",
		Name:     "grok-future-6",
		Status:   store.ModelStatusAvailable,
		Verified: true,
	}); err != nil {
		t.Fatalf("CreateModel() error = %v", err)
	}
	if err := s.CreateAccount(context.Background(), &store.Account{
		AccountType:  "grok",
		ClientCookie: "sso=basic-token",
		Subscription: "basic",
		Enabled:      true,
	}); err != nil {
		t.Fatalf("CreateAccount() error = %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "http://example.com/grok/v1/models/grok-future-6", nil)
	rec := httptest.NewRecorder()

	h.HandleModelByID(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d want=%d body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}
}

func TestHandleModels_KeepsGrokModelsVisibleWhenOnlyBasicPoolExists(t *testing.T) {
	h, s, mini := setupModelValidationHandler(t)
	defer func() {
		_ = s.Close()
		mini.Close()
	}()

	publishModel(t, s,
		&store.Model{Channel: "Grok", ModelID: "grok-4.5"},
		&store.Model{Channel: "Grok", ModelID: "grok-imagine-image"},
		&store.Model{Channel: "Grok", ModelID: "grok-imagine-video"},
	)
	if err := s.CreateAccount(context.Background(), &store.Account{
		AccountType:  "grok",
		ClientCookie: "sso=basic-token",
		Subscription: "basic",
		Enabled:      true,
	}); err != nil {
		t.Fatalf("CreateAccount() error = %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "http://example.com/grok/v1/models", nil)
	rec := httptest.NewRecorder()

	h.HandleModels(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d want=%d body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, "grok-4.5") {
		t.Fatalf("expected current chat model in body=%s", body)
	}
	if !strings.Contains(body, "grok-imagine-image") || !strings.Contains(body, "grok-imagine-video") {
		t.Fatalf("expected enabled grok models to remain visible regardless of pool state, body=%s", body)
	}
}

func TestHandleModels_KeepsGrokModelsVisibleWhenAccountsHaveStatusCode(t *testing.T) {
	h, s, mini := setupModelValidationHandler(t)
	defer func() {
		_ = s.Close()
		mini.Close()
	}()

	// The withdrawn identifiers are published too, so "stays hidden" is proven
	// against the deprecated-name rule rather than against an empty store.
	publishModel(t, s,
		&store.Model{Channel: "Grok", ModelID: "grok-4.5"},
		&store.Model{Channel: "Grok", ModelID: "grok-imagine-image"},
		&store.Model{Channel: "Grok", ModelID: "grok-4.20-0309-non-reasoning"},
		&store.Model{Channel: "Grok", ModelID: "grok-4.3-beta"},
		&store.Model{Channel: "Grok", ModelID: "grok-build-0.1"},
	)
	if err := s.CreateAccount(context.Background(), &store.Account{
		AccountType:  "grok",
		ClientCookie: "sso=super-token",
		Subscription: "super",
		Enabled:      true,
		StatusCode:   "500",
	}); err != nil {
		t.Fatalf("CreateAccount() error = %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "http://example.com/grok/v1/models", nil)
	rec := httptest.NewRecorder()

	h.HandleModels(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d want=%d body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, "grok-4.5") {
		t.Fatalf("expected grok models to remain visible despite account status, body=%s", body)
	}
	if !strings.Contains(body, "grok-imagine-image") {
		t.Fatalf("expected current image model to remain visible, body=%s", body)
	}
	for _, hidden := range []string{"grok-4.20-0309-non-reasoning", "grok-4.3-beta", "grok-4.3", "grok-build-0.1"} {
		if strings.Contains(body, `"id":"`+hidden+`"`) {
			t.Fatalf("expected removed model %s to stay hidden, body=%s", hidden, body)
		}
	}
}

func TestHandleModels_WarpUsesAccountModelPool(t *testing.T) {
	h, s, mini := setupModelValidationHandler(t)
	defer func() {
		_ = s.Close()
		mini.Close()
	}()

	ctx := context.Background()
	if err := s.CreateAccount(ctx, &store.Account{
		AccountType:  "warp",
		RefreshToken: "warp-free-token",
		Subscription: "free",
		Enabled:      true,
	}); err != nil {
		t.Fatalf("CreateAccount() error = %v", err)
	}
	if err := warp.SaveAccountModelChoices(ctx, s, &warp.AccountModelChoices{Accounts: map[string][]string{"1": {"auto-open"}}}); err != nil {
		t.Fatalf("SaveAccountModelChoices() error = %v", err)
	}
	publishModel(t, s,
		&store.Model{Channel: "Warp", ModelID: "auto-open"},
		&store.Model{Channel: "Warp", ModelID: "gpt-5-2-medium"},
		&store.Model{Channel: "Warp", ModelID: "gpt-5-2-high"},
	)

	req := httptest.NewRequest(http.MethodGet, "http://example.com/warp/v1/models", nil)
	rec := httptest.NewRecorder()

	h.HandleModels(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d want=%d body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, "auto-open") {
		t.Fatalf("expected upstream free model in body=%s", body)
	}
	if strings.Contains(body, "gpt-5-2-medium") || strings.Contains(body, "gpt-5-2-high") {
		t.Fatalf("expected non-free models hidden for free-only account pool, body=%s", body)
	}
}

func TestHandleModelByID_WarpRejectsModelOutsideAccountPool(t *testing.T) {
	h, s, mini := setupModelValidationHandler(t)
	defer func() {
		_ = s.Close()
		mini.Close()
	}()

	ctx := context.Background()
	if err := s.CreateAccount(ctx, &store.Account{
		AccountType:  "warp",
		RefreshToken: "warp-free-token",
		Subscription: "free",
		Enabled:      true,
	}); err != nil {
		t.Fatalf("CreateAccount() error = %v", err)
	}
	if err := warp.SaveAccountModelChoices(ctx, s, &warp.AccountModelChoices{Accounts: map[string][]string{"1": {"auto-open"}}}); err != nil {
		t.Fatalf("SaveAccountModelChoices() error = %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "http://example.com/warp/v1/models/gpt-5-2-medium", nil)
	rec := httptest.NewRecorder()

	h.HandleModelByID(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status=%d want=%d body=%s", rec.Code, http.StatusNotFound, rec.Body.String())
	}
}

func TestHandleModelByID_ReturnsGrokModelWithoutRequiredPool(t *testing.T) {
	h, s, mini := setupModelValidationHandler(t)
	defer func() {
		_ = s.Close()
		mini.Close()
	}()

	publishModel(t, s, &store.Model{Channel: "Grok", ModelID: "grok-imagine-video"})
	if err := s.CreateAccount(context.Background(), &store.Account{
		AccountType:  "grok",
		ClientCookie: "sso=basic-token",
		Subscription: "basic",
		Enabled:      true,
	}); err != nil {
		t.Fatalf("CreateAccount() error = %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "http://example.com/grok/v1/models/grok-imagine-video", nil)
	rec := httptest.NewRecorder()

	h.HandleModelByID(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d want=%d body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}
}
