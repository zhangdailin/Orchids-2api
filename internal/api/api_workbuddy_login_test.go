package api

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"orchids-api/internal/config"
	"orchids-api/internal/store"
	"orchids-api/internal/workbuddy"
)

// workbuddyAuthServer stubs the WorkBuddy authorization endpoints the login
// flow uses. It records nothing secret and never echoes credentials.
func workbuddyAuthServer(t *testing.T, tokenStatus []string, catalog string) *httptest.Server {
	t.Helper()
	var polls int
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v2/plugin/auth/state":
			if r.Method != http.MethodPost {
				t.Errorf("auth/state method = %s, want POST", r.Method)
			}
			if r.URL.Query().Get("platform") != "workbuddy-ai" {
				t.Errorf("platform = %q, want workbuddy-ai", r.URL.Query().Get("platform"))
			}
			if got := r.Header.Get("X-No-Authorization"); got == "" {
				t.Error("auth/state must be sent without an Authorization header")
			}
			_, _ = w.Write([]byte(`{"code":0,"msg":"OK","data":{"state":"state-123","authUrl":"https://www.workbuddy.ai/login?platform=workbuddy-ai&state=state-123"}}`))
		case "/v2/plugin/auth/token":
			if got := r.URL.Query().Get("state"); got != "state-123" {
				t.Errorf("state = %q, want state-123", got)
			}
			index := polls
			polls++
			if index >= len(tokenStatus) {
				index = len(tokenStatus) - 1
			}
			_, _ = w.Write([]byte(tokenStatus[index]))
		case "/v2/plugin/login/account":
			_, _ = w.Write([]byte(`{"code":0,"message":"","data":{"uid":"uid-abc","nickname":"operator@example.com","email":"operator@example.com"}}`))
		case "/v3/config":
			_, _ = w.Write([]byte(catalog))
		default:
			http.NotFound(w, r)
		}
	}))
}

const workbuddyCatalogBody = `{"code":0,"data":{"models":[
  {"id":"hy3","name":"HY3","disabled":false},
  {"id":"default-model","name":"Default","disabled":false}
],"agents":[{"name":"cli","models":["default-model","hy3"]}]}}`

// testJWT builds an unsigned token whose payload carries the identity claims the
// client reads. The upstream verifies the signature; the gateway only needs a
// decodable payload to learn the account identity.
func testJWT(uid, email string) string {
	encode := func(value any) string {
		raw, _ := json.Marshal(value)
		return base64.RawURLEncoding.EncodeToString(raw)
	}
	header := encode(map[string]string{"alg": "RS256", "typ": "JWT"})
	payload := encode(map[string]any{
		"sub":   uid,
		"email": email,
		"iss":   "https://www.workbuddy.ai/auth/realms/copilot",
		"exp":   time.Now().Add(48 * time.Hour).Unix(),
	})
	return header + "." + payload + ".signature"
}

func stubWorkBuddyLoginClient(t *testing.T, baseURL string) {
	t.Helper()
	previous := newWorkBuddyLoginClient
	t.Cleanup(func() { newWorkBuddyLoginClient = previous })
	newWorkBuddyLoginClient = func(acc *store.Account, cfg *config.Config) *workbuddy.Client {
		client := workbuddy.NewFromAccount(acc, cfg)
		client.SetBaseURLForTest(baseURL)
		return client
	}
}

func startWorkBuddyLoginRequest(t *testing.T, method, path, body string) *http.Request {
	t.Helper()
	var reader *strings.Reader
	if body == "" {
		reader = strings.NewReader("")
	} else {
		reader = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, path, reader)
	req.Host = "localhost"
	req.Header.Set("Origin", "http://localhost")
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	return req
}

// decodeLoginError returns the machine-readable code the admin UI uses to
// explain a failed login attempt.
func decodeLoginError(t *testing.T, body string) string {
	t.Helper()
	var payload struct {
		Code  string `json:"code"`
		Error string `json:"error"`
	}
	if err := json.Unmarshal([]byte(body), &payload); err != nil {
		t.Fatalf("login error is not JSON: %v (body=%q)", err, body)
	}
	if payload.Error == "" {
		t.Fatalf("login error carried no message: %q", body)
	}
	if payload.Code == "" {
		// A code-less error cannot be translated by the UI.
		t.Fatalf("login error carried no code: %q", body)
	}
	return payload.Code
}

func TestHandleWorkBuddyLogin_RejectsCrossOriginStart(t *testing.T) {
	t.Parallel()

	s, _ := newTestStore(t, "wb-login:")
	a := New(s, "", "", &config.Config{})
	req := startWorkBuddyLoginRequest(t, http.MethodPost, "/api/workbuddy/login", "")
	req.Header.Set("Origin", "https://evil.example.com")
	rec := httptest.NewRecorder()

	a.HandleWorkBuddyLogin(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", rec.Code)
	}
	if code := decodeLoginError(t, rec.Body.String()); code != "origin_mismatch" {
		t.Fatalf("code = %q, want origin_mismatch", code)
	}
	if strings.Contains(rec.Body.String(), "state") {
		t.Fatalf("body leaked login state: %q", rec.Body.String())
	}
}

func TestHandleWorkBuddyLogin_RejectsInsecureOrigin(t *testing.T) {
	t.Parallel()

	s, _ := newTestStore(t, "wb-login:")
	a := New(s, "", "", &config.Config{})
	req := startWorkBuddyLoginRequest(t, http.MethodPost, "/api/workbuddy/login", "")
	req.Host = "admin.example.com"
	req.Header.Set("Origin", "http://admin.example.com")
	rec := httptest.NewRecorder()

	a.HandleWorkBuddyLogin(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", rec.Code)
	}
	if code := decodeLoginError(t, rec.Body.String()); code != "insecure_origin" {
		t.Fatalf("code = %q, want insecure_origin", code)
	}
}

func TestHandleWorkBuddyLogin_ReportsUnreachableUpstream(t *testing.T) {
	s, _ := newTestStore(t, "wb-login:")
	// A closed listener stands in for a blocked or unreachable upstream.
	dead := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	deadURL := dead.URL
	dead.Close()
	stubWorkBuddyLoginClient(t, deadURL)

	a := New(s, "", "", &config.Config{})
	rec := httptest.NewRecorder()
	a.HandleWorkBuddyLogin(rec, startWorkBuddyLoginRequest(t, http.MethodPost, "/api/workbuddy/login", ""))

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", rec.Code)
	}
	if code := decodeLoginError(t, rec.Body.String()); code != "upstream_unreachable" {
		t.Fatalf("code = %q, want upstream_unreachable (body=%q)", code, rec.Body.String())
	}
}

func TestHandleWorkBuddyLogin_ReportsUpstreamRejection(t *testing.T) {
	s, _ := newTestStore(t, "wb-login:")
	auth := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"code":40301,"msg":"forbidden"}`))
	}))
	defer auth.Close()
	stubWorkBuddyLoginClient(t, auth.URL)

	a := New(s, "", "", &config.Config{})
	rec := httptest.NewRecorder()
	a.HandleWorkBuddyLogin(rec, startWorkBuddyLoginRequest(t, http.MethodPost, "/api/workbuddy/login", ""))

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", rec.Code)
	}
	if code := decodeLoginError(t, rec.Body.String()); code != "upstream_rejected" {
		t.Fatalf("code = %q, want upstream_rejected (body=%q)", code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "40301") {
		t.Fatalf("upstream business code leaked to the client: %q", rec.Body.String())
	}
}

func TestHandleWorkBuddyLogin_RequiresStore(t *testing.T) {
	t.Parallel()

	a := New(nil, "", "", &config.Config{})
	rec := httptest.NewRecorder()
	a.HandleWorkBuddyLogin(rec, startWorkBuddyLoginRequest(t, http.MethodPost, "/api/workbuddy/login", ""))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rec.Code)
	}
}

func TestHandleWorkBuddyLogin_RejectsNonJSONBody(t *testing.T) {
	t.Parallel()

	s, _ := newTestStore(t, "wb-login:")
	a := New(s, "", "", &config.Config{})
	req := startWorkBuddyLoginRequest(t, http.MethodPost, "/api/workbuddy/login", `enabled=true`)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()

	a.HandleWorkBuddyLogin(rec, req)
	if rec.Code != http.StatusUnsupportedMediaType {
		t.Fatalf("status = %d, want 415", rec.Code)
	}
}

func TestHandleWorkBuddyLogin_StartReturnsOfficialLoginURL(t *testing.T) {
	s, _ := newTestStore(t, "wb-login:")
	auth := workbuddyAuthServer(t, []string{`{"code":11217,"msg":"11217:login ing..."}`}, workbuddyCatalogBody)
	defer auth.Close()
	stubWorkBuddyLoginClient(t, auth.URL)

	a := New(s, "", "", &config.Config{})
	rec := httptest.NewRecorder()
	a.HandleWorkBuddyLogin(rec, startWorkBuddyLoginRequest(t, http.MethodPost, "/api/workbuddy/login", `{"enabled":true}`))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	var response struct {
		ID                      string `json:"id"`
		Status                  string `json:"status"`
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
		t.Fatalf("login URL is not parseable: %v", err)
	}
	if parsed.Host != "www.workbuddy.ai" || parsed.Path != "/login" {
		t.Fatalf("login URL = %q, want the official workbuddy.ai login page", response.VerificationURIComplete)
	}
	if parsed.Query().Get("state") != "state-123" {
		t.Fatalf("login URL lost the state: %q", response.VerificationURIComplete)
	}
	if parsed.Query().Get("version") != workbuddyClientVersion {
		t.Fatalf("login URL missing version: %q", response.VerificationURIComplete)
	}

	// The pending transaction must be pollable. The upstream state is part of
	// the official login URL the browser needs, but the transport-level fields
	// (device code / user code) must stay empty for this flow.
	rec2 := httptest.NewRecorder()
	a.HandleWorkBuddyLogin(rec2, startWorkBuddyLoginRequest(t, http.MethodGet, "/api/workbuddy/login/"+response.ID, ""))
	if rec2.Code != http.StatusOK {
		t.Fatalf("poll status = %d", rec2.Code)
	}
	var polled deviceLoginResponse
	if err := json.Unmarshal(rec2.Body.Bytes(), &polled); err != nil {
		t.Fatalf("decode poll: %v", err)
	}
	if polled.UserCode != "" {
		t.Fatalf("poll exposed a device user code: %q", polled.UserCode)
	}
	if !strings.HasPrefix(polled.VerificationURIComplete, "https://www.workbuddy.ai/login?") {
		t.Fatalf("poll returned an unexpected login URL: %q", polled.VerificationURIComplete)
	}
}

func TestHandleWorkBuddyLogin_CompletesAndPersistsAccount(t *testing.T) {
	s, _ := newTestStore(t, "wb-login:")
	auth := workbuddyAuthServer(t, []string{
		`{"code":11217,"msg":"11217:login ing..."}`,
		`{"code":0,"data":{"accessToken":"` + testJWT("uid-abc", "operator@example.com") + `","refreshToken":"refresh-abc","expiresIn":31536000}}`,
	}, workbuddyCatalogBody)
	defer auth.Close()
	stubWorkBuddyLoginClient(t, auth.URL)

	a := New(s, "", "", &config.Config{})
	rec := httptest.NewRecorder()
	a.HandleWorkBuddyLogin(rec, startWorkBuddyLoginRequest(t, http.MethodPost, "/api/workbuddy/login", ""))

	var started struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &started); err != nil || started.ID == "" {
		t.Fatalf("start response = %q", rec.Body.String())
	}

	deadline := time.Now().Add(10 * time.Second)
	var final deviceLoginResponse
	for time.Now().Before(deadline) {
		pollRec := httptest.NewRecorder()
		a.HandleWorkBuddyLogin(pollRec, startWorkBuddyLoginRequest(t, http.MethodGet, "/api/workbuddy/login/"+started.ID, ""))
		if pollRec.Code != http.StatusOK {
			t.Fatalf("poll status = %d", pollRec.Code)
		}
		if err := json.Unmarshal(pollRec.Body.Bytes(), &final); err != nil {
			t.Fatalf("decode poll: %v", err)
		}
		if final.Status != "pending" {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if final.Status != "complete" {
		t.Fatalf("final status = %q (%s), want complete", final.Status, final.Message)
	}
	if final.AccountID == 0 {
		t.Fatal("completed login did not report an account id")
	}

	acc, err := s.GetAccount(context.Background(), final.AccountID)
	if err != nil {
		t.Fatalf("GetAccount() error = %v", err)
	}
	if acc.AccountType != "workbuddy" {
		t.Fatalf("account type = %q", acc.AccountType)
	}
	if acc.WorkBuddyRefreshToken != "refresh-abc" {
		t.Fatalf("stored refresh token = %q", acc.WorkBuddyRefreshToken)
	}
	if acc.WorkBuddyUID != "uid-abc" || acc.Email != "operator@example.com" {
		t.Fatalf("identity = %q/%q", acc.WorkBuddyUID, acc.Email)
	}
	if len(acc.WorkBuddyModelIDs) != 2 {
		t.Fatalf("model snapshot = %v, want the account catalog", acc.WorkBuddyModelIDs)
	}
	if acc.ClientCookie != "" || acc.Token != "" || acc.RefreshToken != "" {
		t.Fatalf("login stored credentials in a shared slot: %+v", acc)
	}

	// A second login for the same credential must update, not duplicate.
	auth2 := workbuddyAuthServer(t, []string{
		`{"code":0,"data":{"accessToken":"` + testJWT("uid-abc", "operator@example.com") + `","refreshToken":"refresh-abc","expiresIn":31536000}}`,
	}, workbuddyCatalogBody)
	defer auth2.Close()
	stubWorkBuddyLoginClient(t, auth2.URL)

	rec3 := httptest.NewRecorder()
	a.HandleWorkBuddyLogin(rec3, startWorkBuddyLoginRequest(t, http.MethodPost, "/api/workbuddy/login", ""))
	var second struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(rec3.Body.Bytes(), &second); err != nil || second.ID == "" {
		t.Fatalf("second start response = %q", rec3.Body.String())
	}
	deadline = time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		pollRec := httptest.NewRecorder()
		a.HandleWorkBuddyLogin(pollRec, startWorkBuddyLoginRequest(t, http.MethodGet, "/api/workbuddy/login/"+second.ID, ""))
		if err := json.Unmarshal(pollRec.Body.Bytes(), &final); err != nil {
			break
		}
		if final.Status != "pending" {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if final.Status != "complete" || final.AccountID != acc.ID {
		t.Fatalf("re-login = %+v, want the existing account %d", final, acc.ID)
	}
	accounts, err := s.ListAccounts(context.Background())
	if err != nil {
		t.Fatalf("ListAccounts() error = %v", err)
	}
	workbuddyAccounts := 0
	for _, candidate := range accounts {
		if strings.EqualFold(candidate.AccountType, "workbuddy") {
			workbuddyAccounts++
		}
	}
	if workbuddyAccounts != 1 {
		t.Fatalf("workbuddy accounts = %d, want 1 (re-login must update in place)", workbuddyAccounts)
	}
}

func TestHandleWorkBuddyLogin_ReportsVerificationFailure(t *testing.T) {
	s, _ := newTestStore(t, "wb-login:")
	// A token that cannot list the catalog must not be persisted.
	auth := workbuddyAuthServer(t, []string{
		`{"code":0,"data":{"accessToken":"` + testJWT("uid-abc", "operator@example.com") + `","refreshToken":"refresh-abc","expiresIn":3600}}`,
	}, `{"code":6004,"msg":"6004:usage exceeds frequency limit"}`)
	defer auth.Close()
	stubWorkBuddyLoginClient(t, auth.URL)

	a := New(s, "", "", &config.Config{})
	rec := httptest.NewRecorder()
	a.HandleWorkBuddyLogin(rec, startWorkBuddyLoginRequest(t, http.MethodPost, "/api/workbuddy/login", ""))
	var started struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &started); err != nil || started.ID == "" {
		t.Fatalf("start response = %q", rec.Body.String())
	}

	var final deviceLoginResponse
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		pollRec := httptest.NewRecorder()
		a.HandleWorkBuddyLogin(pollRec, startWorkBuddyLoginRequest(t, http.MethodGet, "/api/workbuddy/login/"+started.ID, ""))
		if err := json.Unmarshal(pollRec.Body.Bytes(), &final); err != nil {
			t.Fatalf("decode poll: %v", err)
		}
		if final.Status != "pending" {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if final.Status != "failed" {
		t.Fatalf("status = %q, want failed", final.Status)
	}
	accounts, err := s.ListAccounts(context.Background())
	if err != nil {
		t.Fatalf("ListAccounts() error = %v", err)
	}
	for _, candidate := range accounts {
		if strings.EqualFold(candidate.AccountType, "workbuddy") {
			t.Fatalf("an unverified account was persisted: %+v", candidate)
		}
	}
}

func TestHandleWorkBuddyLogin_CancelForgetsTransaction(t *testing.T) {
	s, _ := newTestStore(t, "wb-login:")
	auth := workbuddyAuthServer(t, []string{`{"code":11217,"msg":"11217:login ing..."}`}, workbuddyCatalogBody)
	defer auth.Close()
	stubWorkBuddyLoginClient(t, auth.URL)

	a := New(s, "", "", &config.Config{})
	rec := httptest.NewRecorder()
	a.HandleWorkBuddyLogin(rec, startWorkBuddyLoginRequest(t, http.MethodPost, "/api/workbuddy/login", ""))
	var started struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &started); err != nil || started.ID == "" {
		t.Fatalf("start response = %q", rec.Body.String())
	}

	deleteRec := httptest.NewRecorder()
	a.HandleWorkBuddyLogin(deleteRec, startWorkBuddyLoginRequest(t, http.MethodDelete, "/api/workbuddy/login/"+started.ID, ""))
	if deleteRec.Code != http.StatusNoContent {
		t.Fatalf("delete status = %d", deleteRec.Code)
	}

	missingRec := httptest.NewRecorder()
	a.HandleWorkBuddyLogin(missingRec, startWorkBuddyLoginRequest(t, http.MethodGet, "/api/workbuddy/login/"+started.ID, ""))
	if missingRec.Code != http.StatusNotFound {
		t.Fatalf("poll after cancel status = %d, want 404", missingRec.Code)
	}
}

func TestHandleWorkBuddyLogin_RejectsUnknownMethodAndMissingSession(t *testing.T) {
	t.Parallel()

	s, _ := newTestStore(t, "wb-login:")
	a := New(s, "", "", &config.Config{})

	putRec := httptest.NewRecorder()
	a.HandleWorkBuddyLogin(putRec, startWorkBuddyLoginRequest(t, http.MethodPut, "/api/workbuddy/login", "{}"))
	if putRec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("PUT status = %d, want 405", putRec.Code)
	}

	missingRec := httptest.NewRecorder()
	a.HandleWorkBuddyLogin(missingRec, startWorkBuddyLoginRequest(t, http.MethodGet, "/api/workbuddy/login/does-not-exist", ""))
	if missingRec.Code != http.StatusNotFound {
		t.Fatalf("unknown id status = %d, want 404", missingRec.Code)
	}
}
