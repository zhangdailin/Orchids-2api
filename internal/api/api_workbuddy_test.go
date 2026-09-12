package api

import (
	"context"
	"encoding/base64"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/goccy/go-json"

	"orchids-api/internal/config"
	"orchids-api/internal/store"
	"orchids-api/internal/workbuddy"
)

// workBuddyAuthDocument is the shape the WorkBuddy desktop client stores. The
// access token carries the Keycloak identity claims (sub / email), which is what
// lets a pasted document label the account without an extra profile call.
var workBuddyAuthDocument = func() string {
	encode := func(value interface{}) string {
		raw, _ := json.Marshal(value)
		return base64.RawURLEncoding.EncodeToString(raw)
	}
	token := encode(map[string]string{"alg": "RS256", "typ": "JWT"}) + "." +
		encode(map[string]interface{}{
			"sub":   "07ab88c8-5596-4257-8d21-e9fcbe3a3810",
			"email": "operator@example.com",
			"iss":   "https://www.workbuddy.ai/auth/realms/copilot",
			"exp":   time.Now().Add(48 * time.Hour).Unix(),
		}) + ".signature"
	return `{
  "account": {"uid": "07ab88c8-5596-4257-8d21-e9fcbe3a3810", "nickname": "operator@example.com"},
  "auth": {
    "accessToken": "` + token + `",
    "refreshToken": "durable-refresh-token",
    "expiresAt": 1820679206000
  }
}`
}()

func TestNormalizeWorkBuddyCredentials_SplitsDocumentIntoDedicatedFields(t *testing.T) {
	t.Parallel()

	acc := &store.Account{AccountType: "workbuddy", ClientCookie: workBuddyAuthDocument}
	if !NormalizeWorkBuddyCredentials(acc) {
		t.Fatal("NormalizeWorkBuddyCredentials() = false, want true")
	}
	if acc.WorkBuddyRefreshToken != "durable-refresh-token" {
		t.Fatalf("WorkBuddyRefreshToken = %q", acc.WorkBuddyRefreshToken)
	}
	if acc.WorkBuddyAccessToken == "" || workbuddy.DecodeClaims(acc.WorkBuddyAccessToken).Sub != "07ab88c8-5596-4257-8d21-e9fcbe3a3810" {
		t.Fatalf("WorkBuddyAccessToken = %q, want the embedded token", acc.WorkBuddyAccessToken)
	}
	if acc.WorkBuddyUID != "07ab88c8-5596-4257-8d21-e9fcbe3a3810" {
		t.Fatalf("WorkBuddyUID = %q", acc.WorkBuddyUID)
	}
	// The identity the table labels the row with comes from the token claims.
	if acc.Email != "operator@example.com" {
		t.Fatalf("Email = %q, want the address proven by the token claims", acc.Email)
	}
	if acc.Name != "operator@example.com" {
		t.Fatalf("Name = %q, want the signed-in address as the display name", acc.Name)
	}
	if acc.WorkBuddyExpiresAt.IsZero() {
		t.Fatal("WorkBuddyExpiresAt is zero, want the millisecond expiry converted")
	}
	// The generic credential slots are shared with other channels and must stay
	// empty so the refresh token is never echoed through the account list.
	for name, value := range map[string]string{
		"ClientCookie":  acc.ClientCookie,
		"Token":         acc.Token,
		"RefreshToken":  acc.RefreshToken,
		"SessionCookie": acc.SessionCookie,
	} {
		if value != "" {
			t.Fatalf("%s = %q, want empty", name, value)
		}
	}
}

func TestNormalizeWorkBuddyCredentials_AcceptsBareRefreshToken(t *testing.T) {
	t.Parallel()

	acc := &store.Account{AccountType: "workbuddy", ClientCookie: "opaque-refresh-token"}
	if !NormalizeWorkBuddyCredentials(acc) {
		t.Fatal("NormalizeWorkBuddyCredentials() = false, want true")
	}
	if acc.WorkBuddyRefreshToken != "opaque-refresh-token" {
		t.Fatalf("WorkBuddyRefreshToken = %q", acc.WorkBuddyRefreshToken)
	}
}

func TestNormalizeWorkBuddyCredentials_RejectsEmptyInput(t *testing.T) {
	t.Parallel()

	acc := &store.Account{AccountType: "workbuddy"}
	if NormalizeWorkBuddyCredentials(acc) {
		t.Fatal("NormalizeWorkBuddyCredentials() = true, want false for an empty credential")
	}
}

func TestRedactWorkBuddyOutput_HidesDurableSecrets(t *testing.T) {
	t.Parallel()

	acc := &store.Account{
		AccountType:           "workbuddy",
		WorkBuddyAccessToken:  "visible-access-token",
		WorkBuddyRefreshToken: "durable-refresh-token",
		WorkBuddyUID:          "uid-1",
		Token:                 "legacy-token",
		RefreshToken:          "legacy-refresh",
		SessionCookie:         "legacy-cookie",
		ClientCookie:          "legacy-client-cookie",
	}
	out := RedactWorkBuddyOutput(acc)
	if out == nil {
		t.Fatal("RedactWorkBuddyOutput() = nil")
	}
	if out.WorkBuddyRefreshToken != "" || out.RefreshToken != "" || out.SessionCookie != "" || out.Token != "" {
		t.Fatalf("redacted output still carries a secret: %+v", out)
	}
	if out.WorkBuddyAccessToken != "visible-access-token" || out.WorkBuddyUID != "uid-1" {
		t.Fatalf("redacted output dropped the visible fields: %+v", out)
	}
	if acc.WorkBuddyRefreshToken != "durable-refresh-token" {
		t.Fatal("redaction mutated the source account")
	}
}

func TestWorkBuddyCredentialKey_UsesDurableToken(t *testing.T) {
	t.Parallel()

	key := WorkBuddyCredentialKey(&store.Account{
		AccountType:           "workbuddy",
		WorkBuddyAccessToken:  "access",
		WorkBuddyRefreshToken: "refresh",
	})
	if key != "workbuddy:refresh" {
		t.Fatalf("key = %q, want the refresh token to identify the account", key)
	}

	accessOnly := WorkBuddyCredentialKey(&store.Account{WorkBuddyAccessToken: "access-only"})
	if accessOnly != "workbuddy:access-only" {
		t.Fatalf("key = %q, want the access token fallback", accessOnly)
	}

	if empty := WorkBuddyCredentialKey(&store.Account{}); empty != "" {
		t.Fatalf("key = %q, want empty for an account without credentials", empty)
	}
}

func TestVerifyWorkBuddyAccount_RequiresCredential(t *testing.T) {
	t.Parallel()

	_, httpStatus, err := verifyWorkBuddyAccount(context.Background(), &store.Account{AccountType: "workbuddy"}, &config.Config{})
	if err == nil {
		t.Fatal("expected an error for a credential-less account")
	}
	if httpStatus != 400 {
		t.Fatalf("httpStatus = %d, want 400", httpStatus)
	}
}

func TestPreserveWorkBuddyCredentialsOnEdit_KeepsServerSideState(t *testing.T) {
	t.Parallel()

	existing := &store.Account{
		WorkBuddyAccessToken:    "stored-access",
		WorkBuddyRefreshToken:   "stored-refresh",
		WorkBuddyUID:            "stored-uid",
		WorkBuddyModelIDs:       []string{"hy3", "default-model", "gpt-6-astra", "kimi-k3"},
		WorkBuddyModelsSyncedAt: time.Now(),
	}
	edited := &store.Account{AccountType: "workbuddy"}

	PreserveWorkBuddyCredentialsOnEdit(edited, existing)

	if edited.WorkBuddyAccessToken != "stored-access" || edited.WorkBuddyRefreshToken != "stored-refresh" {
		t.Fatalf("credentials were dropped on edit: %+v", edited)
	}
	if len(edited.WorkBuddyModelIDs) != 4 {
		t.Fatalf("WorkBuddyModelIDs = %v, want the stored snapshot", edited.WorkBuddyModelIDs)
	}
	if edited.WorkBuddyModelsSyncedAt.IsZero() {
		t.Fatal("WorkBuddyModelsSyncedAt was reset")
	}
}

func TestBuildQuotaResponseFields_WorkBuddyKeepsRemainingSemantics(t *testing.T) {
	t.Parallel()

	acc := &store.Account{
		AccountType:  "workbuddy",
		UsageLimit:   350,
		UsageCurrent: 147.28,
		WorkBuddyQuota: store.WorkBuddyQuotaSnapshot{
			Limit:             350,
			Remaining:         147.28,
			Used:              202.72,
			PackageRemaining:  147.28,
			LastConsumedUnits: 52,
			PackageName:       "Free Plan Subscription",
			Unit:              "credit",
			ResetAt:           time.Date(2026, 9, 26, 0, 13, 42, 0, time.UTC),
			SyncedAt:          time.Now(),
		},
	}

	fields := buildQuotaResponseFields(acc)
	if fields["quota_limit"] != 350.0 {
		t.Fatalf("quota_limit = %v, want 350", fields["quota_limit"])
	}
	if fields["quota_remaining"] != 147.28 {
		t.Fatalf("quota_remaining = %v, want 147.28", fields["quota_remaining"])
	}
	// UsageCurrent stores REMAINING for this channel; "used" must come from the
	// meter snapshot, never from reading UsageCurrent as used.
	if fields["quota_used"] != 202.72 {
		t.Fatalf("quota_used = %v, want 202.72", fields["quota_used"])
	}
	if fields["quota_supported"] != true {
		t.Fatalf("quota_supported = %v, want true", fields["quota_supported"])
	}
	if fields["quota_plan"] != "Free Plan Subscription" {
		t.Fatalf("quota_plan = %v", fields["quota_plan"])
	}
	if fields["quota_unit"] != "credit" {
		t.Fatalf("quota_unit = %v", fields["quota_unit"])
	}
	if fields["quota_consumed_units"] != 52 {
		t.Fatalf("quota_consumed_units = %v", fields["quota_consumed_units"])
	}
	if _, ok := fields["quota_reset_at"]; !ok {
		t.Fatal("quota_reset_at is missing")
	}
}

func TestBuildQuotaResponseFields_WorkBuddyWithoutMeterIsUnsupported(t *testing.T) {
	t.Parallel()

	fields := buildQuotaResponseFields(&store.Account{AccountType: "workbuddy"})
	if fields["quota_supported"] != false {
		t.Fatalf("quota_supported = %v, want false before the first meter sync", fields["quota_supported"])
	}
	if fields["quota_mode"] != "unknown" {
		t.Fatalf("quota_mode = %v, want unknown", fields["quota_mode"])
	}
}

// workBuddyAPIServer stubs the three upstream endpoints an account sync touches:
// the cli model catalog, the credit meter and the account profile.
func workBuddyAPIServer(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v3/config":
			_, _ = w.Write([]byte(`{"code":0,"data":{"models":[
				{"id":"hy3","name":"HY3"},
				{"id":"default-model","name":"Default"}
			],"agents":[{"name":"cli","models":["default-model","hy3"]}]}}`))
		case "/v2/billing/meter/get-user-resource":
			_, _ = w.Write([]byte(`{"code":0,"data":{"Response":{"Data":{"TotalCount":1,"Accounts":[{
				"PackageName":"Free Plan Subscription",
				"CapacityUnit":"credit",
				"CapacitySize":250,"CapacityRemain":47,
				"CapacityRemainPrecise":"47.28",
				"CycleCapacitySize":250,"CycleCapacityRemain":47,
				"CycleCapacitySizePrecise":"250","CycleCapacityRemainPrecise":"47.28",
				"CycleEndTime":"2026-09-26 00:13:42","Status":0}]}}}}`))
		case "/v2/plugin/login/account":
			_, _ = w.Write([]byte(`{"code":0,"data":{"uid":"uid-abc","nickname":"operator@example.com"}}`))
		default:
			http.NotFound(w, r)
		}
	}))
}

// TestRefreshAccountState_WorkBuddySyncsModelsAndQuota is the regression guard
// for the account table's 等级/配额 columns: without this sync the meter snapshot
// never reaches the API response and both columns render empty.
func TestRefreshAccountState_WorkBuddySyncsModelsAndQuota(t *testing.T) {
	srv := workBuddyAPIServer(t)
	defer srv.Close()

	a := New(nil, "", "", &config.Config{WorkBuddyBaseURL: srv.URL})
	acc := &store.Account{
		ID:                    3,
		AccountType:           "workbuddy",
		WorkBuddyAccessToken:  "access-token",
		WorkBuddyRefreshToken: "refresh-token",
		Enabled:               true,
	}

	status, httpStatus, err := a.refreshAccountState(context.Background(), acc)
	if err != nil {
		t.Fatalf("refreshAccountState() error = %v", err)
	}
	if status != "" || httpStatus != 0 {
		t.Fatalf("status = %q httpStatus = %d, want a clean sync", status, httpStatus)
	}

	if len(acc.WorkBuddyModelIDs) != 2 {
		t.Fatalf("model snapshot = %v, want the cli catalog", acc.WorkBuddyModelIDs)
	}
	if acc.UsageLimit != 250 || acc.UsageCurrent != 47.28 {
		t.Fatalf("usage = %v/%v, want 47.28 remaining of 250", acc.UsageCurrent, acc.UsageLimit)
	}
	if acc.WorkBuddyQuota.PackageName != "Free Plan Subscription" {
		t.Fatalf("plan = %q", acc.WorkBuddyQuota.PackageName)
	}
	if acc.WorkBuddyQuota.SyncedAt.IsZero() {
		t.Fatal("quota snapshot has no timestamp; the UI cannot tell when to re-sync")
	}

	// The columns are rendered from the API response, so assert on that shape.
	fields := buildQuotaResponseFields(acc)
	if fields["quota_supported"] != true {
		t.Fatalf("quota_supported = %v", fields["quota_supported"])
	}
	if fields["quota_remaining"] != 47.28 || fields["quota_limit"] != 250.0 {
		t.Fatalf("remaining/limit = %v/%v", fields["quota_remaining"], fields["quota_limit"])
	}
	if fields["quota_plan"] != "Free Plan Subscription" {
		t.Fatalf("quota_plan = %v", fields["quota_plan"])
	}
	if used, ok := fields["quota_used"].(float64); !ok || math.Abs(used-202.72) > 0.01 {
		t.Fatalf("quota_used = %v, want the derived consumption (202.72)", fields["quota_used"])
	}
}

func TestRefreshAccountState_WorkBuddyMeterFailureKeepsAccountUsable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v3/config":
			_, _ = w.Write([]byte(`{"code":0,"data":{"models":[{"id":"hy3"}],"agents":[{"name":"cli","models":["hy3"]}]}}`))
		case "/v2/billing/meter/get-user-resource":
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(`{"code":10001,"msg":"forbidden"}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	a := New(nil, "", "", &config.Config{WorkBuddyBaseURL: srv.URL})
	acc := &store.Account{ID: 4, AccountType: "workbuddy", WorkBuddyAccessToken: "access"}

	status, httpStatus, err := a.refreshAccountState(context.Background(), acc)
	if err != nil {
		t.Fatalf("a meter failure must not fail the account sync: %v", err)
	}
	if status != "" || httpStatus != 0 {
		t.Fatalf("status = %q httpStatus = %d", status, httpStatus)
	}
	if len(acc.WorkBuddyModelIDs) != 1 {
		t.Fatalf("model snapshot = %v, want the catalog to still sync", acc.WorkBuddyModelIDs)
	}
	fields := buildQuotaResponseFields(acc)
	if fields["quota_supported"] != false {
		t.Fatalf("quota_supported = %v, want false when the meter is unavailable", fields["quota_supported"])
	}
}

// TestHandleAccounts_WorkBuddyRowCarriesTierAndQuota is the end-to-end guard for
// the account table: the list endpoint must expose the plan label, the meter
// numbers and the identity the 账号/邮箱 column renders.
func TestHandleAccounts_WorkBuddyRowCarriesTierAndQuota(t *testing.T) {
	s, _ := newTestStore(t, "wb-list:")
	ctx := context.Background()

	acc := &store.Account{
		AccountType:           "workbuddy",
		Name:                  "operator@example.com",
		Email:                 "operator@example.com",
		Enabled:               true,
		Weight:                1,
		WorkBuddyAccessToken:  "access-token",
		WorkBuddyRefreshToken: "refresh-token",
		WorkBuddyUID:          "uid-abc",
		UsageLimit:            250,
		UsageCurrent:          47.28,
		WorkBuddyQuota: store.WorkBuddyQuotaSnapshot{
			Limit:             250,
			Remaining:         47.28,
			Used:              202.72,
			LastConsumedUnits: 202,
			PackageName:       "Free Plan Subscription",
			Unit:              "credit",
			ResetAt:           time.Date(2026, 9, 26, 0, 13, 42, 0, time.UTC),
			SyncedAt:          time.Now(),
		},
	}
	if err := s.CreateAccount(ctx, acc); err != nil {
		t.Fatalf("CreateAccount() error = %v", err)
	}

	a := New(s, "", "", &config.Config{})
	rec := httptest.NewRecorder()
	a.HandleAccounts(rec, httptest.NewRequest(http.MethodGet, "/api/accounts", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}

	var rows []map[string]interface{}
	if err := json.Unmarshal(rec.Body.Bytes(), &rows); err != nil {
		t.Fatalf("decode accounts: %v (body=%s)", err, rec.Body.String())
	}
	if len(rows) != 1 {
		t.Fatalf("rows = %d, want 1", len(rows))
	}
	row := rows[0]
	if row["email"] != "operator@example.com" {
		t.Fatalf("email = %v, want the signed-in address in the account list", row["email"])
	}
	for key, want := range map[string]interface{}{
		"quota_plan":      "Free Plan Subscription",
		"quota_supported": true,
		"quota_limit":     250.0,
		"quota_remaining": 47.28,
	} {
		if row[key] != want {
			t.Fatalf("%s = %#v, want %#v (account list row: %v)", key, row[key], want, row)
		}
	}
	if used, ok := row["quota_used"].(float64); !ok || math.Abs(used-202.72) > 0.01 {
		t.Fatalf("quota_used = %v", row["quota_used"])
	}
	quota, ok := row["workbuddy_quota"].(map[string]interface{})
	if !ok {
		t.Fatalf("workbuddy_quota = %v, want the meter snapshot", row["workbuddy_quota"])
	}
	if quota["package_name"] != "Free Plan Subscription" || quota["synced_at"] == nil {
		t.Fatalf("workbuddy_quota = %v, want the package label and a sync timestamp", quota)
	}
	if row["workbuddy_refresh_token"] != nil {
		t.Fatalf("the refresh token leaked into the account list: %v", row["workbuddy_refresh_token"])
	}
}

// TestHandleAccounts_PostWorkBuddyDocumentCapturesIdentityAndQuota covers the
// manual/import path: a pasted session document must populate the account
// identity (so 账号/邮箱 is filled) and carry the meter snapshot once synced.
func TestHandleAccounts_PostWorkBuddyDocumentCapturesIdentityAndQuota(t *testing.T) {
	srv := workBuddyAPIServer(t)
	defer srv.Close()

	s, _ := newTestStore(t, "wb-create:")
	a := New(s, "", "", &config.Config{WorkBuddyBaseURL: srv.URL})

	body, err := json.Marshal(map[string]interface{}{
		"account_type":  "workbuddy",
		"client_cookie": workBuddyAuthDocument,
		"enabled":       true,
		"weight":        1,
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/accounts", strings.NewReader(string(body)))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	a.HandleAccounts(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}

	var row map[string]interface{}
	if err := json.Unmarshal(rec.Body.Bytes(), &row); err != nil {
		t.Fatalf("decode: %v (body=%s)", err, rec.Body.String())
	}
	// The stubbed auth document embeds a JWT with sub/email claims.
	if row["email"] != "operator@example.com" {
		t.Fatalf("email = %v, want the identity proven by the pasted document", row["email"])
	}
	if row["workbuddy_uid"] != "07ab88c8-5596-4257-8d21-e9fcbe3a3810" {
		t.Fatalf("workbuddy_uid = %v", row["workbuddy_uid"])
	}
	if row["quota_plan"] != "Free Plan Subscription" {
		t.Fatalf("quota_plan = %v, want the meter plan label", row["quota_plan"])
	}
	if row["quota_remaining"] != 47.28 || row["quota_limit"] != 250.0 {
		t.Fatalf("quota = %v/%v", row["quota_remaining"], row["quota_limit"])
	}
	if row["workbuddy_refresh_token"] != nil {
		t.Fatal("the refresh token leaked in the create response")
	}

	// The same values must come back from the list endpoint the table reads.
	listRec := httptest.NewRecorder()
	a.HandleAccounts(listRec, httptest.NewRequest(http.MethodGet, "/api/accounts", nil))
	var rows []map[string]interface{}
	if err := json.Unmarshal(listRec.Body.Bytes(), &rows); err != nil {
		t.Fatalf("decode list: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("rows = %d, want 1", len(rows))
	}
	if rows[0]["quota_plan"] != "Free Plan Subscription" || rows[0]["email"] != "operator@example.com" {
		t.Fatalf("list row = %v", rows[0])
	}
}

// TestHandleAccounts_GrokRowCarriesSnapshotTimestamp covers the freshness signal
// the accounts page uses to decide whether a row needs an automatic re-sync:
// every channel that can report it must expose a synced_at the client can read.
func TestHandleAccounts_GrokRowCarriesSnapshotTimestamp(t *testing.T) {
	s, _ := newTestStore(t, "grok-list:")
	ctx := context.Background()
	syncedAt := time.Now().Add(-45 * time.Minute).UTC()

	oauth := &store.Account{
		AccountType:        "grok",
		CredentialType:     "oauth",
		GrokProvider:       "build",
		OAuthAccessToken:   "access",
		OAuthRefreshToken:  "refresh",
		Enabled:            true,
		GrokBilling:        store.GrokBillingSnapshot{SyncedAt: syncedAt},
		GrokModels:         []string{"grok-4.6"},
		GrokModelsSyncedAt: syncedAt,
	}
	sso := &store.Account{
		AccountType:    "grok",
		CredentialType: "sso",
		GrokProvider:   "web",
		ClientCookie:   "sso=web-token",
		Enabled:        true,
		GrokWebQuota:   store.GrokWebQuotaSnapshot{SyncedAt: syncedAt},
	}
	for _, acc := range []*store.Account{oauth, sso} {
		if err := s.CreateAccount(ctx, acc); err != nil {
			t.Fatalf("CreateAccount() error = %v", err)
		}
	}

	a := New(s, "", "", &config.Config{})
	rec := httptest.NewRecorder()
	a.HandleAccounts(rec, httptest.NewRequest(http.MethodGet, "/api/accounts", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	var rows []map[string]interface{}
	if err := json.Unmarshal(rec.Body.Bytes(), &rows); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("rows = %d, want 2", len(rows))
	}
	for _, row := range rows {
		credential := row["credential_type"]
		if credential == "oauth" {
			billing, ok := row["grok_billing"].(map[string]interface{})
			if !ok || billing["synced_at"] == nil {
				t.Fatalf("Grok Build row is missing grok_billing.synced_at: %v", row["grok_billing"])
			}
			if row["grok_models_synced_at"] == nil {
				t.Fatal("Grok Build row is missing grok_models_synced_at")
			}
			continue
		}
		webQuota, ok := row["grok_web_quota"].(map[string]interface{})
		if !ok || webQuota["synced_at"] == nil {
			t.Fatalf("Grok Web SSO row is missing grok_web_quota.synced_at: %v", row["grok_web_quota"])
		}
	}
}

// TestAccountStatusReasonIsExposed covers the "未授权 with no explanation" problem:
// the account list must carry the operator-facing reason next to the status code.
func TestAccountStatusReasonIsExposed(t *testing.T) {
	s, _ := newTestStore(t, "status-reason:")
	ctx := context.Background()

	acc := &store.Account{
		AccountType:    "grok",
		CredentialType: "oauth",
		GrokProvider:   "build",
		Enabled:        true,
		StatusCode:     "401",
		StatusMessage:  "上游已不接受该 OAuth 授权（refresh token 被拒绝），需要重新登录",
		LastAttempt:    time.Now(),
	}
	if err := s.CreateAccount(ctx, acc); err != nil {
		t.Fatalf("CreateAccount() error = %v", err)
	}

	a := New(s, "", "", &config.Config{})
	rec := httptest.NewRecorder()
	a.HandleAccounts(rec, httptest.NewRequest(http.MethodGet, "/api/accounts", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	var rows []map[string]interface{}
	if err := json.Unmarshal(rec.Body.Bytes(), &rows); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("rows = %d, want 1", len(rows))
	}
	if rows[0]["status_code"] != "401" {
		t.Fatalf("status_code = %v", rows[0]["status_code"])
	}
	if got, _ := rows[0]["status_message"].(string); got == "" {
		t.Fatal("status_message is missing; the UI can only show a bare 401")
	}

	// A successful refresh must clear both the code and the stale reason.
	acc.StatusCode = ""
	acc.StatusMessage = ""
	if err := s.UpdateAccount(ctx, acc); err != nil {
		t.Fatalf("UpdateAccount() error = %v", err)
	}
	rec2 := httptest.NewRecorder()
	a.HandleAccounts(rec2, httptest.NewRequest(http.MethodGet, "/api/accounts", nil))
	// Decode into a fresh slice: reusing it would keep keys the new payload omits.
	var cleared []map[string]interface{}
	if err := json.Unmarshal(rec2.Body.Bytes(), &cleared); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(cleared) != 1 {
		t.Fatalf("rows = %d, want 1", len(cleared))
	}
	if cleared[0]["status_code"] != "" {
		t.Fatalf("status_code = %v, want cleared", cleared[0]["status_code"])
	}
	if reason := cleared[0]["status_message"]; reason != nil && reason != "" {
		t.Fatalf("status_message = %v, want cleared with the status", reason)
	}
}

func TestWorkBuddyAccessTokenPreview_TruncatesWithoutLeakingRefresh(t *testing.T) {
	t.Parallel()

	preview := WorkBuddyAccessTokenPreview(&store.Account{WorkBuddyAccessToken: "abcdefghijklmnopqrstuvwxyz0123456789"})
	if preview != "abcdefgh...23456789" {
		t.Fatalf("preview = %q", preview)
	}
	if got := WorkBuddyAccessTokenPreview(&store.Account{WorkBuddyRefreshToken: "refresh-only"}); got != "" {
		t.Fatalf("preview = %q, want empty when only a refresh token exists", got)
	}
}

func TestResolveCredentials_MatchesClientResolution(t *testing.T) {
	t.Parallel()

	acc := &store.Account{ClientCookie: workBuddyAuthDocument}
	apiCreds := resolveWorkBuddyCredentials(acc)
	clientCreds := workbuddy.ResolveCredentials(acc)

	if apiCreds.AccessToken != clientCreds.AccessToken || apiCreds.RefreshToken != clientCreds.RefreshToken {
		t.Fatalf("api=%+v client=%+v", apiCreds, clientCreds)
	}
	if apiCreds.UID != clientCreds.UID {
		t.Fatalf("uid api=%q client=%q", apiCreds.UID, clientCreds.UID)
	}
}
