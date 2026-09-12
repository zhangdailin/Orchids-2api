package workbuddy

import (
	"context"
	"encoding/base64"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/goccy/go-json"

	"orchids-api/internal/prompt"
	"orchids-api/internal/store"
	"orchids-api/internal/upstream"
)

func jwtWithClaims(t *testing.T, claims map[string]interface{}) string {
	t.Helper()
	raw, err := json.Marshal(claims)
	if err != nil {
		t.Fatalf("marshal claims: %v", err)
	}
	return "header." + base64.RawURLEncoding.EncodeToString(raw) + ".signature"
}

// jsonMessage builds a message through the wire shape so MessageContent keeps
// its string-vs-blocks union semantics (a nil Blocks slice alone reads as
// "string mode" only for non-empty text).
func jsonMessage(t *testing.T, role, text string) prompt.Message {
	t.Helper()
	var msg prompt.Message
	raw, err := json.Marshal(map[string]string{"role": role, "content": text})
	if err != nil {
		t.Fatalf("marshal message: %v", err)
	}
	if err := json.Unmarshal(raw, &msg); err != nil {
		t.Fatalf("unmarshal message: %v", err)
	}
	return msg
}

func userMessage(t *testing.T, text string) prompt.Message {
	t.Helper()
	return jsonMessage(t, "user", text)
}

func roleMessage(t *testing.T, role, text string) prompt.Message {
	t.Helper()
	return jsonMessage(t, role, text)
}

func TestResolveCredentials_ReadsAuthDocument(t *testing.T) {
	t.Parallel()

	uid := "0f0f0f0f-1111-2222-3333-444455556666"
	expiry := time.Now().Add(48 * time.Hour)
	token := jwtWithClaims(t, map[string]interface{}{
		"sub":   uid,
		"email": "operator@example.com",
		"exp":   expiry.Unix(),
	})
	doc := `{"account":{"uid":"` + uid + `"},"auth":{"accessToken":"` + token +
		`","refreshToken":"refresh-value","expiresAt":` + strconv.FormatInt(expiry.UnixMilli(), 10) + `}}`

	creds := ResolveCredentials(&store.Account{ClientCookie: doc})
	if creds.AccessToken != token {
		t.Fatalf("AccessToken = %q, want the embedded token", creds.AccessToken)
	}
	if creds.RefreshToken != "refresh-value" {
		t.Fatalf("RefreshToken = %q, want refresh-value", creds.RefreshToken)
	}
	if creds.UID != uid {
		t.Fatalf("UID = %q, want %q", creds.UID, uid)
	}
	if creds.Email != "operator@example.com" {
		t.Fatalf("Email = %q, want operator@example.com", creds.Email)
	}
	if creds.ExpiresAt.IsZero() || creds.ExpiresAt.Before(time.Now().Add(24*time.Hour)) {
		t.Fatalf("ExpiresAt = %v, want a future expiry decoded from milliseconds", creds.ExpiresAt)
	}
}

func TestResolveCredentials_SplitsKeyValuePairs(t *testing.T) {
	t.Parallel()

	creds := ResolveCredentials(&store.Account{
		ClientCookie: "accessToken=abc.def.ghi; refreshToken=refresh-xyz",
	})
	if creds.AccessToken != "abc.def.ghi" {
		t.Fatalf("AccessToken = %q", creds.AccessToken)
	}
	if creds.RefreshToken != "refresh-xyz" {
		t.Fatalf("RefreshToken = %q", creds.RefreshToken)
	}
}

func TestResolveCredentials_TreatsOpaqueValueAsRefreshToken(t *testing.T) {
	t.Parallel()

	creds := ResolveCredentials(&store.Account{ClientCookie: "opaque-refresh-value"})
	if creds.RefreshToken != "opaque-refresh-value" {
		t.Fatalf("RefreshToken = %q", creds.RefreshToken)
	}
	if creds.AccessToken != "" {
		t.Fatalf("AccessToken = %q, want empty", creds.AccessToken)
	}
}

func TestResolveCredentials_PrefersDedicatedFields(t *testing.T) {
	t.Parallel()

	creds := ResolveCredentials(&store.Account{
		WorkBuddyAccessToken:  "stored-access",
		WorkBuddyRefreshToken: "stored-refresh",
		WorkBuddyUID:          "stored-uid",
		ClientCookie:          "pasted-refresh",
	})
	if creds.AccessToken != "stored-access" || creds.RefreshToken != "stored-refresh" || creds.UID != "stored-uid" {
		t.Fatalf("credentials = %+v, want the dedicated fields to win", creds)
	}
}

func TestCredentialsToken_RefreshesBeforeExpiry(t *testing.T) {
	t.Parallel()

	fresh := Credentials{AccessToken: "token", ExpiresAt: time.Now().Add(72 * time.Hour)}
	if _, ok := fresh.Token(time.Now()); !ok {
		t.Fatal("a token valid for 72h must be reused")
	}
	stale := Credentials{AccessToken: "token", ExpiresAt: time.Now().Add(time.Hour)}
	if _, ok := stale.Token(time.Now()); ok {
		t.Fatal("a token expiring within the refresh lead must trigger a refresh")
	}
	opaque := Credentials{AccessToken: "token"}
	if _, ok := opaque.Token(time.Now()); !ok {
		t.Fatal("an opaque token without expiry must be used as-is")
	}
}

func TestBuildMessages_RequiresSystemFirst(t *testing.T) {
	t.Parallel()

	messages := buildMessages(upstream.UpstreamRequest{Messages: []prompt.Message{userMessage(t, "hello")}})
	if len(messages) != 2 {
		t.Fatalf("messages = %d, want 2 (synthetic system + user)", len(messages))
	}
	if messages[0].Role != "system" || messages[0].Content != defaultSystem {
		t.Fatalf("messages[0] = %+v, want the default system prompt", messages[0])
	}
	if messages[1].Role != "user" || messages[1].Content != "hello" {
		t.Fatalf("messages[1] = %+v, want the user turn", messages[1])
	}
}

func TestBuildMessages_NormalizesDeveloperRole(t *testing.T) {
	t.Parallel()

	// `developer` is OpenAI's alias for the system role; the upstream rejects
	// it, and the rewrite must preserve content and position.
	messages := buildMessages(upstream.UpstreamRequest{Messages: []prompt.Message{roleMessage(t, "developer", "stay terse")}})
	if len(messages) != 1 {
		t.Fatalf("messages = %d, want 1", len(messages))
	}
	if messages[0].Role != "system" || messages[0].Content != "stay terse" {
		t.Fatalf("messages[0] = %+v, want the developer turn rewritten to system", messages[0])
	}
}

func TestBuildMessages_KeepsSystemItemsAndToolResults(t *testing.T) {
	t.Parallel()

	messages := buildMessages(upstream.UpstreamRequest{
		System: []prompt.SystemItem{{Type: "text", Text: "be brief"}},
		Messages: []prompt.Message{{
			Role: "user",
			Content: prompt.MessageContent{Blocks: []prompt.ContentBlock{
				{Type: "text", Text: "run it"},
				{Type: "tool_result", ToolUseID: "toolu_1", Content: "ok"},
			}},
		}},
	})
	if len(messages) != 3 {
		t.Fatalf("messages = %d, want system + user + tool", len(messages))
	}
	if messages[0].Role != "system" || messages[0].Content != "be brief" {
		t.Fatalf("messages[0] = %+v, want the forwarded system item", messages[0])
	}
	if messages[2].Role != "tool" || messages[2].ToolCallID != "toolu_1" || messages[2].Content != "ok" {
		t.Fatalf("messages[2] = %+v, want the tool result", messages[2])
	}
}

func TestConsumeStream_EmitsTextReasoningAndToolCalls(t *testing.T) {
	t.Parallel()

	body := strings.Join([]string{
		`data: {"id":"cmb-1","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"role":"assistant","content":"","reasoning_content":"think"},"finish_reason":""}]}`,
		`data: {"choices":[{"index":0,"delta":{"content":"hello "},"finish_reason":""}]}`,
		`data: {"choices":[{"index":0,"delta":{"content":"world"},"finish_reason":"stop"}],"usage":null}`,
		`data: {"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"list_files","arguments":"{\"path\":\".\"}"}}]}}]}`,
		`data: {"choices":[],"usage":{"prompt_tokens":11,"completion_tokens":7,"completion_thinking_tokens":2,"prompt_cache_hit_tokens":3}}`,
		`data: [DONE]`,
	}, "\n")

	var events []upstream.SSEMessage
	result, err := consumeStream(strings.NewReader(body), func(msg upstream.SSEMessage) {
		events = append(events, msg)
	})
	if err != nil {
		t.Fatalf("consumeStream() error = %v", err)
	}
	if !result.SawMeaningfulEvent {
		t.Fatal("SawMeaningfulEvent = false")
	}
	if result.ToolCallCount != 1 {
		t.Fatalf("ToolCallCount = %d, want 1", result.ToolCallCount)
	}
	if result.FinishReason() != "tool_use" {
		t.Fatalf("FinishReason() = %q, want tool_use", result.FinishReason())
	}
	if result.Usage["inputTokens"] != 11 || result.Usage["outputTokens"] != 7 {
		t.Fatalf("usage = %+v", result.Usage)
	}

	var text, reasoning, toolName, toolInput string
	for _, event := range events {
		switch event.Type {
		case "model.text-delta":
			text += event.Event["delta"].(string)
		case "model.reasoning-delta":
			reasoning += event.Event["delta"].(string)
		case "model.tool-call":
			toolName, _ = event.Event["toolName"].(string)
			toolInput, _ = event.Event["input"].(string)
		}
	}
	if text != "hello world" {
		t.Fatalf("text = %q", text)
	}
	if reasoning != "think" {
		t.Fatalf("reasoning = %q", reasoning)
	}
	if toolName != "list_files" || toolInput != `{"path":"."}` {
		t.Fatalf("tool call = %q %q", toolName, toolInput)
	}
}

func TestRunChat_RequiresCredentials(t *testing.T) {
	t.Parallel()

	client := NewFromAccount(&store.Account{}, nil)
	client.baseURL = "https://example.invalid"
	err := client.runChat(context.Background(), upstream.UpstreamRequest{Model: defaultModel}, time.Second, nil, nil)
	if err == nil || !strings.Contains(err.Error(), "missing credentials") {
		t.Fatalf("runChat() error = %v, want a missing-credential error", err)
	}
}

func TestRunChat_SendsSystemFirstAndSurfacesBusinessError(t *testing.T) {
	t.Parallel()

	var gotBody map[string]interface{}
	var sawUID bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v2/plugin/auth/token/refresh":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"code":0,"data":{"accessToken":"fresh-access","refreshToken":"rotated-refresh","expiresIn":3600}}`))
		case "/v2/chat/completions":
			sawUID = r.Header.Get("X-User-Id") != ""
			if got := r.Header.Get("Origin"); got != originReferer {
				t.Errorf("Origin = %q, want %q", got, originReferer)
			}
			raw, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(raw, &gotBody)
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = w.Write([]byte(`{"code":6004,"msg":"6004:usage exceeds frequency limit"}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	client := NewFromAccount(&store.Account{WorkBuddyUID: "uid-1", WorkBuddyRefreshToken: "old-refresh"}, nil)
	client.baseURL = srv.URL
	client.httpClient = srv.Client()

	err := client.runChat(context.Background(), upstream.UpstreamRequest{
		Model:    "hy3",
		Messages: []prompt.Message{userMessage(t, "hi")},
	}, 5*time.Second, nil, nil)

	if !sawUID {
		t.Error("X-User-Id header missing on the chat request")
	}
	messages, ok := gotBody["messages"].([]interface{})
	if !ok || len(messages) == 0 {
		t.Fatalf("request body messages = %v", gotBody["messages"])
	}
	first, _ := messages[0].(map[string]interface{})
	if first["role"] != "system" {
		t.Fatalf("messages[0].role = %v, want system", first["role"])
	}
	if gotBody["stream"] != true {
		t.Fatalf("stream = %v, want true", gotBody["stream"])
	}
	if err == nil {
		t.Fatal("runChat() error = nil, want the failure surfaced")
	}
}

func TestApiError_CarriesStatusAndCode(t *testing.T) {
	t.Parallel()

	err := apiError(http.StatusUnauthorized, []byte(`{"code":12153,"msg":"Offline user session not found"}`))
	msg := err.Error()
	for _, want := range []string{"status=401", "code=12153", "Offline user session not found"} {
		if !strings.Contains(msg, want) {
			t.Fatalf("error = %q, want it to contain %q", msg, want)
		}
	}
}

func TestFetchModels_FiltersCLIWhitelist(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"code":0,"data":{
			"models":[
				{"id":"hy3","name":"HY3","disabled":false},
				{"id":"default-model","name":"Default","disabled":false},
				{"id":"hidden-model","name":"Hidden","disabled":false},
				{"id":"disabled-model","name":"Disabled","disabled":true}
			],
			"agents":[{"name":"cli","models":["default-model","hy3","disabled-model"]}]
		}}`))
	}))
	defer srv.Close()

	client := NewFromAccount(&store.Account{}, nil)
	client.baseURL = srv.URL
	client.httpClient = srv.Client()
	client.creds = Credentials{AccessToken: "access", UID: "uid"}

	models, err := client.FetchModels(context.Background())
	if err != nil {
		t.Fatalf("FetchModels() error = %v", err)
	}
	got := make([]string, 0, len(models))
	for _, model := range models {
		got = append(got, model.ID)
	}
	if strings.Join(got, ",") != "hy3,default-model" {
		t.Fatalf("models = %v, want the cli whitelist minus disabled entries", got)
	}
}

func TestTokenUpdater_PersistsRotatedRefreshToken(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("X-Refresh-Token"); got != "old-refresh" {
			t.Errorf("X-Refresh-Token = %q, want old-refresh", got)
		}
		if got := r.Header.Get("X-Auth-Refresh-Source"); got != "plugin" {
			t.Errorf("X-Auth-Refresh-Source = %q, want plugin", got)
		}
		_, _ = w.Write([]byte(`{"code":0,"data":{"accessToken":"new-access","refreshToken":"new-refresh","expiresIn":86400}}`))
	}))
	defer srv.Close()

	acc := &store.Account{ID: 7, AccountType: "workbuddy", WorkBuddyRefreshToken: "old-refresh"}
	updater := &fakeUpdater{}
	client := NewFromAccount(acc, nil)
	client.baseURL = srv.URL
	client.httpClient = srv.Client()
	client.SetAccountStore(updater)

	refreshed, err := newTokenUpdater(srv.URL, srv.Client(), updater, acc).
		RefreshNow(context.Background(), Credentials{RefreshToken: "old-refresh"})
	if err != nil {
		t.Fatalf("RefreshNow() error = %v", err)
	}
	if refreshed.AccessToken != "new-access" {
		t.Fatalf("access token = %q, want new-access", refreshed.AccessToken)
	}
	if updater.saved == nil {
		t.Fatal("rotated refresh token was not persisted")
	}
	if updater.saved.WorkBuddyRefreshToken != "new-refresh" {
		t.Fatalf("persisted refresh token = %q", updater.saved.WorkBuddyRefreshToken)
	}
	if acc.WorkBuddyRefreshToken != "new-refresh" || acc.WorkBuddyAccessToken != "new-access" {
		t.Fatalf("in-memory account = %q/%q", acc.WorkBuddyRefreshToken, acc.WorkBuddyAccessToken)
	}
}

type fakeUpdater struct {
	saved *store.Account
}

func (f *fakeUpdater) UpdateAccount(_ context.Context, acc *store.Account) error {
	copied := *acc
	f.saved = &copied
	return nil
}

func TestBuildMessages_DefaultSystemPromptOnly(t *testing.T) {
	t.Parallel()

	// A request without any history still has to produce a system-first pair,
	// because the upstream rejects anything else with code=11128.
	messages := buildMessages(upstream.UpstreamRequest{})
	if len(messages) != 2 {
		t.Fatalf("messages = %+v, want a system/user pair", messages)
	}
	if messages[0].Role != "system" || messages[0].Content != defaultSystem {
		t.Fatalf("messages[0] = %+v, want the default system prompt", messages[0])
	}
	if messages[1].Role != "user" {
		t.Fatalf("messages[1] = %+v, want a user turn", messages[1])
	}
}
