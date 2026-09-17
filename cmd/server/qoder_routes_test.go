package main

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"

	"orchids-api/internal/api"
	"orchids-api/internal/config"
	"orchids-api/internal/handler"
	"orchids-api/internal/loadbalancer"
	"orchids-api/internal/middleware"
	"orchids-api/internal/provider"
	"orchids-api/internal/qoder"
	"orchids-api/internal/store"
	"orchids-api/internal/template"
)

func decodeQoderBodyForTest(encoded []byte) ([]byte, error) {
	if strings.ContainsAny(string(encoded), "\r\n") {
		return nil, fmt.Errorf("encoded body contains a line break")
	}
	const privateAlphabet = "_doRTgHZBKcGVjlvpC,@aFSx#DPuNJme&i*MzLOEn)sUrthbf%Y^w.(kIQyXqWA!"
	const standardAlphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/"
	q := len(encoded) / 3
	swapped := make([]byte, 0, len(encoded))
	swapped = append(swapped, encoded[len(encoded)-q:]...)
	swapped = append(swapped, encoded[q:len(encoded)-q]...)
	swapped = append(swapped, encoded[:q]...)
	for i, b := range swapped {
		switch b {
		case '$':
			swapped[i] = '='
		default:
			idx := strings.IndexByte(privateAlphabet, b)
			if idx < 0 {
				return nil, fmt.Errorf("invalid private alphabet byte %q", b)
			}
			swapped[i] = standardAlphabet[idx]
		}
	}
	decoded := make([]byte, base64.StdEncoding.DecodedLen(len(swapped)))
	n, err := base64.StdEncoding.Decode(decoded, swapped)
	if err != nil {
		return nil, err
	}
	return decoded[:n], nil
}

// qoderE2EStub serves every endpoint the Qoder channel touches, so a request can
// travel the real route table, the real handler and the real client.
type qoderE2EStub struct {
	*httptest.Server
	chatHeaders    http.Header
	chatBody       []byte
	chatCalls      int
	modelListCalls int
	polls          int
}

func newQoderE2EStub(t *testing.T) *qoderE2EStub {
	t.Helper()
	stub := &qoderE2EStub{}
	stub.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/":
			w.WriteHeader(http.StatusOK)
		case "/api/v1/deviceToken/poll":
			stub.polls++
			if stub.polls < 2 {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			_, _ = w.Write([]byte(`{"token":"access-1","refresh_token":"refresh-1","expires_in":86400,"user_id":"uid-e2e","user_name":"e2e"}`))
		case "/api/v1/userinfo":
			_, _ = w.Write([]byte(`{"uid":"uid-e2e","name":"e2e","email":"e2e@example.com"}`))
		case "/algo/api/v2/model/list":
			// The catalog is now read from the signed control plane, so the
			// refresh depends on this route answering with the account's models.
			if r.Header.Get("Cosy-Key") == "" || r.Header.Get("Cosy-MachineId") == "" {
				t.Errorf("model list request is missing the derived auth chain: %v", r.Header)
			}
			if auth := r.Header.Get("Authorization"); !strings.HasPrefix(auth, "Bearer COSY.") {
				t.Errorf("model list Authorization = %q, want a COSY bearer", auth)
			}
			stub.modelListCalls++
			_, _ = w.Write([]byte(`{"code":0,"data":{"models":[` +
				`{"key":"qmodel_latest","name":"Qwen3.7-Max","display_name":"Qwen3.7-Max","format":"openai","source":"system","enable":true,"is_reasoning":false,"max_input_tokens":1000000},` +
				`{"key":"dmodel","name":"DeepSeek-V4-Pro","display_name":"DeepSeek-V4-Pro","format":"openai","source":"system","enable":true,"is_reasoning":true,"max_input_tokens":1000000}` +
				`]}}`))
		case "/algo/api/v2/service/pro/sse/agent_chat_generation":
			stub.chatCalls++
			stub.chatHeaders = r.Header.Clone()
			body := make([]byte, 1<<20)
			n, _ := r.Body.Read(body)
			stub.chatBody = body[:n]
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = w.Write([]byte(sseEnvelope(`{"id":"1","choices":[{"index":0,"delta":{"content":"e2e answer"},"finish_reason":"stop"}],"usage":{"prompt_tokens":5,"completion_tokens":2}}`)))
			_, _ = w.Write([]byte("event:finish\ndata: {}\n\n"))
		default:
			http.NotFound(w, r)
		}
	}))
	return stub
}

func sseEnvelope(inner string) string {
	raw, _ := json.Marshal(map[string]any{"statusCodeValue": 200, "body": inner})
	return "data: " + string(raw) + "\n\n"
}

// TestQoderChannelEndToEnd drives one chat completion through the registered
// route table with a Qoder account that the login flow created.
//
// This is the integration the unit tests cannot reach: route table, channel
// detection, account selection, client construction, the derived authentication
// chain and the SSE conversion all run together. The requests go through a real
// HTTP server rather than a bare recorder, so the same request lifecycle the
// deployment uses is exercised (and the -race detector observes it).
func TestQoderChannelEndToEnd(t *testing.T) {
	mini := miniredis.RunT(t)
	s, err := store.New(store.Options{RedisAddr: mini.Addr(), RedisPrefix: "qoder-e2e:"})
	if err != nil {
		t.Fatalf("store.New() error = %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	stub := newQoderE2EStub(t)
	defer stub.Close()

	disabled := false
	cfg := &config.Config{
		AdminUser:           "admin",
		AdminPass:           "secret",
		AdminToken:          "admintoken",
		AdminPath:           "/admin",
		InferenceAuth:       &disabled,
		QoderOAuthBaseURL:   stub.URL,
		QoderOpenAPIBaseURL: stub.URL,
		QoderInferenceURL:   stub.URL,
	}

	// Build the client through the same provider table the server uses, so the
	// request path exercises the real seam.
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
			SetAccountStore(qoder.AccountUpdater)
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

	// A real listener is used so request contexts live for the whole handler, as
	// they do in production.
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	httpClient := &http.Client{Timeout: 60 * time.Second}

	do := func(method, path, body string, admin bool) *http.Response {
		t.Helper()
		var reader *strings.Reader
		if body == "" {
			reader = strings.NewReader("")
		} else {
			reader = strings.NewReader(body)
		}
		req, err := http.NewRequest(method, server.URL+path, reader)
		if err != nil {
			t.Fatalf("build request: %v", err)
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Origin", server.URL)
		if admin {
			req.Header.Set("X-Admin-Token", "admintoken")
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

	// 1. Create the account through the device authorization flow.
	startResp := do(http.MethodPost, "/api/qoder/login", "", true)
	startBody := readBody(startResp)
	if startResp.StatusCode != http.StatusOK {
		t.Fatalf("login start status = %d body=%s", startResp.StatusCode, startBody)
	}
	var started struct {
		ID                      string `json:"id"`
		VerificationURIComplete string `json:"verification_uri_complete"`
	}
	if err := json.Unmarshal([]byte(startBody), &started); err != nil || started.ID == "" {
		t.Fatalf("login start response = %q", startBody)
	}
	if !strings.Contains(started.VerificationURIComplete, "/device/selectAccounts?") {
		t.Fatalf("login start did not return the device authorization URL: %q", started.VerificationURIComplete)
	}

	var final struct {
		Status    string `json:"status"`
		AccountID int64  `json:"account_id"`
	}
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		pollResp := do(http.MethodGet, "/api/qoder/login/"+started.ID, "", true)
		pollBody := readBody(pollResp)
		if pollResp.StatusCode != http.StatusOK {
			t.Fatalf("poll status = %d body=%s", pollResp.StatusCode, pollBody)
		}
		if err := json.Unmarshal([]byte(pollBody), &final); err != nil {
			t.Fatalf("decode poll: %v", err)
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
	refreshResp := do(http.MethodPost, "/api/models/refresh?channel=qoder", "", true)
	refreshBody := readBody(refreshResp)
	if refreshResp.StatusCode != http.StatusOK {
		t.Fatalf("model refresh status = %d body=%s", refreshResp.StatusCode, refreshBody)
	}
	var refreshed modelRefreshResult
	if err := json.Unmarshal([]byte(refreshBody), &refreshed); err != nil {
		t.Fatalf("decode refresh: %v", err)
	}
	if refreshed.Channel != "Qoder" || refreshed.Discovered == 0 {
		t.Fatalf("refresh result = %+v, want a discovered Qoder catalog", refreshed)
	}
	// The catalog must have come from the signed upstream read, not from a
	// compiled-in list.
	if refreshed.Source != "qoder_upstream_models" {
		t.Fatalf("refresh source = %q, want qoder_upstream_models", refreshed.Source)
	}
	if stub.modelListCalls == 0 {
		t.Fatal("the refresh did not read the upstream model list")
	}
	if refreshed.Verified != refreshed.Discovered {
		t.Fatalf("verified=%d discovered=%d, want every observed row counted as verified", refreshed.Verified, refreshed.Discovered)
	}

	// 3. Run one chat completion through the channel route.
	chatResp := do(http.MethodPost, "/qoder/v1/chat/completions",
		`{"model":"Qwen3.7-Max","stream":true,"messages":[{"role":"user","content":"hello"}]}`, false)
	chatBody := readBody(chatResp)
	if chatResp.StatusCode != http.StatusOK {
		t.Fatalf("chat status = %d body=%s", chatResp.StatusCode, chatBody)
	}
	if !strings.Contains(chatBody, "e2e answer") {
		t.Fatalf("chat body did not carry the upstream text: %s", chatBody)
	}
	if stub.chatCalls != 1 {
		t.Fatalf("upstream chat calls = %d, want 1", stub.chatCalls)
	}

	// The upstream request must carry the derived authentication chain, not the
	// bare device token.
	for _, header := range []string{"Authorization", "Cosy-Key", "Cosy-MachineId", "Cosy-MachineToken", "Cosy-User", "Cosy-Date", "Cosy-Scene", "Cosy-Data-Policy", "Login-Version", "X-Model-Key", "X-Model-Source"} {
		if stub.chatHeaders.Get(header) == "" {
			t.Errorf("upstream chat request is missing %s", header)
		}
	}
	if auth := stub.chatHeaders.Get("Authorization"); !strings.HasPrefix(auth, "Bearer COSY.") {
		t.Errorf("Authorization = %q, want a COSY bearer", auth)
	}
	if got := stub.chatHeaders.Get("X-Model-Key"); got != "qmodel_latest" {
		t.Errorf("X-Model-Key = %q, want the resolved internal key", got)
	}
	if got := stub.chatHeaders.Get("Cosy-User"); got != "uid-e2e" {
		t.Errorf("Cosy-User = %q, want the signed-in user", got)
	}

	// The device token must never appear in a header: the runtime fields are the
	// request credential.
	if strings.Contains(fmtHeaders(stub.chatHeaders), "access-1") {
		t.Fatal("the device access token leaked into an upstream header")
	}

	// The body is in the private encoding and carries the chat contract.
	if len(stub.chatBody) == 0 {
		t.Fatal("the upstream request carried no body")
	}
	decoded, err := decodeQoderBodyForTest(stub.chatBody)
	if err != nil {
		t.Fatalf("DecodeBody() error = %v", err)
	}
	for _, want := range []string{`"chat_task":"FREE_INPUT"`, `"session_type":"qodercli"`, `"agent_id":"agent_common"`, `"stream":true`} {
		if !strings.Contains(string(decoded), want) {
			t.Errorf("decoded body = %s, want it to contain %s", decoded, want)
		}
	}

	// 4. The account row must not expose either secret.
	redactResp := do(http.MethodGet, "/api/accounts", "", true)
	redactBody := readBody(redactResp)
	if redactResp.StatusCode != http.StatusOK {
		t.Fatalf("accounts status = %d body=%s", redactResp.StatusCode, redactBody)
	}
	if strings.Contains(redactBody, "refresh-1") {
		t.Fatal("the durable refresh token was returned by the account API")
	}
	if strings.Contains(redactBody, "runtime-key") {
		t.Fatal("the derived runtime key was returned by the account API")
	}
}

func fmtHeaders(headers http.Header) string {
	var builder strings.Builder
	for name, values := range headers {
		builder.WriteString(name)
		builder.WriteString(":")
		builder.WriteString(strings.Join(values, " "))
		builder.WriteString("\n")
	}
	return builder.String()
}
