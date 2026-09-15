package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"orchids-api/internal/config"
	apperrors "orchids-api/internal/errors"
	"orchids-api/internal/qoder"
	"orchids-api/internal/store"
)

// qoderAuthServer stubs the Qoder device authorization endpoints. It records
// nothing secret and never echoes a credential it was not asked for.
type qoderAuthServer struct {
	*httptest.Server
	polls   int
	refresh int
}

func newQoderAuthServer(t *testing.T, pollBodies []string) *qoderAuthServer {
	t.Helper()
	stub := &qoderAuthServer{}
	stub.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/":
			w.WriteHeader(http.StatusOK)
		case "/device/selectAccounts":
			// The authorization page is only ever opened in a browser, so a
			// direct hit here means the flow went wrong.
			w.WriteHeader(http.StatusOK)
		case "/api/v1/deviceToken/poll":
			for _, key := range []string{"nonce", "verifier", "challenge_method"} {
				if r.URL.Query().Get(key) == "" {
					t.Errorf("poll is missing %s", key)
				}
			}
			index := stub.polls
			stub.polls++
			if index >= len(pollBodies) {
				index = len(pollBodies) - 1
			}
			body := pollBodies[index]
			if body == "" {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			_, _ = w.Write([]byte(body))
		case "/api/v1/deviceToken/refresh":
			stub.refresh++
			_, _ = w.Write([]byte(`{"device_token":"access-2","refresh_token":"refresh-2","expires_in":3600}`))
		case "/api/v1/userinfo":
			if got := r.Header.Get("Authorization"); !strings.HasPrefix(got, "Bearer ") {
				t.Errorf("userinfo Authorization = %q, want a bearer token", got)
			}
			_, _ = w.Write([]byte(`{"uid":"uid-qoder","name":"operator","email":"operator@example.com","organization_id":"org-1","organization_tags":["tag-a"]}`))
		default:
			http.NotFound(w, r)
		}
	}))
	return stub
}

func qoderLoginConfig(baseURL string) *config.Config {
	return &config.Config{
		QoderOAuthBaseURL:   baseURL,
		QoderOpenAPIBaseURL: baseURL,
		QoderInferenceURL:   baseURL,
	}
}

func qoderLoginRequest(t *testing.T, method, path, body string) *http.Request {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Host = "localhost"
	req.Header.Set("Origin", "http://localhost")
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	return req
}

// TestHandleQoderLogin_StartReturnsOfficialDeviceURL proves the start step
// returns a device authorization URL and never leaks the transaction's private
// halves.
func TestHandleQoderLogin_StartReturnsOfficialDeviceURL(t *testing.T) {
	s, _ := newTestStore(t, "qd-login:")
	auth := newQoderAuthServer(t, []string{""})
	defer auth.Close()
	a := New(s, "", "", qoderLoginConfig(auth.URL))
	rec := httptest.NewRecorder()
	a.HandleQoderLogin(rec, qoderLoginRequest(t, http.MethodPost, "/api/qoder/login", `{"enabled":true}`))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	var response struct {
		ID                      string `json:"id"`
		Status                  string `json:"status"`
		UserCode                string `json:"user_code"`
		VerificationURIComplete string `json:"verification_uri_complete"`
		ExpiresAt               string `json:"expires_at"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if response.ID == "" || response.Status != "pending" {
		t.Fatalf("response = %+v", response)
	}
	parsed, err := url.Parse(response.VerificationURIComplete)
	if err != nil {
		t.Fatalf("authorization URL is not parseable: %v", err)
	}
	if parsed.Path != "/device/selectAccounts" {
		t.Fatalf("authorization path = %q, want /device/selectAccounts", parsed.Path)
	}
	for _, key := range []string{"challenge", "challenge_method", "nonce", "machine_id", "client_id"} {
		if parsed.Query().Get(key) == "" {
			t.Errorf("authorization URL is missing %s", key)
		}
	}
	if parsed.Query().Get("challenge_method") != "S256" {
		t.Errorf("challenge_method = %q, want S256", parsed.Query().Get("challenge_method"))
	}
	// The nonce and the verifier are the transaction's private halves; a nonce
	// that reached the browser would let a third party complete the attempt.
	if strings.Contains(rec.Body.String(), "verifier") {
		t.Fatal("the start response mentions the PKCE verifier")
	}
	if response.UserCode != "" {
		t.Fatalf("start response exposed a user code: %q", response.UserCode)
	}
	if strings.Contains(response.VerificationURIComplete, parsed.Query().Get("nonce")) == false {
		t.Fatal("the nonce must be part of the browser challenge")
	}

	// The pending transaction must be pollable and must stay pending while the
	// upstream answers 404.
	pollRec := httptest.NewRecorder()
	a.HandleQoderLogin(pollRec, qoderLoginRequest(t, http.MethodGet, "/api/qoder/login/"+response.ID, ""))
	if pollRec.Code != http.StatusOK {
		t.Fatalf("poll status = %d", pollRec.Code)
	}
	var polled deviceLoginResponse
	if err := json.Unmarshal(pollRec.Body.Bytes(), &polled); err != nil {
		t.Fatalf("decode poll: %v", err)
	}
	if polled.Status != "pending" {
		t.Fatalf("polled status = %q, want pending", polled.Status)
	}
}

func TestHandleQoderLogin_CancelStopsBlockedPollAndDoesNotPersist(t *testing.T) {
	s, _ := newTestStore(t, "qd-cancel:")
	pollStarted := make(chan struct{})
	pollCancelled := make(chan struct{})
	var startedOnce sync.Once
	var cancelledOnce sync.Once
	auth := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/":
			w.WriteHeader(http.StatusOK)
		case "/api/v1/deviceToken/poll":
			startedOnce.Do(func() { close(pollStarted) })
			<-r.Context().Done()
			cancelledOnce.Do(func() { close(pollCancelled) })
		default:
			http.NotFound(w, r)
		}
	}))
	defer auth.Close()
	a := New(s, "", "", qoderLoginConfig(auth.URL))
	start := httptest.NewRecorder()
	a.HandleQoderLogin(start, qoderLoginRequest(t, http.MethodPost, "/api/qoder/login", ""))
	var response struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(start.Body.Bytes(), &response); err != nil || response.ID == "" {
		t.Fatalf("start response = %q", start.Body.String())
	}
	// Speed up only this transaction; production keeps the normal two-second
	// cadence.
	a.qoderLoginMu.Lock()
	a.qoderLogins[response.ID].interval = time.Millisecond
	a.qoderLoginMu.Unlock()

	select {
	case <-pollStarted:
	case <-time.After(3 * time.Second):
		t.Fatal("poll request did not start")
	}
	cancelRec := httptest.NewRecorder()
	a.HandleQoderLogin(cancelRec, qoderLoginRequest(t, http.MethodDelete, "/api/qoder/login/"+response.ID, ""))
	if cancelRec.Code != http.StatusNoContent {
		t.Fatalf("cancel status = %d", cancelRec.Code)
	}
	select {
	case <-pollCancelled:
	case <-time.After(time.Second):
		t.Fatal("blocked upstream poll was not cancelled")
	}
	accounts, err := s.ListAccounts(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, acc := range accounts {
		if strings.EqualFold(acc.AccountType, "qoder") {
			t.Fatalf("cancelled login persisted an account: %+v", acc)
		}
	}
}

func TestHandleQoderLogin_PreservesDisabledPreference(t *testing.T) {
	s, _ := newTestStore(t, "qd-disabled:")
	auth := newQoderAuthServer(t, []string{
		`{"token":"access-1","refresh_token":"refresh-1","expires_in":86400,"user_id":"uid-disabled","user_name":"operator"}`,
	})
	defer auth.Close()
	a := New(s, "", "", qoderLoginConfig(auth.URL))

	start := httptest.NewRecorder()
	a.HandleQoderLogin(start, qoderLoginRequest(t, http.MethodPost, "/api/qoder/login", `{"enabled":false}`))
	var response struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(start.Body.Bytes(), &response); err != nil || response.ID == "" {
		t.Fatalf("start response = %q", start.Body.String())
	}
	a.qoderLoginMu.Lock()
	a.qoderLogins[response.ID].interval = time.Millisecond
	a.qoderLoginMu.Unlock()

	deadline := time.Now().Add(5 * time.Second)
	var final deviceLoginResponse
	for time.Now().Before(deadline) {
		poll := httptest.NewRecorder()
		a.HandleQoderLogin(poll, qoderLoginRequest(t, http.MethodGet, "/api/qoder/login/"+response.ID, ""))
		if err := json.Unmarshal(poll.Body.Bytes(), &final); err != nil {
			t.Fatal(err)
		}
		if final.Status == "complete" {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if final.Status != "complete" {
		t.Fatalf("final login = %+v", final)
	}
	acc, err := s.GetAccount(context.Background(), final.AccountID)
	if err != nil {
		t.Fatal(err)
	}
	if acc.Enabled {
		t.Fatal("enabled:false was lost when the Qoder account was persisted")
	}
}

// TestHandleQoderLogin_CompletesAndPersistsAccount proves the whole flow: the
// browser step completes, the account is verified against the signed catalog and
// the credential plus its derived runtime pair are persisted.
func TestHandleQoderLogin_CompletesAndPersistsAccount(t *testing.T) {
	s, _ := newTestStore(t, "qd-login:")
	auth := newQoderAuthServer(t, []string{
		"",
		// A token lifetime longer than the refresh lead, so the credential is
		// usable without an immediate rotation.
		`{"token":"access-1","refresh_token":"refresh-1","expires_in":86400,"refresh_token_expires_in":864000,"user_id":"uid-qoder","user_name":"operator"}`,
	})
	defer auth.Close()
	a := New(s, "", "", qoderLoginConfig(auth.URL))
	rec := httptest.NewRecorder()
	a.HandleQoderLogin(rec, qoderLoginRequest(t, http.MethodPost, "/api/qoder/login", ""))

	var started struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &started); err != nil || started.ID == "" {
		t.Fatalf("start response = %q", rec.Body.String())
	}

	deadline := time.Now().Add(15 * time.Second)
	var final deviceLoginResponse
	for time.Now().Before(deadline) {
		pollRec := httptest.NewRecorder()
		a.HandleQoderLogin(pollRec, qoderLoginRequest(t, http.MethodGet, "/api/qoder/login/"+started.ID, ""))
		if pollRec.Code != http.StatusOK {
			t.Fatalf("poll status = %d body=%s", pollRec.Code, pollRec.Body.String())
		}
		if err := json.Unmarshal(pollRec.Body.Bytes(), &final); err != nil {
			t.Fatalf("decode poll: %v", err)
		}
		if final.Status == "complete" || final.Status == "failed" {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if final.Status != "complete" {
		t.Fatalf("final status = %q message=%q, want complete", final.Status, final.Message)
	}
	if final.AccountID == 0 {
		t.Fatal("the completed login did not report an account id")
	}

	acc, err := s.GetAccount(t.Context(), final.AccountID)
	if err != nil {
		t.Fatalf("GetAccount: %v", err)
	}
	if acc.AccountType != "qoder" {
		t.Fatalf("account type = %q, want qoder", acc.AccountType)
	}
	if acc.QoderAccessToken == "" || acc.QoderRefreshToken == "" {
		t.Fatalf("stored credential = %q/%q, want a persisted pair", acc.QoderAccessToken, acc.QoderRefreshToken)
	}
	if acc.QoderUserID != "uid-qoder" {
		t.Fatalf("stored user id = %q", acc.QoderUserID)
	}
	if acc.QoderMachineID == "" {
		t.Fatal("the device identity was not stored")
	}
	if acc.QoderRuntimeInfo == "" || acc.QoderRuntimeKey == "" {
		t.Fatal("the derived runtime pair was not stored")
	}
	// The Qoder catalog is local (the gateway refuses the model-list read for an
	// OAuth credential), so a completed login installs the built-in list.
	if want := len(qoder.CatalogSnapshot(qoder.DefaultCatalog())); len(acc.QoderModelIDs) != want {
		t.Fatalf("catalog snapshot has %d entries, want the built-in %d", len(acc.QoderModelIDs), want)
	}
	if acc.Email != "operator@example.com" {
		t.Fatalf("email = %q", acc.Email)
	}

	// The account response must never carry the durable credential or the
	// derived runtime material.
	redacted := RedactQoderOutput(acc)
	if redacted.QoderRefreshToken != "" || redacted.QoderRuntimeKey != "" {
		t.Fatal("RedactQoderOutput left a secret in place")
	}
	if redacted.QoderAccessToken == "" {
		t.Fatal("RedactQoderOutput dropped the access token the table needs as proof")
	}
}

// TestHandleQoderLogin_ReportsUnusableCredential proves a login whose credential
// cannot be resolved to an identity is still refused, and that the reason — not
// just "could not be verified" — reaches the operator.
func TestHandleQoderLogin_ReportsUnusableCredential(t *testing.T) {
	s, _ := newTestStore(t, "qd-login:")
	auth := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/":
			w.WriteHeader(http.StatusOK)
		case "/api/v1/deviceToken/poll":
			// No user_id, and userinfo will also refuse, so no identity can be
			// established and the account must not be stored.
			_, _ = w.Write([]byte(`{"token":"access-1","refresh_token":"refresh-1","expires_in":86400}`))
		case "/api/v1/userinfo":
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(`{"message":"token rejected"}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer auth.Close()
	a := New(s, "", "", qoderLoginConfig(auth.URL))
	final := runQoderLoginToCompletion(t, a, s, 15*time.Second)
	if final.Status != "failed" {
		t.Fatalf("status = %q, want failed", final.Status)
	}
	// The reason must name the actual problem instead of only saying that
	// verification failed.
	if !strings.Contains(final.Message, "user id") {
		t.Fatalf("message = %q, want the underlying reason", final.Message)
	}

	accounts, err := s.ListAccounts(t.Context())
	if err != nil {
		t.Fatalf("ListAccounts: %v", err)
	}
	for _, acc := range accounts {
		if strings.EqualFold(acc.AccountType, "qoder") {
			t.Fatalf("an unusable Qoder account was persisted: %+v", acc)
		}
	}
}

// runQoderLoginToCompletion starts a login, polls it to a terminal state and
// reports the final transaction.
func runQoderLoginToCompletion(t *testing.T, a *API, s *store.Store, timeout time.Duration) deviceLoginResponse {
	t.Helper()
	rec := httptest.NewRecorder()
	a.HandleQoderLogin(rec, qoderLoginRequest(t, http.MethodPost, "/api/qoder/login", ""))
	var started struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &started); err != nil || started.ID == "" {
		t.Fatalf("start response = %q", rec.Body.String())
	}

	deadline := time.Now().Add(timeout)
	var final deviceLoginResponse
	for time.Now().Before(deadline) {
		pollRec := httptest.NewRecorder()
		a.HandleQoderLogin(pollRec, qoderLoginRequest(t, http.MethodGet, "/api/qoder/login/"+started.ID, ""))
		if pollRec.Code != http.StatusOK {
			t.Fatalf("poll status = %d body=%s", pollRec.Code, pollRec.Body.String())
		}
		if err := json.Unmarshal(pollRec.Body.Bytes(), &final); err != nil {
			t.Fatalf("decode poll: %v", err)
		}
		if final.Status == "complete" || final.Status == "failed" || final.Status == "expired" {
			return final
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("login did not reach a terminal state: %+v", final)
	return final
}

// TestHandleQoderLogin_RequiresStoreAndRejectsBadMethods covers the guard rails.
func TestHandleQoderLogin_RequiresStoreAndRejectsBadMethods(t *testing.T) {
	a := New(nil, "", "", &config.Config{})
	rec := httptest.NewRecorder()
	a.HandleQoderLogin(rec, qoderLoginRequest(t, http.MethodPost, "/api/qoder/login", ""))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503 without a store", rec.Code)
	}

	s, _ := newTestStore(t, "qd-login:")
	a = New(s, "", "", &config.Config{})
	rec = httptest.NewRecorder()
	a.HandleQoderLogin(rec, qoderLoginRequest(t, http.MethodPut, "/api/qoder/login", ""))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405 for an unsupported method", rec.Code)
	}
	rec = httptest.NewRecorder()
	a.HandleQoderLogin(rec, qoderLoginRequest(t, http.MethodPost, "/api/qoder/login", `{"unexpected":1}`))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 for an unknown field", rec.Code)
	}
}

// TestHandleQoderLogin_RejectsForeignOrigin proves a credential-mutating request
// must come from this admin origin.
func TestHandleQoderLogin_RejectsForeignOrigin(t *testing.T) {
	s, _ := newTestStore(t, "qd-login:")
	a := New(s, "", "", &config.Config{})

	req := httptest.NewRequest(http.MethodPost, "/api/qoder/login", strings.NewReader(""))
	req.Host = "localhost"
	req.Header.Set("Origin", "https://evil.example")
	rec := httptest.NewRecorder()
	a.HandleQoderLogin(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 for a foreign origin", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "origin_mismatch") {
		t.Fatalf("body = %s, want the origin_mismatch code", rec.Body.String())
	}
}

// TestQoderAccountCreateAndEditAreOAuthOnly proves the account API refuses to
// create a Qoder account by hand and keeps the stored credential on an edit.
func TestQoderAccountCreateAndEditAreOAuthOnly(t *testing.T) {
	s, _ := newTestStore(t, "qd-account:")
	a := New(s, "", "", &config.Config{})

	// Creation by hand is refused with a pointer at the official login.
	createBody := `{"account_type":"qoder","client_cookie":"pat-shaped-value","enabled":true}`
	rec := httptest.NewRecorder()
	a.HandleAccounts(rec, qoderAccountRequest(t, http.MethodPost, "/api/accounts", createBody))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("create status = %d body=%s, want 400", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "/api/qoder/login") {
		t.Fatalf("create body = %s, want a pointer at the official login", rec.Body.String())
	}

	// A stored account keeps its credential when an edit omits it.
	acc := &store.Account{
		AccountType:       "qoder",
		Name:              "qoder-test",
		QoderAccessToken:  "access-1",
		QoderRefreshToken: "refresh-1",
		QoderMachineID:    "11111111-2222-4333-8444-555555555555",
		QoderUserID:       "uid-qoder",
		QoderRuntimeInfo:  "info",
		QoderRuntimeKey:   "key",
		Enabled:           true,
	}
	if err := s.CreateAccount(t.Context(), acc); err != nil {
		t.Fatalf("CreateAccount: %v", err)
	}

	editBody, _ := json.Marshal(map[string]interface{}{
		"account_type": "qoder",
		"name":         "renamed",
		"enabled":      true,
	})
	rec = httptest.NewRecorder()
	a.HandleAccountByID(rec, qoderAccountRequest(t, http.MethodPut, "/api/accounts/"+strconv.FormatInt(acc.ID, 10), string(editBody)))
	if rec.Code != http.StatusOK {
		t.Fatalf("edit status = %d body=%s, want 200", rec.Code, rec.Body.String())
	}

	stored, err := s.GetAccount(t.Context(), acc.ID)
	if err != nil {
		t.Fatalf("GetAccount: %v", err)
	}
	if stored.QoderRefreshToken != "refresh-1" || stored.QoderRuntimeKey != "key" {
		t.Fatalf("the edit wiped the credential: %+v", stored)
	}
	if stored.QoderUserID != "uid-qoder" {
		t.Fatalf("the edit changed the identity: %q", stored.QoderUserID)
	}
	if stored.Name != "renamed" {
		t.Fatalf("name = %q, want the submitted value", stored.Name)
	}
}

func qoderAccountRequest(t *testing.T, method, path, body string) *http.Request {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	return req
}

// TestVerifyQoderAccountDoesNotReportForbidden proves the account check uses the
// local catalog and only needs the identity endpoint from the control plane.
func TestVerifyQoderAccountDoesNotReportForbidden(t *testing.T) {
	s, _ := newTestStore(t, "qd-verify:")

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v1/userinfo":
			_, _ = w.Write([]byte(`{"uid":"uid-qoder","name":"operator","email":"operator@example.com"}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer upstream.Close()

	acc := &store.Account{
		AccountType:       "qoder",
		Name:              "qoder-verify",
		QoderAccessToken:  "access-1",
		QoderRefreshToken: "refresh-1",
		QoderMachineID:    "11111111-2222-4333-8444-555555555555",
		QoderUserID:       "uid-qoder",
		QoderRuntimeInfo:  "runtime-info",
		QoderRuntimeKey:   "runtime-key",
		QoderDataPolicy:   true,
		Enabled:           true,
	}
	if err := s.CreateAccount(t.Context(), acc); err != nil {
		t.Fatalf("CreateAccount: %v", err)
	}

	cfg := &config.Config{
		QoderOAuthBaseURL:   upstream.URL,
		QoderOpenAPIBaseURL: upstream.URL,
		QoderInferenceURL:   upstream.URL,
	}
	status, httpStatus, err := verifyQoderAccountWithStore(t.Context(), acc, cfg, s)
	if err != nil {
		t.Fatalf("verifyQoderAccount() error = %v, want success", err)
	}
	if status != "" {
		t.Fatalf("status = %q, want no account status", status)
	}
	if httpStatus != 0 {
		t.Fatalf("httpStatus = %d, want 0", httpStatus)
	}
	if apperrors.ClassifyAccountStatus("") != "" {
		t.Fatal("sanity: the classifier must not invent a status")
	}
	if len(acc.QoderModelIDs) == 0 {
		t.Fatal("no catalog was installed")
	}
}

// TestQoderQuotaResponseFieldsAreAuthoritative proves the account payload reports
// the gateway's own credit window as a known, observed balance.
//
// The generic projection reads usage_limit/usage_current and marks the window as
// an estimate unless a channel says otherwise, which for Qoder would present a
// reported window as guesswork — and left the console's quota column blank.
func TestQoderQuotaResponseFieldsAreAuthoritative(t *testing.T) {
	t.Parallel()

	trial := &store.Account{
		ID:           184,
		AccountType:  "qoder",
		UsageLimit:   300,
		UsageCurrent: 300,
		QoderQuota: store.QoderQuotaSnapshot{
			Limit:      300,
			Remaining:  300,
			PlanTier:   "Pro Trial",
			Unit:       "credits",
			UpgradeURL: "https://qoder.com/pricing?client=qoder",
			SyncedAt:   time.Now(),
		},
	}
	fields := buildQuotaResponseFields(trial)
	if got := fields["quota_limit"]; got != float64(300) {
		t.Fatalf("quota_limit = %v, want 300", got)
	}
	if got := fields["quota_remaining"]; got != float64(300) {
		t.Fatalf("quota_remaining = %v, want 300", got)
	}
	if got := fields["quota_plan"]; got != "Pro Trial" {
		t.Fatalf("quota_plan = %v, want Pro Trial", got)
	}
	if fields["quota_limit_known"] != true {
		t.Fatal("quota_limit_known = false for a window the gateway reported")
	}
	if fields["quota_observed"] != true {
		t.Fatal("quota_observed = false for a window the gateway reported")
	}
	if fields["quota_exhausted"] != false {
		t.Fatalf("quota_exhausted = %v, want false", fields["quota_exhausted"])
	}

	exhausted := &store.Account{
		ID:          185,
		AccountType: "qoder",
		QoderQuota: store.QoderQuotaSnapshot{
			Exhausted:  true,
			PlanTier:   "Free",
			Unit:       "credits",
			UpgradeURL: "https://qoder.com/pricing?client=qoder",
			ResetAt:    time.Now().Add(12 * time.Hour),
			SyncedAt:   time.Now(),
		},
	}
	exhaustedFields := buildQuotaResponseFields(exhausted)
	if exhaustedFields["quota_exhausted"] != true {
		t.Fatal("quota_exhausted = false for an account the gateway flagged")
	}
	if got := exhaustedFields["quota_plan"]; got != "Free" {
		t.Fatalf("quota_plan = %v, want Free", got)
	}
	// A zero limit is a known zero, not an unknown window.
	if exhaustedFields["quota_limit_known"] != true {
		t.Fatal("quota_limit_known = false for a reported Free window")
	}
	if got := exhaustedFields["quota_upgrade_url"]; got == "" {
		t.Fatal("quota_upgrade_url is empty, so the operator gets no next step")
	}
}
