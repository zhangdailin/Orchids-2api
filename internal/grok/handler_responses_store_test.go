package grok

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/goccy/go-json"

	"orchids-api/internal/config"
	"orchids-api/internal/middleware"
	"orchids-api/internal/store"
)

func TestBuildStoredResponseLifecycleAndOwnerIsolation(t *testing.T) {
	var calls []string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls = append(calls, r.Method+" "+r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/v1/responses":
			_, _ = io.WriteString(w, `{"id":"resp_owned","object":"response","status":"completed","model":"grok-4.6","output":[]}`)
		case r.Method == http.MethodGet && r.URL.Path == "/v1/responses/resp_owned":
			_, _ = io.WriteString(w, `{"id":"resp_owned","object":"response","status":"completed","output":[]}`)
		case r.Method == http.MethodDelete && r.URL.Path == "/v1/responses/resp_owned":
			_, _ = io.WriteString(w, `{"id":"resp_owned","object":"response.deleted","deleted":true}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer upstream.Close()

	h, s, mini := setupValidationHandler(t)
	defer func() { _ = s.Close(); mini.Close() }()
	configureStoredResponseTestHandler(t, h, s, upstream)

	call := func(token, method, path, body string, handler http.HandlerFunc) *httptest.ResponseRecorder {
		wrapped := middleware.APIKeyAuth(func() bool { return true }, func(context.Context, string) (*middleware.APIKeyPrincipal, error) {
			return &middleware.APIKeyPrincipal{}, nil
		}, handler)
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		wrapped(rec, req)
		return rec
	}

	created := call("owner-a", http.MethodPost, "/v1/responses", `{"model":"grok-4.6","input":"hello","stream":false}`, h.HandleResponses)
	if created.Code != http.StatusOK || !strings.Contains(created.Body.String(), "resp_owned") {
		t.Fatalf("create status=%d body=%s", created.Code, created.Body.String())
	}
	deniedContinuation := call("owner-b", http.MethodPost, "/v1/responses", `{"model":"grok-4.6","input":"continue","stream":false,"previous_response_id":"resp_owned"}`, h.HandleResponses)
	if deniedContinuation.Code != http.StatusNotFound {
		t.Fatalf("cross-owner continuation status=%d body=%s", deniedContinuation.Code, deniedContinuation.Body.String())
	}
	denied := call("owner-b", http.MethodGet, "/v1/responses/resp_owned", "", h.HandleResponseResource)
	if denied.Code != http.StatusNotFound {
		t.Fatalf("cross-owner get status=%d body=%s", denied.Code, denied.Body.String())
	}
	got := call("owner-a", http.MethodGet, "/v1/responses/resp_owned", "", h.HandleResponseResource)
	if got.Code != http.StatusOK || !strings.Contains(got.Body.String(), "resp_owned") {
		t.Fatalf("get status=%d body=%s", got.Code, got.Body.String())
	}
	deleted := call("owner-a", http.MethodDelete, "/v1/responses/resp_owned", "", h.HandleResponseResource)
	if deleted.Code != http.StatusOK || !strings.Contains(deleted.Body.String(), `"deleted":true`) {
		t.Fatalf("delete status=%d body=%s", deleted.Code, deleted.Body.String())
	}
	missing := call("owner-a", http.MethodGet, "/v1/responses/resp_owned", "", h.HandleResponseResource)
	if missing.Code != http.StatusNotFound {
		t.Fatalf("get after delete status=%d body=%s", missing.Code, missing.Body.String())
	}
	if len(calls) != 3 {
		t.Fatalf("upstream calls=%v, want POST+GET+DELETE only", calls)
	}
}

func TestCompatibilityStoredResponseResourceAndHistoryExpansion(t *testing.T) {
	h, s, mini := setupValidationHandler(t)
	defer func() { _ = s.Close(); mini.Close() }()
	body := []byte(`{"id":"resp_web","object":"response","status":"completed","model":"grok-chat-fast","output":[{"type":"reasoning","encrypted_content":"cipher","summary":[]},{"type":"message","role":"assistant","content":[{"type":"output_text","text":"answer"}]}]}`)
	if err := s.SaveStoredResponse(context.Background(), &store.StoredResponse{
		ResponseID: "resp_web", OwnerHash: "anonymous", Model: "grok-chat-fast", Provider: ProviderWeb,
		PromptCacheKey: "cache-key", ContentType: "application/json", Body: body,
	}, time.Hour); err != nil {
		t.Fatal(err)
	}

	wrapper := middleware.APIKeyAuth(func() bool { return false }, nil, h.HandleResponseResource)
	get := httptest.NewRecorder()
	wrapper(get, httptest.NewRequest(http.MethodGet, "/v1/responses/resp_web", nil))
	if get.Code != http.StatusOK || get.Body.String() != string(body) {
		t.Fatalf("GET status=%d body=%s", get.Code, get.Body.String())
	}
	expanded, err := expandStoredResponseInput(body, "continue")
	if err != nil {
		t.Fatal(err)
	}
	items := expanded.([]interface{})
	if len(items) != 3 || items[2].(map[string]interface{})["role"] != "user" {
		t.Fatalf("expanded input=%#v", items)
	}

	deleted := httptest.NewRecorder()
	wrapper(deleted, httptest.NewRequest(http.MethodDelete, "/v1/responses/resp_web", nil))
	if deleted.Code != http.StatusOK || !strings.Contains(deleted.Body.String(), `"deleted":true`) {
		t.Fatalf("DELETE status=%d body=%s", deleted.Code, deleted.Body.String())
	}
}

func TestBuildPreviousResponsePinsCreatingAccount(t *testing.T) {
	var authHeaders []string
	responseNumber := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		authHeaders = append(authHeaders, r.Header.Get("Authorization"))
		responseNumber++
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, fmt.Sprintf(`{"id":"resp_%d","object":"response","status":"completed","output":[]}`, responseNumber))
	}))
	defer upstream.Close()

	h, s, mini := setupValidationHandler(t)
	defer func() { _ = s.Close(); mini.Close() }()
	configureStoredResponseTestHandler(t, h, s, upstream)
	if err := s.CreateAccount(context.Background(), buildTestAccount(t, "user-2", "team-2")); err != nil {
		t.Fatal(err)
	}

	wrapped := middleware.APIKeyAuth(func() bool { return true }, func(context.Context, string) (*middleware.APIKeyPrincipal, error) {
		return &middleware.APIKeyPrincipal{}, nil
	}, h.HandleResponses)
	request := func(body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer owner")
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		wrapped(rec, req)
		return rec
	}
	first := request(`{"model":"grok-4.6","input":"first","stream":false}`)
	if first.Code != http.StatusOK {
		t.Fatalf("first status=%d body=%s", first.Code, first.Body.String())
	}
	var firstBody map[string]interface{}
	_ = json.Unmarshal(first.Body.Bytes(), &firstBody)
	second := request(`{"model":"grok-4.6","input":"second","stream":false,"previous_response_id":"` + parseLooseStringAny(firstBody["id"]) + `"}`)
	if second.Code != http.StatusOK {
		t.Fatalf("second status=%d body=%s", second.Code, second.Body.String())
	}
	if len(authHeaders) != 2 || authHeaders[0] == "" || authHeaders[0] != authHeaders[1] {
		t.Fatalf("requests were not pinned to the creating account: %v", authHeaders)
	}
}

func TestResponsesCompactForcesNonStreamingNativeEndpoint(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/responses/compact" {
			t.Fatalf("path=%s", r.URL.Path)
		}
		var payload map[string]interface{}
		_ = json.NewDecoder(r.Body).Decode(&payload)
		if stream, _ := payload["stream"].(bool); stream {
			t.Fatal("compact request remained streaming")
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"resp_compact","object":"response","output":[{"type":"compaction","encrypted_content":"opaque"}]}`)
	}))
	defer upstream.Close()
	h, s, mini := setupValidationHandler(t)
	defer func() { _ = s.Close(); mini.Close() }()
	configureStoredResponseTestHandler(t, h, s, upstream)
	req := httptest.NewRequest(http.MethodPost, "/v1/responses/compact", strings.NewReader(`{"model":"grok-4.6","input":[{"type":"compaction_trigger"}],"stream":true}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.HandleResponsesCompact(rec, req)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "encrypted_content") {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
}

func configureStoredResponseTestHandler(t *testing.T, h *Handler, s *store.Store, upstream *httptest.Server) {
	t.Helper()
	if err := s.CreateModel(context.Background(), &store.Model{
		Channel: "Grok", ModelID: "grok-4.6", Name: "Grok 4.6", Status: store.ModelStatusAvailable, Verified: true,
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateAccount(context.Background(), buildTestAccount(t, "user-1", "team-1")); err != nil {
		t.Fatal(err)
	}
	h.cfg = &config.Config{GrokCLIBaseURL: upstream.URL + "/v1", ResponseStoreTTL: 24}
	h.cliClient = NewCLIClient(h.cfg)
	h.cliClient.SetAccountStore(s)
	h.cliClient.httpClient = upstream.Client()
	h.cliClient.oauth.httpClient = upstream.Client()
}

func buildTestAccount(t *testing.T, userID, teamID string) *store.Account {
	t.Helper()
	return &store.Account{
		AccountType: "grok", Enabled: true, CredentialType: "oauth", GrokProvider: ProviderBuild,
		OAuthAccessToken: jwtWithClaims(t, `{"sub":"`+userID+`","team_id":"`+teamID+`"}`),
		OAuthExpiresAt:   time.Now().Add(time.Hour), TeamID: teamID,
		GrokModels: []string{"grok-4.6"}, GrokModelsSyncedAt: time.Now(),
	}
}

func TestResponseIDCaptureFromSSE(t *testing.T) {
	input := "event: response.completed\ndata: {\"response\":\ndata: {\"id\":\"resp_stream\"}}\n\n"
	recorder := httptest.NewRecorder()
	if got, _, _ := copyNativeCLIResponseAndCaptureModel(recorder, strings.NewReader(input), "text/event-stream", "grok-4.6"); got != "resp_stream" {
		t.Fatalf("response id=%q", got)
	}
}
