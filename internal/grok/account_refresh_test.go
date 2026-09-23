package grok

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

func TestProbeWebAccountQuotaSecondWitnessMustAlsoBeAuthFailure(t *testing.T) {
	var quotaCalls int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/auth/session":
			_, _ = w.Write([]byte(`{"status":"authenticated","session":{"userId":"user-ok"}}`))
		case "/rest/rate-limits":
			if atomic.AddInt32(&quotaCalls, 1) == 1 {
				w.WriteHeader(http.StatusUnauthorized)
				_, _ = w.Write([]byte(`{"status":"unauthenticated"}`))
				return
			}
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"error":"temporary"}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer upstream.Close()

	result := ProbeWebAccount(context.Background(), New(&config.Config{GrokAPIBaseURL: upstream.URL}), "sso=token", WebRefreshOptions{
		IdentityTimeout: time.Second,
		QuotaTimeout:    time.Second,
		RetryDelay:      time.Millisecond,
	})
	if result.AuthRejected {
		t.Fatal("one auth rejection followed by a non-auth error became a credential verdict")
	}
	if result.QuotaErr == nil || atomic.LoadInt32(&quotaCalls) != 3 {
		t.Fatalf("quota error/calls = %v/%d, want second non-auth observation after retry", result.QuotaErr, quotaCalls)
	}
}

func TestProbeWebAccountDoubleQuotaRejectionIsFinal(t *testing.T) {
	var quotaCalls int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/api/auth/session" {
			_, _ = w.Write([]byte(`{"status":"authenticated","session":{"userId":"user-ok"}}`))
			return
		}
		atomic.AddInt32(&quotaCalls, 1)
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"status":"unauthenticated"}`))
	}))
	defer upstream.Close()

	result := ProbeWebAccount(context.Background(), New(&config.Config{GrokAPIBaseURL: upstream.URL}), "token", WebRefreshOptions{RetryDelay: time.Millisecond})
	if !result.AuthRejected || atomic.LoadInt32(&quotaCalls) != 4 {
		t.Fatalf("AuthRejected/calls = %v/%d, want true/4 (auto and fast per witness)", result.AuthRejected, quotaCalls)
	}
}

func TestApplyWebRefreshKeepsLastKnownGoodOnQuotaFailure(t *testing.T) {
	acc := &store.Account{UserID: "old-user", GrokWebQuota: store.GrokWebQuotaSnapshot{
		Auto: store.GrokQuotaWindow{HasRemaining: true, Remaining: 17},
	}}
	ApplyWebRefresh(acc, WebRefreshResult{
		Identity:    AccountIdentity{UserID: "new-user"},
		QuotaErr:    context.DeadlineExceeded,
		IdentityErr: nil,
	})
	if acc.UserID != "new-user" {
		t.Fatalf("identity = %q, want successful observation applied", acc.UserID)
	}
	if !acc.GrokWebQuota.Auto.HasRemaining || acc.GrokWebQuota.Auto.Remaining != 17 {
		t.Fatalf("last-known-good quota overwritten: %+v", acc.GrokWebQuota.Auto)
	}
}
