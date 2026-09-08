package api

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"orchids-api/internal/config"
	"orchids-api/internal/middleware"
	"orchids-api/internal/puter"
	"orchids-api/internal/store"
)

func puterLoginRequest(body string) *http.Request {
	r := httptest.NewRequest(http.MethodPost, "https://admin.example/api/puter/web-login", strings.NewReader(body))
	r.Header.Set("Origin", "https://admin.example")
	r.Header.Set("Content-Type", "application/json")
	return r
}

func TestPuterWebLoginPersistsVerifiedGrant(t *testing.T) {
	a, s, cleanup := newTestAPI(t)
	defer cleanup()
	oldUser, oldUsage := puterFetchUser, puterFetchMonthlyUsage
	t.Cleanup(func() { puterFetchUser, puterFetchMonthlyUsage = oldUser, oldUsage })
	puterFetchUser = func(_ context.Context, acc *store.Account, _ *config.Config) (*puter.User, error) {
		if puter.ResolveAuthToken(acc) != "popup-secret" {
			t.Fatal("grant not passed to Puter verifier")
		}
		return &puter.User{UUID: "user-id", Username: "official-user"}, nil
	}
	puterFetchMonthlyUsage = func(context.Context, *store.Account, *config.Config) (*puter.MonthlyUsage, error) {
		return &puter.MonthlyUsage{AllowanceInfo: puter.UsageAllowanceInfo{Remaining: 20, MonthUsageAllowance: 100}}, nil
	}
	for _, want := range []int{http.StatusCreated, http.StatusOK} {
		rec := httptest.NewRecorder()
		a.HandlePuterWebLogin(rec, puterLoginRequest(`{"token":"popup-secret","enabled":true}`))
		if rec.Code != want || strings.Contains(rec.Body.String(), "popup-secret") || rec.Header().Get("Cache-Control") != "no-store" {
			t.Fatalf("unexpected login response: status=%d body=%s", rec.Code, rec.Body.String())
		}
	}
	accounts, err := s.ListAccounts(context.Background())
	if err != nil || len(accounts) != 1 {
		t.Fatalf("expected one deduplicated account: %v", err)
	}
	acc := accounts[0]
	if acc.UserID != "user-id" || acc.Name != "official-user" || acc.UsageCurrent != 20 || !acc.Enabled || acc.ClientCookie != "popup-secret" {
		t.Fatal("verified identity, quota and credential must be saved together")
	}
}

func TestPuterWebLoginDoesNotSaveFailedVerification(t *testing.T) {
	a, s, cleanup := newTestAPI(t)
	defer cleanup()
	oldUser, oldUsage := puterFetchUser, puterFetchMonthlyUsage
	t.Cleanup(func() { puterFetchUser, puterFetchMonthlyUsage = oldUser, oldUsage })
	for _, stage := range []string{"identity", "usage"} {
		puterFetchUser = func(context.Context, *store.Account, *config.Config) (*puter.User, error) {
			if stage == "identity" {
				return nil, errors.New("upstream echoed popup-secret")
			}
			return &puter.User{UUID: "id", Username: "user"}, nil
		}
		puterFetchMonthlyUsage = func(context.Context, *store.Account, *config.Config) (*puter.MonthlyUsage, error) {
			return nil, errors.New("upstream echoed popup-secret")
		}
		rec := httptest.NewRecorder()
		a.HandlePuterWebLogin(rec, puterLoginRequest(`{"token":"popup-secret","enabled":true}`))
		if rec.Code != http.StatusBadGateway || strings.Contains(rec.Body.String(), "popup-secret") {
			t.Fatalf("unsafe failure: %s", rec.Body.String())
		}
	}
	accounts, err := s.ListAccounts(context.Background())
	if err != nil || len(accounts) != 0 {
		t.Fatal("failed login must not persist an account")
	}
}

func TestPuterWebLoginRejectsUnsafeRequests(t *testing.T) {
	a, _, cleanup := newTestAPI(t)
	defer cleanup()
	cases := []struct {
		name, body, origin, contentType, site, method string
		want                                          int
	}{
		{"foreign", `{"token":"x"}`, "https://evil.example", "application/json", "", "POST", 403},
		{"missing origin", `{"token":"x"}`, "", "application/json", "", "POST", 403},
		{"insecure remote", `{"token":"x"}`, "http://admin.example", "application/json", "", "POST", 403},
		{"cross-site", `{"token":"x"}`, "https://admin.example", "application/json", "cross-site", "POST", 403},
		{"form", `token=x`, "https://admin.example", "application/x-www-form-urlencoded", "", "POST", 415},
		{"missing token", `{}`, "https://admin.example", "application/json", "", "POST", 400},
		{"forged identity", `{"token":"x","username":"forged"}`, "https://admin.example", "application/json", "", "POST", 400},
		{"trailing data", `{"token":"x"}{}`, "https://admin.example", "application/json", "", "POST", 400},
		{"oversized", `{"token":"` + strings.Repeat("x", 33<<10) + `"}`, "https://admin.example", "application/json", "", "POST", 400},
		{"method", ``, "https://admin.example", "application/json", "", "GET", 405},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := puterLoginRequest(tc.body)
			r.Method = tc.method
			r.Header.Set("Origin", tc.origin)
			r.Header.Set("Content-Type", tc.contentType)
			r.Header.Set("Sec-Fetch-Site", tc.site)
			rec := httptest.NewRecorder()
			a.HandlePuterWebLogin(rec, r)
			if rec.Code != tc.want {
				t.Fatalf("status=%d want=%d", rec.Code, tc.want)
			}
		})
	}
	rec := httptest.NewRecorder()
	middleware.SessionAuthDynamic(func() (string, string) { return "test-admin", "" }, a.HandlePuterWebLogin)(rec, puterLoginRequest(`{"token":"x"}`))
	if rec.Code != http.StatusUnauthorized {
		t.Fatal("anonymous login submission was accepted")
	}
}
