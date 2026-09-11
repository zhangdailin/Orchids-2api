package grok

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/goccy/go-json"

	"orchids-api/internal/config"
	"orchids-api/internal/store"
)

func TestCollectRefreshTokens(t *testing.T) {
	req := adminTokenRefreshRequest{
		Token:  "sso=t0; Path=/",
		Tokens: []string{"t1", "sso=t1", "  t2  "},
	}
	got := collectRefreshTokens(req)
	if len(got) != 3 {
		t.Fatalf("collectRefreshTokens len=%d want=3", len(got))
	}
	want := []string{"t0", "t1", "t2"}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("collectRefreshTokens[%d]=%q want=%q", i, got[i], want[i])
		}
	}
}

func TestCollectRefreshTokens_Empty(t *testing.T) {
	if got := collectRefreshTokens(adminTokenRefreshRequest{}); len(got) != 0 {
		t.Fatalf("collectRefreshTokens empty len=%d want=0", len(got))
	}
}

func TestHandleAdminTokensRefresh_MethodNotAllowed(t *testing.T) {
	h := &Handler{}
	req := httptest.NewRequest(http.MethodGet, "/api/v1/admin/tokens/refresh", strings.NewReader(""))
	rec := httptest.NewRecorder()
	h.HandleAdminTokensRefresh(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status=%d want=%d", rec.Code, http.StatusMethodNotAllowed)
	}
}

func TestHandleAdminTokensRefreshAsync_MethodNotAllowed(t *testing.T) {
	h := &Handler{}
	req := httptest.NewRequest(http.MethodGet, "/api/v1/admin/tokens/refresh/async", strings.NewReader(""))
	rec := httptest.NewRecorder()
	h.HandleAdminTokensRefreshAsync(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status=%d want=%d", rec.Code, http.StatusMethodNotAllowed)
	}
}

func TestCollectGrokAccountsByTokenUsesCanonicalVisibleWebSource(t *testing.T) {
	webHighID := &store.Account{ID: 9, Name: "web-high", AccountType: "grok", CredentialType: "sso", GrokProvider: ProviderWeb, ClientCookie: "sso=shared"}
	webLowID := &store.Account{ID: 3, Name: "web-low", AccountType: "grok", CredentialType: "sso", GrokProvider: ProviderWeb, ClientCookie: "sso=shared"}
	hiddenConsole := &store.Account{ID: 1, Name: "hidden-console", AccountType: "grok", CredentialType: "sso", GrokProvider: ProviderConsole, GrokSSOParentID: webLowID.ID, ClientCookie: "sso=shared", StatusCode: "429", UsageCurrent: 7}
	standaloneConsole := &store.Account{ID: 2, Name: "standalone-console", AccountType: "grok", CredentialType: "sso", GrokProvider: ProviderConsole, ClientCookie: "sso=standalone"}
	build := &store.Account{ID: 4, Name: "build", AccountType: "grok", CredentialType: "oauth", GrokProvider: ProviderBuild, OAuthAccessToken: "access"}

	groups := collectGrokAccountsByToken([]*store.Account{hiddenConsole, standaloneConsole, webHighID, build, webLowID})
	if len(groups) != 1 || len(groups["shared"]) != 1 || groups["shared"][0].ID != webLowID.ID {
		t.Fatalf("groups=%#v want canonical visible Web source only", groups)
	}
}

func TestRunTokenRefreshBatchUpdatesOnlyWebSource(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != defaultRateLimitsPath {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]interface{}{"remainingQueries": 20, "totalQueries": 50})
	}))
	defer upstream.Close()

	h, s, mini := setupValidationHandler(t)
	defer func() {
		_ = s.Close()
		mini.Close()
	}()
	h.client = New(&config.Config{GrokAPIBaseURL: upstream.URL})

	web := &store.Account{Name: "web", AccountType: "grok", CredentialType: "sso", GrokProvider: ProviderWeb, ClientCookie: "sso=shared", Enabled: true, UsageLimit: 50, UsageCurrent: 12}
	if err := s.CreateAccount(context.Background(), web); err != nil {
		t.Fatalf("CreateAccount(web) error = %v", err)
	}
	console := &store.Account{Name: "console", AccountType: "grok", CredentialType: "sso", GrokProvider: ProviderConsole, GrokSSOParentID: web.ID, ClientCookie: web.ClientCookie, Enabled: true, StatusCode: "429", UsageLimit: 90, UsageCurrent: 7, RequestCount: 4, GrokModels: []string{"console/grok-4.20"}}
	if err := s.CreateAccount(context.Background(), console); err != nil {
		t.Fatalf("CreateAccount(console) error = %v", err)
	}

	groups := collectGrokAccountsByToken([]*store.Account{console, web})
	results := h.runTokenRefreshBatch(context.Background(), []string{"shared"}, "", groups, 1, nil)
	if !results["shared"] {
		t.Fatalf("refresh result=%#v want success", results)
	}

	gotWeb, err := s.GetAccount(context.Background(), web.ID)
	if err != nil {
		t.Fatalf("GetAccount(web) error = %v", err)
	}
	if gotWeb.StatusCode != "" || gotWeb.UsageLimit != 50 || gotWeb.UsageCurrent != 20 {
		t.Fatalf("Web refresh was not persisted: %#v", gotWeb)
	}
	gotConsole, err := s.GetAccount(context.Background(), console.ID)
	if err != nil {
		t.Fatalf("GetAccount(console) error = %v", err)
	}
	if gotConsole.StatusCode != "429" || gotConsole.UsageLimit != 90 || gotConsole.UsageCurrent != 7 || gotConsole.RequestCount != 4 || len(gotConsole.GrokModels) != 1 {
		t.Fatalf("refresh changed internal Console runtime state: %#v", gotConsole)
	}
}

func TestUpdateGrokUsageAccount_SuccessClearsStatus(t *testing.T) {
	resetAt := time.Now().Add(10 * time.Minute).UTC().Truncate(time.Second)
	acc := &store.Account{
		StatusCode:   "429",
		LastAttempt:  time.Now().Add(-time.Minute),
		UsageLimit:   1,
		UsageCurrent: 1,
	}
	info := &RateLimitInfo{
		Limit:        120,
		HasLimit:     true,
		Remaining:    25,
		HasRemaining: true,
		ResetAt:      resetAt,
	}

	updateGrokUsageAccount(acc, info, "")

	if acc.StatusCode != "" {
		t.Fatalf("status=%q want empty", acc.StatusCode)
	}
	if !acc.LastAttempt.IsZero() {
		t.Fatalf("last_attempt=%v want zero", acc.LastAttempt)
	}
	if acc.UsageLimit != 120 {
		t.Fatalf("usage_limit=%v want=120", acc.UsageLimit)
	}
	if acc.UsageCurrent != 25 {
		t.Fatalf("usage_current=%v want=25", acc.UsageCurrent)
	}
	if !acc.QuotaResetAt.Equal(resetAt) {
		t.Fatalf("quota_reset_at=%v want=%v", acc.QuotaResetAt, resetAt)
	}
}

func TestUpdateGrokUsageAccount_IncompleteInfoDoesNotOverwriteQuota(t *testing.T) {
	acc := &store.Account{
		UsageLimit:   80,
		UsageCurrent: 12,
	}
	info := &RateLimitInfo{
		Limit:    120,
		HasLimit: true,
	}

	updateGrokUsageAccount(acc, info, "")

	if acc.UsageLimit != 80 || acc.UsageCurrent != 12 {
		t.Fatalf("quota should remain unchanged on incomplete info, got limit=%v current=%v", acc.UsageLimit, acc.UsageCurrent)
	}
}

func TestUpdateGrokUsageAccount_FailureSetsStatusAndAttempt(t *testing.T) {
	acc := &store.Account{}
	updateGrokUsageAccount(acc, nil, "500")

	if acc.StatusCode != "500" {
		t.Fatalf("status=%q want=500", acc.StatusCode)
	}
	if acc.LastAttempt.IsZero() {
		t.Fatalf("last_attempt should be set on failure")
	}
}

func TestUpdateGrokUsageAccount_RemainingOnlyUsesDefaultQuota(t *testing.T) {
	acc := &store.Account{Subscription: "super"}
	info := &RateLimitInfo{
		Remaining:    118,
		HasRemaining: true,
	}

	updateGrokUsageAccount(acc, info, "")

	if acc.UsageLimit != 140 {
		t.Fatalf("usage_limit=%v want=140", acc.UsageLimit)
	}
	if acc.UsageCurrent != 118 {
		t.Fatalf("usage_current=%v want=118", acc.UsageCurrent)
	}
}
