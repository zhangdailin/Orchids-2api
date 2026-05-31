package api

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"

	"github.com/alicebob/miniredis/v2"
	"github.com/goccy/go-json"

	"orchids-api/internal/config"
	"orchids-api/internal/orchids"
	"orchids-api/internal/store"
	"orchids-api/internal/warp"
)

func TestRefreshAccountState_GrokSyncsRemainingQuota(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/rest/rate-limits" {
			t.Fatalf("unexpected path: %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"remainingQueries": 0,
			"totalQueries":     0,
			"remainingTokens":  80,
			"totalTokens":      80,
		})
	}))
	defer srv.Close()

	cfg := &config.Config{GrokAPIBaseURL: srv.URL}
	a := New(nil, "", "", cfg)
	acc := &store.Account{
		ID:           1,
		AccountType:  "grok",
		ClientCookie: "token-abc",
		AgentMode:    "grok-4.20-0309",
	}

	status, httpStatus, err := a.refreshAccountState(context.Background(), acc)
	if err != nil {
		t.Fatalf("refreshAccountState() error: %v", err)
	}
	if status != "" || httpStatus != 0 {
		t.Fatalf("unexpected status=%q httpStatus=%d", status, httpStatus)
	}
	if acc.UsageCurrent != 80 || acc.UsageLimit != 80 {
		t.Fatalf("unexpected quota current=%v limit=%v", acc.UsageCurrent, acc.UsageLimit)
	}
}

func TestRefreshAccountState_GrokQuotaIgnoresStaleAgentMode(t *testing.T) {
	t.Parallel()

	var requestedModels []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/rest/rate-limits" {
			t.Fatalf("unexpected path: %s", r.URL.Path)
		}
		var payload map[string]interface{}
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Fatalf("decode payload: %v", err)
		}
		model, _ := payload["modelName"].(string)
		requestedModels = append(requestedModels, model)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"remainingQueries": 7,
			"totalQueries":     7,
		})
	}))
	defer srv.Close()

	a := New(nil, "", "", &config.Config{GrokAPIBaseURL: srv.URL})
	acc := &store.Account{
		ID:           23,
		AccountType:  "grok",
		ClientCookie: "token-abc",
		AgentMode:    "grok-3",
		Subscription: "basic",
		UsageCurrent: 30,
		UsageLimit:   30,
	}

	status, httpStatus, err := a.refreshAccountState(context.Background(), acc)
	if err != nil {
		t.Fatalf("refreshAccountState() error: %v", err)
	}
	if status != "" || httpStatus != 0 {
		t.Fatalf("unexpected status=%q httpStatus=%d", status, httpStatus)
	}
	if len(requestedModels) != 1 || requestedModels[0] != "auto" {
		t.Fatalf("requestedModels=%v want [auto]", requestedModels)
	}
	if acc.AgentMode != "grok-3" {
		t.Fatalf("AgentMode=%q want grok-3", acc.AgentMode)
	}
	if acc.UsageCurrent != 7 || acc.UsageLimit != 7 {
		t.Fatalf("unexpected quota current=%v limit=%v", acc.UsageCurrent, acc.UsageLimit)
	}
}

func TestBuildQuotaResponseFields_WarpSplitQuota(t *testing.T) {
	t.Parallel()

	acc := &store.Account{
		AccountType:          "warp",
		UsageCurrent:         1429,
		UsageLimit:           1550,
		WarpMonthlyLimit:     1550,
		WarpMonthlyRemaining: 121,
		WarpBonusRemaining:   1000,
	}

	fields := buildQuotaResponseFields(acc)
	if got := fields["quota_limit"].(float64); got != 1550 {
		t.Fatalf("quota_limit=%v want 1550", got)
	}
	if got := fields["quota_remaining"].(float64); got != 1121 {
		t.Fatalf("quota_remaining=%v want 1121", got)
	}
	if got := fields["quota_base_remaining"].(float64); got != 121 {
		t.Fatalf("quota_base_remaining=%v want 121", got)
	}
	if got := fields["quota_bonus_remaining"].(float64); got != 1000 {
		t.Fatalf("quota_bonus_remaining=%v want 1000", got)
	}
	if got := fields["quota_mode"].(string); got != "warp_split" {
		t.Fatalf("quota_mode=%q want warp_split", got)
	}
}

func TestRefreshAccountState_GrokMissingToken(t *testing.T) {
	t.Parallel()

	a := New(nil, "", "", &config.Config{})
	acc := &store.Account{AccountType: "grok"}

	status, httpStatus, err := a.refreshAccountState(context.Background(), acc)
	if err == nil {
		t.Fatalf("expected error")
	}
	if status != "" {
		t.Fatalf("unexpected status=%q", status)
	}
	if httpStatus != http.StatusBadRequest {
		t.Fatalf("httpStatus=%d want=%d", httpStatus, http.StatusBadRequest)
	}
}

func TestRefreshAccountState_OrchidsUsesTokenGetterAndCreditsFetcher(t *testing.T) {
	prevGetToken := orchidsGetAccountToken
	prevFetchCredits := orchidsFetchCredits
	t.Cleanup(func() {
		orchidsGetAccountToken = prevGetToken
		orchidsFetchCredits = prevFetchCredits
	})

	orchidsGetAccountToken = func(acc *store.Account, cfg *config.Config) (string, error) {
		if acc.ClientCookie != "client-cookie" {
			t.Fatalf("unexpected client cookie: %q", acc.ClientCookie)
		}
		return "header." + encodeJWTClaims(`{"sid":"sess_123","sub":"user_123","exp":4102444800}`) + ".sig", nil
	}
	orchidsFetchCredits = func(ctx context.Context, sessionJWT string, userID string, proxyFunc func(*http.Request) (*url.URL, error)) (*orchids.CreditsInfo, error) {
		if userID != "user_123" {
			t.Fatalf("userID=%q want user_123", userID)
		}
		return &orchids.CreditsInfo{Credits: 42, Plan: "PRO"}, nil
	}

	a := New(nil, "", "", &config.Config{})
	acc := &store.Account{AccountType: "orchids", ClientCookie: "client-cookie"}

	status, httpStatus, err := a.refreshAccountState(context.Background(), acc)
	if err != nil {
		t.Fatalf("refreshAccountState() error: %v", err)
	}
	if status != "" || httpStatus != 0 {
		t.Fatalf("unexpected status=%q httpStatus=%d", status, httpStatus)
	}
	if acc.SessionID != "sess_123" {
		t.Fatalf("SessionID=%q want sess_123", acc.SessionID)
	}
	if acc.UserID != "user_123" {
		t.Fatalf("UserID=%q want user_123", acc.UserID)
	}
	if acc.Subscription != "pro" || acc.UsageCurrent != 42 || acc.UsageLimit != orchids.PlanCreditLimit("PRO") {
		t.Fatalf("unexpected orchids quota sync: subscription=%q current=%v limit=%v", acc.Subscription, acc.UsageCurrent, acc.UsageLimit)
	}
}

func TestRefreshAccountState_OrchidsCreditsServerActionMismatchStillSucceeds(t *testing.T) {
	prevGetToken := orchidsGetAccountToken
	prevFetchCredits := orchidsFetchCredits
	t.Cleanup(func() {
		orchidsGetAccountToken = prevGetToken
		orchidsFetchCredits = prevFetchCredits
	})

	sessionJWT := "header." + encodeJWTClaims(`{"sid":"sess_refresh_ok","sub":"user_refresh_ok","exp":4102444800}`) + ".sig"
	orchidsGetAccountToken = func(acc *store.Account, cfg *config.Config) (string, error) {
		return sessionJWT, nil
	}
	orchidsFetchCredits = func(ctx context.Context, sessionJWTArg string, userID string, proxyFunc func(*http.Request) (*url.URL, error)) (*orchids.CreditsInfo, error) {
		if sessionJWTArg != sessionJWT {
			t.Fatalf("sessionJWT=%q want %q", sessionJWTArg, sessionJWT)
		}
		if userID != "user_refresh_ok" {
			t.Fatalf("userID=%q want user_refresh_ok", userID)
		}
		return nil, errors.New("credits request failed with status 404: Server action not found.")
	}

	a := New(nil, "", "", &config.Config{})
	acc := &store.Account{ID: 77, AccountType: "orchids", ClientCookie: "client-cookie"}

	status, httpStatus, err := a.refreshAccountState(context.Background(), acc)
	if err != nil {
		t.Fatalf("refreshAccountState() error: %v", err)
	}
	if status != "" || httpStatus != 0 {
		t.Fatalf("unexpected status=%q httpStatus=%d", status, httpStatus)
	}
	if acc.Token != sessionJWT {
		t.Fatalf("Token=%q want %q", acc.Token, sessionJWT)
	}
	if acc.SessionID != "sess_refresh_ok" {
		t.Fatalf("SessionID=%q want sess_refresh_ok", acc.SessionID)
	}
	if acc.UserID != "user_refresh_ok" {
		t.Fatalf("UserID=%q want user_refresh_ok", acc.UserID)
	}
}

func TestRefreshAccountState_OrchidsCreditsTimeoutStillSucceeds(t *testing.T) {
	prevGetToken := orchidsGetAccountToken
	prevFetchCredits := orchidsFetchCredits
	t.Cleanup(func() {
		orchidsGetAccountToken = prevGetToken
		orchidsFetchCredits = prevFetchCredits
	})

	sessionJWT := "header." + encodeJWTClaims(`{"sid":"sess_refresh_timeout","sub":"user_refresh_timeout","exp":4102444800}`) + ".sig"
	orchidsGetAccountToken = func(acc *store.Account, cfg *config.Config) (string, error) {
		return sessionJWT, nil
	}
	orchidsFetchCredits = func(ctx context.Context, sessionJWTArg string, userID string, proxyFunc func(*http.Request) (*url.URL, error)) (*orchids.CreditsInfo, error) {
		if sessionJWTArg != sessionJWT {
			t.Fatalf("sessionJWT=%q want %q", sessionJWTArg, sessionJWT)
		}
		if userID != "user_refresh_timeout" {
			t.Fatalf("userID=%q want user_refresh_timeout", userID)
		}
		return nil, errors.New("failed to fetch credits: Post \"https://www.orchids.app/\": context deadline exceeded")
	}

	a := New(nil, "", "", &config.Config{})
	acc := &store.Account{ID: 78, AccountType: "orchids", ClientCookie: "client-cookie"}

	status, httpStatus, err := a.refreshAccountState(context.Background(), acc)
	if err != nil {
		t.Fatalf("refreshAccountState() error: %v", err)
	}
	if status != "" || httpStatus != 0 {
		t.Fatalf("unexpected status=%q httpStatus=%d", status, httpStatus)
	}
	if acc.Token != sessionJWT {
		t.Fatalf("Token=%q want %q", acc.Token, sessionJWT)
	}
	if acc.SessionID != "sess_refresh_timeout" {
		t.Fatalf("SessionID=%q want sess_refresh_timeout", acc.SessionID)
	}
	if acc.UserID != "user_refresh_timeout" {
		t.Fatalf("UserID=%q want user_refresh_timeout", acc.UserID)
	}
}

func TestRefreshAccountState_OrchidsTokenRefreshFailureMarksUnauthorized(t *testing.T) {
	prevGetToken := orchidsGetAccountToken
	t.Cleanup(func() {
		orchidsGetAccountToken = prevGetToken
	})

	orchidsGetAccountToken = func(acc *store.Account, cfg *config.Config) (string, error) {
		return "", errors.New("no active sessions found")
	}

	a := New(nil, "", "", &config.Config{})
	acc := &store.Account{AccountType: "orchids", SessionCookie: "session-cookie"}

	status, httpStatus, err := a.refreshAccountState(context.Background(), acc)
	if err == nil {
		t.Fatal("expected error")
	}
	if status != "401" {
		t.Fatalf("status=%q want 401", status)
	}
	if httpStatus != http.StatusUnauthorized {
		t.Fatalf("httpStatus=%d want %d", httpStatus, http.StatusUnauthorized)
	}
}

func TestRefreshAccountState_OrchidsTokenRefreshFailureWithQuotaFallbackReturnsSuccess(t *testing.T) {
	prevGetToken := orchidsGetAccountToken
	prevFetchCredits := orchidsFetchCredits
	t.Cleanup(func() {
		orchidsGetAccountToken = prevGetToken
		orchidsFetchCredits = prevFetchCredits
	})

	sessionJWT := encodeJWT(`{"sid":"sess_quota","sub":"user_quota","exp":4102444800}`)
	orchidsGetAccountToken = func(acc *store.Account, cfg *config.Config) (string, error) {
		return "", errors.New("unexpected status code 429: too many requests")
	}
	orchidsFetchCredits = func(ctx context.Context, sessionJWTArg string, userID string, proxyFunc func(*http.Request) (*url.URL, error)) (*orchids.CreditsInfo, error) {
		if sessionJWTArg != sessionJWT {
			t.Fatalf("sessionJWT=%q want %q", sessionJWTArg, sessionJWT)
		}
		if userID != "user_quota" {
			t.Fatalf("userID=%q want user_quota", userID)
		}
		return &orchids.CreditsInfo{Credits: 321, Plan: "PRO"}, nil
	}

	a := New(nil, "", "", &config.Config{})
	acc := &store.Account{
		AccountType:   "orchids",
		SessionCookie: sessionJWT,
	}

	status, httpStatus, err := a.refreshAccountState(context.Background(), acc)
	if err != nil {
		t.Fatalf("refreshAccountState() error: %v", err)
	}
	if status != "" {
		t.Fatalf("status=%q want empty", status)
	}
	if httpStatus != 0 {
		t.Fatalf("httpStatus=%d want 0", httpStatus)
	}
	if acc.SessionID != "sess_quota" {
		t.Fatalf("SessionID=%q want sess_quota", acc.SessionID)
	}
	if acc.UserID != "user_quota" {
		t.Fatalf("UserID=%q want user_quota", acc.UserID)
	}
	if acc.Subscription != "pro" || acc.UsageCurrent != 321 || acc.UsageLimit != orchids.PlanCreditLimit("PRO") {
		t.Fatalf("unexpected orchids quota sync: subscription=%q current=%v limit=%v", acc.Subscription, acc.UsageCurrent, acc.UsageLimit)
	}
}

func TestShouldSyncAccountOnCreate(t *testing.T) {
	t.Parallel()

	if shouldSyncAccountOnCreate(&store.Account{AccountType: "orchids"}) {
		t.Fatal("expected Orchids account create to skip initial sync")
	}
	if !shouldSyncAccountOnCreate(&store.Account{AccountType: "grok"}) {
		t.Fatal("expected non-Orchids account create to keep initial sync")
	}
	if shouldSyncAccountOnCreate(nil) {
		t.Fatal("nil account should not sync on create")
	}
}

func TestApplyAccountStatusFromError(t *testing.T) {
	t.Parallel()

	acc := &store.Account{}
	applyAccountStatusFromError(acc, errors.New("unexpected status code 429: too many requests"))
	if acc.StatusCode != "429" {
		t.Fatalf("StatusCode=%q want 429", acc.StatusCode)
	}
	if acc.LastAttempt.IsZero() {
		t.Fatal("LastAttempt should be set")
	}

	unknown := &store.Account{}
	applyAccountStatusFromError(unknown, errors.New("plain failure"))
	if unknown.StatusCode != "" {
		t.Fatalf("StatusCode=%q want empty", unknown.StatusCode)
	}

	noActiveSession := &store.Account{}
	applyAccountStatusFromError(noActiveSession, errors.New("no active sessions found"))
	if noActiveSession.StatusCode != "401" {
		t.Fatalf("StatusCode=%q want 401", noActiveSession.StatusCode)
	}
}

func TestNormalizeOrchidsCredentialInput_SessionJWT(t *testing.T) {
	t.Parallel()

	sessionJWT := encodeJWT(`{"sid":"sess_abc","sub":"user_xyz","exp":4102444800}`)
	acc := &store.Account{
		AccountType:  "orchids",
		ClientCookie: sessionJWT,
	}

	if err := normalizeOrchidsCredentialInput(acc); err != nil {
		t.Fatalf("normalizeOrchidsCredentialInput() error = %v", err)
	}
	if acc.Token != sessionJWT {
		t.Fatalf("Token=%q want %q", acc.Token, sessionJWT)
	}
	if acc.SessionCookie != sessionJWT {
		t.Fatalf("SessionCookie=%q want %q", acc.SessionCookie, sessionJWT)
	}
	if acc.ClientCookie != "" {
		t.Fatalf("ClientCookie=%q want empty", acc.ClientCookie)
	}
	if acc.SessionID != "sess_abc" || acc.UserID != "user_xyz" {
		t.Fatalf("unexpected session info sid=%q user=%q", acc.SessionID, acc.UserID)
	}
}

func TestNormalizeOrchidsCredentialInput_RawClientJWT(t *testing.T) {
	t.Parallel()

	clientJWT := encodeJWT(`{"id":"client_abc","rotating_token":"rotating_xyz","exp":4102444800}`)
	acc := &store.Account{
		AccountType:  "orchids",
		ClientCookie: clientJWT,
	}

	if err := normalizeOrchidsCredentialInput(acc); err != nil {
		t.Fatalf("normalizeOrchidsCredentialInput() error = %v", err)
	}
	if acc.ClientCookie != clientJWT {
		t.Fatalf("ClientCookie=%q want %q", acc.ClientCookie, clientJWT)
	}
	if acc.SessionCookie != "" {
		t.Fatalf("SessionCookie=%q want empty", acc.SessionCookie)
	}
	if acc.Token != "" {
		t.Fatalf("Token=%q want empty", acc.Token)
	}
	if acc.SessionID != "" {
		t.Fatalf("SessionID=%q want empty", acc.SessionID)
	}
}

func TestNormalizeOrchidsCredentialInput_CookieJSON(t *testing.T) {
	t.Parallel()

	sessionJWT := encodeJWT(`{"sid":"sess_json","sub":"user_json","exp":4102444800}`)
	acc := &store.Account{
		AccountType: "orchids",
		ClientCookie: `[
			{"name":"__session","value":"` + sessionJWT + `"},
			{"name":"__client_uat","value":"1773712060"}
		]`,
	}

	if err := normalizeOrchidsCredentialInput(acc); err != nil {
		t.Fatalf("normalizeOrchidsCredentialInput() error = %v", err)
	}
	if acc.Token != sessionJWT {
		t.Fatalf("Token=%q want %q", acc.Token, sessionJWT)
	}
	if acc.SessionCookie != sessionJWT {
		t.Fatalf("SessionCookie=%q want %q", acc.SessionCookie, sessionJWT)
	}
	if acc.ClientUat != "1773712060" {
		t.Fatalf("ClientUat=%q want %q", acc.ClientUat, "1773712060")
	}
	if acc.SessionID != "sess_json" || acc.UserID != "user_json" {
		t.Fatalf("unexpected session info sid=%q user=%q", acc.SessionID, acc.UserID)
	}
}

func TestHandleAccounts_PostOrchidsSessionInputHydratesAccountInfo(t *testing.T) {
	prevGetToken := orchidsGetAccountToken
	t.Cleanup(func() {
		orchidsGetAccountToken = prevGetToken
	})

	sessionJWT := encodeJWT(`{"sid":"sess_create","sub":"user_create","exp":4102444800}`)
	orchidsGetAccountToken = func(acc *store.Account, cfg *config.Config) (string, error) {
		if acc.AccountType != "orchids" {
			t.Fatalf("AccountType=%q want orchids", acc.AccountType)
		}
		if acc.SessionCookie != sessionJWT {
			t.Fatalf("SessionCookie=%q want %q", acc.SessionCookie, sessionJWT)
		}
		if acc.SessionID != "sess_create" {
			t.Fatalf("SessionID=%q want sess_create", acc.SessionID)
		}
		acc.ClientCookie = "bootstrapped-client"
		acc.ClientUat = "1773712060"
		acc.ProjectID = "proj_create"
		acc.UserID = "user_create"
		acc.Email = "create@example.com"
		return sessionJWT, nil
	}

	a, s, cleanup := newTestAPI(t)
	defer cleanup()

	req := httptest.NewRequest(http.MethodPost, "/api/accounts", strings.NewReader(`{"account_type":"orchids","client_cookie":"`+sessionJWT+`","enabled":false}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	a.HandleAccounts(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("status=%d want 201 body=%s", rec.Code, rec.Body.String())
	}

	var resp store.Account
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp.Email != "create@example.com" {
		t.Fatalf("Email=%q want create@example.com", resp.Email)
	}
	if resp.UserID != "user_create" {
		t.Fatalf("UserID=%q want user_create", resp.UserID)
	}
	if resp.ProjectID != "proj_create" {
		t.Fatalf("ProjectID=%q want proj_create", resp.ProjectID)
	}
	if resp.ClientCookie != "bootstrapped-client" {
		t.Fatalf("ClientCookie=%q want bootstrapped-client", resp.ClientCookie)
	}

	stored, err := s.GetAccount(req.Context(), resp.ID)
	if err != nil {
		t.Fatalf("GetAccount() error = %v", err)
	}
	if stored.Email != "create@example.com" {
		t.Fatalf("stored.Email=%q want create@example.com", stored.Email)
	}
	if stored.ClientCookie != "bootstrapped-client" {
		t.Fatalf("stored.ClientCookie=%q want bootstrapped-client", stored.ClientCookie)
	}
}

func TestHandleAccounts_PostOrchidsClientInputHydratesAccountInfo(t *testing.T) {
	prevGetToken := orchidsGetAccountToken
	t.Cleanup(func() {
		orchidsGetAccountToken = prevGetToken
	})

	clientJWT := encodeJWT(`{"id":"client_create","rotating_token":"rotating_create","exp":4102444800}`)
	sessionJWT := encodeJWT(`{"sid":"sess_client_create","sub":"user_client_create","exp":4102444800}`)
	orchidsGetAccountToken = func(acc *store.Account, cfg *config.Config) (string, error) {
		if acc.AccountType != "orchids" {
			t.Fatalf("AccountType=%q want orchids", acc.AccountType)
		}
		if acc.ClientCookie != clientJWT {
			t.Fatalf("ClientCookie=%q want %q", acc.ClientCookie, clientJWT)
		}
		if acc.SessionCookie != "" {
			t.Fatalf("SessionCookie=%q want empty", acc.SessionCookie)
		}
		acc.ClientUat = "1773712060"
		acc.ProjectID = "proj_client_create"
		acc.UserID = "user_client_create"
		acc.Email = "client-create@example.com"
		return sessionJWT, nil
	}

	a, s, cleanup := newTestAPI(t)
	defer cleanup()

	req := httptest.NewRequest(http.MethodPost, "/api/accounts", strings.NewReader(`{"account_type":"orchids","client_cookie":"`+clientJWT+`","enabled":false}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	a.HandleAccounts(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("status=%d want 201 body=%s", rec.Code, rec.Body.String())
	}

	var resp store.Account
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp.Email != "client-create@example.com" {
		t.Fatalf("Email=%q want client-create@example.com", resp.Email)
	}
	if resp.UserID != "user_client_create" {
		t.Fatalf("UserID=%q want user_client_create", resp.UserID)
	}
	if resp.ProjectID != "proj_client_create" {
		t.Fatalf("ProjectID=%q want proj_client_create", resp.ProjectID)
	}
	if resp.ClientCookie != clientJWT {
		t.Fatalf("ClientCookie=%q want %q", resp.ClientCookie, clientJWT)
	}
	if resp.SessionID != "sess_client_create" {
		t.Fatalf("SessionID=%q want sess_client_create", resp.SessionID)
	}
	if resp.Token != sessionJWT {
		t.Fatalf("Token=%q want %q", resp.Token, sessionJWT)
	}

	stored, err := s.GetAccount(req.Context(), resp.ID)
	if err != nil {
		t.Fatalf("GetAccount() error = %v", err)
	}
	if stored.Email != "client-create@example.com" {
		t.Fatalf("stored.Email=%q want client-create@example.com", stored.Email)
	}
	if stored.ClientCookie != clientJWT {
		t.Fatalf("stored.ClientCookie=%q want %q", stored.ClientCookie, clientJWT)
	}
	if stored.ClientUat != "1773712060" {
		t.Fatalf("stored.ClientUat=%q want %q", stored.ClientUat, "1773712060")
	}
}

func TestHandleAccountByID_PutOrchidsSessionInputHydratesAccountInfo(t *testing.T) {
	prevGetToken := orchidsGetAccountToken
	t.Cleanup(func() {
		orchidsGetAccountToken = prevGetToken
	})

	sessionJWT := encodeJWT(`{"sid":"sess_update","sub":"user_update","exp":4102444800}`)
	orchidsGetAccountToken = func(acc *store.Account, cfg *config.Config) (string, error) {
		if acc.SessionCookie != sessionJWT {
			t.Fatalf("SessionCookie=%q want %q", acc.SessionCookie, sessionJWT)
		}
		acc.ClientCookie = "updated-client"
		acc.ClientUat = "1773712099"
		acc.ProjectID = "proj_update"
		acc.UserID = "user_update"
		acc.Email = "update@example.com"
		return sessionJWT, nil
	}

	a, s, cleanup := newTestAPI(t)
	defer cleanup()

	existing := &store.Account{
		AccountType:   "orchids",
		ClientCookie:  "old-client",
		SessionCookie: "old-session",
		SessionID:     "sess_old",
		ClientUat:     "1773700000",
		ProjectID:     "proj_old",
		UserID:        "user_old",
		Email:         "old@example.com",
		Enabled:       true,
		Weight:        1,
	}
	if err := s.CreateAccount(context.Background(), existing); err != nil {
		t.Fatalf("CreateAccount() error = %v", err)
	}

	req := httptest.NewRequest(http.MethodPut, "/api/accounts/"+strconv.FormatInt(existing.ID, 10), strings.NewReader(`{"account_type":"orchids","client_cookie":"`+sessionJWT+`","enabled":true,"weight":1}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	a.HandleAccountByID(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d want 200 body=%s", rec.Code, rec.Body.String())
	}

	var resp store.Account
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp.Email != "update@example.com" {
		t.Fatalf("Email=%q want update@example.com", resp.Email)
	}
	if resp.UserID != "user_update" {
		t.Fatalf("UserID=%q want user_update", resp.UserID)
	}
	if resp.ProjectID != "proj_update" {
		t.Fatalf("ProjectID=%q want proj_update", resp.ProjectID)
	}
	if resp.ClientCookie != "updated-client" {
		t.Fatalf("ClientCookie=%q want updated-client", resp.ClientCookie)
	}

	stored, err := s.GetAccount(context.Background(), existing.ID)
	if err != nil {
		t.Fatalf("GetAccount() error = %v", err)
	}
	if stored.Email != "update@example.com" {
		t.Fatalf("stored.Email=%q want update@example.com", stored.Email)
	}
	if stored.ClientCookie != "updated-client" {
		t.Fatalf("stored.ClientCookie=%q want updated-client", stored.ClientCookie)
	}
}

func TestHandleAccounts_PostRejectsDuplicateWarpRefreshToken(t *testing.T) {
	a, s, cleanup := newTestAPI(t)
	defer cleanup()

	existing := &store.Account{
		AccountType:  "warp",
		RefreshToken: "warp-token-1",
		Enabled:      true,
	}
	normalizeWarpTokenInput(existing)
	if err := s.CreateAccount(context.Background(), existing); err != nil {
		t.Fatalf("CreateAccount() error = %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/api/accounts", strings.NewReader(`{"account_type":"warp","refresh_token":"warp-token-1","enabled":true}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	a.HandleAccounts(rec, req)

	if rec.Code != http.StatusConflict {
		t.Fatalf("status=%d want 409 body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "duplicate warp token") {
		t.Fatalf("body=%q want duplicate warp token", rec.Body.String())
	}
}

func TestHandleWarpUserFileImport_CreatesWarpAccount(t *testing.T) {
	a, s, cleanup := newTestAPI(t)
	defer cleanup()

	restore := warp.SetLocalUserStorageTestHooks(nil, func(encrypted []byte) (string, error) {
		if string(encrypted) != "encrypted-user-file" {
			t.Fatalf("encrypted=%q want encrypted-user-file", encrypted)
		}
		return `{"id_token":{"id_token":"runtime-jwt","refresh_token":"warp-upload-token"},"refresh_token":""}`, nil
	})
	defer restore()

	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	part, err := writer.CreateFormFile("file", "dev.warp.Warp-User")
	if err != nil {
		t.Fatalf("CreateFormFile() error = %v", err)
	}
	if _, err := part.Write([]byte("encrypted-user-file")); err != nil {
		t.Fatalf("part.Write() error = %v", err)
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("writer.Close() error = %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/api/warp/import-user-file", &body)
	req.Header.Set("Content-Type", writer.FormDataContentType())
	rec := httptest.NewRecorder()

	a.HandleWarpUserFileImport(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d want 200 body=%s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "runtime-jwt") {
		t.Fatalf("response leaked persisted user JSON: %s", rec.Body.String())
	}
	var resp store.Account
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp.ID == 0 {
		t.Fatal("response ID is empty")
	}
	if resp.RefreshToken != "warp-upload-token" {
		t.Fatalf("RefreshToken=%q want warp-upload-token", resp.RefreshToken)
	}
	stored, err := s.GetAccount(context.Background(), resp.ID)
	if err != nil {
		t.Fatalf("GetAccount() error = %v", err)
	}
	if stored.RefreshToken != "warp-upload-token" || !stored.Enabled || stored.AccountType != "warp" {
		t.Fatalf("stored account=%#v", stored)
	}
}

func TestHandleWarpUserFileImport_CreatesWarpAccountFromPlaintextJSON(t *testing.T) {
	a, s, cleanup := newTestAPI(t)
	defer cleanup()

	restore := warp.SetLocalUserStorageTestHooks(nil, func([]byte) (string, error) {
		t.Fatal("decrypt hook should not be called for plaintext User JSON")
		return "", nil
	})
	defer restore()

	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	part, err := writer.CreateFormFile("file", "user.json")
	if err != nil {
		t.Fatalf("CreateFormFile() error = %v", err)
	}
	if _, err := part.Write([]byte(`{"id_token":{"id_token":"runtime-jwt","refresh_token":"warp-plaintext-token"},"refresh_token":""}`)); err != nil {
		t.Fatalf("part.Write() error = %v", err)
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("writer.Close() error = %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/api/warp/import-user-file", &body)
	req.Header.Set("Content-Type", writer.FormDataContentType())
	rec := httptest.NewRecorder()

	a.HandleWarpUserFileImport(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d want 200 body=%s", rec.Code, rec.Body.String())
	}
	var resp store.Account
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	stored, err := s.GetAccount(context.Background(), resp.ID)
	if err != nil {
		t.Fatalf("GetAccount() error = %v", err)
	}
	if stored.RefreshToken != "warp-plaintext-token" || !stored.Enabled || stored.AccountType != "warp" {
		t.Fatalf("stored account=%#v", stored)
	}
}

func TestHandleAccountByID_PutRejectsDuplicateGrokToken(t *testing.T) {
	a, s, cleanup := newTestAPI(t)
	defer cleanup()

	acc1 := &store.Account{AccountType: "grok", ClientCookie: "grok-token-a", Enabled: true}
	normalizeGrokTokenInput(acc1)
	if err := s.CreateAccount(context.Background(), acc1); err != nil {
		t.Fatalf("CreateAccount(acc1) error = %v", err)
	}

	acc2 := &store.Account{AccountType: "grok", ClientCookie: "grok-token-b", Enabled: true}
	normalizeGrokTokenInput(acc2)
	if err := s.CreateAccount(context.Background(), acc2); err != nil {
		t.Fatalf("CreateAccount(acc2) error = %v", err)
	}

	req := httptest.NewRequest(http.MethodPut, "/api/accounts/"+strconv.FormatInt(acc2.ID, 10), strings.NewReader(`{"account_type":"grok","client_cookie":"grok-token-a","enabled":true}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	a.HandleAccountByID(rec, req)

	if rec.Code != http.StatusConflict {
		t.Fatalf("status=%d want 409 body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "duplicate grok token") {
		t.Fatalf("body=%q want duplicate grok token", rec.Body.String())
	}
}

func TestHandleAccountByID_PutAllowsSameAccountCredential(t *testing.T) {
	a, s, cleanup := newTestAPI(t)
	defer cleanup()

	acc := &store.Account{
		AccountType:   "puter",
		SessionCookie: "puter-token-1",
		Enabled:       true,
		Name:          "before",
	}
	if err := s.CreateAccount(context.Background(), acc); err != nil {
		t.Fatalf("CreateAccount() error = %v", err)
	}

	req := httptest.NewRequest(http.MethodPut, "/api/accounts/"+strconv.FormatInt(acc.ID, 10), strings.NewReader(`{"account_type":"puter","session_cookie":"puter-token-1","enabled":true,"name":"after"}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	a.HandleAccountByID(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d want 200 body=%s", rec.Code, rec.Body.String())
	}
}

func newTestAPI(t *testing.T) (*API, *store.Store, func()) {
	t.Helper()

	mini := miniredis.RunT(t)
	s, err := store.New(store.Options{
		StoreMode:   "redis",
		RedisAddr:   mini.Addr(),
		RedisPrefix: "api_test:",
	})
	if err != nil {
		mini.Close()
		t.Fatalf("store.New() error = %v", err)
	}

	return New(s, "", "", &config.Config{}), s, func() {
		_ = s.Close()
		mini.Close()
	}
}

func encodeJWTClaims(raw string) string {
	return base64.RawURLEncoding.EncodeToString([]byte(raw))
}

func encodeJWT(raw string) string {
	return encodeJWTClaims(`{"alg":"none","typ":"JWT"}`) + "." + encodeJWTClaims(raw) + ".sigpayload"
}
