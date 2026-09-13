package qoder

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/goccy/go-json"

	apperrors "orchids-api/internal/errors"
	"orchids-api/internal/prompt"
	"orchids-api/internal/store"
	"orchids-api/internal/upstream"
)

// signedTestAccount builds an account whose credential and derived pair are
// complete, so a request can be assembled without touching the token endpoints.
func signedTestAccount() *store.Account {
	return &store.Account{
		ID:                1,
		AccountType:       "qoder",
		QoderAccessToken:  "access-1",
		QoderRefreshToken: "refresh-1",
		QoderExpiresAt:    time.Now().Add(6 * time.Hour),
		QoderUserID:       "uid-1",
		QoderMachineID:    "11111111-2222-4333-8444-555555555555",
		QoderRuntimeInfo:  "runtime-info",
		QoderRuntimeKey:   "runtime-key",
		QoderDataPolicy:   true,
	}
}

// TestSendRequestSetsTheFullHeaderContract pins the signed header set, including
// the conditional presence rules. The gateway rejects a request that carries an
// empty organization header, so presence is part of correctness, not style.
func TestSendRequestSetsTheFullHeaderContract(t *testing.T) {
	t.Parallel()

	type captured struct {
		headers http.Header
		url     string
		body    []byte
	}
	capturedCh := make(chan captured, 1)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		capturedCh <- captured{headers: r.Header.Clone(), url: r.URL.String(), body: body}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(envelope(`{"id":"1","choices":[{"index":0,"delta":{"content":"hi"},"finish_reason":"stop"}]}`)))
		_, _ = w.Write([]byte("event:finish\ndata: {}\n\n"))
	}))
	defer server.Close()

	acc := signedTestAccount()
	client := NewFromAccount(acc, nil)
	client.SetEndpointsForTest(server.URL, server.URL, server.URL, server.URL)

	var events []upstream.SSEMessage
	err := client.SendRequestWithPayload(context.Background(), upstream.UpstreamRequest{
		Model:    "Qwen3.7-Max",
		Messages: []prompt.Message{{Role: "user", Content: prompt.MessageContent{Text: "hello"}}},
	}, func(msg upstream.SSEMessage) {
		events = append(events, msg)
	}, nil)
	if err != nil {
		t.Fatalf("SendRequestWithPayload() error = %v", err)
	}

	var got captured
	select {
	case got = <-capturedCh:
	case <-time.After(2 * time.Second):
		t.Fatal("the stub server received no request")
	}

	if !strings.Contains(got.url, "/algo/api/v2/service/pro/sse/agent_chat_generation?FetchKeys=llm_model_result&AgentId=agent_common&Encode=1") {
		t.Fatalf("url = %q, want the fixed chat path and query", got.url)
	}

	// NOTE: net/http canonicalizes header names, so Cosy-ClientType reads back
	// as Cosy-Clienttype and Login-Version as Login-Version.
	want := map[string]string{
		"Accept":                "text/event-stream",
		"Cache-Control":         "no-cache",
		"Connection":            "keep-alive",
		"Content-Type":          "application/json",
		"Cosy-Business-Product": "cli",
		"Cosy-Business-Type":    "agent",
		"Cosy-Clienttype":       "5",
		"Cosy-Data-Policy":      "agree",
		"Cosy-Key":              "runtime-key",
		"Cosy-Machineid":        acc.QoderMachineID,
		"Cosy-Machinetoken":     acc.QoderMachineID,
		"Cosy-Machinetype":      "5",
		"Cosy-Scene":            "assistant",
		"Cosy-User":             "uid-1",
		"Login-Version":         "v2",
		"X-Model-Key":           "qmodel_latest",
		"X-Model-Source":        "system",
	}
	for name, value := range want {
		if got.headers.Get(name) != value {
			t.Errorf("header %s = %q, want %q", name, got.headers.Get(name), value)
		}
	}
	if got.headers.Get("Cosy-Date") == "" {
		t.Error("Cosy-Date is empty")
	}
	if auth := got.headers.Get("Authorization"); !strings.HasPrefix(auth, "Bearer COSY.") {
		t.Errorf("Authorization = %q, want a COSY bearer", auth)
	}
	if got.headers.Get("Cosy-Organization-Id") != "" {
		t.Error("an empty organization id was sent as a header")
	}
	if got := len(got.headers); got < 20 {
		t.Errorf("header count = %d, want the full signed set", got)
	}

	// The body is in the private encoding and decodes to the chat payload.
	decoded, err := DecodeBody(got.body)
	if err != nil {
		t.Fatalf("DecodeBody() error = %v", err)
	}
	text := string(decoded)
	for _, want := range []string{`"chat_task":"FREE_INPUT"`, `"session_type":"qodercli"`, `"agent_id":"agent_common"`, `"task_id":"common"`, `"stream":true`, `"version":"3"`, `"key":"qmodel_latest"`, `"role":"user"`, `"context_length":1000000`} {
		if !strings.Contains(text, want) {
			t.Errorf("decoded body = %s, want it to contain %s", text, want)
		}
	}

	if len(events) == 0 || events[len(events)-1].Type != "model.finish" {
		t.Fatalf("events = %+v, want a trailing model.finish", events)
	}
	if reason, _ := events[len(events)-1].Event["finishReason"].(string); reason != "end_turn" {
		t.Fatalf("finishReason = %v, want end_turn", events[len(events)-1].Event["finishReason"])
	}
}

// TestSendRequestRefreshesOnceOnUnauthorized proves a single 401 forces one
// token refresh and the retry succeeds. The refresh budget is one attempt: a
// second 401 after a fresh token is a real credential problem, and retrying it
// would hammer the token endpoint.
func TestSendRequestRefreshesOnceOnUnauthorized(t *testing.T) {
	t.Parallel()

	var chatCalls, refreshCalls int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.URL.Path, "deviceToken/refresh"):
			refreshCalls++
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"device_token":"access-2","refresh_token":"refresh-2","expires_in":3600}`))
		case strings.Contains(r.URL.Path, "agent_chat_generation"):
			chatCalls++
			if chatCalls == 1 {
				w.WriteHeader(http.StatusUnauthorized)
				_, _ = w.Write([]byte(`{"statusCodeValue":401,"body":"{\"message\":\"login expired\"}"}`))
				return
			}
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = w.Write([]byte(envelope(`{"id":"1","choices":[{"index":0,"delta":{"content":"ok"},"finish_reason":"stop"}]}`)))
			_, _ = w.Write([]byte("event:finish\ndata: {}\n\n"))
		default:
			t.Errorf("unexpected path %q", r.URL.Path)
		}
	}))
	defer server.Close()

	acc := signedTestAccount()
	client := NewFromAccount(acc, nil)
	client.SetEndpointsForTest(server.URL, server.URL, server.URL, server.URL)

	err := client.SendRequestWithPayload(context.Background(), upstream.UpstreamRequest{
		Model:    "Qwen3.7-Max",
		Messages: []prompt.Message{{Role: "user", Content: prompt.MessageContent{Text: "hello"}}},
	}, nil, nil)
	if err != nil {
		t.Fatalf("SendRequestWithPayload() error = %v", err)
	}
	if chatCalls != 2 {
		t.Fatalf("chat calls = %d, want 2 (one rejected, one retried)", chatCalls)
	}
	if refreshCalls != 1 {
		t.Fatalf("refresh calls = %d, want exactly 1", refreshCalls)
	}
}

// TestSendRequestDoesNotReplayAfterOutput proves a retry is refused once content
// has reached the caller: replaying would duplicate the answer.
func TestSendRequestDoesNotReplayAfterOutput(t *testing.T) {
	t.Parallel()

	var chatCalls int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.URL.Path, "deviceToken/refresh"):
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"device_token":"access-2","refresh_token":"refresh-2","expires_in":3600}`))
		case strings.Contains(r.URL.Path, "agent_chat_generation"):
			chatCalls++
			w.Header().Set("Content-Type", "text/event-stream")
			// Emit a delta and then fail without a finish event.
			_, _ = w.Write([]byte(envelope(`{"id":"1","choices":[{"index":0,"delta":{"content":"partial"}}]}`)))
		default:
			t.Errorf("unexpected path %q", r.URL.Path)
		}
	}))
	defer server.Close()

	acc := signedTestAccount()
	client := NewFromAccount(acc, nil)
	client.SetEndpointsForTest(server.URL, server.URL, server.URL, server.URL)

	var events []upstream.SSEMessage
	err := client.SendRequestWithPayload(context.Background(), upstream.UpstreamRequest{
		Model:    "Qwen3.7-Max",
		Messages: []prompt.Message{{Role: "user", Content: prompt.MessageContent{Text: "hello"}}},
	}, func(msg upstream.SSEMessage) {
		events = append(events, msg)
	}, nil)
	if err == nil {
		t.Fatal("SendRequestWithPayload() error = nil for a truncated stream")
	}
	if !errors.Is(err, ErrStreamTruncated) {
		t.Fatalf("error = %v, want ErrStreamTruncated", err)
	}
	if chatCalls != 1 {
		t.Fatalf("chat calls = %d, want 1: output must not be replayed", chatCalls)
	}
	if len(events) != 1 {
		t.Fatalf("events = %+v, want only the delivered delta", events)
	}
}

// TestClassifyStatus pins the retry verdicts, including the busy code arriving
// under a 401.
func TestClassifyStatus(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name      string
		status    int
		body      string
		unauth    bool
		retryable bool
		busy      bool
	}{
		{name: "unauthorized", status: 401, body: `{"message":"login expired"}`, unauth: true},
		{name: "forbidden", status: 403, body: `{"message":"nope"}`, unauth: true},
		{name: "busy under 401", status: 401, body: `{"code":"10605","message":"queue"}`, busy: true, retryable: true},
		{name: "server error", status: 503, body: `{}`, retryable: true},
		{name: "rate limited", status: 429, body: `{}`, retryable: true},
		{name: "bad request", status: 400, body: `{"message":"bad"}`},
	}
	for _, tc := range cases {
		err := classifyStatus(tc.status, "", []byte(tc.body))
		var target *attemptStreamError
		if !asAttemptError(err, &target) {
			t.Fatalf("%s: error %v is not an attempt error", tc.name, err)
		}
		if target.unauth != tc.unauth || target.retryable != tc.retryable || target.busy != tc.busy {
			t.Errorf("%s: verdict = unauth=%v retryable=%v busy=%v, want %v/%v/%v",
				tc.name, target.unauth, target.retryable, target.busy, tc.unauth, tc.retryable, tc.busy)
		}
	}
}

// TestBusyWaitIsCapped proves a hostile or buggy backoff hint cannot park a
// request indefinitely.
func TestBusyWaitIsCapped(t *testing.T) {
	t.Parallel()

	if got := busyWait("", []byte(`{"retryAfterMs":600000}`)); got != 30*time.Second {
		t.Fatalf("busyWait() = %v, want the 30s cap", got)
	}
	if got := busyWait("", []byte(`{"queue":{"isQueued":true,"waitTime":1500}}`)); got != 1500*time.Millisecond {
		t.Fatalf("busyWait() = %v, want 1.5s", got)
	}
	if got := busyWait("7", nil); got != 7*time.Second {
		t.Fatalf("busyWait() = %v, want 7s from Retry-After", got)
	}
	if got := busyWait("not-a-number", nil); got != 2*time.Second {
		t.Fatalf("busyWait() = %v, want the 2s default", got)
	}
}

// TestFetchModelsServesTheBuiltInCatalog proves the channel serves a usable
// catalog with no network access at all, which is what makes the device login
// independent of the gateway's model-list endpoint.
func TestFetchModelsServesTheBuiltInCatalog(t *testing.T) {
	t.Parallel()

	acc := signedTestAccount()
	acc.QoderModelIDs = nil
	client := NewFromAccount(acc, nil)
	// Point everything at a closed port: a network read would fail here.
	client.SetEndpointsForTest("http://127.0.0.1:1", "http://127.0.0.1:1", "http://127.0.0.1:1", "http://127.0.0.1:1")

	catalog, err := client.FetchModels(context.Background())
	if err != nil {
		t.Fatalf("FetchModels() error = %v", err)
	}
	if catalog.Len() == 0 {
		t.Fatal("the built-in catalog is empty")
	}
	entry, err := catalog.Resolve("Qwen3.7-Max")
	if err != nil {
		t.Fatalf("Resolve(display name) error = %v", err)
	}
	if entry.Key != "qmodel_latest" {
		t.Fatalf("resolved key = %q, want qmodel_latest", entry.Key)
	}
	if entry, err = catalog.Resolve("qmodel_latest"); err != nil || entry.Name != "Qwen3.7-Max" {
		t.Fatalf("Resolve(internal key) = %+v, %v, want the same row", entry, err)
	}
	if _, err := catalog.Resolve("not-a-model"); err == nil {
		t.Fatal("Resolve(unknown) error = nil")
	}
}

// TestCatalogSnapshotRoundTrip proves the stored snapshot preserves the display
// name, so a restart does not turn a display name into a key.
func TestCatalogSnapshotRoundTrip(t *testing.T) {
	t.Parallel()

	original := newCatalog([]modelEntry{{Key: "kmodel", Name: "Kimi-K2.7-Code"}, {Key: "dmodel", Name: "dmodel"}})
	ids := catalogToIDs(original)
	restored := catalogFromIDs(ids)
	if got := restored.Len(); got != 2 {
		t.Fatalf("restored length = %d, want 2", got)
	}
	entry, err := restored.Resolve("Kimi-K2.7-Code")
	if err != nil {
		t.Fatalf("Resolve() error = %v", err)
	}
	if entry.Key != "kmodel" {
		t.Fatalf("restored key = %q, want kmodel", entry.Key)
	}
	// A bare key snapshot (an imported or hand-edited value) still resolves.
	imported := catalogFromIDs([]string{"qmodel_38max"})
	if entry, err := imported.Resolve("qmodel_38max"); err != nil || entry.Key != "qmodel_38max" {
		t.Fatalf("Resolve(bare key) = %+v, %v", entry, err)
	}
}

// TestDefaultCatalogResolvesEverythingItAdvertises proves the fallback catalog
// is self-consistent, since a fresh account serves its first request from it.
func TestDefaultCatalogResolvesEverythingItAdvertises(t *testing.T) {
	t.Parallel()

	catalog := DefaultCatalog()
	if catalog.Len() == 0 {
		t.Fatal("the fallback catalog is empty")
	}
	for _, name := range catalog.Names() {
		if _, err := catalog.Resolve(name); err != nil {
			t.Errorf("Resolve(%q) error = %v", name, err)
		}
	}
	for _, key := range []string{"qmodel_38max", "dmodel", "mmodel"} {
		entry, err := catalog.Resolve(key)
		if err != nil {
			t.Errorf("Resolve(%q) error = %v", key, err)
			continue
		}
		if entry.Key != key {
			t.Errorf("Resolve(%q).Key = %q", key, entry.Key)
		}
	}
	// The default entry must not be the flagship when a cheaper tier exists.
	if entry := catalog.defaultEntry(); entry.Key == "ultimate" {
		t.Errorf("default model = %q, want a cheaper tier", entry.Key)
	}
}

// TestVerifyModelRejectsUnsupportedName proves a bad model name fails before any
// upstream call.
func TestVerifyModelRejectsUnsupportedName(t *testing.T) {
	t.Parallel()

	acc := signedTestAccount()
	client := NewFromAccount(acc, nil)
	client.SetEndpointsForTest("http://127.0.0.1:1", "http://127.0.0.1:1", "http://127.0.0.1:1", "http://127.0.0.1:1")
	if err := client.VerifyModel(context.Background(), "definitely-not-a-model"); err == nil {
		t.Fatal("VerifyModel() error = nil for an unsupported model")
	}
}

// TestEnsureRuntimeFieldsRequiresIdentity proves the derivation is refused
// before the UID is known, instead of encrypting an empty identity the gateway
// would reject.
func TestEnsureRuntimeFieldsRequiresIdentity(t *testing.T) {
	t.Parallel()

	acc := signedTestAccount()
	acc.QoderRuntimeInfo = ""
	acc.QoderRuntimeKey = ""
	acc.QoderUserID = ""
	client := NewFromAccount(acc, nil)
	if _, err := client.ensureRuntimeFields(context.Background(), credsOf(acc)); err == nil {
		t.Fatal("ensureRuntimeFields() error = nil without a user id")
	}
}

// TestEnsureRuntimeFieldsDerivesOnce proves the pair is derived on demand and
// then reused, which is what keeps a request from re-deriving the identity
// material on every call.
func TestEnsureRuntimeFieldsDerivesOnce(t *testing.T) {
	t.Parallel()

	acc := signedTestAccount()
	acc.QoderRuntimeInfo = ""
	acc.QoderRuntimeKey = ""
	client := NewFromAccount(acc, nil)
	client.SetEntropyForTest(strings.NewReader(strings.Repeat("\x11", 4096)))

	first, err := client.ensureRuntimeFields(context.Background(), credsOf(acc))
	if err != nil {
		t.Fatalf("ensureRuntimeFields() error = %v", err)
	}
	if !first.Complete() {
		t.Fatal("the derived pair is incomplete")
	}
	second, err := client.ensureRuntimeFields(context.Background(), credsOf(acc))
	if err != nil {
		t.Fatalf("second ensureRuntimeFields() error = %v", err)
	}
	if first != second {
		t.Fatalf("the pair was re-derived: %+v vs %+v", first, second)
	}
}

// TestApplyAuthHeadersOmitsOrganizationWhenAbsent pins the conditional presence
// rule: the gateway rejects an empty organization header.
func TestApplyAuthHeadersOmitsOrganizationWhenAbsent(t *testing.T) {
	t.Parallel()

	acc := signedTestAccount()
	client := NewFromAccount(acc, nil)
	creds := credsOf(acc)
	fields := RuntimeFields{EncryptUserInfo: "info", Key: "key"}

	req, err := http.NewRequest(http.MethodPost, "https://example.invalid/algo/api/v2/model/list?Encode=1", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := client.applyAuthHeaders(req, creds, fields, "req-1", "", "", "", signPath(req.URL.String())); err != nil {
		t.Fatalf("applyAuthHeaders() error = %v", err)
	}
	if got := req.Header.Get("Cosy-Organization-Id"); got != "" {
		t.Errorf("Cosy-Organization-Id = %q, want absent", got)
	}
	if got := req.Header.Get("X-Model-Key"); got != "" {
		t.Errorf("X-Model-Key = %q, want absent for a catalog read", got)
	}

	creds.OrgID = "org-1"
	creds.OrgTags = []string{"a", "b"}
	req2, err := http.NewRequest(http.MethodPost, "https://example.invalid/algo/api/v2/service/pro/sse/agent_chat_generation", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := client.applyAuthHeaders(req2, creds, fields, "req-2", "dmodel", "", "body", signPath(req2.URL.String())); err != nil {
		t.Fatalf("applyAuthHeaders() error = %v", err)
	}
	if got := req2.Header.Get("Cosy-Organization-Id"); got != "org-1" {
		t.Errorf("Cosy-Organization-Id = %q, want org-1", got)
	}
	if got := req2.Header.Get("Cosy-Organization-Tags"); got != "a,b" {
		t.Errorf("Cosy-Organization-Tags = %q, want a,b", got)
	}
	if got := req2.Header.Get("X-Model-Source"); got != "" {
		// The source header is gated on the key, not on its own value; an empty
		// source must still be present when a key is sent.
		t.Errorf("X-Model-Source = %q, want present and empty", got)
	}
}

func credsOf(acc *store.Account) Credentials {
	return ResolveCredentials(acc)
}

// TestEntitlementRefusalKeepsTheAccountUsable drives one real streaming request
// against a stub that answers exactly what a live Qoder account without a
// subscription answers, then feeds the resulting error to the shared account
// classifier the handler uses.
//
// This is the whole bug in one assertion. On the live deployment the classifier
// read the upstream "403" out of the message, marked the account status "403",
// and the console showed 「禁止访问」 — and the channel raised a "no usable
// account" alarm — for an account whose credential the gateway had just accepted.
func TestEntitlementRefusalKeepsTheAccountUsable(t *testing.T) {
	t.Parallel()

	// Verbatim from the live gateway: HTTP 200, a business status of 403, and a
	// body naming the pricing page.
	inner := `{"code":"112","message":"{\"pricingUrl\":\"https://qoder.com/pricing?client=qoder\"}"}`
	envelopeBody, err := json.Marshal(map[string]any{
		"headers":         map[string][]string{"Content-Type": {"application/json"}},
		"body":            inner,
		"statusCodeValue": 403,
		"statusCode":      "FORBIDDEN",
	})
	if err != nil {
		t.Fatalf("marshal fixture: %v", err)
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data:" + string(envelopeBody) + "\n\n"))
	}))
	defer server.Close()

	acc := signedTestAccount()
	client := NewFromAccount(acc, nil)
	client.SetEndpointsForTest(server.URL, server.URL, server.URL, server.URL)

	requestErr := client.SendRequestWithPayload(context.Background(), upstream.UpstreamRequest{
		Model:    "Qwen3.7-Max",
		Messages: []prompt.Message{{Role: "user", Content: prompt.MessageContent{Text: "hello"}}},
	}, nil, nil)
	if requestErr == nil {
		t.Fatal("SendRequestWithPayload() error = nil, want an entitlement refusal")
	}
	if !errors.Is(requestErr, ErrNoEntitlement) {
		t.Fatalf("error = %v, want ErrNoEntitlement", requestErr)
	}

	// The handler's own classifier, on the handler's own input.
	if status := apperrors.ClassifyAccountStatus(requestErr.Error()); status != "" {
		t.Fatalf("ClassifyAccountStatus(%q) = %q, want \"\" — a non-empty status disables the account", requestErr.Error(), status)
	}

	// And the reason still reaches whoever reads the request error.
	if !strings.Contains(requestErr.Error(), "pricing") {
		t.Fatalf("error = %v, want the pricing pointer", requestErr)
	}
}

// TestEntitlementRefusalIsTerminal pins the retry verdict. Four attempts were
// spent on a live account before this: the shared classifier buckets an
// unrecognised error as retryable, so a missing subscription was retried until
// the budget ran out even though no retry can change a plan.
func TestEntitlementRefusalIsTerminal(t *testing.T) {
	t.Parallel()

	err := entitlementError(`{"code":"112","message":"{\"pricingUrl\":\"https://qoder.com/pricing?client=qoder\"}"}`)
	class := apperrors.ClassifyUpstreamError(err.Error())
	if class.Category != "client" {
		t.Fatalf("category = %q, want client", class.Category)
	}
	if class.Retryable {
		t.Fatal("an entitlement refusal must not be retried")
	}
	if class.SwitchAccount {
		t.Fatal("an entitlement refusal must not switch accounts: every account on the pool would fail the same way")
	}
	if status := apperrors.ClassifyAccountStatus(err.Error()); status != "" {
		t.Fatalf("account status = %q, want \"\"", status)
	}
}
