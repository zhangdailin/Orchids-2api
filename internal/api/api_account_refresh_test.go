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
