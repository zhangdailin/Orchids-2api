package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"orchids-api/internal/config"
	"orchids-api/internal/store"
)

// grokSSOFlakyUpstream rejects the session endpoint only for the first N calls,
// then answers normally. It models the transient "unauthenticated" answer that
// made a working account flip to 未授权 for a whole refresh cycle.
func grokSSOFlakyUpstream(t *testing.T, failures int32, seen *int32) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/auth/session":
			if atomic.AddInt32(seen, 1) <= failures {
				w.WriteHeader(http.StatusUnauthorized)
				_, _ = w.Write([]byte(`{"status":"unauthenticated"}`))
				return
			}
			_, _ = w.Write([]byte(`{"status":"authenticated","session":{"userId":"user-ok","email":"ok@example.com","organizationId":"team-ok"}}`))
		case "/rest/rate-limits":
			_, _ = w.Write([]byte(`{"remainingQueries":100,"totalQueries":100,"remainingTokens":1000,"totalTokens":1000,"windowSizeSeconds":3600}`))
		default:
			_, _ = w.Write([]byte(`{}`))
		}
	}))
}

// TestRefreshAccountState_GrokSSORetriesSingleRejection covers the reported
// flapping: one upstream "unauthenticated" answer must not be treated as the
// account's death while the retry accepts the very same cookie.
func TestRefreshAccountState_GrokSSORetriesSingleRejection(t *testing.T) {
	oldDelay := grokSSOAuthRetryDelay
	grokSSOAuthRetryDelay = time.Millisecond
	t.Cleanup(func() { grokSSOAuthRetryDelay = oldDelay })

	var seen int32
	srv := grokSSOFlakyUpstream(t, 1, &seen)
	defer srv.Close()

	s, _ := newTestStore(t, "grok-sso-retry:")
	a := New(s, "", "", &config.Config{GrokAPIBaseURL: srv.URL})

	acc := &store.Account{
		Name:           "grok-sso",
		AccountType:    "grok",
		CredentialType: "sso",
		GrokProvider:   "web",
		ClientCookie:   "sso=token-good",
		Enabled:        true,
		Weight:         1,
	}

	status, _, err := a.refreshAccountState(context.Background(), acc)
	if err != nil {
		t.Fatalf("refreshAccountState() error = %v (status=%q)", err, status)
	}
	if status != "" {
		t.Fatalf("status = %q, want clean: a retried rejection is not a verdict", status)
	}
	if acc.StatusCode != "" || acc.StatusMessage != "" {
		t.Fatalf("account marked %q / %q after a successful retry", acc.StatusCode, acc.StatusMessage)
	}
	if acc.Email != "ok@example.com" || acc.UserID != "user-ok" {
		t.Fatalf("identity was not applied after the retry: user=%q email=%q", acc.UserID, acc.Email)
	}
	if got := atomic.LoadInt32(&seen); got < 2 {
		t.Fatalf("session endpoint calls = %d, want the rejection to be re-asked", got)
	}
}

// TestRefreshAccountState_GrokSSOTwoRejectionsAreFinal is the counterweight: when
// both attempts are refused the account must still be reported as unauthorized.
func TestRefreshAccountState_GrokSSOTwoRejectionsAreFinal(t *testing.T) {
	oldDelay := grokSSOAuthRetryDelay
	grokSSOAuthRetryDelay = time.Millisecond
	t.Cleanup(func() { grokSSOAuthRetryDelay = oldDelay })

	var seen int32
	// Every session call fails; the quota endpoint is never reached.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		atomic.AddInt32(&seen, 1)
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"status":"unauthenticated"}`))
	}))
	defer srv.Close()

	s, _ := newTestStore(t, "grok-sso-final:")
	a := New(s, "", "", &config.Config{GrokAPIBaseURL: srv.URL})

	acc := &store.Account{
		Name:           "grok-sso",
		AccountType:    "grok",
		CredentialType: "sso",
		GrokProvider:   "web",
		ClientCookie:   "sso=token-dead",
		Enabled:        true,
		Weight:         1,
	}

	status, httpStatus, err := a.refreshAccountState(context.Background(), acc)
	if err == nil {
		t.Fatal("refreshAccountState() returned no error for a doubly rejected cookie")
	}
	if status != "401" || httpStatus != http.StatusUnauthorized {
		t.Fatalf("status=%q http=%d, want 401/401 (err=%v)", status, httpStatus, err)
	}
	if got := atomic.LoadInt32(&seen); got != 2 {
		t.Fatalf("session endpoint calls = %d, want exactly 2 (one retry)", got)
	}
}
