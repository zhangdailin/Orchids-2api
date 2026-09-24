package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"orchids-api/internal/config"
	"orchids-api/internal/store"
)

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
	if strings.Contains(rec.Body.String(), "warp-refresh") || !strings.Contains(rec.Body.String(), `"has_credential":true`) {
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
