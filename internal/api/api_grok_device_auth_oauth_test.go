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
func TestGrokDeviceLogin_SecondLoginForSameAccountUpdatesInPlace(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/token" {
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"access_token":  grokOAuthJWT(t, "user-one", "one@example.com", "team-one"),
				"refresh_token": "refresh-new",
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
		Name:              "grok-device-login",
		AccountType:       "grok",
		CredentialType:    "oauth",
		GrokProvider:      "build",
		UserID:            "user-one",
		OAuthAccessToken:  grokOAuthJWT(t, "user-one", "one@example.com", "team-one"),
		OAuthRefreshToken: "refresh-old",
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
	a.grokLogins.admit("dup", &deviceLogin{
		deviceCode: "device-code-1",
		expiresAt:  time.Now().Add(time.Minute),
		interval:   20 * time.Millisecond,
		cancel:     pollCancel,
		status:     "pending",
	})

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

	var state *deviceLogin
	a.grokLogins.update("dup", func(login *deviceLogin) { state = login })
	if state == nil || state.status != "complete" {
		t.Fatalf("login state = %+v, want complete", state)
	}
	if state.accountID != existing.ID {
		t.Fatalf("login reported account %d, want the existing account %d", state.accountID, existing.ID)
	}
	updated, err := s.GetAccount(ctx, existing.ID)
	if err != nil {
		t.Fatal(err)
	}
	if updated.OAuthRefreshToken != "refresh-new" || updated.Email != "one@example.com" || updated.Name != "one@example.com" {
		t.Fatalf("existing OAuth row was not refreshed in place: %+v", updated)
	}
}
