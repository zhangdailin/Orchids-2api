package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"

	"orchids-api/internal/api"
	"orchids-api/internal/cline"
	"orchids-api/internal/config"
	"orchids-api/internal/handler"
	"orchids-api/internal/loadbalancer"
	"orchids-api/internal/middleware"
	"orchids-api/internal/provider"
	"orchids-api/internal/store"
	"orchids-api/internal/template"
)

// clineE2EStub serves every endpoint the Cline channel touches, so a request can
// travel the real route table, the real handler and the real client.
type clineE2EStub struct {
	*httptest.Server
	chatHeaders http.Header
	chatBody    []byte
	chatCalls   int
	feedCalls   int
	polls       int
}

func newClineE2EStub(t *testing.T) *clineE2EStub {
	t.Helper()
	stub := &clineE2EStub{}
	stub.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/user_management/authorize/device":
			page := "http://" + r.Host + "/device"
			_, _ = w.Write([]byte(`{"device_code":"dev-e2e","user_code":"ABCD-EFGH",` +
				`"verification_uri":"` + page + `",` +
				`"verification_uri_complete":"` + page + `?code=ABCD-EFGH",` +
				`"interval":1,"expires_in":900}`))
		case "/user_management/authenticate":
			stub.polls++
			if stub.polls < 2 {
				w.WriteHeader(http.StatusBadRequest)
				_, _ = w.Write([]byte(`{"error":"authorization_pending"}`))
				return
			}
			_, _ = w.Write([]byte(`{"access_token":"workos-access","refresh_token":"workos-refresh"}`))
		case "/api/v1/auth/register":
			_, _ = w.Write([]byte(`{"data":{"accessToken":"cline-access","refreshToken":"cline-refresh",` +
				`"expiresAt":4102444800000,"userInfo":{"email":"e2e@example.com"}}}`))
		case "/api/v1/ai/cline/recommended-models":
			stub.feedCalls++
			_, _ = w.Write([]byte(`{"free":[{"id":"x-ai/grok-4.1-fast","name":"Grok 4.1 Fast"}]}`))
		case "/api/v1/chat/completions":
			stub.chatCalls++
			stub.chatHeaders = r.Header.Clone()
			raw, _ := io.ReadAll(r.Body)
			stub.chatBody = raw
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"e2e answer\"},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":5,\"completion_tokens\":2}}\n\n"))
			_, _ = w.Write([]byte("data: [DONE]\n\n"))
		default:
			http.NotFound(w, r)
		}
	}))
	return stub
}

// TestClineChannelEndToEnd drives one chat completion through the registered
// route table with a Cline account the WorkOS login flow created.
//
// This is the integration the unit tests cannot reach: route table, channel
// detection, account selection, client construction, the request credential and
// the SSE conversion all run together. The requests go through a real HTTP
// server so the same lifecycle the deployment uses is exercised.
func TestClineChannelEndToEnd(t *testing.T) {
	mini := miniredis.RunT(t)
	s, err := store.New(store.Options{RedisAddr: mini.Addr(), RedisPrefix: "cline-e2e:"})
	if err != nil {
		t.Fatalf("store.New() error = %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	stub := newClineE2EStub(t)
	defer stub.Close()

	digest := sha256.Sum256([]byte("sk-cline-e2e"))
	if err := s.CreateApiKey(context.Background(), &store.ApiKey{
		Name: "cline-e2e", KeyHash: hex.EncodeToString(digest[:]), KeyPrefix: "sk-", KeySuffix: "-e2e", Enabled: true,
	}); err != nil {
		t.Fatalf("CreateApiKey() error = %v", err)
	}

	const managedKey = "sk-cline-e2e"
	cfg := &config.Config{
		AdminUser:               "admin",
		AdminPass:               "secret",
		AdminToken:              "admintoken",
		AdminPath:               "/admin",
		ClineAPIBaseURL:         stub.URL + "/api/v1",
		ClineWorkOSAuthorizeURL: stub.URL + "/user_management/authorize/device",
		ClineWorkOSTokenURL:     stub.URL + "/user_management/authenticate",
	}

	lb := loadbalancer.NewWithCacheTTL(s, 0)
	h := handler.NewWithLoadBalancer(cfg, lb)
	h.SetClientFactory(func(acc *store.Account, c *config.Config) handler.UpstreamClient {
		factory, ok := provider.Get(acc.AccountType)
		if !ok {
			t.Fatalf("no provider registered for %q", acc.AccountType)
		}
		client, ok := factory(acc, c).(handler.UpstreamClient)
		if !ok {
			t.Fatalf("provider %q returned an unusable client", acc.AccountType)
		}
		if setter, ok := client.(interface {
			SetAccountStore(cline.AccountUpdater)
		}); ok {
			setter.SetAccountStore(s)
		}
		return client
	})
	t.Cleanup(h.Close)

	apiHandler := api.New(s, cfg.AdminUser, cfg.AdminPass, cfg)
	renderer, err := template.NewRenderer()
	if err != nil {
		t.Fatalf("template.NewRenderer() error = %v", err)
	}
	limiter := middleware.NewConcurrencyLimiter(4, 0, false)
	mux := http.NewServeMux()
	registerRoutes(mux, cfg, s, h, nil, apiHandler, limiter, nil, renderer)

	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	httpClient := &http.Client{Timeout: 60 * time.Second}

	do := func(method, path, body string, admin bool) *http.Response {
		t.Helper()
		req, err := http.NewRequest(method, server.URL+path, strings.NewReader(body))
		if err != nil {
			t.Fatalf("build request: %v", err)
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Origin", server.URL)
		if admin {
			req.Header.Set("X-Admin-Token", "admintoken")
		} else {
			req.Header.Set("Authorization", "Bearer "+managedKey)
		}
		resp, err := httpClient.Do(req)
		if err != nil {
			t.Fatalf("%s %s: %v", method, path, err)
		}
		return resp
	}
	readBody := func(resp *http.Response) string {
		t.Helper()
		defer resp.Body.Close()
		raw, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatalf("read body: %v", err)
		}
		return string(raw)
	}

	// 1. Create the account through the WorkOS device authorization flow.
	startBody := readBody(do(http.MethodPost, "/api/cline/login", "", true))
	var started struct {
		ID                      string `json:"id"`
		VerificationURIComplete string `json:"verification_uri_complete"`
	}
	if err := json.Unmarshal([]byte(startBody), &started); err != nil || started.ID == "" {
		t.Fatalf("login start response = %q", startBody)
	}
	if started.VerificationURIComplete == "" {
		t.Fatalf("login start did not return the authorization URL: %q", startBody)
	}

	var final struct {
		Status    string `json:"status"`
		AccountID int64  `json:"account_id"`
	}
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		pollBody := readBody(do(http.MethodGet, "/api/cline/login/"+started.ID, "", true))
		if err := json.Unmarshal([]byte(pollBody), &final); err != nil {
			t.Fatalf("decode poll: %v (%s)", err, pollBody)
		}
		if final.Status == "complete" || final.Status == "failed" {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if final.Status != "complete" {
		t.Fatalf("login status = %q, want complete", final.Status)
	}

	// 2. Refresh the channel catalog from the account.
	refreshBody := readBody(do(http.MethodPost, "/api/models/refresh?channel=cline", "", true))
	var refreshed modelRefreshResult
	if err := json.Unmarshal([]byte(refreshBody), &refreshed); err != nil {
		t.Fatalf("decode refresh: %v (%s)", err, refreshBody)
	}
	if refreshed.Channel != "Cline" || refreshed.Discovered == 0 {
		t.Fatalf("refresh result = %+v, want a discovered Cline catalog", refreshed)
	}
	// The catalog must have come from the upstream feed, never a compiled-in
	// list.
	if refreshed.Source != "cline_recommended_models" {
		t.Fatalf("refresh source = %q, want cline_recommended_models", refreshed.Source)
	}
	if stub.feedCalls == 0 {
		t.Fatal("the refresh did not read the upstream model feed")
	}

	// 3. Run one chat completion through the channel route.
	chatBody := readBody(do(http.MethodPost, "/cline/v1/chat/completions",
		`{"model":"x-ai/grok-4.1-fast","stream":true,"messages":[{"role":"user","content":"hello"}]}`, false))
	if !strings.Contains(chatBody, "e2e answer") {
		t.Fatalf("chat body did not carry the upstream text: %s", chatBody)
	}
	if stub.chatCalls != 1 {
		t.Fatalf("upstream chat calls = %d, want 1", stub.chatCalls)
	}

	// The request credential is the Cline pair with the literal workos: prefix,
	// and the task id doubles as the session id.
	if auth := stub.chatHeaders.Get("Authorization"); auth != "Bearer workos:cline-access" {
		t.Errorf("Authorization = %q, want Bearer workos:cline-access", auth)
	}
	if got := stub.chatHeaders.Get("X-Task-ID"); got == "" {
		t.Errorf("the upstream request is missing X-Task-ID")
	}
	// The WorkOS tokens are the exchanged halves, never the request credential.
	if strings.Contains(fmtHeaders(stub.chatHeaders), "workos-access") {
		t.Fatal("a WorkOS token leaked into an upstream header")
	}
	if !strings.Contains(string(stub.chatBody), `"session_id"`) {
		t.Fatalf("the upstream body is missing session_id: %s", stub.chatBody)
	}
	if !strings.Contains(string(stub.chatBody), `"reasoning_effort":"high"`) {
		t.Fatalf("the upstream body is missing reasoning_effort: %s", stub.chatBody)
	}
}
