package grok

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/goccy/go-json"

	"orchids-api/internal/store"
)

func TestCollectAdminTokenEntries(t *testing.T) {
	payload := map[string]interface{}{
		"ssoBasic": []interface{}{
			"t0",
			map[string]interface{}{
				"token":     "sso=t1",
				"status":    "cooling",
				"quota":     float64(80),
				"use_count": float64(2),
				"note":      "note-1",
			},
		},
		"ssoSuper": []interface{}{
			map[string]interface{}{
				"token":  "t2",
				"status": "invalid",
			},
		},
		"ssoHeavy": []interface{}{
			map[string]interface{}{
				"token": "t3",
			},
		},
	}
	entries := collectAdminTokenEntries(payload)
	if len(entries) != 4 {
		t.Fatalf("entries len=%d want=4", len(entries))
	}
	if entries[0].Token != "t0" || entries[1].Token != "t1" || entries[2].Token != "t2" || entries[3].Token != "t3" {
		t.Fatalf("unexpected tokens: %+v", entries)
	}
	if entries[1].Status != "cooling" || entries[1].Quota != 80 || entries[1].UseCount != 2 || entries[1].Note != "note-1" {
		t.Fatalf("unexpected entry[1]: %+v", entries[1])
	}
	if entries[2].Status != "expired" {
		t.Fatalf("invalid status should normalize to expired, got=%q", entries[2].Status)
	}
}

func TestAdminTokenViewUsesOnlyCanonicalVisibleWebSource(t *testing.T) {
	webHighID := &store.Account{ID: 8, Name: "web-high", AccountType: "grok", CredentialType: "sso", GrokProvider: ProviderWeb, ClientCookie: "sso=shared", Enabled: true, Subscription: "basic", UsageCurrent: 9, RequestCount: 5}
	webLowID := &store.Account{ID: 3, Name: "web-low", AccountType: "grok", CredentialType: "sso", GrokProvider: ProviderWeb, ClientCookie: "sso=shared", Enabled: true, Subscription: "super", UsageCurrent: 17, RequestCount: 2}
	hiddenConsole := &store.Account{ID: 1, Name: "hidden-console", AccountType: "grok", CredentialType: "sso", GrokProvider: ProviderConsole, GrokSSOParentID: webLowID.ID, ClientCookie: "sso=shared", Enabled: true, Subscription: "heavy", UsageCurrent: 99, RequestCount: 88}
	standaloneConsole := &store.Account{ID: 2, Name: "standalone-console", AccountType: "grok", CredentialType: "sso", GrokProvider: ProviderConsole, ClientCookie: "sso=standalone", Enabled: true}

	sources := CollectWebSSOSourcesByToken([]*store.Account{hiddenConsole, standaloneConsole, webHighID, webLowID}, true)
	got := sources["shared"]
	if len(sources) != 1 || got == nil || got.ID != webLowID.ID {
		t.Fatalf("sources=%#v want lowest-ID visible Web source only", sources)
	}
	if got.Name != "web-low" || got.UsageCurrent != 17 || got.RequestCount != 2 || inferTokenPool(got) != "ssoSuper" {
		t.Fatalf("token management would expose incorrect source data: %#v", got)
	}
}

func TestHandleAdminTokensListHidesConsoleAndUsesWebSource(t *testing.T) {
	h, s, mini := setupValidationHandler(t)
	defer func() {
		_ = s.Close()
		mini.Close()
	}()

	web := &store.Account{Name: "visible-web", AccountType: "grok", CredentialType: "sso", GrokProvider: ProviderWeb, ClientCookie: "sso=shared", Enabled: true, Subscription: "super", UsageCurrent: 17, RequestCount: 2}
	if err := s.CreateAccount(context.Background(), web); err != nil {
		t.Fatalf("CreateAccount(web) error = %v", err)
	}
	hidden := &store.Account{Name: "hidden-console", AccountType: "grok", CredentialType: "sso", GrokProvider: ProviderConsole, GrokSSOParentID: web.ID, ClientCookie: web.ClientCookie, Enabled: true, Subscription: "heavy", UsageCurrent: 99, RequestCount: 88}
	if err := s.CreateAccount(context.Background(), hidden); err != nil {
		t.Fatalf("CreateAccount(hidden) error = %v", err)
	}

	rec := httptest.NewRecorder()
	h.HandleAdminTokens(rec, httptest.NewRequest(http.MethodGet, "/api/v1/admin/tokens", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d want 200 body=%s", rec.Code, rec.Body.String())
	}
	var pools map[string][]map[string]interface{}
	if err := json.Unmarshal(rec.Body.Bytes(), &pools); err != nil {
		t.Fatalf("decode token list: %v", err)
	}
	items := pools["ssoSuper"]
	if len(pools) != 1 || len(items) != 1 || items[0]["token"] != "shared" || items[0]["note"] != "visible-web" || items[0]["quota"] != float64(17) || items[0]["use_count"] != float64(2) || strings.Contains(rec.Body.String(), "hidden-console") {
		t.Fatalf("token list leaked hidden Console state: %#v", pools)
	}
}

func TestNormalizeAdminTokenStatus(t *testing.T) {
	if got := normalizeAdminTokenStatus("active"); got != "active" {
		t.Fatalf("status active=%q", got)
	}
	if got := normalizeAdminTokenStatus("cooling"); got != "cooling" {
		t.Fatalf("status cooling=%q", got)
	}
	if got := normalizeAdminTokenStatus("invalid"); got != "expired" {
		t.Fatalf("status invalid=%q want expired", got)
	}
	if got := normalizeAdminTokenStatus("anything"); got != "active" {
		t.Fatalf("unknown status=%q want active", got)
	}
}

func TestApplyTokenEntryToAccount_QuotaIsRemaining(t *testing.T) {
	acc := &store.Account{}
	entry := adminTokenEntry{Token: "t-basic", Pool: "ssoBasic", Quota: 35}
	applyTokenEntryToAccount(acc, entry)
	if acc.UsageLimit != 35 {
		t.Fatalf("usage_limit=%v want=35", acc.UsageLimit)
	}
	if acc.UsageCurrent != 35 {
		t.Fatalf("usage_current=%v want=35", acc.UsageCurrent)
	}
}

func TestApplyTokenEntryToAccount_LitePool(t *testing.T) {
	acc := &store.Account{}
	entry := adminTokenEntry{Token: "t-lite", Pool: "ssoLite", Quota: 60}
	applyTokenEntryToAccount(acc, entry)
	if acc.Subscription != "lite" {
		t.Fatalf("subscription=%q want lite", acc.Subscription)
	}
	if acc.UsageLimit != 70 {
		t.Fatalf("usage_limit=%v want=70", acc.UsageLimit)
	}
	if got := inferTokenPool(acc); got != "ssoLite" {
		t.Fatalf("inferTokenPool=%q want ssoLite", got)
	}
	if got := grokAccountPool(acc); got != "lite" {
		t.Fatalf("grokAccountPool=%q want lite", got)
	}
}

func TestApplyTokenEntryToAccount_SuperPoolUses140DefaultQuota(t *testing.T) {
	acc := &store.Account{}
	entry := adminTokenEntry{Token: "t-super", Pool: "ssoSuper", Quota: 120}
	applyTokenEntryToAccount(acc, entry)
	if acc.UsageLimit != 140 {
		t.Fatalf("usage_limit=%v want=140", acc.UsageLimit)
	}
	if acc.UsageCurrent != 120 {
		t.Fatalf("usage_current=%v want=120", acc.UsageCurrent)
	}
}

func TestApplyTokenEntryToAccount_HeavyPool(t *testing.T) {
	acc := &store.Account{}
	entry := adminTokenEntry{Token: "t-heavy", Pool: "ssoHeavy", Quota: 100}
	applyTokenEntryToAccount(acc, entry)
	if acc.Subscription != "heavy" {
		t.Fatalf("subscription=%q want heavy", acc.Subscription)
	}
	if got := inferTokenPool(acc); got != "ssoHeavy" {
		t.Fatalf("inferTokenPool=%q want ssoHeavy", got)
	}
	if got := grokAccountPool(acc); got != "heavy" {
		t.Fatalf("grokAccountPool=%q want heavy", got)
	}
}
