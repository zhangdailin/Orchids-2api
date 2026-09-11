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

	"orchids-api/internal/grok"
	"orchids-api/internal/store"
)

func TestGrokSSOSourceCreatesLinkedConsoleAccount(t *testing.T) {
	a, s, cleanup := newTestAPI(t)
	defer cleanup()

	// Older callers may still submit "console". It is accepted as one SSO
	// credential and normalized into a visible Web source plus an internal
	// Console runtime companion. Public callers cannot assign a parent link.
	req := httptest.NewRequest(http.MethodPost, "/api/accounts", strings.NewReader(`{"account_type":"grok","name":"primary","grok_provider":"console","grok_sso_parent_id":999,"client_cookie":"sso=shared-sso; cf_clearance=web-clearance","enabled":true,"weight":3,"max_concurrent":4,"nsfw_enabled":false}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Account-Sync", "async")
	rec := httptest.NewRecorder()
	a.HandleAccounts(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status=%d want 201 body=%s", rec.Code, rec.Body.String())
	}

	accounts, err := s.ListAccounts(context.Background())
	if err != nil {
		t.Fatalf("ListAccounts() error = %v", err)
	}
	if len(accounts) != 2 {
		t.Fatalf("stored account count=%d want 2", len(accounts))
	}
	var web, console *store.Account
	for _, acc := range accounts {
		switch grok.ProviderForAccount(acc) {
		case grok.ProviderWeb:
			web = acc
		case grok.ProviderConsole:
			console = acc
		}
	}
	if web == nil || console == nil || console.GrokSSOParentID != web.ID {
		t.Fatalf("stored provider link missing: %#v", accounts)
	}
	if web.GrokSSOParentID != 0 || console.ClientCookie != web.ClientCookie || console.UserID != web.UserID || console.Email != web.Email || console.TeamID != web.TeamID || console.Weight != web.Weight || console.MaxConcurrent != web.MaxConcurrent || console.Enabled != web.Enabled || console.NSFWEnabled != web.NSFWEnabled {
		t.Fatalf("initial linked Console settings mismatch: web=%#v console=%#v", web, console)
	}
	if console.StatusCode != "" || console.UsageCurrent != 0 || len(console.GrokModels) != 0 {
		t.Fatalf("linked Console inherited runtime state: %#v", console)
	}

	getReq := httptest.NewRequest(http.MethodGet, "/api/accounts", nil)
	getRec := httptest.NewRecorder()
	a.HandleAccounts(getRec, getReq)
	if getRec.Code != http.StatusOK {
		t.Fatalf("list status=%d body=%s", getRec.Code, getRec.Body.String())
	}
	var visible []map[string]any
	if err := json.Unmarshal(getRec.Body.Bytes(), &visible); err != nil {
		t.Fatalf("decode visible accounts: %v", err)
	}
	if len(visible) != 1 || visible[0]["grok_provider"] != grok.ProviderWeb {
		t.Fatalf("visible accounts=%#v want one Web source", visible)
	}
}

func TestGrokSSOSourceCreationRepairsExistingMissingConsoleCompanion(t *testing.T) {
	a, s, cleanup := newTestAPI(t)
	defer cleanup()

	web := &store.Account{
		Name:           "existing-web",
		AccountType:    "grok",
		CredentialType: "sso",
		GrokProvider:   grok.ProviderWeb,
		ClientCookie:   "sso=repairable",
		Enabled:        true,
		Weight:         1,
	}
	if err := s.CreateAccount(context.Background(), web); err != nil {
		t.Fatalf("CreateAccount(web) error = %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/api/accounts", strings.NewReader(`{"account_type":"grok","name":"retry","client_cookie":"sso=repairable","enabled":true}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Account-Sync", "async")
	rec := httptest.NewRecorder()
	a.HandleAccounts(rec, req)
	if rec.Code != http.StatusConflict {
		t.Fatalf("status=%d want 409 body=%s", rec.Code, rec.Body.String())
	}

	accounts, err := s.ListAccounts(context.Background())
	if err != nil {
		t.Fatalf("ListAccounts() error = %v", err)
	}
	if len(accounts) != 2 {
		t.Fatalf("stored account count=%d want repaired pair", len(accounts))
	}
	var console *store.Account
	for _, acc := range accounts {
		if grok.ProviderForAccount(acc) == grok.ProviderConsole {
			console = acc
			break
		}
	}
	if console == nil || console.GrokSSOParentID != web.ID {
		t.Fatalf("missing repaired Console companion: %#v", accounts)
	}
}

func TestGrokSSOSourceCredentialUpdatePreservesConsoleRuntimeState(t *testing.T) {
	a, s, cleanup := newTestAPI(t)
	defer cleanup()

	web := &store.Account{
		Name:           "web",
		AccountType:    "grok",
		CredentialType: "sso",
		GrokProvider:   grok.ProviderWeb,
		ClientCookie:   "sso=old-sso",
		Enabled:        true,
		Weight:         1,
		MaxConcurrent:  1,
		NSFWEnabled:    true,
	}
	if err := s.CreateAccount(context.Background(), web); err != nil {
		t.Fatalf("CreateAccount(web) error = %v", err)
	}
	console := &store.Account{
		Name:            "console",
		AccountType:     "grok",
		CredentialType:  "sso",
		GrokProvider:    grok.ProviderConsole,
		GrokSSOParentID: web.ID,
		ClientCookie:    web.ClientCookie,
		Enabled:         false,
		Weight:          7,
		MaxConcurrent:   9,
		NSFWEnabled:     false,
		StatusCode:      "429",
		UsageCurrent:    9,
		GrokModels:      []string{"console/grok-4.20"},
	}
	if err := s.CreateAccount(context.Background(), console); err != nil {
		t.Fatalf("CreateAccount(console) error = %v", err)
	}

	req := httptest.NewRequest(http.MethodPut, "/api/accounts/"+strconv.FormatInt(web.ID, 10), strings.NewReader(`{"account_type":"grok","client_cookie":"sso=new-sso","user_id":"new-user","email":"new@example.test","team_id":"team-new","enabled":true,"weight":2,"max_concurrent":4,"nsfw_enabled":true}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	a.HandleAccountByID(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d want 200 body=%s", rec.Code, rec.Body.String())
	}

	updatedConsole, err := s.GetAccount(context.Background(), console.ID)
	if err != nil {
		t.Fatalf("GetAccount(console) error = %v", err)
	}
	updatedWeb, err := s.GetAccount(context.Background(), web.ID)
	if err != nil {
		t.Fatalf("GetAccount(web) error = %v", err)
	}
	if updatedConsole.ClientCookie != "sso=new-sso" || updatedConsole.UserID != "new-user" || updatedConsole.Email != "new@example.test" || updatedConsole.TeamID != "team-new" {
		t.Fatalf("console identity=%#v want source identity", updatedConsole)
	}
	if updatedConsole.Enabled != updatedWeb.Enabled || updatedConsole.Weight != 2 || updatedConsole.MaxConcurrent != 4 || !updatedConsole.NSFWEnabled || updatedConsole.StatusCode != "429" || updatedConsole.UsageCurrent != 9 || len(updatedConsole.GrokModels) != 1 {
		t.Fatalf("console source/runtime state mismatch: %#v", updatedConsole)
	}
}

func TestLinkedGrokConsoleAccountIsInternalToManagementAPI(t *testing.T) {
	a, s, cleanup := newTestAPI(t)
	defer cleanup()

	web := &store.Account{Name: "web", AccountType: "grok", CredentialType: "sso", GrokProvider: grok.ProviderWeb, ClientCookie: "sso=shared", Enabled: true, Weight: 2}
	if err := s.CreateAccount(context.Background(), web); err != nil {
		t.Fatalf("CreateAccount(web) error = %v", err)
	}
	console := &store.Account{
		Name:               "console",
		AccountType:        "grok",
		CredentialType:     "sso",
		GrokProvider:       grok.ProviderConsole,
		GrokSSOParentID:    web.ID,
		ClientCookie:       web.ClientCookie,
		Enabled:            true,
		Weight:             web.Weight,
		StatusCode:         "429",
		UsageCurrent:       7,
		UsageLimit:         20,
		GrokModels:         []string{"console/grok-4.20"},
		GrokModelsSyncedAt: time.Now().UTC().Round(time.Second),
	}
	if err := s.CreateAccount(context.Background(), console); err != nil {
		t.Fatalf("CreateAccount(console) error = %v", err)
	}

	for _, tc := range []struct {
		name   string
		method string
		path   string
		body   string
	}{
		{"get", http.MethodGet, "", ""},
		{"usage", http.MethodGet, "/usage", ""},
		{"check", http.MethodGet, "/check", ""},
		{"refresh", http.MethodGet, "/refresh", ""},
		{"verify", http.MethodGet, "/verify", ""},
		{"unknown path", http.MethodGet, "/internal", ""},
		{"put", http.MethodPut, "", `{"account_type":"grok","enabled":false,"weight":7}`},
		{"delete", http.MethodDelete, "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(tc.method, "/api/accounts/"+strconv.FormatInt(console.ID, 10)+tc.path, strings.NewReader(tc.body))
			req.Header.Set("Content-Type", "application/json")
			rec := httptest.NewRecorder()
			a.HandleAccountByID(rec, req)
			if rec.Code != http.StatusNotFound {
				t.Fatalf("status=%d want 404 body=%s", rec.Code, rec.Body.String())
			}
		})
	}

	updatedConsole, err := s.GetAccount(context.Background(), console.ID)
	if err != nil {
		t.Fatalf("GetAccount(console) error = %v", err)
	}
	if !updatedConsole.Enabled || updatedConsole.Weight != web.Weight || updatedConsole.StatusCode != "429" || updatedConsole.UsageCurrent != 7 || len(updatedConsole.GrokModels) != 1 {
		t.Fatalf("management request changed hidden Console: %#v", updatedConsole)
	}
	a.checkMu.Lock()
	defer a.checkMu.Unlock()
	if a.checkInFlight[console.ID] || a.checkFailCount[console.ID] != 0 || !a.checkNextAllowed[console.ID].IsZero() {
		t.Fatalf("hidden Console check touched bookkeeping: in_flight=%t failures=%d next=%v", a.checkInFlight[console.ID], a.checkFailCount[console.ID], a.checkNextAllowed[console.ID])
	}
}

func TestDeletingWebSourceDeletesLinkedConsoleAccount(t *testing.T) {
	a, s, cleanup := newTestAPI(t)
	defer cleanup()

	web := &store.Account{Name: "web", AccountType: "grok", CredentialType: "sso", GrokProvider: grok.ProviderWeb, ClientCookie: "sso=shared", Enabled: true, Weight: 1}
	if err := s.CreateAccount(context.Background(), web); err != nil {
		t.Fatalf("CreateAccount(web) error = %v", err)
	}
	console := &store.Account{Name: "console", AccountType: "grok", CredentialType: "sso", GrokProvider: grok.ProviderConsole, GrokSSOParentID: web.ID, ClientCookie: web.ClientCookie, Enabled: true, Weight: 1}
	if err := s.CreateAccount(context.Background(), console); err != nil {
		t.Fatalf("CreateAccount(console) error = %v", err)
	}

	directConsoleDelete := httptest.NewRecorder()
	a.HandleAccountByID(directConsoleDelete, httptest.NewRequest(http.MethodDelete, "/api/accounts/"+strconv.FormatInt(console.ID, 10), nil))
	if directConsoleDelete.Code != http.StatusNotFound {
		t.Fatalf("Console delete status=%d want 404 body=%s", directConsoleDelete.Code, directConsoleDelete.Body.String())
	}
	if _, err := s.GetAccount(context.Background(), console.ID); err != nil {
		t.Fatalf("linked Console was deleted directly: %v", err)
	}

	req := httptest.NewRequest(http.MethodDelete, "/api/accounts/"+strconv.FormatInt(web.ID, 10), nil)
	rec := httptest.NewRecorder()
	a.HandleAccountByID(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status=%d want 204 body=%s", rec.Code, rec.Body.String())
	}
	if _, err := s.GetAccount(context.Background(), web.ID); err == nil {
		t.Fatal("Web source still exists after delete")
	}
	if _, err := s.GetAccount(context.Background(), console.ID); err == nil {
		t.Fatal("linked Console account still exists after source delete")
	}
}

func TestGrokAvailabilityCountsEnabledProvidersWithoutAccountDetails(t *testing.T) {
	a, s, cleanup := newTestAPI(t)
	defer cleanup()

	web := &store.Account{Name: "web", AccountType: "grok", CredentialType: "sso", GrokProvider: grok.ProviderWeb, ClientCookie: "sso=web", Enabled: true}
	console := &store.Account{Name: "console", AccountType: "grok", CredentialType: "sso", GrokProvider: grok.ProviderConsole, GrokSSOParentID: 1, ClientCookie: "sso=web", Enabled: true}
	build := &store.Account{Name: "build", AccountType: "grok", CredentialType: "oauth", GrokProvider: grok.ProviderBuild, OAuthAccessToken: "access", OAuthRefreshToken: "refresh", Enabled: true}
	disabledConsole := &store.Account{Name: "disabled-console", AccountType: "grok", CredentialType: "sso", GrokProvider: grok.ProviderConsole, ClientCookie: "sso=other", Enabled: false}
	for _, acc := range []*store.Account{web, console, build, disabledConsole} {
		if err := s.CreateAccount(context.Background(), acc); err != nil {
			t.Fatalf("CreateAccount(%s) error = %v", acc.Name, err)
		}
	}
	console.GrokSSOParentID = web.ID
	if err := s.UpdateAccount(context.Background(), console); err != nil {
		t.Fatalf("UpdateAccount(console) error = %v", err)
	}

	for _, method := range []string{http.MethodGet, http.MethodPost} {
		t.Run(method, func(t *testing.T) {
			rec := httptest.NewRecorder()
			a.HandleGrokAvailability(rec, httptest.NewRequest(method, "/api/grok/availability", nil))
			if method != http.MethodGet {
				if rec.Code != http.StatusMethodNotAllowed {
					t.Fatalf("status=%d want 405 body=%s", rec.Code, rec.Body.String())
				}
				return
			}
			if rec.Code != http.StatusOK {
				t.Fatalf("status=%d want 200 body=%s", rec.Code, rec.Body.String())
			}
			var response struct {
				Counts map[string]int `json:"counts"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
				t.Fatalf("decode availability: %v", err)
			}
			if response.Counts[grok.ProviderBuild] != 1 || response.Counts[grok.ProviderWeb] != 1 || response.Counts[grok.ProviderConsole] != 1 || len(response.Counts) != 3 {
				t.Fatalf("availability counts=%#v want one enabled provider each", response.Counts)
			}
			if strings.Contains(rec.Body.String(), `"name"`) || strings.Contains(rec.Body.String(), `"id"`) || strings.Contains(rec.Body.String(), "grok_sso_parent_id") {
				t.Fatalf("availability leaked account detail: %s", rec.Body.String())
			}
		})
	}
}

func TestGrokSSOExportAndImportKeepConsoleCompanionInternal(t *testing.T) {
	a, s, cleanup := newTestAPI(t)
	defer cleanup()

	web := &store.Account{Name: "web", AccountType: "grok", CredentialType: "sso", GrokProvider: grok.ProviderWeb, ClientCookie: "sso=exported", Enabled: true}
	if err := s.CreateAccount(context.Background(), web); err != nil {
		t.Fatalf("CreateAccount(web) error = %v", err)
	}
	console := &store.Account{Name: "console", AccountType: "grok", CredentialType: "sso", GrokProvider: grok.ProviderConsole, GrokSSOParentID: web.ID, ClientCookie: web.ClientCookie, Enabled: true}
	if err := s.CreateAccount(context.Background(), console); err != nil {
		t.Fatalf("CreateAccount(console) error = %v", err)
	}
	malformed := &store.Account{Name: "malformed", AccountType: "grok", CredentialType: "sso", GrokProvider: grok.ProviderConsole, GrokSSOParentID: web.ID, Enabled: true}
	if err := s.CreateAccount(context.Background(), malformed); err != nil {
		t.Fatalf("CreateAccount(malformed) error = %v", err)
	}

	listRec := httptest.NewRecorder()
	a.HandleAccounts(listRec, httptest.NewRequest(http.MethodGet, "/api/accounts", nil))
	if listRec.Code != http.StatusOK || strings.Contains(listRec.Body.String(), console.Name) || strings.Contains(listRec.Body.String(), malformed.Name) {
		t.Fatalf("management list exposed internal child: status=%d body=%s", listRec.Code, listRec.Body.String())
	}

	exportRec := httptest.NewRecorder()
	a.HandleExport(exportRec, httptest.NewRequest(http.MethodGet, "/api/export", nil))
	if exportRec.Code != http.StatusOK {
		t.Fatalf("export status=%d body=%s", exportRec.Code, exportRec.Body.String())
	}
	var exported ExportData
	if err := json.Unmarshal(exportRec.Body.Bytes(), &exported); err != nil {
		t.Fatalf("decode export: %v", err)
	}
	if len(exported.Accounts) != 1 || exported.Accounts[0].GrokProvider != grok.ProviderWeb || exported.Accounts[0].GrokSSOParentID != 0 {
		t.Fatalf("exported accounts=%#v want visible Web source only", exported.Accounts)
	}

	importRec := httptest.NewRecorder()
	a.HandleImport(importRec, httptest.NewRequest(http.MethodPost, "/api/import", strings.NewReader(`{"accounts":[{"account_type":"grok","name":"external-child","credential_type":"sso","grok_provider":"console","grok_sso_parent_id":99,"client_cookie":"sso=skip"}]}`)))
	if importRec.Code != http.StatusOK {
		t.Fatalf("import status=%d body=%s", importRec.Code, importRec.Body.String())
	}
	var result ImportResult
	if err := json.Unmarshal(importRec.Body.Bytes(), &result); err != nil {
		t.Fatalf("decode import result: %v", err)
	}
	if result.Total != 1 || result.Imported != 0 || result.Skipped != 1 {
		t.Fatalf("import result=%+v want marked child skipped", result)
	}
}

func TestGrokSSOImportCreatesLinkedConsoleAccount(t *testing.T) {
	a, s, cleanup := newTestAPI(t)
	defer cleanup()

	req := httptest.NewRequest(http.MethodPost, "/api/import", strings.NewReader(`{"accounts":[{"account_type":"grok","name":"imported","grok_provider":"console","client_cookie":"sso=imported-sso","enabled":true}]}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	a.HandleImport(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d want 200 body=%s", rec.Code, rec.Body.String())
	}

	accounts, err := s.ListAccounts(context.Background())
	if err != nil {
		t.Fatalf("ListAccounts() error = %v", err)
	}
	if len(accounts) != 2 {
		t.Fatalf("stored account count=%d want 2", len(accounts))
	}
	var web, console *store.Account
	for _, acc := range accounts {
		switch grok.ProviderForAccount(acc) {
		case grok.ProviderWeb:
			web = acc
		case grok.ProviderConsole:
			console = acc
		}
	}
	if web == nil || console == nil || console.GrokSSOParentID != web.ID {
		t.Fatalf("imported provider link missing: %#v", accounts)
	}
}

func TestGrokSSOImportSkipsExistingSourceAfterRepair(t *testing.T) {
	a, s, cleanup := newTestAPI(t)
	defer cleanup()

	web := &store.Account{Name: "legacy-web", AccountType: "grok", CredentialType: "sso", GrokProvider: grok.ProviderWeb, ClientCookie: "sso=import-retry", Enabled: true}
	if err := s.CreateAccount(context.Background(), web); err != nil {
		t.Fatalf("CreateAccount(web) error = %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/api/import", strings.NewReader(`{"accounts":[{"account_type":"grok","name":"duplicate","client_cookie":"sso=import-retry","enabled":true}]}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	a.HandleImport(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d want 200 body=%s", rec.Code, rec.Body.String())
	}
	var result ImportResult
	if err := json.Unmarshal(rec.Body.Bytes(), &result); err != nil {
		t.Fatalf("decode import result: %v", err)
	}
	if result.Imported != 0 || result.Skipped != 1 {
		t.Fatalf("import result=%+v want duplicate skipped", result)
	}

	accounts, err := s.ListAccounts(context.Background())
	if err != nil {
		t.Fatalf("ListAccounts() error = %v", err)
	}
	if len(accounts) != 2 {
		t.Fatalf("stored account count=%d want repaired pair", len(accounts))
	}
	for _, acc := range accounts {
		if grok.ProviderForAccount(acc) == grok.ProviderConsole && acc.GrokSSOParentID != web.ID {
			t.Fatalf("Console companion=%#v want parent %d", acc, web.ID)
		}
	}
}

func TestGrokSSOSourceCannotTransitionWithLinkedConsoleCompanion(t *testing.T) {
	a, s, cleanup := newTestAPI(t)
	defer cleanup()

	web := &store.Account{Name: "web", AccountType: "grok", CredentialType: "sso", GrokProvider: grok.ProviderWeb, ClientCookie: "sso=shared", Enabled: true, Weight: 2}
	if err := s.CreateAccount(context.Background(), web); err != nil {
		t.Fatalf("CreateAccount(web) error = %v", err)
	}
	console := &store.Account{Name: "console", AccountType: "grok", CredentialType: "sso", GrokProvider: grok.ProviderConsole, GrokSSOParentID: web.ID, ClientCookie: web.ClientCookie, Enabled: true, Weight: web.Weight, StatusCode: "429", UsageCurrent: 7, GrokModels: []string{"console/grok-4.20"}}
	if err := s.CreateAccount(context.Background(), console); err != nil {
		t.Fatalf("CreateAccount(console) error = %v", err)
	}

	for _, tc := range []struct {
		name string
		body string
	}{
		{"account type", `{"account_type":"puter","name":"replacement","enabled":true}`},
		{"credential mode", `{"account_type":"grok","credential_type":"oauth","oauth_access_token":"replacement-access","oauth_refresh_token":"replacement-refresh","enabled":true}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodPut, "/api/accounts/"+strconv.FormatInt(web.ID, 10), strings.NewReader(tc.body))
			req.Header.Set("Content-Type", "application/json")
			a.HandleAccountByID(rec, req)
			if rec.Code != http.StatusConflict {
				t.Fatalf("status=%d want 409 body=%s", rec.Code, rec.Body.String())
			}
		})
	}

	unchangedWeb, err := s.GetAccount(context.Background(), web.ID)
	if err != nil {
		t.Fatalf("GetAccount(web) error = %v", err)
	}
	unchangedConsole, err := s.GetAccount(context.Background(), console.ID)
	if err != nil {
		t.Fatalf("GetAccount(console) error = %v", err)
	}
	if unchangedWeb.AccountType != "grok" || grok.ProviderForAccount(unchangedWeb) != grok.ProviderWeb || unchangedWeb.ClientCookie != web.ClientCookie ||
		unchangedConsole.GrokSSOParentID != web.ID || unchangedConsole.StatusCode != "429" || unchangedConsole.UsageCurrent != 7 || len(unchangedConsole.GrokModels) != 1 {
		t.Fatalf("rejected transition changed linked pair: web=%#v console=%#v", unchangedWeb, unchangedConsole)
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPut, "/api/accounts/"+strconv.FormatInt(web.ID, 10), strings.NewReader(`{"account_type":"grok","client_cookie":"sso=shared","enabled":false,"weight":4}`))
	req.Header.Set("Content-Type", "application/json")
	a.HandleAccountByID(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("unchanged SSO status=%d want 200 body=%s", rec.Code, rec.Body.String())
	}
	updatedConsole, err := s.GetAccount(context.Background(), console.ID)
	if err != nil {
		t.Fatalf("GetAccount(updated console) error = %v", err)
	}
	if updatedConsole.Enabled || updatedConsole.Weight != 4 || updatedConsole.StatusCode != "429" || updatedConsole.UsageCurrent != 7 || len(updatedConsole.GrokModels) != 1 {
		t.Fatalf("SSO source update did not preserve Console runtime: %#v", updatedConsole)
	}
}

func TestEnsureGrokSSOProviderViewsRepairsMalformedLinkedConsoleCompanion(t *testing.T) {
	a, s, cleanup := newTestAPI(t)
	defer cleanup()

	web := &store.Account{Name: "web", AccountType: "grok", CredentialType: "sso", GrokProvider: grok.ProviderWeb, ClientCookie: "sso=shared", Enabled: true, Weight: 3}
	if err := s.CreateAccount(context.Background(), web); err != nil {
		t.Fatalf("CreateAccount(web) error = %v", err)
	}
	brokenChild := &store.Account{Name: "broken-console", AccountType: "grok", CredentialType: "sso", GrokProvider: grok.ProviderConsole, GrokSSOParentID: web.ID, ClientCookie: "", Enabled: false, Weight: 9, StatusCode: "429", UsageCurrent: 7, GrokModels: []string{"console/grok-4.20"}}
	if err := s.CreateAccount(context.Background(), brokenChild); err != nil {
		t.Fatalf("CreateAccount(broken child) error = %v", err)
	}

	if err := a.EnsureGrokSSOProviderViews(context.Background()); err != nil {
		t.Fatalf("EnsureGrokSSOProviderViews() error = %v", err)
	}
	accounts, err := s.ListAccounts(context.Background())
	if err != nil || len(accounts) != 2 {
		t.Fatalf("reconciliation created duplicate companion: accounts=%#v err=%v", accounts, err)
	}
	got, err := s.GetAccount(context.Background(), brokenChild.ID)
	if err != nil {
		t.Fatalf("GetAccount(broken child) error = %v", err)
	}
	if got.ClientCookie != web.ClientCookie || got.Enabled != web.Enabled || got.Weight != web.Weight || got.StatusCode != "429" || got.UsageCurrent != 7 || len(got.GrokModels) != 1 {
		t.Fatalf("malformed child repair changed Console runtime or skipped source synchronization: %#v", got)
	}
}

func TestEnsureGrokSSOProviderViewsDeterministicallyRemovesRedundantConsoles(t *testing.T) {
	a, s, cleanup := newTestAPI(t)
	defer cleanup()

	web := &store.Account{Name: "web", AccountType: "grok", CredentialType: "sso", GrokProvider: grok.ProviderWeb, ClientCookie: "sso=shared", Enabled: true, Weight: 3, MaxConcurrent: 4, NSFWEnabled: false}
	if err := s.CreateAccount(context.Background(), web); err != nil {
		t.Fatalf("CreateAccount(web) error = %v", err)
	}
	canonical := &store.Account{Name: "canonical", AccountType: "grok", CredentialType: "sso", GrokProvider: grok.ProviderConsole, GrokSSOParentID: web.ID, ClientCookie: web.ClientCookie, Enabled: false, Weight: 8, StatusCode: "429", UsageCurrent: 6, RequestCount: 9, GrokModels: []string{"console/grok-4.20"}}
	unlinked := &store.Account{Name: "redundant-unlinked", AccountType: "grok", CredentialType: "sso", GrokProvider: grok.ProviderConsole, ClientCookie: web.ClientCookie, Enabled: true}
	duplicate := &store.Account{Name: "redundant-linked", AccountType: "grok", CredentialType: "sso", GrokProvider: grok.ProviderConsole, GrokSSOParentID: web.ID, ClientCookie: web.ClientCookie, Enabled: true}
	for _, acc := range []*store.Account{canonical, unlinked, duplicate} {
		if err := s.CreateAccount(context.Background(), acc); err != nil {
			t.Fatalf("CreateAccount(%s) error = %v", acc.Name, err)
		}
	}

	if err := a.EnsureGrokSSOProviderViews(context.Background()); err != nil {
		t.Fatalf("EnsureGrokSSOProviderViews() error = %v", err)
	}
	accounts, err := s.ListAccounts(context.Background())
	if err != nil {
		t.Fatalf("ListAccounts() error = %v", err)
	}
	if len(accounts) != 2 {
		t.Fatalf("account count=%d want canonical pair", len(accounts))
	}
	got, err := s.GetAccount(context.Background(), canonical.ID)
	if err != nil {
		t.Fatalf("GetAccount(canonical) error = %v", err)
	}
	if got.GrokSSOParentID != web.ID || got.ClientCookie != web.ClientCookie || got.Enabled != web.Enabled || got.Weight != web.Weight || got.MaxConcurrent != web.MaxConcurrent || got.NSFWEnabled != web.NSFWEnabled ||
		got.StatusCode != "429" || got.UsageCurrent != 6 || got.RequestCount != 9 || len(got.GrokModels) != 1 {
		t.Fatalf("canonical Console did not preserve runtime while synchronizing source fields: %#v", got)
	}
	if _, err := s.GetAccount(context.Background(), unlinked.ID); err == nil {
		t.Fatal("redundant unlinked Console still exists")
	}
	if _, err := s.GetAccount(context.Background(), duplicate.ID); err == nil {
		t.Fatal("redundant linked Console still exists")
	}
	if err := a.EnsureGrokSSOProviderViews(context.Background()); err != nil {
		t.Fatalf("second EnsureGrokSSOProviderViews() error = %v", err)
	}
	accounts, err = s.ListAccounts(context.Background())
	if err != nil || len(accounts) != 2 {
		t.Fatalf("second reconciliation was not idempotent: accounts=%#v err=%v", accounts, err)
	}
}

func TestEnsureGrokSSOProviderViewsRejectsConflictingSourceOwnership(t *testing.T) {
	a, s, cleanup := newTestAPI(t)
	defer cleanup()

	webOne := &store.Account{Name: "web-one", AccountType: "grok", CredentialType: "sso", GrokProvider: grok.ProviderWeb, ClientCookie: "sso=shared", Enabled: true}
	webTwo := &store.Account{Name: "web-two", AccountType: "grok", CredentialType: "sso", GrokProvider: grok.ProviderWeb, ClientCookie: "sso=shared", Enabled: true}
	for _, acc := range []*store.Account{webOne, webTwo} {
		if err := s.CreateAccount(context.Background(), acc); err != nil {
			t.Fatalf("CreateAccount(%s) error = %v", acc.Name, err)
		}
	}
	if err := a.EnsureGrokSSOProviderViews(context.Background()); err == nil || !strings.Contains(err.Error(), "conflicting visible Web sources") {
		t.Fatalf("duplicate sources error=%v want conflict", err)
	}

	// A linked child owned by another live source must also fail rather than be
	// reparented into the first source's provider pair.
	if err := s.DeleteAccount(context.Background(), webTwo.ID); err != nil {
		t.Fatalf("DeleteAccount(web-two) error = %v", err)
	}
	foreignSource := &store.Account{Name: "foreign", AccountType: "grok", CredentialType: "sso", GrokProvider: grok.ProviderWeb, ClientCookie: "sso=other", Enabled: true}
	if err := s.CreateAccount(context.Background(), foreignSource); err != nil {
		t.Fatalf("CreateAccount(foreign) error = %v", err)
	}
	foreignChild := &store.Account{Name: "foreign-child", AccountType: "grok", CredentialType: "sso", GrokProvider: grok.ProviderConsole, GrokSSOParentID: foreignSource.ID, ClientCookie: webOne.ClientCookie, Enabled: true, StatusCode: "401"}
	if err := s.CreateAccount(context.Background(), foreignChild); err != nil {
		t.Fatalf("CreateAccount(foreign child) error = %v", err)
	}
	if err := a.EnsureGrokSSOProviderViews(context.Background()); err == nil || !strings.Contains(err.Error(), "linked to different source") {
		t.Fatalf("foreign child error=%v want conflict", err)
	}
	got, err := s.GetAccount(context.Background(), foreignChild.ID)
	if err != nil || got.GrokSSOParentID != foreignSource.ID || got.StatusCode != "401" {
		t.Fatalf("foreign child was mutated: account=%#v err=%v", got, err)
	}
}

func TestEnsureGrokSSOProviderViewsMigratesConsoleOnlyAccount(t *testing.T) {
	a, s, cleanup := newTestAPI(t)
	defer cleanup()

	legacyConsole := &store.Account{
		Name:           "legacy-console",
		AccountType:    "grok",
		CredentialType: "sso",
		GrokProvider:   grok.ProviderConsole,
		ClientCookie:   "sso=legacy-sso",
		Enabled:        true,
		Weight:         3,
		MaxConcurrent:  5,
		NSFWEnabled:    false,
		StatusCode:     "429",
		UsageCurrent:   7,
		GrokModels:     []string{"grok-4.20"},
	}
	if err := s.CreateAccount(context.Background(), legacyConsole); err != nil {
		t.Fatalf("CreateAccount() error = %v", err)
	}
	if err := a.EnsureGrokSSOProviderViews(context.Background()); err != nil {
		t.Fatalf("EnsureGrokSSOProviderViews() error = %v", err)
	}

	accounts, err := s.ListAccounts(context.Background())
	if err != nil {
		t.Fatalf("ListAccounts() error = %v", err)
	}
	var web, console *store.Account
	for _, acc := range accounts {
		if grok.ProviderForAccount(acc) == grok.ProviderWeb {
			web = acc
		} else if grok.ProviderForAccount(acc) == grok.ProviderConsole {
			console = acc
		}
	}
	if web == nil || console == nil || console.GrokSSOParentID != web.ID {
		t.Fatalf("legacy Console was not converted into a linked provider account: %#v", accounts)
	}
	if console.ClientCookie != web.ClientCookie || console.Enabled != web.Enabled || console.Weight != web.Weight || console.MaxConcurrent != web.MaxConcurrent || console.NSFWEnabled != web.NSFWEnabled {
		t.Fatalf("legacy Console source-owned fields were not repaired: web=%#v console=%#v", web, console)
	}
	if console.StatusCode != "429" || console.UsageCurrent != 7 || len(console.GrokModels) != 1 {
		t.Fatalf("legacy Console runtime state changed: %#v", console)
	}
}
