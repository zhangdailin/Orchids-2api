package handler

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"orchids-api/internal/store"
	"orchids-api/internal/warp"
)

func TestHandleModelByID_HidesOfflineModel(t *testing.T) {
	h, s, mini := setupModelValidationHandler(t)
	defer func() {
		_ = s.Close()
		mini.Close()
	}()

	if err := s.CreateModel(context.Background(), &store.Model{
		Channel: "Orchids",
		ModelID: "offline-only-model",
		Name:    "Offline Only",
		Status:  store.ModelStatusOffline,
	}); err != nil {
		t.Fatalf("CreateModel() error = %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "http://example.com/orchids/v1/models/offline-only-model", nil)
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

	if err := s.CreateAccount(context.Background(), &store.Account{
		AccountType:  "grok",
		ClientCookie: "sso=super-token",
		Subscription: "super",
		Enabled:      true,
	}); err != nil {
		t.Fatalf("CreateAccount() error = %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "http://example.com/grok/v1/models/grok-4.3", nil)
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
		ModelID:  "grok-5",
		Name:     "grok-5",
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

	req := httptest.NewRequest(http.MethodGet, "http://example.com/grok/v1/models/grok-5", nil)
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
	if !strings.Contains(body, "grok-4.20-0309-non-reasoning") {
		t.Fatalf("expected basic model in body=%s", body)
	}
	if !strings.Contains(body, "grok-4.20-0309-super") || !strings.Contains(body, "grok-imagine-video") {
		t.Fatalf("expected enabled grok models to remain visible regardless of pool state, body=%s", body)
	}
}

func TestHandleModels_KeepsGrokModelsVisibleWhenAccountsHaveStatusCode(t *testing.T) {
	h, s, mini := setupModelValidationHandler(t)
	defer func() {
		_ = s.Close()
		mini.Close()
	}()

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
	if !strings.Contains(body, "grok-4.3") {
		t.Fatalf("expected grok models to remain visible despite account status, body=%s", body)
	}
	if strings.Contains(body, "grok-4.3-beta") {
		t.Fatalf("expected removed beta model to stay hidden, body=%s", body)
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
	if err := warp.SaveAccountModelChoicesForAccount(ctx, s, 1, []string{"auto-open"}); err != nil {
		t.Fatalf("SaveAccountModelChoicesForAccount() error = %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "http://example.com/warp/v1/models", nil)
	rec := httptest.NewRecorder()

	h.HandleModels(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d want=%d body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, "auto-open") {
		t.Fatalf("expected free model in body=%s", body)
	}
	if strings.Contains(body, "gpt-5-2-medium") || strings.Contains(body, "gpt-5-2-high") {
		t.Fatalf("expected non-free models hidden for free-only account pool, body=%s", body)
	}
}

func TestHandleModels_WarpExhaustedPaidAccountBecomesFreeOnly(t *testing.T) {
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

	req := httptest.NewRequest(http.MethodGet, "http://example.com/warp/v1/models", nil)
	rec := httptest.NewRecorder()

	h.HandleModels(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d want=%d body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, "auto-open") {
		t.Fatalf("expected free model in body=%s", body)
	}
	if strings.Contains(body, "gpt-5-2-medium") {
		t.Fatalf("expected paid model hidden for exhausted paid account, body=%s", body)
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
	if err := warp.SaveAccountModelChoicesForAccount(ctx, s, 1, []string{"auto-open"}); err != nil {
		t.Fatalf("SaveAccountModelChoicesForAccount() error = %v", err)
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
