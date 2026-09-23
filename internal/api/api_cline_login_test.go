package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"orchids-api/internal/config"
	"orchids-api/internal/store"
)

// clineAuthServer stubs the two upstreams a Cline login touches: WorkOS for the
// device grant and the Cline API for the exchange and the model feed. It records
// no credential it was not asked for.
type clineAuthServer struct {
	*httptest.Server
	polls        int
	registerBody map[string]string
	host         string
}

func newClineAuthServer(t *testing.T, pollAnswers []func(w http.ResponseWriter)) *clineAuthServer {
	t.Helper()
	stub := &clineAuthServer{}
	stub.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/user_management/authorize/device":
			if got := r.FormValue("client_id"); got == "" {
				t.Errorf("device authorize is missing client_id")
			}
			// The stub stands in for WorkOS, so it answers with its own host:
			// the client refuses to hand a browser a page on a foreign host,
			// and this test is about the console's response, not the check.
			page := "http://" + r.Host + "/device"
			_, _ = w.Write([]byte(`{"device_code":"dev-1","user_code":"ABCD-EFGH",` +
				`"verification_uri":"` + page + `",` +
				`"verification_uri_complete":"` + page + `?code=ABCD-EFGH",` +
				`"interval":5,"expires_in":900}`))
		case "/user_management/authenticate":
			index := stub.polls
			stub.polls++
			if index >= len(pollAnswers) {
				index = len(pollAnswers) - 1
			}
			pollAnswers[index](w)
		case "/api/v1/auth/register":
			var body map[string]string
			_ = json.NewDecoder(r.Body).Decode(&body)
			stub.registerBody = body
			if strings.TrimSpace(body["accessToken"]) == "" {
				t.Errorf("register is missing the WorkOS access token")
			}
			_, _ = w.Write([]byte(`{"data":{"accessToken":"cline-access-1",` +
				`"refreshToken":"cline-refresh-1","expiresAt":4102444800000,` +
				`"userInfo":{"email":"operator@example.com"}}}`))
		case "/api/v1/ai/cline/recommended-models":
			_, _ = w.Write([]byte(`{"free":[{"id":"x-ai/grok-4.1-fast","name":"Grok 4.1 Fast"}]}`))
		default:
			http.NotFound(w, r)
		}
	}))
	stub.host = strings.TrimPrefix(stub.URL, "http://")
	return stub
}

func clineLoginConfig(baseURL string) *config.Config {
	return &config.Config{
		ClineAPIBaseURL:         baseURL + "/api/v1",
		ClineWorkOSAuthorizeURL: baseURL + "/user_management/authorize/device",
		ClineWorkOSTokenURL:     baseURL + "/user_management/authenticate",
	}
}

func clineLoginRequest(t *testing.T, method, path, body string) *http.Request {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Host = "localhost"
	req.Header.Set("Origin", "http://localhost")
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	return req
}

// TestHandleClineLogin_StartHidesDeviceCode proves the start response carries
// only what a browser needs: the official page and the user code. The device
// code is the half that can be exchanged for a credential and must never reach
// the console.
func TestHandleClineLogin_StartHidesDeviceCode(t *testing.T) {
	s, _ := newTestStore(t, "cline-login:")
	auth := newClineAuthServer(t, []func(w http.ResponseWriter){
		func(w http.ResponseWriter) { w.WriteHeader(http.StatusNotFound) },
	})
	defer auth.Close()
	a := New(s, "", "", clineLoginConfig(auth.URL))
	rec := httptest.NewRecorder()
	a.HandleClineLogin(rec, clineLoginRequest(t, http.MethodPost, "/api/cline/login", `{"enabled":true}`))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (%s)", rec.Code, rec.Body.String())
	}
	var response struct {
		ID                 string `json:"id"`
		VerifyURI          string `json:"verification_uri"`
		VerifyFull         string `json:"verification_uri_complete"`
		UserCode           string `json:"user_code"`
		DeviceCode         string `json:"device_code"`
		SessionFingerprint string `json:"session_fingerprint"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if response.ID == "" {
		t.Fatal("start returned no transaction id")
	}
	if response.VerifyURI == "" || response.VerifyFull == "" {
		t.Fatalf("start returned no verification page: %+v", response)
	}
	if response.UserCode == "" {
		t.Fatal("start returned no user code for the operator to confirm")
	}
	if response.DeviceCode != "" {
		t.Errorf("start response leaked the device code (%q)", response.DeviceCode)
	}
	if strings.Contains(rec.Body.String(), "dev-1") {
		t.Errorf("start response leaked the device code: %s", rec.Body.String())
	}
}

// TestHandleClineLogin_PollPersistsOAuthAccount drives the flow from start to a
// stored account and pins what the record holds: the Cline pair in its own
// fields, the feed snapshot, and no generic credential slot.
func TestHandleClineLogin_PollPersistsOAuthAccount(t *testing.T) {
	s, _ := newTestStore(t, "cline-login:")
	auth := newClineAuthServer(t, []func(w http.ResponseWriter){
		func(w http.ResponseWriter) {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":"authorization_pending"}`))
		},
		func(w http.ResponseWriter) {
			_, _ = w.Write([]byte(`{"access_token":"workos-access","refresh_token":"workos-refresh"}`))
		},
	})
	defer auth.Close()
	a := New(s, "", "", clineLoginConfig(auth.URL))
	start := httptest.NewRecorder()
	a.HandleClineLogin(start, clineLoginRequest(t, http.MethodPost, "/api/cline/login", ""))
	var started struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(start.Body.Bytes(), &started); err != nil {
		t.Fatalf("decode start: %v", err)
	}
	a.clineLogins.update(started.ID, func(login *clineLoginTransaction) { login.interval = time.Millisecond })

	var stored *store.Account
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		rec := httptest.NewRecorder()
		a.HandleClineLogin(rec, clineLoginRequest(t, http.MethodGet, "/api/cline/login/"+started.ID, ""))
		var state struct {
			Status string `json:"status"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &state); err != nil {
			t.Fatalf("decode poll: %v", err)
		}
		accounts, err := s.ListAccounts(t.Context())
		if err != nil {
			t.Fatalf("list accounts: %v", err)
		}
		for _, acc := range accounts {
			if strings.EqualFold(acc.AccountType, "cline") {
				stored = acc
			}
		}
		if state.Status != "pending" {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if stored == nil {
		t.Fatal("login completed without storing a cline account")
	}
	if stored.ClineAccessToken == "" || stored.ClineRefreshToken == "" {
		t.Fatalf("stored account is missing the cline pair: %+v", stored)
	}
	if stored.ClineEmail != "operator@example.com" {
		t.Errorf("email = %q, want operator@example.com", stored.ClineEmail)
	}
	if len(stored.ClineModelIDs) != 1 || !strings.Contains(stored.ClineModelIDs[0], "x-ai/grok-4.1-fast") {
		t.Errorf("model ids = %v, want the recommended-models feed", stored.ClineModelIDs)
	}
	if stored.ClineModelsSyncedAt.IsZero() {
		t.Error("model sync timestamp is zero after successful login catalog read")
	}
	if stored.Token != "" || stored.RefreshToken != "" || stored.ClientCookie != "" {
		t.Errorf("stored account wrote a generic credential slot: %+v", stored)
	}
	if got := auth.registerBody["accessToken"]; got != "workos-access" {
		t.Errorf("register accessToken = %q, want workos-access", got)
	}
}

// TestHandleClineLogin_RejectsManualCredential pins the OAuth-only contract: a
// Cline account cannot be created by pasting a token.
func TestHandleClineLogin_RejectsManualCredential(t *testing.T) {
	s, _ := newTestStore(t, "cline-manual:")
	a := New(s, "", "", &config.Config{})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/accounts", strings.NewReader(
		`{"account_type":"cline","name":"manual","cline_access_token":"access","cline_refresh_token":"refresh"}`))
	req.Header.Set("Content-Type", "application/json")
	a.HandleAccounts(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (%s)", rec.Code, rec.Body.String())
	}
	if !strings.Contains(strings.ToLower(rec.Body.String()), "cline/login") {
		t.Errorf("rejection must point at the official login, got %q", rec.Body.String())
	}
	accounts, err := s.ListAccounts(t.Context())
	if err != nil {
		t.Fatalf("list accounts: %v", err)
	}
	if len(accounts) != 0 {
		t.Fatalf("account count = %d, want 0", len(accounts))
	}
}

// TestHandleClineLogin_AccountOutputHidesRefreshToken proves the durable
// credential is write-only: the account API may show that a credential exists
// but must not return it.
func TestHandleClineLogin_AccountOutputHidesRefreshToken(t *testing.T) {
	s, _ := newTestStore(t, "cline-output:")
	acc := &store.Account{
		Name:              "cline@example.com",
		AccountType:       "cline",
		ClineAccessToken:  "access-secret",
		ClineRefreshToken: "refresh-secret",
	}
	if err := s.CreateAccount(t.Context(), acc); err != nil {
		t.Fatalf("create account: %v", err)
	}
	out := normalizeAccountOutput(acc)
	if out == nil || out.Account == nil {
		t.Fatal("normalizeAccountOutput returned nothing")
	}
	if out.Account.ClineRefreshToken != "" {
		t.Errorf("account output leaked the cline refresh token (%q)", out.Account.ClineRefreshToken)
	}
	raw, err := json.Marshal(out)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(raw), "refresh-secret") {
		t.Errorf("account output leaked the cline refresh token: %s", raw)
	}
}
