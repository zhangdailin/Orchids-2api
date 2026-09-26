package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"orchids-api/internal/qoder"
	"orchids-api/internal/store"
)

// A short-lived device token is rotated while the account still has ID zero.
// The return value, rather than the temporary client's no-op store writes, is
// what the browser flow ultimately persists.
func TestQoderLoginRetainsRotationsAndProfileSnapshot(t *testing.T) {
	refreshes := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v1/deviceToken/refresh":
			refreshes++
			var body struct {
				RefreshToken string `json:"refresh_token"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Error(err)
			}
			want := "refresh-1"
			if refreshes > 1 {
				want = fmt.Sprintf("refresh-%d", refreshes)
			}
			if body.RefreshToken != want {
				t.Errorf("refresh token %q, want %q", body.RefreshToken, want)
			}
			fmt.Fprintf(w, `{"device_token":"access-%d","refresh_token":"refresh-%d","expires_in":3600}`, refreshes+1, refreshes+1)
		case "/api/v1/userinfo":
			if r.Header.Get("Authorization") != "Bearer access-2" {
				t.Errorf("profile bearer = %q", r.Header.Get("Authorization"))
			}
			_, _ = w.Write([]byte(`{"uid":"profile-uid","name":"profile-name","email":"profile@example.com","organization_id":"profile-org","organization_tags":["profile-tag"]}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	a := &API{}
	acc, err := a.buildQoderAccountFromCredentialsWithFactory(t.Context(), "test", "machine-id", qoder.Credentials{
		AccessToken: "access-1", RefreshToken: "refresh-1", AccessExpiresAt: time.Now().Add(10 * time.Minute), Name: "poll-name",
	}, qoderLoginConfig(server.URL), qoder.NewFromAccount)
	if err != nil {
		t.Fatal(err)
	}
	if refreshes < 2 {
		t.Fatalf("refresh calls = %d; catalog/quota should renew short-lived credentials", refreshes)
	}
	wantAccess := fmt.Sprintf("access-%d", refreshes+1)
	wantRefresh := fmt.Sprintf("refresh-%d", refreshes+1)
	if acc.QoderAccessToken != wantAccess || acc.QoderRefreshToken != wantRefresh {
		t.Fatalf("returned tokens = %q/%q, want %q/%q", acc.QoderAccessToken, acc.QoderRefreshToken, wantAccess, wantRefresh)
	}
	if acc.QoderUserID != "profile-uid" || acc.QoderOrganizationID != "profile-org" || acc.QoderUserName != "profile-name" || acc.Email != "profile@example.com" || len(acc.QoderOrganizationTags) != 1 || acc.QoderOrganizationTags[0] != "profile-tag" {
		t.Fatalf("profile not returned: %+v", acc)
	}
	if acc.QoderRuntimeInfo == "" || acc.QoderRuntimeKey == "" {
		t.Fatal("final runtime fields missing")
	}
	s, _ := newTestStore(t, "qoder-rotated-login:")
	if err := s.CreateAccount(t.Context(), acc); err != nil {
		t.Fatal(err)
	}
	saved, err := s.GetAccount(t.Context(), acc.ID)
	if err != nil {
		t.Fatal(err)
	}
	if saved.QoderAccessToken != wantAccess || saved.QoderRefreshToken != wantRefresh || saved.QoderRuntimeInfo != acc.QoderRuntimeInfo {
		t.Fatalf("persisted snapshot differs: %+v", saved)
	}
}

func TestVerifyQoderRetainsRotationDuringWholeAccountSave(t *testing.T) {
	refreshes := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v1/deviceToken/refresh":
			refreshes++
			fmt.Fprintf(w, `{"device_token":"access-%d","refresh_token":"refresh-%d","expires_in":86400}`, refreshes+1, refreshes+1)
		case "/api/v1/userinfo":
			if r.Header.Get("Authorization") != "Bearer access-2" {
				t.Errorf("profile received %q", r.Header.Get("Authorization"))
			}
			_, _ = w.Write([]byte(`{"uid":"new-uid","organization_id":"new-org"}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	s, _ := newTestStore(t, "qoder-rotated-verify:")
	acc := &store.Account{AccountType: "qoder", Name: "qoder-verify", QoderAccessToken: "access-1", QoderRefreshToken: "refresh-1", QoderExpiresAt: time.Now().Add(time.Minute), QoderMachineID: "machine-id", Enabled: true}
	if err := s.CreateAccount(t.Context(), acc); err != nil {
		t.Fatal(err)
	}
	status, code, err := verifyQoderAccountWithStore(t.Context(), acc, qoderLoginConfig(server.URL), s)
	if err != nil || status != "" || code != 0 {
		t.Fatalf("verify = %q %d %v", status, code, err)
	}
	if refreshes != 1 || acc.QoderAccessToken != "access-2" || acc.QoderRefreshToken != "refresh-2" || acc.QoderUserID != "new-uid" || acc.QoderOrganizationID != "new-org" {
		t.Fatalf("verified snapshot %+v, refreshes=%d", acc, refreshes)
	}
	if err := s.UpdateAccount(context.Background(), acc); err != nil {
		t.Fatal(err)
	}
	stored, err := s.GetAccount(t.Context(), acc.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.QoderRefreshToken != "refresh-2" || stored.QoderAccessToken != "access-2" || stored.QoderUserID != "new-uid" {
		t.Fatalf("verification re-saved stale credential: %+v", stored)
	}
}

func TestVerifyQoderQuotaFailurePreservesKnownExhaustion(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/userinfo" {
			_, _ = w.Write([]byte(`{"uid":"uid"}`))
			return
		}
		http.NotFound(w, r)
	}))
	defer server.Close()
	for _, tc := range []struct {
		name   string
		quota  bool
		status string
	}{
		{"quota-snapshot", true, ""}, {"status-code", false, store.AccountStatusQoderQuotaExhausted},
	} {
		t.Run(tc.name, func(t *testing.T) {
			acc := &store.Account{AccountType: "qoder", QoderAccessToken: "access", QoderRefreshToken: "refresh", QoderMachineID: "machine", QoderUserID: "uid", StatusCode: tc.status, QoderQuota: store.QoderQuotaSnapshot{Exhausted: tc.quota}}
			status, code, err := verifyQoderAccountWithStore(t.Context(), acc, qoderLoginConfig(server.URL), nil)
			if err != nil || code != 0 || !strings.EqualFold(status, store.AccountStatusQoderQuotaExhausted) {
				t.Fatalf("quota outage = %q %d %v; want exhausted", status, code, err)
			}
		})
	}
}
