package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"orchids-api/internal/config"
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
		case "/algo/api/v2/model/list":
			// The catalog read is signed, so it proves the full credential chain
			// rather than just the token endpoint.
			for _, header := range []string{"Authorization", "Cosy-Key", "Cosy-MachineId", "Cosy-User", "Cosy-Date"} {
				if r.Header.Get(header) == "" {
					t.Errorf("catalog request is missing %s", header)
				}
			}
			if !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer COSY.") {
				t.Errorf("catalog Authorization = %q, want a COSY bearer", r.Header.Get("Authorization"))
			}
			_, _ = w.Write([]byte(`{"chat":[{"key":"qmodel_latest","display_name":"Qwen3.7-Max","enable":true},{"key":"dfmodel","display_name":"DeepSeek-V4-Flash","enable":true}]}`))
		case "/algo/api/v3/user/jobToken":
			_, _ = w.Write([]byte(`{"name":"operator","id":"uid-qoder","userType":"personal_standard","refreshToken":"gw-refresh","securityOauthToken":"gw-sot","expireTime":` + "1700000000000" + `}`))
		default:
			http.NotFound(w, r)
		}
	}))
	return stub
}

// stubQoderLoginClient redirects the login flow at the stub server.
func stubQoderLoginClient(t *testing.T, baseURL string) {
	t.Helper()
	previous := newQoderLoginClient
	t.Cleanup(func() { newQoderLoginClient = previous })
	newQoderLoginClient = func(acc *store.Account, cfg *config.Config) *qoder.Client {
		client := qoder.NewFromAccount(acc, cfg)
		client.SetEndpointsForTest(baseURL, baseURL, baseURL, baseURL)
		return client
	}
}

func qoderLoginConfig(baseURL string) *config.Config {
	return &config.Config{
		QoderOAuthBaseURL:   baseURL,
		QoderOpenAPIBaseURL: baseURL,
		QoderInferenceURL:   baseURL,
		QoderAuthBaseURL:    baseURL,
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
	stubQoderLoginClient(t, auth.URL)

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
	stubQoderLoginClient(t, auth.URL)

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
	if len(acc.QoderModelIDs) != 2 {
		t.Fatalf("catalog snapshot = %v, want two models", acc.QoderModelIDs)
	}
	if acc.QoderJobToken != "gw-sot" {
		t.Fatalf("job token = %q, want the handshake result", acc.QoderJobToken)
	}
	if acc.Email != "operator@example.com" {
		t.Fatalf("email = %q", acc.Email)
	}

	// The account response must never carry the durable credential or the
	// derived runtime material.
	redacted := RedactQoderOutput(acc)
	if redacted.QoderRefreshToken != "" || redacted.QoderRuntimeKey != "" || redacted.QoderJobToken != "" {
		t.Fatal("RedactQoderOutput left a secret in place")
	}
	if redacted.QoderAccessToken == "" {
		t.Fatal("RedactQoderOutput dropped the access token the table needs as proof")
	}
}

// TestHandleQoderLogin_ReportsVerificationFailure proves a credential that
// cannot read the account catalog is not persisted as a broken pool entry.
func TestHandleQoderLogin_ReportsVerificationFailure(t *testing.T) {
	s, _ := newTestStore(t, "qd-login:")
	auth := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/":
			w.WriteHeader(http.StatusOK)
		case "/api/v1/deviceToken/poll":
			_, _ = w.Write([]byte(`{"token":"access-1","refresh_token":"refresh-1","expires_in":86400,"user_id":"uid-qoder"}`))
		case "/api/v1/userinfo":
			_, _ = w.Write([]byte(`{"uid":"uid-qoder"}`))
		case "/algo/api/v2/model/list":
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(`{"message":"forbidden"}`))
		case "/algo/api/v3/user/jobToken":
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(`{"message":"forbidden"}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer auth.Close()
	stubQoderLoginClient(t, auth.URL)

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
			t.Fatalf("poll status = %d", pollRec.Code)
		}
		if err := json.Unmarshal(pollRec.Body.Bytes(), &final); err != nil {
			t.Fatalf("decode poll: %v", err)
		}
		if final.Status == "complete" || final.Status == "failed" {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if final.Status != "failed" {
		t.Fatalf("final status = %q, want failed", final.Status)
	}

	accounts, err := s.ListAccounts(t.Context())
	if err != nil {
		t.Fatalf("ListAccounts: %v", err)
	}
	for _, acc := range accounts {
		if strings.EqualFold(acc.AccountType, "qoder") {
			t.Fatalf("a Qoder account was persisted despite failed verification: %+v", acc)
		}
	}
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
