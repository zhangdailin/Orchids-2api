package api

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"orchids-api/internal/config"
	"orchids-api/internal/grok"
	"orchids-api/internal/store"
)

// grokOAuthJWT mints a decodable access token so the login flow can copy the
// non-secret identity claims the way the real xAI token does.
func grokOAuthJWT(t *testing.T, sub, email, team string) string {
	t.Helper()
	encode := func(value interface{}) string {
		raw, err := json.Marshal(value)
		if err != nil {
			t.Fatalf("marshal claim: %v", err)
		}
		return base64.RawURLEncoding.EncodeToString(raw)
	}
	return encode(map[string]string{"alg": "RS256", "typ": "JWT"}) + "." +
		encode(map[string]string{"sub": sub, "email": email, "team_id": team}) + ".sig"
}

// TestGrokDeviceLogin_AddingOAuthAccountDoesNotDisturbExistingAccounts is the
// regression guard for "adding a second Grok account invalidates the first one".
// Completing a device login must only ever create the account it owns.
func TestGrokDeviceLogin_AddingOAuthAccountDoesNotDisturbExistingAccounts(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/device":
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"device_code":      "device-code-1",
				"user_code":        "ABCD-EFGH",
				"verification_uri": "https://accounts.x.ai/device",
				"expires_in":       600,
				"interval":         1,
			})
		case "/token":
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"access_token":  grokOAuthJWT(t, "user-two", "two@example.com", "team-two"),
				"refresh_token": "refresh-two",
				"expires_in":    3600,
			})
		default:
			_, _ = w.Write([]byte(`{}`))
		}
	}))
	defer upstream.Close()

	s, _ := newTestStore(t, "grok-oauth:")
	ctx := context.Background()

	existingSSO := &store.Account{
		Name:           "existing-sso",
		AccountType:    "grok",
		CredentialType: "sso",
		GrokProvider:   "web",
		ClientCookie:   "sso=existing-web-token",
		Enabled:        true,
		Weight:         1,
	}
	existingOAuth := &store.Account{
		Name:              "existing-oauth",
		AccountType:       "grok",
		CredentialType:    "oauth",
		GrokProvider:      "build",
		OAuthAccessToken:  grokOAuthJWT(t, "user-one", "one@example.com", "team-one"),
		OAuthRefreshToken: "refresh-one",
		AgentMode:         "grok-build-0.1",
		Enabled:           true,
		Weight:            1,
	}
	for _, acc := range []*store.Account{existingSSO, existingOAuth} {
		if err := s.CreateAccount(ctx, acc); err != nil {
			t.Fatalf("CreateAccount() error = %v", err)
		}
	}
	beforeOAuth, _ := s.GetAccount(ctx, existingOAuth.ID)

	cfg := &config.Config{
		GrokCLIOAuthDeviceURL: upstream.URL + "/device",
		GrokCLIOAuthTokenURL:  upstream.URL + "/token",
		// Keep the post-create sync away from the real network.
		GrokAPIBaseURL:     upstream.URL + "/unused",
		GrokCLIBaseURL:     upstream.URL + "/unused-cli",
		GrokConsoleBaseURL: upstream.URL + "/unused-console",
	}
	a := New(s, "", "", cfg)

	startRec := httptest.NewRecorder()
	a.HandleGrokDeviceAuthorization(startRec, httptest.NewRequest(http.MethodPost, "/api/grok/device-auth", nil))
	if startRec.Code != http.StatusOK {
		t.Fatalf("start status = %d body=%s", startRec.Code, startRec.Body.String())
	}
	var started deviceLoginResponse
	if err := json.Unmarshal(startRec.Body.Bytes(), &started); err != nil || started.ID == "" {
		t.Fatalf("start response = %q", startRec.Body.String())
	}

	// Drive the poll loop directly against the stubbed token endpoint.
	a.grokDeviceLoginMu.Lock()
	login := a.grokDeviceLogins[started.ID]
	if login == nil {
		a.grokDeviceLoginMu.Unlock()
		t.Fatal("login transaction was not registered")
	}
	login.deviceCode = "device-code-1"
	login.interval = 50 * time.Millisecond
	a.grokDeviceLoginMu.Unlock()

	pollCtx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	done := make(chan struct{})
	go func() {
		a.pollGrokDeviceAuthorization(pollCtx, started.ID, grok.NewDeviceAuthenticator(cfg))
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(8 * time.Second):
		t.Fatal("device login did not settle")
	}

	accounts, err := s.ListAccounts(ctx)
	if err != nil {
		t.Fatalf("ListAccounts() error = %v", err)
	}
	created := false
	for _, acc := range accounts {
		if acc.Name == "grok-device-login" {
			created = true
		}
	}
	if !created {
		t.Fatal("the new OAuth account was not created")
	}

	// The pre-existing accounts must be untouched.
	afterSSO, err := s.GetAccount(ctx, existingSSO.ID)
	if err != nil {
		t.Fatalf("the existing SSO account disappeared: %v", err)
	}
	afterOAuth, err := s.GetAccount(ctx, existingOAuth.ID)
	if err != nil {
		t.Fatalf("the existing OAuth account disappeared: %v", err)
	}
	if afterSSO.ClientCookie != "sso=existing-web-token" || afterSSO.CredentialType != "sso" {
		t.Fatalf("existing SSO credential changed: %+v", afterSSO)
	}
	if !afterSSO.Enabled || afterSSO.StatusCode != "" {
		t.Fatalf("existing SSO account was disturbed: enabled=%v status=%q", afterSSO.Enabled, afterSSO.StatusCode)
	}
	if afterOAuth.OAuthRefreshToken != "refresh-one" || afterOAuth.OAuthAccessToken != beforeOAuth.OAuthAccessToken {
		t.Fatal("existing OAuth credentials were overwritten by the new login")
	}
	if !afterOAuth.Enabled || afterOAuth.StatusCode != "" {
		t.Fatalf("existing OAuth account was disturbed: enabled=%v status=%q", afterOAuth.Enabled, afterOAuth.StatusCode)
	}
	if afterOAuth.GrokSSOParentID != 0 || afterOAuth.GrokProvider != "build" {
		t.Fatalf("existing OAuth account was reclassified: %+v", afterOAuth)
	}
}

// TestGrokDeviceLogin_SecondLoginForSameAccountUpdatesInPlace pins the intended
// duplicate behaviour: the same xAI identity must not create a second row.
func TestGrokDeviceLogin_SecondLoginForSameAccountUpdatesInPlace(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/token" {
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"access_token":  grokOAuthJWT(t, "user-one", "one@example.com", "team-one"),
				"refresh_token": "refresh-one",
				"expires_in":    3600,
			})
			return
		}
		_, _ = w.Write([]byte(`{}`))
	}))
	defer upstream.Close()

	s, _ := newTestStore(t, "grok-dup:")
	ctx := context.Background()

	existing := &store.Account{
		Name:              "existing-oauth",
		AccountType:       "grok",
		CredentialType:    "oauth",
		GrokProvider:      "build",
		OAuthAccessToken:  grokOAuthJWT(t, "user-one", "one@example.com", "team-one"),
		OAuthRefreshToken: "refresh-one",
		Enabled:           true,
		Weight:            1,
	}
	if err := s.CreateAccount(ctx, existing); err != nil {
		t.Fatalf("CreateAccount() error = %v", err)
	}

	cfg := &config.Config{GrokCLIOAuthTokenURL: upstream.URL + "/token"}
	a := New(s, "", "", cfg)

	pollCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	pollCancel := func() {}
	a.grokDeviceLoginMu.Lock()
	a.grokDeviceLogins["dup"] = &grokDeviceLogin{
		deviceCode: "device-code-1",
		expiresAt:  time.Now().Add(time.Minute),
		interval:   20 * time.Millisecond,
		cancel:     pollCancel,
		status:     "pending",
	}
	a.grokDeviceLoginMu.Unlock()

	done := make(chan struct{})
	go func() {
		a.pollGrokDeviceAuthorization(pollCtx, "dup", grok.NewDeviceAuthenticator(cfg))
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("device login did not settle")
	}

	accounts, err := s.ListAccounts(ctx)
	if err != nil {
		t.Fatalf("ListAccounts() error = %v", err)
	}
	grokAccounts := 0
	for _, acc := range accounts {
		if acc.AccountType == "grok" {
			grokAccounts++
		}
	}
	if grokAccounts != 1 {
		t.Fatalf("grok accounts = %d, want 1: a re-login must not create a duplicate row", grokAccounts)
	}

	a.grokDeviceLoginMu.Lock()
	state := a.grokDeviceLogins["dup"]
	a.grokDeviceLoginMu.Unlock()
	if state == nil || state.status != "complete" {
		t.Fatalf("login state = %+v, want complete", state)
	}
	if state.accountID != existing.ID {
		t.Fatalf("login reported account %d, want the existing account %d", state.accountID, existing.ID)
	}
}
