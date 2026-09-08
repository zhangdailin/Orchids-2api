package grok

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"

	"orchids-api/internal/config"
	"orchids-api/internal/store"
)

func TestCLIOAuthAccessTokenUnexpired(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("token endpoint must not be called when access token is unexpired")
	}))
	defer server.Close()

	cfg := &config.Config{}
	cfg.GrokCLIOAuthTokenURL = server.URL
	oauth := NewCLIOAuth(cfg, server.Client())
	acc := &store.Account{
		OAuthAccessToken:  "existing-token",
		OAuthRefreshToken: "refresh",
		OAuthExpiresAt:    time.Now().Add(time.Hour),
	}
	token, err := oauth.AccessToken(context.Background(), acc)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if token != "existing-token" {
		t.Fatalf("token = %q", token)
	}
}

func TestCLIOAuthAccessTokenRefreshes(t *testing.T) {
	var called int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called++
		if r.Method != http.MethodPost {
			t.Fatalf("method = %s", r.Method)
		}
		if err := r.ParseForm(); err != nil {
			t.Fatalf("parse form: %v", err)
		}
		if r.Form.Get("grant_type") != "refresh_token" {
			t.Fatalf("grant_type = %q", r.Form.Get("grant_type"))
		}
		if r.Form.Get("refresh_token") != "old-refresh" {
			t.Fatalf("refresh_token = %q", r.Form.Get("refresh_token"))
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_token":"new-access","refresh_token":"new-refresh","expires_in":3600}`))
	}))
	defer server.Close()

	cfg := &config.Config{}
	cfg.GrokCLIOAuthTokenURL = server.URL
	oauth := NewCLIOAuth(cfg, server.Client())
	acc := &store.Account{
		OAuthAccessToken:  "stale-token",
		OAuthRefreshToken: "old-refresh",
		OAuthExpiresAt:    time.Now().Add(-time.Hour), // expired → forces refresh
	}
	token, err := oauth.AccessToken(context.Background(), acc)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if called != 1 {
		t.Fatalf("expected exactly 1 refresh call, got %d", called)
	}
	if token != "new-access" {
		t.Fatalf("token = %q", token)
	}
	if acc.OAuthAccessToken != "new-access" {
		t.Fatalf("persisted access = %q", acc.OAuthAccessToken)
	}
	if acc.OAuthRefreshToken != "new-refresh" {
		t.Fatalf("persisted refresh (rotated) = %q", acc.OAuthRefreshToken)
	}
}

func TestCLIResponsesRefreshesRejectedUnexpiredTokenOnSameAccount(t *testing.T) {
	var responseCalls, refreshCalls int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/oauth/token":
			refreshCalls++
			_, _ = w.Write([]byte(`{"access_token":"new-access","refresh_token":"new-refresh","expires_in":3600}`))
		case "/v1/responses":
			responseCalls++
			if r.Header.Get("Authorization") == "Bearer old-access" {
				w.WriteHeader(http.StatusUnauthorized)
				_, _ = w.Write([]byte(`{"error":{"message":"expired"}}`))
				return
			}
			if r.Header.Get("Authorization") != "Bearer new-access" {
				t.Fatalf("authorization=%q", r.Header.Get("Authorization"))
			}
			_, _ = w.Write([]byte(`{"id":"resp_refreshed","object":"response"}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	cfg := &config.Config{GrokCLIBaseURL: server.URL + "/v1", GrokCLIOAuthTokenURL: server.URL + "/oauth/token"}
	client := NewCLIClient(cfg)
	client.httpClient = server.Client()
	client.oauth.httpClient = server.Client()
	acc := &store.Account{
		OAuthAccessToken: "old-access", OAuthRefreshToken: "old-refresh", OAuthExpiresAt: time.Now().Add(time.Hour),
	}
	resp, err := client.doResponses(context.Background(), acc, map[string]interface{}{"model": "grok-4.6", "input": "hello"})
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if responseCalls != 2 || refreshCalls != 1 || acc.OAuthAccessToken != "new-access" {
		t.Fatalf("response_calls=%d refresh_calls=%d access=%q", responseCalls, refreshCalls, acc.OAuthAccessToken)
	}
}

func TestCLIOAuthAccessTokenCoalescesConcurrentRefreshes(t *testing.T) {
	var calls int
	var callsMu sync.Mutex
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callsMu.Lock()
		calls++
		callsMu.Unlock()
		time.Sleep(20 * time.Millisecond)
		_, _ = w.Write([]byte(`{"access_token":"shared-access","refresh_token":"shared-refresh","expires_in":3600}`))
	}))
	defer server.Close()

	cfg := &config.Config{GrokCLIOAuthTokenURL: server.URL}
	oauth := NewCLIOAuth(cfg, server.Client())
	acc := &store.Account{OAuthRefreshToken: "shared-refresh", OAuthExpiresAt: time.Now().Add(-time.Minute)}
	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			token, err := oauth.AccessToken(context.Background(), acc)
			if err != nil {
				errs <- err
				return
			}
			if token != "shared-access" {
				errs <- fmt.Errorf("token=%q", token)
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	callsMu.Lock()
	defer callsMu.Unlock()
	if calls != 1 {
		t.Fatalf("refresh calls=%d want 1", calls)
	}
}

func TestCLIOAuthRefreshDenied(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":"invalid_grant","error_description":"token expired"}`))
	}))
	defer server.Close()

	cfg := &config.Config{}
	cfg.GrokCLIOAuthTokenURL = server.URL
	oauth := NewCLIOAuth(cfg, server.Client())
	acc := &store.Account{
		OAuthAccessToken:  "stale",
		OAuthRefreshToken: "dead-refresh",
		OAuthExpiresAt:    time.Now().Add(-time.Minute),
	}
	_, err := oauth.AccessToken(context.Background(), acc)
	if err == nil {
		t.Fatal("expected error for denied refresh")
	}
	if !IsCLIPermanentOAuthError(err) {
		t.Fatalf("invalid_grant should be permanent (401): %v", err)
	}
	if strings.Contains(err.Error(), "token expired") {
		t.Fatalf("OAuth diagnostic must not expose upstream text: %v", err)
	}
}

func TestCLIOAuthMissingRefreshToken(t *testing.T) {
	cfg := &config.Config{}
	oauth := NewCLIOAuth(cfg, nil)
	acc := &store.Account{OAuthAccessToken: "", OAuthRefreshToken: "", OAuthExpiresAt: time.Time{}}
	if _, err := oauth.AccessToken(context.Background(), acc); err == nil {
		t.Fatal("expected error when no token present")
	}
}

func TestCLIOAuthRefreshServerErrorTransient(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()

	cfg := &config.Config{}
	cfg.GrokCLIOAuthTokenURL = server.URL
	oauth := NewCLIOAuth(cfg, server.Client())
	acc := &store.Account{OAuthRefreshToken: "r", OAuthExpiresAt: time.Now().Add(-time.Minute)}
	_, err := oauth.AccessToken(context.Background(), acc)
	if err == nil {
		t.Fatal("expected error")
	}
	if IsCLIPermanentOAuthError(err) {
		t.Fatalf("5xx should be transient, got permanent: %v", err)
	}
}

func TestCLIOAuthErrorStatus(t *testing.T) {
	e := &cliOAuthError{status: http.StatusUnauthorized, message: "denied"}
	if e.Status() != "401" {
		t.Fatalf("status = %q", e.Status())
	}
	zero := &cliOAuthError{}
	if zero.Status() != "" {
		t.Fatalf("zero status = %q", zero.Status())
	}
	if !strings.Contains(e.Error(), "denied") {
		t.Fatalf("error message = %q", e.Error())
	}
}

func TestCLIOAuthAccessTokenPersistsToStore(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_token":"stored-access","refresh_token":"stored-refresh","expires_in":3600}`))
	}))
	defer server.Close()

	mini := miniredis.RunT(t)
	s, err := store.New(store.Options{StoreMode: "redis", RedisAddr: mini.Addr(), RedisDB: 0, RedisPrefix: "test:"})
	if err != nil {
		t.Fatalf("store.New() error = %v", err)
	}
	t.Cleanup(func() {
		_ = s.Close()
		mini.Close()
	})

	acc := &store.Account{
		AccountType:       "grok",
		CredentialType:    "oauth",
		OAuthAccessToken:  "stale",
		OAuthRefreshToken: "old-refresh",
		OAuthExpiresAt:    time.Now().Add(-time.Minute),
		Enabled:           true,
	}
	if err := s.CreateAccount(context.Background(), acc); err != nil {
		t.Fatalf("CreateAccount() error = %v", err)
	}

	cfg := &config.Config{}
	cfg.GrokCLIOAuthTokenURL = server.URL
	oauth := NewCLIOAuth(cfg, server.Client())
	oauth.SetAccountStore(s)

	token, err := oauth.AccessToken(context.Background(), acc)
	if err != nil {
		t.Fatalf("AccessToken() error = %v", err)
	}
	if token != "stored-access" {
		t.Fatalf("token=%q", token)
	}

	got, err := s.GetAccount(context.Background(), acc.ID)
	if err != nil {
		t.Fatalf("GetAccount() error = %v", err)
	}
	if got.OAuthAccessToken != "stored-access" {
		t.Fatalf("stored access=%q", got.OAuthAccessToken)
	}
	if got.OAuthRefreshToken != "stored-refresh" {
		t.Fatalf("stored refresh=%q", got.OAuthRefreshToken)
	}
}

func TestCLIClientFetchModelsReadsOfficialControlPlaneCatalog(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/models" {
			t.Fatalf("request=%s %s want GET /models", r.Method, r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer active-access" {
			t.Fatalf("authorization=%q", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[{"id":"grok-4.6"},{"id":"grok-4.6"},{"modelId":"grok-4.5"},{"id":"hidden-model","hidden":true},{"id":""}]}`))
	}))
	defer server.Close()

	client := NewCLIClient(&config.Config{GrokCLIBaseURL: server.URL})
	models, err := client.FetchModels(context.Background(), &store.Account{
		AccountType:      "grok",
		CredentialType:   "oauth",
		OAuthAccessToken: "active-access",
		OAuthExpiresAt:   time.Now().Add(time.Hour),
	})
	if err != nil {
		t.Fatalf("FetchModels() error = %v", err)
	}
	if strings.Join(models, ",") != "grok-4.6,grok-4.5" {
		t.Fatalf("models=%v want grok-4.6,grok-4.5", models)
	}
}

func TestCLIResponsesWaitsForTeamModelCooldownBeforeUpstream(t *testing.T) {
	previous := teamCooldown
	teamCooldown = newTeamCooldownRegistry()
	defer func() { teamCooldown = previous }()
	teamCooldown.Note(RateLimitScopeRPM, ProviderBuild+":team:team-1", "grok-4.6", time.Minute)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	client := &CLIClient{}
	_, err := client.doResponses(ctx, &store.Account{TeamID: "team-1"}, map[string]interface{}{"model": "grok-4.6"})
	if err == nil || !strings.Contains(err.Error(), "context deadline exceeded") {
		t.Fatalf("error=%v want cancellable team cooldown", err)
	}
}
