package grok

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/goccy/go-json"

	"orchids-api/internal/config"
	"orchids-api/internal/store"
)

func TestClassifyQualityHold(t *testing.T) {
	dumpEncrypted := strings.Repeat("a", int(qualityMinEncryptedChars)+64)
	cases := []struct {
		name string
		sig  qualityStreamSignals
		want qualityVerdict
	}{
		{
			name: "plaintext reasoning releases immediately",
			sig:  qualityStreamSignals{HasThinking: true, HasReasoningDelta: true, ReasoningTokens: 10, VisibleTokens: 3},
			want: qualityDeliver,
		},
		{
			name: "cipher-only thinking with no answer yet waits",
			sig:  qualityStreamSignals{HasThinking: true, EncryptedBytes: 400, EncryptedFloor: 256},
			want: qualityWait,
		},
		{
			// The upstream deliberately does not gate this on visible size: the
			// 18190/18183 dumps answered in under two seconds and leaked through
			// the minimum-output check.
			name: "cipher blob then a fast answer is withheld even when short",
			sig:  qualityStreamSignals{HasThinking: true, EncryptedBytes: 400, EncryptedFloor: 256, VisibleTokens: 5, FirstVisible: true, VisibleFlushMS: 150},
			want: qualityWithhold,
		},
		{
			name: "cipher-only thinking delivers after two seconds of visible text",
			sig:  qualityStreamSignals{HasThinking: true, EncryptedBytes: 400, EncryptedFloor: 256, VisibleTokens: 100, FirstVisible: true, VisibleFlushMS: 2500},
			want: qualityDeliver,
		},
		{
			// Cipher-only thinking with a zero reasoning bill and a large visible
			// answer is the status-loop drool, terminal event or not.
			name: "cipher-only thinking with zero reasoning tokens is withheld at the terminal event",
			sig:  qualityStreamSignals{HasThinking: true, EncryptedBytes: 400, EncryptedFloor: 256, VisibleTokens: 200, Terminal: true},
			want: qualityWithhold,
		},
		{
			// A healthy encrypted-thinking stream bills reasoning tokens, so the
			// drool detector steps aside and the terminal event releases it.
			name: "cipher-only thinking with a reasoning bill delivers at the terminal event",
			sig:  qualityStreamSignals{HasThinking: true, EncryptedBytes: 400, EncryptedFloor: 256, ReasoningTokens: 300, VisibleTokens: 200, Terminal: true},
			want: qualityDeliver,
		},
		{
			name: "no reasoning at all with enough visible text is withheld at the end",
			sig:  qualityStreamSignals{VisibleTokens: 75, Terminal: true},
			want: qualityWithhold,
		},
		{
			name: "reasoning stub then a short dump is withheld (burst)",
			sig: qualityStreamSignals{
				ReasoningStarted: true, ReasoningTokens: 954, VisibleTokens: 1,
				FirstVisible: true, VisibleFlushMS: 30,
			},
			want: qualityWithhold,
		},
		{
			name: "large encrypted blob then a fast answer is withheld (fake encrypted dump)",
			sig: qualityStreamSignals{
				HasThinking: true, EncryptedBytes: int64(len(dumpEncrypted)), VisibleTokens: 1,
				FirstVisible: true, VisibleFlushMS: 1800,
			},
			want: qualityWithhold,
		},
		{
			name: "plaintext reasoning dumped in 1ms with an 80% bill is withheld",
			sig: qualityStreamSignals{
				HasThinking: true, HasReasoningDelta: true, ReasoningTokens: 900,
				OutputTokens: 1000, VisibleTokens: 25, FirstVisible: true, VisibleFlushMS: 1,
			},
			want: qualityWithhold,
		},
		{
			name: "cipher drool (status loop) is withheld once visible text is large",
			sig: qualityStreamSignals{
				HasThinking: true, EncryptedBytes: 400, EncryptedFloor: 256,
				VisibleTokens: qualityCipherDroolVisible + 1,
			},
			want: qualityWithhold,
		},
		{
			name: "empty turn keeps waiting",
			sig:  qualityStreamSignals{},
			want: qualityWait,
		},
		{
			name: "expired hold with nothing visible keeps waiting rather than releasing junk",
			sig:  qualityStreamSignals{HoldExpired: true},
			want: qualityWait,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := classifyQualityHold(tc.sig, qualityHoldMinOutputDefault); got != tc.want {
				t.Fatalf("verdict=%s want %s", got, tc.want)
			}
		})
	}
}

func TestDecideQualityRetryPolicy(t *testing.T) {
	// Six attempts: the first five withhold-and-retry, the sixth delivers.
	for attempt := 0; attempt < qualityHoldMaxAttemptsDefault-1; attempt++ {
		if got := decideQualityRetry(qualityWithhold, attempt, qualityHoldMaxAttemptsDefault, qualityRetryFailOpen); got != qualityActionRetry {
			t.Fatalf("attempt=%d action=%s want retry", attempt, got)
		}
	}
	if got := decideQualityRetry(qualityWithhold, qualityHoldMaxAttemptsDefault-1, qualityHoldMaxAttemptsDefault, qualityRetryFailOpen); got != qualityActionDeliverLast {
		t.Fatalf("last attempt action=%s want deliver_last", got)
	}
	if got := decideQualityRetry(qualityWithhold, qualityHoldMaxAttemptsDefault-1, qualityHoldMaxAttemptsDefault, qualityRetryFailClosed); got != qualityActionReject {
		t.Fatalf("fail-closed last attempt action=%s want reject", got)
	}
	if got := decideQualityRetry(qualityDeliver, 0, qualityHoldMaxAttemptsDefault, qualityRetryFailOpen); got != qualityActionDeliver {
		t.Fatalf("deliver verdict action=%s", got)
	}
	// No routing attempt left: a retry must degrade into deliver-last or reject.
	if got := boundQualityRetry(qualityActionRetry, false, qualityRetryFailOpen); got != qualityActionDeliverLast {
		t.Fatalf("bound fail-open action=%s", got)
	}
	if got := boundQualityRetry(qualityActionRetry, false, qualityRetryFailClosed); got != qualityActionReject {
		t.Fatalf("bound fail-closed action=%s", got)
	}
	if got := boundQualityRetry(qualityActionRetry, true, qualityRetryFailClosed); got != qualityActionRetry {
		t.Fatalf("bound with a next account action=%s", got)
	}
}

func TestQualityRequestReplayUnsafe(t *testing.T) {
	if qualityRequestReplayUnsafe(nil) {
		t.Fatal("nil request must be replay-safe")
	}
	if qualityRequestReplayUnsafe(&ChatCompletionsRequest{}) {
		t.Fatal("a plain request must be replay-safe")
	}
	functionTool := map[string]interface{}{"type": "function", "name": "weather"}
	if qualityRequestReplayUnsafe(&ChatCompletionsRequest{ResponsesTools: []map[string]interface{}{functionTool}}) {
		t.Fatal("a client-executed function tool must be replay-safe")
	}
	hosted := map[string]interface{}{"type": "web_search"}
	if !qualityRequestReplayUnsafe(&ChatCompletionsRequest{ResponsesTools: []map[string]interface{}{hosted}}) {
		t.Fatal("a hosted search tool must block replay")
	}
	if !qualityRequestReplayUnsafe(&ChatCompletionsRequest{WebSearchOptions: map[string]interface{}{"search_context_size": "low"}}) {
		t.Fatal("web_search_options must block replay")
	}
	if !qualityRequestReplayUnsafe(&ChatCompletionsRequest{MCPServers: []map[string]interface{}{{"url": "https://example.test"}}}) {
		t.Fatal("mcp_servers must block replay")
	}
	remoteShell := map[string]interface{}{"type": "shell", "environment": map[string]interface{}{"type": "container"}}
	if !qualityRequestReplayUnsafe(&ChatCompletionsRequest{ResponsesTools: []map[string]interface{}{remoteShell}}) {
		t.Fatal("a hosted shell must block replay")
	}
	localShell := map[string]interface{}{"type": "shell", "environment": map[string]interface{}{"type": "local"}}
	if qualityRequestReplayUnsafe(&ChatCompletionsRequest{ResponsesTools: []map[string]interface{}{localShell}}) {
		t.Fatal("a local shell must stay replay-safe")
	}
	// Unknown tool types default to no replay.
	if !qualityRequestReplayUnsafe(&ChatCompletionsRequest{ResponsesTools: []map[string]interface{}{{"type": "code_execution"}}}) {
		t.Fatal("an unknown hosted tool must block replay")
	}
}

func TestDeferredResponseWriterHoldsThenCommits(t *testing.T) {
	rec := httptest.NewRecorder()
	deferred := newDeferredResponseWriter(rec)
	deferred.Header().Set("Content-Type", "text/event-stream")
	deferred.WriteHeader(http.StatusOK)
	if _, err := io.WriteString(deferred, "data: first\n\n"); err != nil {
		t.Fatalf("write: %v", err)
	}
	deferred.Flush()

	// Nothing may reach the client while the response is held.
	if rec.Body.Len() != 0 || rec.Code != http.StatusOK {
		t.Fatalf("held response leaked: code=%d body=%q", rec.Code, rec.Body.String())
	}
	if deferred.Buffered() == 0 {
		t.Fatal("held bytes were not buffered")
	}
	if err := deferred.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}
	if got := rec.Body.String(); got != "data: first\n\n" {
		t.Fatalf("committed body=%q", got)
	}
	if got := rec.Header().Get("Content-Type"); got != "text/event-stream" {
		t.Fatalf("committed Content-Type=%q", got)
	}
	// After the commit the writer is transparent: no second status line.
	if _, err := io.WriteString(deferred, "data: second\n\n"); err != nil {
		t.Fatalf("write after commit: %v", err)
	}
	if got := rec.Body.String(); got != "data: first\n\ndata: second\n\n" {
		t.Fatalf("body after commit=%q", got)
	}
}

func TestDeferredResponseWriterParksWithoutRevealing(t *testing.T) {
	rec := httptest.NewRecorder()
	deferred := newDeferredResponseWriter(rec)
	deferred.WriteHeader(http.StatusOK)
	_, _ = io.WriteString(deferred, "degraded dump")

	parked := deferred.Park()
	if parked == nil || string(parked.body) != "degraded dump" {
		t.Fatalf("parked=%+v", parked)
	}
	// A parked response stays invisible and later writes are swallowed.
	if _, err := io.WriteString(deferred, "more"); err != nil {
		t.Fatalf("write after park: %v", err)
	}
	if rec.Body.Len() != 0 {
		t.Fatalf("parked response leaked: %q", rec.Body.String())
	}
	// Fail-open delivery writes the parked body to the real client.
	if err := parked.CommitTo(rec); err != nil {
		t.Fatalf("commit parked: %v", err)
	}
	if got := rec.Body.String(); got != "degraded dump" {
		t.Fatalf("delivered body=%q", got)
	}
}

func TestDeferredResponseWriterOverflowDelivers(t *testing.T) {
	rec := httptest.NewRecorder()
	deferred := newDeferredResponseWriter(rec)
	chunk := make([]byte, 1<<20)
	for i := range chunk {
		chunk[i] = 'x'
	}
	for i := 0; i < (qualityHoldMaxBytes>>20)+1; i++ {
		if _, err := deferred.Write(chunk); err != nil {
			t.Fatalf("write: %v", err)
		}
	}
	// Past the cap the response is delivered instead of buffered without bound.
	if rec.Body.Len() == 0 {
		t.Fatal("overflow did not deliver the response")
	}
}

func TestQualityHoldPolicyDefaults(t *testing.T) {
	policy := normalizeQualityHoldPolicy(qualityHoldPolicy{})
	if policy.MaxAttempts != qualityHoldMaxAttemptsDefault || policy.HoldTimeout != qualityHoldTimeoutDefault {
		t.Fatalf("policy=%+v", policy)
	}
	// The zero value normalizes to fail-closed, matching the upstream helper; the
	// handler's own default is fail-open, asserted below.
	if policy.failOpen() {
		t.Fatal("the zero-value policy must normalize to fail-closed")
	}
	if got := normalizeQualityHoldPolicy(qualityHoldPolicy{OnExhausted: qualityRetryFailClosed}); got.failOpen() {
		t.Fatal("fail_closed was not honoured")
	}
	if got := normalizeQualityHoldPolicy(qualityHoldPolicy{OnExhausted: qualityRetryFailOpen}); !got.failOpen() {
		t.Fatal("fail_open was not honoured")
	}
	if !(&Handler{}).qualityHoldPolicy().failOpen() {
		t.Fatal("the handler default must fail open so a strange pool still answers")
	}
	enabled := false
	h := &Handler{cfg: &config.Config{QualityHoldEnabled: &enabled, QualityHoldMaxAttempts: 3, QualityHoldOnExhausted: qualityRetryFailClosed}}
	if h.qualityHoldPolicy().Enabled {
		t.Fatal("an explicit disable was ignored")
	}
	h.cfg = &config.Config{QualityHoldMaxAttempts: 3, QualityHoldTimeoutMs: 2500}
	policy = h.qualityHoldPolicy()
	if !policy.Enabled || policy.MaxAttempts != 3 || policy.HoldTimeout != 2500*time.Millisecond {
		t.Fatalf("policy=%+v", policy)
	}
}

// degradedUpstream replays the cipher-drool dump: an encrypted stub with zero
// reasoning tokens, then the whole visible answer at once.
func degradedUpstream(t *testing.T) *httptest.Server {
	t.Helper()
	encrypted := strings.Repeat("ZmFrZS1jaXBoZXI", 40)
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "event: response.output_item.added\ndata: {\"type\":\"response.output_item.added\",\"item\":{\"id\":\"rs_1\",\"type\":\"reasoning\",\"encrypted_content\":\""+encrypted+"\"}}\n\n")
		dump := strings.Repeat("status loop ", 120)
		payload, _ := json.Marshal(map[string]interface{}{"type": "response.output_text.delta", "delta": dump})
		_, _ = io.WriteString(w, "event: response.output_text.delta\ndata: "+string(payload)+"\n\n")
		_, _ = io.WriteString(w, "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_degraded\",\"status\":\"completed\",\"output\":[],\"usage\":{\"input_tokens\":10,\"output_tokens\":1440,\"output_tokens_details\":{\"reasoning_tokens\":0}}}}\n\n")
	}))
}

// healthyUpstream streams plaintext reasoning before the answer.
func healthyUpstream(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "event: response.output_item.added\ndata: {\"type\":\"response.output_item.added\",\"item\":{\"id\":\"rs_1\",\"type\":\"reasoning\"}}\n\n")
		_, _ = io.WriteString(w, "event: response.reasoning_summary_text.delta\ndata: {\"type\":\"response.reasoning_summary_text.delta\",\"item_id\":\"rs_1\",\"delta\":\"weighing the request\"}\n\n")
		_, _ = io.WriteString(w, "event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"healthy answer\"}\n\n")
		_, _ = io.WriteString(w, "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_healthy\",\"status\":\"completed\",\"output\":[{\"id\":\"rs_1\",\"type\":\"reasoning\"}],\"usage\":{\"input_tokens\":10,\"output_tokens\":12,\"output_tokens_details\":{\"reasoning_tokens\":8}}}}\n\n")
	}))
}

// TestServeNativeChatWithholdsDegradedTurnAndRetriesAnotherAccount is the
// end-to-end proof for A6-1: a degraded stream never reaches the client, the
// credential is penalised, and the answer comes from the next account.
//
// The first attempt is pinned to the degraded credential by account id, because
// which account the balancer picks first is not what this test is about: the hold
// and the retry are.
func TestServeNativeChatWithholdsDegradedTurnAndRetriesAnotherAccount(t *testing.T) {
	degraded := degradedUpstream(t)
	defer degraded.Close()
	healthy := healthyUpstream(t)

	var healthyCalls int
	var degradedCalls int
	routing := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Route by the credential the request carries.
		if strings.Contains(r.Header.Get("Authorization"), "jwt-user-healthy") {
			healthyCalls++
			healthy.Config.Handler.ServeHTTP(w, r)
			return
		}
		degradedCalls++
		degraded.Config.Handler.ServeHTTP(w, r)
	}))
	defer routing.Close()

	h, s, mini := setupValidationHandler(t)
	defer func() {
		_ = s.Close()
		mini.Close()
	}()
	ctx := context.Background()
	if err := s.CreateModel(ctx, &store.Model{
		Channel: "Grok", ModelID: "grok-4.5", Name: "Grok 4.5",
		Status: store.ModelStatusAvailable, Verified: true,
	}); err != nil {
		t.Fatalf("CreateModel: %v", err)
	}
	degradedAcc := &store.Account{
		AccountType: "grok", Enabled: true, CredentialType: "oauth", GrokProvider: ProviderBuild, Weight: 1,
		OAuthAccessToken: "jwt-user-degraded", GrokModels: []string{"grok-4.5"}, GrokModelsSyncedAt: time.Now(),
	}
	healthyAcc := &store.Account{
		AccountType: "grok", Enabled: true, CredentialType: "oauth", GrokProvider: ProviderBuild, Weight: 1,
		OAuthAccessToken: "jwt-user-healthy", GrokModels: []string{"grok-4.5"}, GrokModelsSyncedAt: time.Now(),
	}
	for _, acc := range []*store.Account{degradedAcc, healthyAcc} {
		if err := s.CreateAccount(ctx, acc); err != nil {
			t.Fatalf("CreateAccount: %v", err)
		}
	}

	h.cfg = &config.Config{GrokCLIBaseURL: routing.URL + "/v1"}
	h.cliClient = NewCLIClient(h.cfg)
	h.cliClient.SetAccountStore(s)
	h.cliClient.httpClient = routing.Client()
	h.cliClient.oauth.httpClient = routing.Client()

	sess, err := h.openCLIAccountSessionByID(ctx, degradedAcc.ID, "grok-4.5")
	if err != nil {
		t.Fatalf("openCLIAccountSessionByID: %v", err)
	}
	defer sess.Close()
	spec, ok := h.resolveConversationModel(ctx, "grok-4.5")
	if !ok {
		t.Fatal("grok-4.5 did not resolve")
	}
	effort := "high"
	req := &ChatCompletionsRequest{
		Model: "grok-4.5", Stream: true, ReasoningEffort: &effort,
		Messages: []ChatMessage{{Role: "user", Content: "hello"}},
	}
	rec := httptest.NewRecorder()
	h.serveNativeChat(ctx, rec, req, spec, sess, nil, true)

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	stream := rec.Body.String()
	if strings.Contains(stream, "status loop") {
		t.Fatalf("the degraded dump reached the client: %s", stream)
	}
	if !strings.Contains(stream, "healthy answer") {
		t.Fatalf("the healthy answer never arrived: %s", stream)
	}
	if healthyCalls == 0 {
		t.Fatal("the request was never retried on the healthy account")
	}
	if degradedCalls != 1 {
		t.Fatalf("degraded upstream calls=%d want 1 (the withheld attempt)", degradedCalls)
	}

	// The degraded credential is parked so it stops serving dumps.
	stored, err := s.ListAccounts(ctx)
	if err != nil {
		t.Fatalf("ListAccounts: %v", err)
	}
	parked := 0
	for _, acc := range stored {
		if acc.QualityFailures > 0 {
			parked++
			if acc.QualityCooldownUntil.IsZero() || !acc.QualityCooldownUntil.After(time.Now()) {
				t.Fatalf("account %d was counted but not cooled: %v", acc.ID, acc.QualityCooldownUntil)
			}
		}
	}
	if parked != 1 {
		t.Fatalf("parked accounts=%d want 1", parked)
	}
}

// The HTTP entry point never hands a degraded dump to a client either, whichever
// account the balancer happens to pick first.
func TestHandleChatCompletionsNeverDeliversDegradedDump(t *testing.T) {
	degraded := degradedUpstream(t)
	defer degraded.Close()
	healthy := healthyUpstream(t)
	routing := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.Header.Get("Authorization"), "jwt-user-healthy") {
			healthy.Config.Handler.ServeHTTP(w, r)
			return
		}
		degraded.Config.Handler.ServeHTTP(w, r)
	}))
	defer routing.Close()

	h, s, mini := setupValidationHandler(t)
	defer func() {
		_ = s.Close()
		mini.Close()
	}()
	ctx := context.Background()
	if err := s.CreateModel(ctx, &store.Model{
		Channel: "Grok", ModelID: "grok-4.5", Name: "Grok 4.5",
		Status: store.ModelStatusAvailable, Verified: true,
	}); err != nil {
		t.Fatalf("CreateModel: %v", err)
	}
	for _, token := range []string{"jwt-user-degraded", "jwt-user-healthy"} {
		if err := s.CreateAccount(ctx, &store.Account{
			AccountType: "grok", Enabled: true, CredentialType: "oauth", GrokProvider: ProviderBuild, Weight: 1,
			OAuthAccessToken: token, GrokModels: []string{"grok-4.5"}, GrokModelsSyncedAt: time.Now(),
		}); err != nil {
			t.Fatalf("CreateAccount: %v", err)
		}
	}
	h.cfg = &config.Config{GrokCLIBaseURL: routing.URL + "/v1"}
	h.cliClient = NewCLIClient(h.cfg)
	h.cliClient.SetAccountStore(s)
	h.cliClient.httpClient = routing.Client()
	h.cliClient.oauth.httpClient = routing.Client()

	body := `{"model":"grok-4.5","stream":true,"reasoning_effort":"high","messages":[{"role":"user","content":"hello"}]}`
	req := httptest.NewRequest(http.MethodPost, "/grok/v1/chat/completions", strings.NewReader(body))
	rec := httptest.NewRecorder()
	h.HandleChatCompletions(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	stream := rec.Body.String()
	if strings.Contains(stream, "status loop") {
		t.Fatalf("the degraded dump reached the client: %s", stream)
	}
	if !strings.Contains(stream, "healthy answer") {
		t.Fatalf("no healthy answer was delivered: %s", stream)
	}
}

// A request whose tools have already produced an external side effect is still
// judged and penalised, but it is never replayed on another account.
func TestServeNativeChatDoesNotReplayHostedToolTurn(t *testing.T) {
	degraded := degradedUpstream(t)
	defer degraded.Close()
	calls := 0
	routing := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		degraded.Config.Handler.ServeHTTP(w, r)
	}))
	defer routing.Close()

	h, s, mini := setupValidationHandler(t)
	defer func() {
		_ = s.Close()
		mini.Close()
	}()
	ctx := context.Background()
	if err := s.CreateModel(ctx, &store.Model{
		Channel: "Grok", ModelID: "grok-4.5", Name: "Grok 4.5",
		Status: store.ModelStatusAvailable, Verified: true,
	}); err != nil {
		t.Fatalf("CreateModel: %v", err)
	}
	if err := s.CreateAccount(ctx, &store.Account{
		AccountType: "grok", Enabled: true, CredentialType: "oauth", GrokProvider: ProviderBuild, Weight: 1,
		OAuthAccessToken: "jwt-user-degraded", GrokModels: []string{"grok-4.5"}, GrokModelsSyncedAt: time.Now(),
	}); err != nil {
		t.Fatalf("CreateAccount: %v", err)
	}
	h.cfg = &config.Config{GrokCLIBaseURL: routing.URL + "/v1"}
	h.cliClient = NewCLIClient(h.cfg)
	h.cliClient.SetAccountStore(s)
	h.cliClient.httpClient = routing.Client()
	h.cliClient.oauth.httpClient = routing.Client()

	body, _ := json.Marshal(map[string]interface{}{
		"model": "grok-4.5", "stream": true, "reasoning_effort": "high",
		"messages":          []map[string]interface{}{{"role": "user", "content": "search the web"}},
		"x_responses_tools": []map[string]interface{}{{"type": "web_search"}},
	})
	req := httptest.NewRequest(http.MethodPost, "/grok/v1/chat/completions", strings.NewReader(string(body)))
	rec := httptest.NewRecorder()
	h.HandleChatCompletions(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	// Fail-open: the held body is delivered because it cannot be replayed.
	if !strings.Contains(rec.Body.String(), "status loop") {
		t.Fatalf("fail-open delivery did not write the held body: %s", rec.Body.String())
	}
	if calls != 1 {
		t.Fatalf("upstream calls=%d want 1 (a hosted-tool turn must not be replayed)", calls)
	}
}

// Fail-closed reports the degradation instead of delivering it.
func TestServeNativeChatFailClosedRejectsWithheldTurn(t *testing.T) {
	degraded := degradedUpstream(t)
	defer degraded.Close()
	h, s, mini := setupValidationHandler(t)
	defer func() {
		_ = s.Close()
		mini.Close()
	}()
	ctx := context.Background()
	if err := s.CreateModel(ctx, &store.Model{
		Channel: "Grok", ModelID: "grok-4.5", Name: "Grok 4.5",
		Status: store.ModelStatusAvailable, Verified: true,
	}); err != nil {
		t.Fatalf("CreateModel: %v", err)
	}
	if err := s.CreateAccount(ctx, &store.Account{
		AccountType: "grok", Enabled: true, CredentialType: "oauth", GrokProvider: ProviderBuild, Weight: 1,
		OAuthAccessToken: "jwt-user-degraded", GrokModels: []string{"grok-4.5"}, GrokModelsSyncedAt: time.Now(),
	}); err != nil {
		t.Fatalf("CreateAccount: %v", err)
	}
	h.cfg = &config.Config{
		GrokCLIBaseURL:         degraded.URL + "/v1",
		QualityHoldMaxAttempts: 1,
		QualityHoldOnExhausted: qualityRetryFailClosed,
	}
	h.cliClient = NewCLIClient(h.cfg)
	h.cliClient.SetAccountStore(s)
	h.cliClient.httpClient = degraded.Client()
	h.cliClient.oauth.httpClient = degraded.Client()

	body := `{"model":"grok-4.5","stream":true,"reasoning_effort":"high","messages":[{"role":"user","content":"hello"}]}`
	req := httptest.NewRequest(http.MethodPost, "/grok/v1/chat/completions", strings.NewReader(body))
	rec := httptest.NewRecorder()
	h.HandleChatCompletions(rec, req)

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "status loop") {
		t.Fatalf("fail-closed leaked the degraded body: %s", rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "quality_degraded") {
		t.Fatalf("error body did not name the degradation: %s", rec.Body.String())
	}
}

// A disabled hold keeps the historical behaviour: the dump is streamed through.
func TestServeNativeChatStreamsThroughWhenHoldDisabled(t *testing.T) {
	degraded := degradedUpstream(t)
	defer degraded.Close()
	h, s, mini := setupValidationHandler(t)
	defer func() {
		_ = s.Close()
		mini.Close()
	}()
	ctx := context.Background()
	if err := s.CreateModel(ctx, &store.Model{
		Channel: "Grok", ModelID: "grok-4.5", Name: "Grok 4.5",
		Status: store.ModelStatusAvailable, Verified: true,
	}); err != nil {
		t.Fatalf("CreateModel: %v", err)
	}
	if err := s.CreateAccount(ctx, &store.Account{
		AccountType: "grok", Enabled: true, CredentialType: "oauth", GrokProvider: ProviderBuild, Weight: 1,
		OAuthAccessToken: "jwt-user-degraded", GrokModels: []string{"grok-4.5"}, GrokModelsSyncedAt: time.Now(),
	}); err != nil {
		t.Fatalf("CreateAccount: %v", err)
	}
	disabled := false
	h.cfg = &config.Config{GrokCLIBaseURL: degraded.URL + "/v1", QualityHoldEnabled: &disabled}
	h.cliClient = NewCLIClient(h.cfg)
	h.cliClient.SetAccountStore(s)
	h.cliClient.httpClient = degraded.Client()
	h.cliClient.oauth.httpClient = degraded.Client()

	body := `{"model":"grok-4.5","stream":true,"reasoning_effort":"high","messages":[{"role":"user","content":"hello"}]}`
	req := httptest.NewRequest(http.MethodPost, "/grok/v1/chat/completions", strings.NewReader(body))
	rec := httptest.NewRecorder()
	h.HandleChatCompletions(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "status loop") {
		t.Fatalf("with the hold disabled the stream must pass through: %s", rec.Body.String())
	}
}
