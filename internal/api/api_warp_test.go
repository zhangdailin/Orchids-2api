package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/goccy/go-json"

	"orchids-api/internal/store"
)

func TestNormalizeWarpTokenInput_UsesExplicitRefreshTokenOnly(t *testing.T) {
	t.Parallel()

	acc := &store.Account{
		AccountType:  "warp",
		RefreshToken: "  token-123  ",
		ClientCookie: "should-be-cleared",
	}

	normalizeWarpTokenInput(acc)

	if acc.RefreshToken != "token-123" {
		t.Fatalf("RefreshToken=%q want token-123", acc.RefreshToken)
	}
	if acc.ClientCookie != "" {
		t.Fatalf("ClientCookie=%q want empty", acc.ClientCookie)
	}
	if acc.SessionCookie != "" {
		t.Fatalf("SessionCookie=%q want empty", acc.SessionCookie)
	}
}

func TestWarpManualCreationAndImportDisabled(t *testing.T) {
	a, s, cleanup := newTestAPI(t)
	defer cleanup()
	for _, accountType := range []string{"warp", "WARP", " Warp "} {
		for _, credential := range []string{"refresh_token", "token", "client_cookie", "session_cookie", "oauth_refresh_token"} {
			body, _ := json.Marshal(map[string]string{"account_type": accountType, credential: "manual-secret"})
			rec := httptest.NewRecorder()
			a.HandleAccounts(rec, httptest.NewRequest(http.MethodPost, "/api/accounts", strings.NewReader(string(body))))
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("manual create %q/%s: status=%d", accountType, credential, rec.Code)
			}
		}
	}
	rec := httptest.NewRecorder()
	a.HandleImport(rec, httptest.NewRequest(http.MethodPost, "/api/import", strings.NewReader(`{"accounts":[{"account_type":"warp","refresh_token":"manual-secret"},{"account_type":" Warp ","refresh_token":"manual-secret-2"},{"account_type":"puter","client_cookie":"puter-secret"}]}`)))
	var result ImportResult
	if err := json.Unmarshal(rec.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusOK || result.Imported != 1 || result.Skipped != 2 {
		t.Fatalf("unexpected import result: %s", rec.Body.String())
	}
	accounts, err := s.ListAccounts(context.Background())
	if err != nil || len(accounts) != 1 || accounts[0].AccountType != "puter" {
		t.Fatalf("manual path persisted a Warp account: count=%d err=%v", len(accounts), err)
	}
}

func TestWarpCredentialReplacementAndTypeConversionDisabled(t *testing.T) {
	a, s, cleanup := newTestAPI(t)
	defer cleanup()
	acc := &store.Account{AccountType: "warp", RefreshToken: "private-session", Enabled: true}
	other := &store.Account{AccountType: "puter", ClientCookie: "puter-secret", Enabled: true}
	for _, item := range []*store.Account{acc, other} {
		if err := s.CreateAccount(context.Background(), item); err != nil {
			t.Fatal(err)
		}
	}
	for _, credential := range []string{"refresh_token", "token", "client_cookie", "session_cookie", "oauth_access_token", "oauth_refresh_token"} {
		body, _ := json.Marshal(map[string]string{"account_type": " Warp ", credential: "replacement"})
		rec := httptest.NewRecorder()
		a.HandleAccountByID(rec, httptest.NewRequest(http.MethodPut, "/api/accounts/"+strconv.FormatInt(acc.ID, 10), strings.NewReader(string(body))))
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("replacement %s: status=%d", credential, rec.Code)
		}
	}
	for _, tc := range []struct {
		id          int64
		accountType string
	}{{acc.ID, "puter"}, {other.ID, "warp"}} {
		rec := httptest.NewRecorder()
		a.HandleAccountByID(rec, httptest.NewRequest(http.MethodPut, "/api/accounts/"+strconv.FormatInt(tc.id, 10), strings.NewReader(`{"account_type":"`+tc.accountType+`"}`)))
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("type conversion to %s: status=%d", tc.accountType, rec.Code)
		}
	}
	stored, err := s.GetAccount(context.Background(), acc.ID)
	if err != nil || stored.RefreshToken != "private-session" || stored.AccountType != "warp" {
		t.Fatal("rejected update changed the existing login session")
	}
}

func TestWarpManagementResponsesHideSessionCredentials(t *testing.T) {
	a, s, cleanup := newTestAPI(t)
	defer cleanup()
	acc := &store.Account{AccountType: "warp", RefreshToken: "private-session", Token: "private-runtime", Enabled: true}
	if err := s.CreateAccount(context.Background(), acc); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"/api/accounts", "/api/accounts/" + strconv.FormatInt(acc.ID, 10)} {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, path, nil)
		if path == "/api/accounts" {
			a.HandleAccounts(rec, req)
		} else {
			a.HandleAccountByID(rec, req)
		}
		if rec.Code != http.StatusOK || strings.Contains(rec.Body.String(), "private-") || !strings.Contains(rec.Body.String(), `"warp_authenticated":true`) {
			t.Fatalf("unexpected public account response: %s", rec.Body.String())
		}
	}
	if out := normalizeAccountOutput(&store.Account{AccountType: "warp"}); out.WarpAuthenticated {
		t.Fatal("missing session must not be reported as configured")
	}
	rec := httptest.NewRecorder()
	a.HandleExport(rec, httptest.NewRequest(http.MethodGet, "/api/export", nil))
	var export ExportData
	if err := json.Unmarshal(rec.Body.Bytes(), &export); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusOK || len(export.Accounts) != 0 || strings.Contains(rec.Body.String(), "private-") {
		t.Fatalf("Warp session must not be exported: %s", rec.Body.String())
	}
	stored, err := s.GetAccount(context.Background(), acc.ID)
	if err != nil || stored.RefreshToken != "private-session" {
		t.Fatal("redaction must not change the stored login session")
	}
}

func TestNormalizeWarpTokenOutput_DoesNotUseLegacyValue(t *testing.T) {
	t.Parallel()

	acc := &store.Account{
		AccountType:  "warp",
		ClientCookie: "foo=1; refresh_token=token-123; Path=/",
	}

	normalized := normalizeWarpTokenOutput(acc)
	if normalized == nil {
		t.Fatal("normalizeWarpTokenOutput returned nil")
	}
	if normalized.RefreshToken != "" {
		t.Fatalf("RefreshToken=%q want empty", normalized.RefreshToken)
	}
	if normalized.ClientCookie != "" {
		t.Fatalf("ClientCookie=%q want empty", normalized.ClientCookie)
	}
}

func TestNormalizeWarpTokenInput_DoesNotUseLegacyTokenField(t *testing.T) {
	t.Parallel()

	acc := &store.Account{
		AccountType: "warp",
		Token:       "legacy-refresh-token",
	}

	normalizeWarpTokenInput(acc)

	if acc.RefreshToken != "" {
		t.Fatalf("RefreshToken=%q want empty", acc.RefreshToken)
	}
	if acc.Token != "" {
		t.Fatalf("Token=%q want empty", acc.Token)
	}
}

func TestNormalizeWarpTokenOutput_HidesRuntimeJWT(t *testing.T) {
	t.Parallel()

	acc := &store.Account{
		AccountType:  "warp",
		RefreshToken: "",
		Token:        "aaaaaaaaaa.bbbbbbbbbb.cccccccccc",
	}

	normalized := normalizeWarpTokenOutput(acc)
	if normalized == nil {
		t.Fatal("normalizeWarpTokenOutput returned nil")
	}
	if normalized.RefreshToken != "" {
		t.Fatalf("RefreshToken=%q want empty", normalized.RefreshToken)
	}
	if normalized.Token != "" {
		t.Fatalf("Token=%q want empty", normalized.Token)
	}
}

func TestNormalizeAccountOutput_InfersWarpTierFromStoredQuota(t *testing.T) {
	t.Parallel()

	acc := &store.Account{
		AccountType:      "warp",
		Subscription:     "free",
		WarpMonthlyLimit: 1500,
	}

	normalized := normalizeAccountOutput(acc)
	if normalized == nil {
		t.Fatal("normalizeAccountOutput returned nil")
	}
	if normalized.Subscription != "build/business" {
		t.Fatalf("Subscription=%q want build/business", normalized.Subscription)
	}
}
