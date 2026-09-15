package workbuddy

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
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
			Role: "assistant",
			Content: prompt.MessageContent{Blocks: []prompt.ContentBlock{
				{Type: "tool_use", ID: "toolu_1", Name: "run", Input: map[string]interface{}{}},
			}},
		}, {
			Role: "user",
			Content: prompt.MessageContent{Blocks: []prompt.ContentBlock{
				{Type: "text", Text: "run it"},
				{Type: "tool_result", ToolUseID: "toolu_1", Content: "ok"},
			}},
		}},
	})
	if len(messages) != 4 {
		t.Fatalf("messages = %d, want system + assistant + user + tool", len(messages))
	}
	if messages[0].Role != "system" || messages[0].Content != "be brief" {
		t.Fatalf("messages[0] = %+v, want the forwarded system item", messages[0])
	}
	if messages[3].Role != "tool" || messages[3].ToolCallID != "toolu_1" || messages[3].Content != "ok" {
		t.Fatalf("messages[3] = %+v, want the tool result", messages[3])
	}
}

func TestBuildMessagesDropsDanglingToolResult(t *testing.T) {
	t.Parallel()
	messages := buildMessages(upstream.UpstreamRequest{Messages: []prompt.Message{{
		Role: "user",
		Content: prompt.MessageContent{Blocks: []prompt.ContentBlock{{
			Type: "tool_result", ToolUseID: "missing", Content: "must not be sent",
		}}},
	}}})
	for _, message := range messages {
		if message.Role == "tool" {
			t.Fatalf("dangling tool result was forwarded: %#v", messages)
		}
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

func TestConsumeStream_ReassemblesSplitToolArguments(t *testing.T) {
	t.Parallel()

	body := strings.Join([]string{
		`data: {"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_split","type":"function","function":{"name":"write_file","arguments":"{\"path\":"}}]},"finish_reason":""}]}`,
		`data: {"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"notes.txt\",\"content\":\"ok\"}"}}]},"finish_reason":"tool_calls"}]}`,
		`data: [DONE]`,
	}, "\n")

	var calls []upstream.SSEMessage
	result, err := consumeStream(strings.NewReader(body), func(msg upstream.SSEMessage) {
		if msg.Type == "model.tool-call" {
			calls = append(calls, msg)
		}
	})
	if err != nil {
		t.Fatalf("consumeStream() error = %v", err)
	}
	if result.ToolCallCount != 1 || len(calls) != 1 {
		t.Fatalf("tool calls = %d/%d, want exactly one", result.ToolCallCount, len(calls))
	}
	if got := calls[0].Event["input"]; got != `{"path":"notes.txt","content":"ok"}` {
		t.Fatalf("tool input = %q", got)
	}
}

func TestConsumeStream_DoesNotMergeReusedToolIndex(t *testing.T) {
	t.Parallel()
	body := strings.Join([]string{
		`data: {"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_a","function":{"name":"first","arguments":"{}"}}]}}]}`,
		`data: {"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_b","function":{"name":"second","arguments":"{\"n\":2}"}}]},"finish_reason":"tool_calls"}]}`,
		`data: [DONE]`,
	}, "\n")
	var calls []upstream.SSEMessage
	result, err := consumeStream(strings.NewReader(body), func(message upstream.SSEMessage) {
		if message.Type == "model.tool-call" {
			calls = append(calls, message)
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.ToolCallCount != 2 || len(calls) != 2 {
		t.Fatalf("calls=%#v count=%d, want two distinct calls", calls, result.ToolCallCount)
	}
	if calls[0].Event["toolName"] != "first" || calls[0].Event["input"] != "{}" ||
		calls[1].Event["toolName"] != "second" || calls[1].Event["input"] != `{"n":2}` {
		t.Fatalf("reused index calls were corrupted: %#v", calls)
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

func TestBuildBodyNormalizesStringOnlyToolChoice(t *testing.T) {
	client := NewFromAccount(nil, nil)
	body, err := client.buildBody(upstream.UpstreamRequest{
		Tools: []interface{}{map[string]interface{}{
			"name": "read", "input_schema": map[string]interface{}{"type": "object"},
		}},
		ToolChoice: map[string]interface{}{"type": "tool", "name": "read"},
	})
	if err != nil {
		t.Fatal(err)
	}
	var decoded map[string]interface{}
	if err := json.Unmarshal(body, &decoded); err != nil {
		t.Fatal(err)
	}
	if got := decoded["tool_choice"]; got != "required" {
		t.Fatalf("tool_choice=%#v want required", got)
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

func TestApplyChatHeaders_MatchesDesktopFingerprintAndReusesTurnID(t *testing.T) {
	t.Parallel()

	const (
		conversationID = "conv-camel"
		turnID         = "client-req-1"
		traceID        = "client-trace"
	)
	headers := make([]http.Header, 0, 2)
	for range 2 {
		req := httptest.NewRequest(http.MethodPost, DefaultBaseURL+"/v2/chat/completions", nil)
		applyChatHeaders(req, "access", "uid-1", conversationID, turnID, traceID)
		headers = append(headers, req.Header.Clone())
	}

	want := map[string]string{
		"Accept":                    "application/json, text/event-stream",
		"Accept-Language":           "en-US",
		"Authorization":             "Bearer access",
		"Origin":                    originReferer,
		"Referer":                   originReferer + "/",
		"User-Agent":                clientUA,
		"X-Agent-Purpose":           "conversation",
		"X-CodeBuddy-Request":       "1",
		"X-Conversation-ID":         conversationID,
		"X-Conversation-Request-ID": turnID,
		"X-Domain":                  "www.workbuddy.ai",
		"X-IDE-Name":                "WorkBuddy",
		"X-IDE-Type":                "WorkBuddy",
		"X-IDE-Version":             clientVersion,
		"X-No-Enterprise-Id":        "1",
		"X-Product":                 "WorkBuddy",
		"X-Root-Request-ID":         turnID,
		"X-Trace-ID":                traceID,
		"X-User-Id":                 "uid-1",
	}
	for i, header := range headers {
		for name, expected := range want {
			if got := header.Get(name); got != expected {
				t.Errorf("request %d %s = %q, want %q", i+1, name, got, expected)
			}
		}
		messageID := header.Get("X-Conversation-Message-ID")
		if len(messageID) != 32 || validWorkBuddyTraceID(messageID) == "" {
			t.Errorf("request %d message id = %q, want 32 hex", i+1, messageID)
		}
		if got := header.Get("X-Request-ID"); got != messageID {
			t.Errorf("request %d X-Request-ID = %q, want message id %q", i+1, got, messageID)
		}
		if got := header.Get("X-B3-TraceId"); len(got) != 32 || validWorkBuddyTraceID(got) == "" {
			t.Errorf("request %d X-B3-TraceId = %q, want valid fallback", i+1, got)
		}
		if got := header.Get("X-B3-SpanId"); got != messageID[:16] {
			t.Errorf("request %d X-B3-SpanId = %q, want %q", i+1, got, messageID[:16])
		}
		if got := header.Get("X-B3-Sampled"); got != "1" {
			t.Errorf("request %d X-B3-Sampled = %q, want 1", i+1, got)
		}
	}
	if first, second := headers[0].Get("X-Conversation-Message-ID"), headers[1].Get("X-Conversation-Message-ID"); first == second {
		t.Fatalf("message id was reused across attempts: %q", first)
	}
}

func TestBuildBody_IncludesUsageAndCamelCaseConversationID(t *testing.T) {
	t.Parallel()

	body, err := NewFromAccount(nil, nil).buildBody(upstream.UpstreamRequest{ConversationID: "conv-1"})
	if err != nil {
		t.Fatal(err)
	}
	var decoded map[string]interface{}
	if err := json.Unmarshal(body, &decoded); err != nil {
		t.Fatal(err)
	}
	if got := decoded["conversationId"]; got != "conv-1" {
		t.Fatalf("conversationId = %#v, want conv-1", got)
	}
	options, ok := decoded["stream_options"].(map[string]interface{})
	if !ok || options["include_usage"] != true {
		t.Fatalf("stream_options = %#v, want include_usage=true", decoded["stream_options"])
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
				{"id":"hy3","name":"HY3","maxInputTokens":131072,"maxOutputTokens":8192,"supportsToolCall":true,"supportsReasoning":true,"disabled":false},
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
	if !models[0].SupportsTools || !models[0].SupportsReason || models[0].MaxInputTokens != 131072 || models[0].MaxOutputTokens != 8192 {
		t.Fatalf("model capabilities were lost: %+v", models[0])
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
	if acc.WorkBuddyRefreshToken != "old-refresh" || acc.WorkBuddyAccessToken != "" {
		t.Fatalf("refresh mutated the caller-owned account snapshot: %q/%q", acc.WorkBuddyRefreshToken, acc.WorkBuddyAccessToken)
	}
}

func TestClientConcurrentFirstUseRefreshesOnlyOnce(t *testing.T) {
	t.Parallel()

	var refreshes atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		refreshes.Add(1)
		_, _ = w.Write([]byte(`{"code":0,"data":{"accessToken":"new-access","refreshToken":"new-refresh","expiresIn":172800}}`))
	}))
	defer srv.Close()

	client := NewFromAccount(&store.Account{
		AccountType:           "workbuddy",
		WorkBuddyRefreshToken: "old-refresh",
	}, nil)
	client.baseURL = srv.URL
	client.httpClient = srv.Client()

	const callers = 24
	var wg sync.WaitGroup
	errs := make(chan error, callers)
	for i := 0; i < callers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			token, err := client.ensureAccessToken(context.Background())
			if err == nil && token != "new-access" {
				err = fmt.Errorf("token = %q", token)
			}
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if got := refreshes.Load(); got != 1 {
		t.Fatalf("refresh calls = %d, want 1", got)
	}
}

type fakeUpdater struct {
	saved *store.Account
}

type flakyUpdater struct {
	fail  bool
	calls int
}

func (f *flakyUpdater) UpdateAccount(_ context.Context, _ *store.Account) error {
	f.calls++
	if f.fail {
		return errors.New("write failed")
	}
	return nil
}

func TestTokenUpdaterReportsPersistenceFailureAndRetriesWithoutRotatingAgain(t *testing.T) {
	t.Parallel()
	refreshes := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		refreshes++
		_, _ = w.Write([]byte(`{"code":0,"data":{"accessToken":"new-access","refreshToken":"new-refresh","expiresIn":172800}}`))
	}))
	defer srv.Close()

	storeUpdater := &flakyUpdater{fail: true}
	updater := newTokenUpdater(srv.URL, srv.Client(), storeUpdater, &store.Account{ID: 1, AccountType: "workbuddy"})
	if _, err := updater.RefreshNow(context.Background(), Credentials{RefreshToken: "old-refresh"}); err == nil {
		t.Fatal("RefreshNow succeeded even though the rotated token was not persisted")
	}
	storeUpdater.fail = false
	token, err := updater.Token(context.Background(), Credentials{RefreshToken: "old-refresh"})
	if err != nil {
		t.Fatal(err)
	}
	if token != "new-access" {
		t.Fatalf("token = %q", token)
	}
	if refreshes != 1 {
		t.Fatalf("refresh calls = %d, want 1", refreshes)
	}
	if storeUpdater.calls != 2 {
		t.Fatalf("persistence calls = %d, want failed write plus retry", storeUpdater.calls)
	}
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
