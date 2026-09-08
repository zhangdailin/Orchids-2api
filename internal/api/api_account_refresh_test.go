package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/goccy/go-json"

	"orchids-api/internal/config"
	"orchids-api/internal/store"
)

func TestRefreshAccountState_GrokSyncsRemainingQuota(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/auth/session" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"status":"authenticated","session":{"userId":"user-1"}}`))
			return
		}
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
		if r.URL.Path == "/api/auth/session" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"status":"authenticated","session":{"userId":"user-23"}}`))
			return
		}
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
	if len(requestedModels) != 2 || requestedModels[0] != "auto" || requestedModels[1] != "fast" {
		t.Fatalf("requestedModels=%v want [auto fast]", requestedModels)
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

func TestBuildQuotaResponseFields_PuterMonthlyUsage(t *testing.T) {
	t.Parallel()

	acc := &store.Account{
		AccountType:  "puter",
		UsageCurrent: 13494935.4,
		UsageLimit:   25000000,
	}

	fields := buildQuotaResponseFields(acc)
	if got := fields["quota_supported"].(bool); !got {
		t.Fatal("quota_supported=false want true")
	}
	if got := fields["quota_limit"].(float64); got != 25000000 {
		t.Fatalf("quota_limit=%v want 25000000", got)
	}
	if got := fields["quota_remaining"].(float64); got != 13494935.4 {
		t.Fatalf("quota_remaining=%v want 13494935.4", got)
	}
	if got := fields["quota_used"].(float64); got != 11505064.6 {
		t.Fatalf("quota_used=%v want 11505064.6", got)
	}
	if got := fields["quota_unit"].(string); got != "credits" {
		t.Fatalf("quota_unit=%q want credits", got)
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

func TestHandleAccounts_PostRejectsManualWarpLogin(t *testing.T) {
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

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status=%d want 400 body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "official web login") {
		t.Fatalf("body=%q want official web login guidance", rec.Body.String())
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

func TestNormalizeGrokTokenInput_PreservesFullCookie(t *testing.T) {
	acc := &store.Account{
		AccountType:   "grok",
		ClientCookie:  "foo=1; sso=grok-token-a; x-userid=user-1; cf_clearance=cf-1; __cf_bm=bm-1; Path=/; HttpOnly",
		RefreshToken:  "stale-refresh",
		SessionCookie: "stale-session",
		SessionID:     "stale-session-id",
		ClientUat:     "stale-uat",
		ProjectID:     "stale-project",
	}

	normalizeGrokTokenInput(acc)
	if acc.CredentialType != "sso" {
		t.Fatalf("CredentialType=%q want sso", acc.CredentialType)
	}

	for _, want := range []string{"sso=grok-token-a", "x-userid=user-1", "cf_clearance=cf-1", "__cf_bm=bm-1"} {
		if !strings.Contains(acc.ClientCookie, want) {
			t.Fatalf("ClientCookie=%q missing %q", acc.ClientCookie, want)
		}
	}
	if acc.RefreshToken != "" || acc.SessionCookie != "" || acc.SessionID != "" || acc.ClientUat != "" || acc.ProjectID != "" {
		t.Fatalf("unrelated fields should be cleared: %#v", acc)
	}
	if key := normalizedAccountCredentialKey(acc); key != "grok:grok-token-a" {
		t.Fatalf("credential key=%q want grok:grok-token-a", key)
	}
}

func TestVerifyGrokAccount_ModelNotFoundKeepsAuthenticatedSSOUsable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/rest/rate-limits":
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"code":5,"message":"Model not found.","details":[]}`))
		case "/api/auth/session":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"status":"authenticated","session":{"userId":"user-1","email":"user@example.com","organizationId":"team-1"}}`))
		default:
			t.Fatalf("unexpected path: %s", r.URL.Path)
		}
	}))
	defer srv.Close()

	acc := &store.Account{ID: 144, AccountType: "grok", ClientCookie: "sso=test-cookie"}
	normalizeGrokTokenInput(acc)
	if err := verifyGrokAccount(context.Background(), acc, &config.Config{GrokAPIBaseURL: srv.URL}, nil); err != nil {
		t.Fatalf("verifyGrokAccount() error=%v", err)
	}
	if acc.UserID != "user-1" || acc.Email != "user@example.com" || acc.TeamID != "team-1" {
		t.Fatalf("identity not applied: user=%q email=%q team=%q", acc.UserID, acc.Email, acc.TeamID)
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

func TestHandleAccountByID_PutClearsLegacyWarpCredentialFields(t *testing.T) {
	a, s, cleanup := newTestAPI(t)
	defer cleanup()

	acc := &store.Account{
		AccountType:   "warp",
		RefreshToken:  "warp-refresh",
		Token:         "legacy-jwt",
		ClientCookie:  "legacy-cookie",
		SessionCookie: "legacy-session",
		Enabled:       true,
	}
	if err := s.CreateAccount(context.Background(), acc); err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodPut, "/api/accounts/"+strconv.FormatInt(acc.ID, 10), strings.NewReader(`{"account_type":"warp","enabled":true,"name":"renamed"}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	a.HandleAccountByID(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "legacy-jwt") {
		t.Fatalf("response leaked legacy JWT: %s", rec.Body.String())
	}
	stored, err := s.GetAccount(context.Background(), acc.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Token != "" || stored.ClientCookie != "" || stored.SessionCookie != "" {
		t.Fatalf("legacy credential fields were retained: %#v", stored)
	}
	if stored.RefreshToken != "warp-refresh" || stored.Name != "renamed" {
		t.Fatal("settings edit must preserve the private login session")
	}
	if strings.Contains(rec.Body.String(), "warp-refresh") || !strings.Contains(rec.Body.String(), `"warp_authenticated":true`) {
		t.Fatalf("expected credential presence without the secret: %s", rec.Body.String())
	}
}

func newTestAPI(t *testing.T) (*API, *store.Store, func()) {
	t.Helper()

	s, mini := newTestStore(t, "api_test:")

	return New(s, "", "", &config.Config{}), s, func() {
		_ = s.Close()
		mini.Close()
	}
}

func TestHandleAccountByID_PutPreservesGrokOAuthTokens(t *testing.T) {
	a, s, cleanup := newTestAPI(t)
	defer cleanup()

	acc := &store.Account{
		AccountType:       "grok",
		CredentialType:    "oauth",
		OAuthAccessToken:  "keep-access",
		OAuthRefreshToken: "keep-refresh",
		OAuthExpiresAt:    time.Now().UTC().Add(time.Hour).Truncate(time.Second),
		TeamID:            "team-1",
		Enabled:           true,
		Name:              "oauth-acc",
	}
	if err := s.CreateAccount(context.Background(), acc); err != nil {
		t.Fatalf("CreateAccount() error = %v", err)
	}

	// Simulate admin UI edit/save with redacted empty secrets.
	body := `{"account_type":"grok","credential_type":"oauth","name":"oauth-acc","enabled":false,"oauth_access_token":"","oauth_refresh_token":""}`
	req := httptest.NewRequest(http.MethodPut, "/api/accounts/"+strconv.FormatInt(acc.ID, 10), strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	a.HandleAccountByID(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d want 200 body=%s", rec.Code, rec.Body.String())
	}

	got, err := s.GetAccount(context.Background(), acc.ID)
	if err != nil {
		t.Fatalf("GetAccount() error = %v", err)
	}
	if got.OAuthAccessToken != "keep-access" {
		t.Fatalf("OAuthAccessToken=%q want keep-access", got.OAuthAccessToken)
	}
	if got.OAuthRefreshToken != "keep-refresh" {
		t.Fatalf("OAuthRefreshToken=%q want keep-refresh", got.OAuthRefreshToken)
	}
	if got.Enabled {
		t.Fatal("expected enabled=false after update")
	}
	if got.TeamID != "team-1" {
		t.Fatalf("TeamID=%q want team-1", got.TeamID)
	}
}

func TestHandleAccounts_PostRejectsEmptyGrokOAuth(t *testing.T) {
	a, _, cleanup := newTestAPI(t)
	defer cleanup()

	body := `{"account_type":"grok","credential_type":"oauth","enabled":true}`
	req := httptest.NewRequest(http.MethodPost, "/api/accounts", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	a.HandleAccounts(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status=%d want 400 body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "missing oauth token") {
		t.Fatalf("body=%q want missing oauth token", rec.Body.String())
	}
}
