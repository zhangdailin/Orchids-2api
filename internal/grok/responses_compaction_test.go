package grok

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/goccy/go-json"

	"orchids-api/internal/config"
	"orchids-api/internal/secureblob"
	"orchids-api/internal/store"
)

const compactionTestSummary = "1. Primary Request and Intent: keep the session going across account switches.\n" +
	"2. Key Technical Concepts: Responses wire format, remote-v2 compaction, sealed gateway state, summary turn sampling.\n" +
	"3. Files and Code Sections: internal/grok/responses_compaction.go carries the codec, the classifier and the cleaner.\n" +
	"4. Errors and Fixes: an undecodable blob is a 400 that names the input item, never a silent drop or a relay.\n" +
	"5. Problem Solving: the summary travels inside the client's own history instead of a row on one account.\n" +
	"6. All User Messages: compact this session and continue from the summary that comes back.\n"

func testCompactionCipher(t *testing.T) *secureblob.Cipher {
	t.Helper()
	cipher, err := secureblob.NewCipher([]byte("0123456789abcdef0123456789abcdef"))
	if err != nil {
		t.Fatalf("secureblob.NewCipher: %v", err)
	}
	if !cipher.Available() {
		t.Fatal("cipher is not available")
	}
	return cipher
}

// setupCompactionHandler wires a Build account to a mock upstream and enables
// gateway compaction, the same shape the CLI responses tests use.
func setupCompactionHandler(t *testing.T, upstream *httptest.Server) (*Handler, *store.Store, func()) {
	t.Helper()
	h, s, mini := setupValidationHandler(t)
	if err := s.CreateModel(context.Background(), &store.Model{
		Channel: "Grok", ModelID: "grok-4.5", Name: "Grok 4.5",
		Status: store.ModelStatusAvailable, Verified: true,
	}); err != nil {
		t.Fatalf("CreateModel: %v", err)
	}
	if err := s.CreateAccount(context.Background(), &store.Account{
		AccountType: "grok", Enabled: true, CredentialType: "oauth", GrokProvider: ProviderBuild,
		OAuthAccessToken:   jwtWithClaims(t, `{"sub":"user-1","team_id":"team-1"}`),
		OAuthExpiresAt:     time.Now().Add(time.Hour),
		GrokModels:         []string{"grok-4.5"},
		GrokModelsSyncedAt: time.Now(),
	}); err != nil {
		t.Fatalf("CreateAccount: %v", err)
	}
	h.cfg = &config.Config{GrokCLIBaseURL: upstream.URL + "/v1"}
	h.cliClient = NewCLIClient(h.cfg)
	h.cliClient.SetAccountStore(s)
	h.cliClient.httpClient = upstream.Client()
	h.cliClient.oauth.httpClient = upstream.Client()
	h.SetCompactionCipher(testCompactionCipher(t))
	return h, s, func() {
		_ = s.Close()
		mini.Close()
	}
}

func compactionUpstream(t *testing.T, summary string, received *map[string]interface{}) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("read upstream body: %v", err)
		}
		if received != nil {
			var decoded map[string]interface{}
			if err := json.Unmarshal(body, &decoded); err != nil {
				t.Fatalf("decode upstream body: %v", err)
			}
			*received = decoded
		}
		completed := map[string]interface{}{
			"id": "resp_upstream_1", "object": "response", "status": "completed", "model": "grok-4.5",
			"output": []interface{}{map[string]interface{}{
				"id": "msg_1", "type": "message", "role": "assistant",
				"content": []interface{}{map[string]interface{}{"type": "output_text", "text": summary}},
			}},
			"usage": map[string]interface{}{"input_tokens": float64(1200), "output_tokens": float64(300)},
		}
		completedJSON, _ := json.Marshal(map[string]interface{}{"type": "response.completed", "response": completed})
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "event: response.created\ndata: {\"type\":\"response.created\"}\n\n")
		_, _ = io.WriteString(w, "event: response.completed\ndata: "+string(completedJSON)+"\n\n")
	}))
}

func TestClassifyResponsesCompactionPayload(t *testing.T) {
	cases := []struct {
		name    string
		payload map[string]interface{}
		want    gatewayCompactionKind
	}{
		{
			name:    "codex remote-v2 trigger",
			payload: map[string]interface{}{"input": []interface{}{map[string]interface{}{"type": "compaction_trigger"}}},
			want:    responsesCompactionTrigger,
		},
		{
			name: "trigger mixed with ordinary items",
			payload: map[string]interface{}{"input": []interface{}{
				map[string]interface{}{"type": "message", "role": "user", "content": "hello"},
				map[string]interface{}{"type": "compaction_trigger"},
			}},
			want: responsesCompactionTrigger,
		},
		{
			name: "grok tui prompt as last user item",
			payload: map[string]interface{}{"input": []interface{}{
				map[string]interface{}{"type": "message", "role": "user", "content": "earlier turn"},
				map[string]interface{}{"type": "message", "role": "user", "content": []interface{}{
					map[string]interface{}{"type": "input_text", "text": "Please summarize. Note: " + clientCompactionPromptMarker},
				}},
			}},
			want: responsesCompactionTUI,
		},
		{
			name: "tui prompt recognised on the messages wire too",
			payload: map[string]interface{}{"messages": []interface{}{
				map[string]interface{}{"role": "user", "content": clientCompactionPromptMarker},
			}},
			want: responsesCompactionTUI,
		},
		{
			name: "tui marker not last is an ordinary turn",
			payload: map[string]interface{}{"input": []interface{}{
				map[string]interface{}{"type": "message", "role": "user", "content": clientCompactionPromptMarker},
				map[string]interface{}{"type": "message", "role": "assistant", "content": "ok"},
			}},
			want: responsesCompactionNone,
		},
		{
			name:    "assistant cannot trigger it",
			payload: map[string]interface{}{"input": []interface{}{map[string]interface{}{"type": "message", "role": "assistant", "content": clientCompactionPromptMarker}}},
			want:    responsesCompactionNone,
		},
		{
			name:    "ordinary conversation",
			payload: map[string]interface{}{"input": []interface{}{map[string]interface{}{"type": "message", "role": "user", "content": "hello"}}},
			want:    responsesCompactionNone,
		},
		{name: "empty payload", payload: nil, want: responsesCompactionNone},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := classifyResponsesCompactionPayload(tc.payload); got != tc.want {
				t.Fatalf("kind=%d want %d", got, tc.want)
			}
		})
	}
}

func TestGatewayCompactionCodecRoundTripAndRejections(t *testing.T) {
	codec := newGatewayCompactionCodec(testCompactionCipher(t))
	blob, err := codec.encode("session-a", "Summary:\nkept text")
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	if !strings.HasPrefix(blob, gatewayCompactionPrefix) {
		t.Fatalf("blob=%q missing prefix", blob)
	}

	summary, owned, drifted, err := codec.decode("session-a", blob)
	if err != nil || !owned || drifted || summary != "Summary:\nkept text" {
		t.Fatalf("decode=%q owned=%v drifted=%v err=%v", summary, owned, drifted, err)
	}
	// Session is advisory: a drifted key still decrypts, and the caller learns it.
	if _, owned, drifted, err := codec.decode("session-b", blob); err != nil || !owned || !drifted {
		t.Fatalf("drifted decode owned=%v drifted=%v err=%v", owned, drifted, err)
	}
	// A foreign blob is not ours and must pass through untouched.
	if summary, owned, _, err := codec.decode("session-a", "upstream-opaque-blob"); err != nil || owned || summary != "" {
		t.Fatalf("foreign blob summary=%q owned=%v err=%v", summary, owned, err)
	}
	// A prefixed blob that cannot be opened is an error, never an empty summary.
	if _, owned, _, err := codec.decode("session-a", gatewayCompactionPrefix+"not-base64!!"); err == nil || !owned {
		t.Fatalf("tampered blob owned=%v err=%v", owned, err)
	}
	// Another instance (different key) must not be able to read ours.
	otherCodec := newGatewayCompactionCodec(mustCipher(t, "fedcba9876543210fedcba9876543210"))
	if _, _, _, err := otherCodec.decode("session-a", blob); err == nil {
		t.Fatal("a blob was decoded with a foreign key")
	}
	// Size limits are enforced on both ends.
	if _, err := codec.encode("s", ""); err == nil {
		t.Fatal("empty summary was sealed")
	}
	if _, err := codec.encode("s", strings.Repeat("x", maxGatewayCompactionSummary+1)); err == nil {
		t.Fatal("oversized summary was sealed")
	}
}

func mustCipher(t *testing.T, key string) *secureblob.Cipher {
	t.Helper()
	cipher, err := secureblob.NewCipher([]byte(key))
	if err != nil {
		t.Fatalf("secureblob.NewCipher: %v", err)
	}
	return cipher
}

func TestExpandGatewayCompactionHistory(t *testing.T) {
	codec := newGatewayCompactionCodec(testCompactionCipher(t))
	blob, err := codec.encode("session-a", "Summary:\nreplayed")
	if err != nil {
		t.Fatalf("encode: %v", err)
	}

	payload := map[string]interface{}{"input": []interface{}{
		map[string]interface{}{"type": "message", "role": "user", "content": "hello"},
		map[string]interface{}{"id": "cmp_1", "type": "compaction", "encrypted_content": blob},
		map[string]interface{}{"id": "cmp_2", "type": "compaction", "encrypted_content": "upstream-opaque"},
	}}
	drifted, err := expandGatewayCompactionHistory(payload, codec, "session-a")
	if err != nil || drifted != 0 {
		t.Fatalf("drifted=%d err=%v", drifted, err)
	}
	items := payload["input"].([]interface{})
	expanded := items[1].(map[string]interface{})
	if expanded["type"] != "message" || expanded["role"] != "user" {
		t.Fatalf("expanded item=%#v", expanded)
	}
	parts := expanded["content"].([]interface{})
	if text := parts[0].(map[string]interface{})["text"]; text != "Summary:\nreplayed" {
		t.Fatalf("expanded text=%v", text)
	}
	// The upstream's own blob is handed to the upstream unchanged.
	foreign := items[2].(map[string]interface{})
	if foreign["type"] != "compaction" || foreign["encrypted_content"] != "upstream-opaque" {
		t.Fatalf("foreign item=%#v", foreign)
	}

	// An undecodable gateway blob names the item that has to be dropped.
	bad := map[string]interface{}{"input": []interface{}{
		map[string]interface{}{"type": "compaction", "encrypted_content": gatewayCompactionPrefix + "broken"},
	}}
	_, err = expandGatewayCompactionHistory(bad, codec, "session-a")
	if err == nil {
		t.Fatal("expected an error for an undecodable gateway blob")
	}
	var blobErr *compactionBlobError
	if !asError(err, &blobErr) || blobErr.Param() != "input[0].encrypted_content" {
		t.Fatalf("err=%v param=%q", err, compactionErrorParam(err))
	}
}

func TestCleanGatewayCompactionSummary(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want string
	}{
		{
			name: "analysis block ahead of the summary is dropped",
			raw:  "<analysis>thinking out loud</analysis>\n<summary>1. Request: keep going</summary>",
			want: "Summary:\n1. Request: keep going",
		},
		{
			// The upstream cleaner deletes an <analysis> block whenever it
			// precedes <summary>, even with prose in front of it. Ported verbatim,
			// so the expectation records that behaviour rather than an ideal.
			name: "analysis block ahead of the summary is dropped even with leading prose",
			raw:  "The user said <analysis> earlier.\n<summary>1. Request: keep going</summary>",
			want: "The user said Summary:\n1. Request: keep going",
		},
		{
			name: "a stray opening tag after the summary is defused",
			raw:  "Summary:\n1. Request: keep going <analysis> not a block",
			want: "Summary:\n1. Request: keep going <\u200banalysis> not a block",
		},
		{
			name: "orphan closing tag after markdown scratchpad is stripped",
			raw:  "<summary># scratch\n</analysis>1. Request: keep going</summary>",
			want: "Summary:\n1. Request: keep going",
		},
		{
			name: "numbered summary keeps its own analysis mention",
			raw:  "<summary>1. Request: it mentioned </analysis> verbatim</summary>",
			want: "Summary:\n1. Request: it mentioned <\u200b/analysis> verbatim",
		},
		{
			name: "blank runs collapse",
			raw:  "Summary:\n\n\n1. Request: keep going",
			want: "Summary:\n\n1. Request: keep going",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := cleanGatewayCompactionSummary(tc.raw); got != tc.want {
				t.Fatalf("cleaned=%q want %q", got, tc.want)
			}
		})
	}
}

func TestDegenerateGatewayCompactionSummary(t *testing.T) {
	if !isDegenerateGatewayCompactionSummary("too short") {
		t.Fatal("a short summary must count as degenerate")
	}
	// Tags and blank padding must not be able to fake length.
	if !isDegenerateGatewayCompactionSummary("<summary>" + strings.Repeat("\n", 600) + "short</summary>") {
		t.Fatal("padding counted towards the summary length")
	}
	if isDegenerateGatewayCompactionSummary(compactionTestSummary) {
		t.Fatal("a real summary was rejected")
	}
	if utf8.RuneCountInString(cleanGatewayCompactionSummary(compactionTestSummary)) < minGatewayCompactionRunes {
		t.Fatalf("fixture summary is too short: %d runes", utf8.RuneCountInString(compactionTestSummary))
	}
}

func TestPrepareGatewayCompactionSample(t *testing.T) {
	payload := map[string]interface{}{
		"model": "grok-4.5", "stream": false, "instructions": "be terse",
		"temperature": 0.2, "store": true, "previous_response_id": "resp_1",
		"max_output_tokens": 4096, "text": map[string]interface{}{"format": "json"},
		"input":       []interface{}{map[string]interface{}{"type": "message", "role": "user", "content": "hello"}},
		"tools":       []interface{}{map[string]interface{}{"type": "function", "name": "weather"}},
		"tool_choice": "none",
	}
	sample := prepareGatewayCompactionSample(payload)

	if sample["stream"] != true || sample["store"] != false {
		t.Fatalf("stream=%v store=%v", sample["stream"], sample["store"])
	}
	if sample["instructions"] != nil {
		t.Fatalf("instructions=%v want nil", sample["instructions"])
	}
	if sample["temperature"] != 1.0 || sample["tool_choice"] != "auto" {
		t.Fatalf("temperature=%v tool_choice=%v", sample["temperature"], sample["tool_choice"])
	}
	reasoning, _ := sample["reasoning"].(map[string]interface{})
	if reasoning["summary"] != "concise" {
		t.Fatalf("reasoning=%v", sample["reasoning"])
	}
	for _, dropped := range []string{"previous_response_id", "text", "max_output_tokens", "max_completion_tokens"} {
		if _, present := sample[dropped]; present {
			t.Fatalf("%s survived sample preparation", dropped)
		}
	}
	items := sample["input"].([]interface{})
	last := items[len(items)-1].(map[string]interface{})
	if last["content"] != gatewayCompactionPrompt {
		t.Fatal("the canonical compaction prompt was not appended")
	}
	// The caller's payload must not be mutated: it is still the request body.
	if _, present := payload["reasoning"]; present {
		t.Fatal("prepareGatewayCompactionSample mutated its input")
	}
	if len(payload["input"].([]interface{})) != 1 {
		t.Fatal("prepareGatewayCompactionSample appended to the caller's input")
	}
	// Without tools, a stale tool_choice is removed rather than forwarded.
	noTools := prepareGatewayCompactionSample(map[string]interface{}{"model": "grok-4.5", "input": []interface{}{}, "tool_choice": "none"})
	if _, present := noTools["tool_choice"]; present {
		t.Fatal("tool_choice survived without tools")
	}
}

func TestParseGatewayCompactionStream(t *testing.T) {
	completed := map[string]interface{}{
		"id": "resp_1", "output": []interface{}{map[string]interface{}{
			"id": "msg_1", "type": "message", "role": "assistant",
			"content": []interface{}{map[string]interface{}{"type": "output_text", "text": compactionTestSummary}},
		}},
	}
	completedJSON, _ := json.Marshal(map[string]interface{}{"type": "response.completed", "response": completed})
	stream := "event: response.created\ndata: {\"type\":\"response.created\"}\n\n" +
		"event: response.completed\ndata: " + string(completedJSON) + "\n\n"
	sample, err := parseGatewayCompactionStream([]byte(stream))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if sample.summary != strings.TrimSpace(compactionTestSummary) || sample.response["id"] != "resp_1" {
		t.Fatalf("sample=%+v", sample)
	}

	// No completed event at all is a retryable failure, not an empty summary.
	if _, err := parseGatewayCompactionStream([]byte("event: response.created\ndata: {\"type\":\"response.created\"}\n\n")); err == nil {
		t.Fatal("expected an error for a stream without response.completed")
	}

	// A failed response carries the upstream reason and its retryability.
	failed := "event: response.failed\ndata: {\"type\":\"response.failed\",\"response\":{\"error\":{\"code\":\"invalid_request_error\",\"message\":\"bad input\"}}}\n\n"
	_, err = parseGatewayCompactionStream([]byte(failed))
	if err == nil || gatewayCompactionErrorIsTransient(err) {
		t.Fatalf("err=%v transient=%v", err, gatewayCompactionErrorIsTransient(err))
	}
	transient := "event: response.failed\ndata: {\"type\":\"response.failed\",\"response\":{\"error\":{\"code\":\"server_error\",\"message\":\"try later\"}}}\n\n"
	_, err = parseGatewayCompactionStream([]byte(transient))
	if err == nil || !gatewayCompactionErrorIsTransient(err) {
		t.Fatalf("err=%v transient=%v", err, gatewayCompactionErrorIsTransient(err))
	}

	// A summary that only arrives as streamed output items is still usable.
	streamedOnly := "event: response.output_item.done\ndata: {\"type\":\"response.output_item.done\",\"item\":{\"type\":\"message\",\"content\":[{\"type\":\"output_text\",\"text\":\"" +
		strings.ReplaceAll(compactionTestSummary, "\n", "\\n") + "\"}]}}\n\n" +
		"event: response.completed\ndata: " + string(completedJSON) + "\n\n"
	sample, err = parseGatewayCompactionStream([]byte(streamedOnly))
	if err != nil || sample.summary == "" {
		t.Fatalf("summary=%q err=%v", sample.summary, err)
	}
}

func TestBuildGatewayCompactionResponseShape(t *testing.T) {
	response := map[string]interface{}{
		"id": "resp_abc", "status": "completed", "output_text": "leak me not",
		"usage": map[string]interface{}{"input_tokens": float64(10), "output_tokens": float64(5)},
	}
	result := buildGatewayCompactionResponse(response, "g2a_compact_v1.xyz", "grok-4.5")

	if result["id"] != "resp_abc" || result["object"] != "response" || result["status"] != "completed" || result["model"] != "grok-4.5" {
		t.Fatalf("result=%#v", result)
	}
	if _, present := result["output_text"]; present {
		t.Fatal("output_text leaked into the compaction response")
	}
	output := result["output"].([]interface{})
	item := output[0].(map[string]interface{})
	if item["type"] != "compaction" || item["encrypted_content"] != "g2a_compact_v1.xyz" || item["id"] != "cmp_abc" {
		t.Fatalf("item=%#v", item)
	}
	usage := result["usage"].(map[string]interface{})
	if usage["total_tokens"] != int64(15) {
		t.Fatalf("usage=%#v", usage)
	}
	// A response without usage keeps it absent instead of inventing zeros.
	noUsage := buildGatewayCompactionResponse(map[string]interface{}{"id": "resp_x"}, "blob", "grok-4.5")
	if _, present := noUsage["usage"]; present {
		t.Fatal("usage was fabricated")
	}

	// The streamed form is the same answer as six ordered events.
	var builder strings.Builder
	if err := writeGatewayCompactionStream(&builder, result); err != nil {
		t.Fatalf("write stream: %v", err)
	}
	names := make([]string, 0, 6)
	var completedPayload map[string]interface{}
	if err := consumeCompatibleSSE(strings.NewReader(builder.String()), func(event compatibleSSEEvent) error {
		names = append(names, event.Event)
		if event.Event == "response.completed" {
			if err := json.Unmarshal(event.Data(), &completedPayload); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatalf("consume stream: %v", err)
	}
	want := []string{"response.created", "response.in_progress", "response.output_item.added", "keepalive", "response.output_item.done", "response.completed"}
	if strings.Join(names, ",") != strings.Join(want, ",") {
		t.Fatalf("events=%v want %v", names, want)
	}
	completedResponse := completedPayload["response"].(map[string]interface{})
	if completedResponse["status"] != "completed" {
		t.Fatalf("completed response=%#v", completedResponse)
	}
}

// TestHandleResponsesCompactSealsGatewaySummary is the end-to-end proof: the
// gateway runs the summary turn itself, seals it, and returns a portable
// compaction item instead of an upstream blob only one account can read.
func TestHandleResponsesCompactSealsGatewaySummary(t *testing.T) {
	var received map[string]interface{}
	upstream := compactionUpstream(t, compactionTestSummary, &received)
	defer upstream.Close()
	codecCipher := testCompactionCipher(t)
	h, s, cleanup := setupCompactionHandler(t, upstream)
	defer cleanup()

	body := `{"model":"grok-4.5","input":[{"type":"message","role":"user","content":"hello"}],"stream":false}`
	req := httptest.NewRequest(http.MethodPost, "/v1/responses/compact", strings.NewReader(body))
	rec := httptest.NewRecorder()
	h.HandleResponsesCompact(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("Content-Type"); !strings.Contains(got, "application/json") {
		t.Fatalf("Content-Type=%q", got)
	}
	_ = s

	// The sample the gateway sent upstream is the canonical Build compaction turn.
	if received == nil {
		t.Fatal("upstream saw no request")
	}
	if received["stream"] != true || received["store"] != false {
		t.Fatalf("sample stream=%v store=%v", received["stream"], received["store"])
	}
	if received["instructions"] != nil {
		t.Fatalf("sample instructions=%v", received["instructions"])
	}
	items := received["input"].([]interface{})
	last := items[len(items)-1].(map[string]interface{})
	if last["content"] != gatewayCompactionPrompt {
		t.Fatal("the canonical compaction prompt was not sent upstream")
	}

	var payload map[string]interface{}
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	output := payload["output"].([]interface{})
	item := output[0].(map[string]interface{})
	if item["type"] != "compaction" {
		t.Fatalf("item=%#v", item)
	}
	blob := item["encrypted_content"].(string)
	if !strings.HasPrefix(blob, gatewayCompactionPrefix) {
		t.Fatalf("blob=%q is not gateway-owned", blob)
	}
	codec := newGatewayCompactionCodec(codecCipher)
	summary, owned, _, err := codec.decode("", blob)
	if err != nil || !owned {
		t.Fatalf("decode blob owned=%v err=%v", owned, err)
	}
	if !strings.Contains(summary, "This session is being continued from a previous conversation") {
		t.Fatalf("summary=%q is missing the continuation preamble", summary)
	}
	if !strings.Contains(summary, "Primary Request and Intent") {
		t.Fatalf("summary=%q lost the model's summary", summary)
	}
}

// TestHandleResponsesExpandsGatewayCompactionHistory proves the other half: the
// sealed summary comes back as an ordinary user message, so any account can
// serve the continuation.
func TestHandleResponsesExpandsGatewayCompactionHistory(t *testing.T) {
	var received map[string]interface{}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var decoded map[string]interface{}
		if err := json.Unmarshal(body, &decoded); err != nil {
			t.Fatalf("decode upstream body: %v", err)
		}
		received = decoded
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "event: response.created\ndata: {\"type\":\"response.created\"}\n\n")
		_, _ = io.WriteString(w, "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_2\",\"output\":[]}}\n\n")
	}))
	defer upstream.Close()

	h, _, cleanup := setupCompactionHandler(t, upstream)
	defer cleanup()

	codec := newGatewayCompactionCodec(testCompactionCipher(t))
	blob, err := codec.encode("session-a", "Summary:\ncarried forward")
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	body, _ := json.Marshal(map[string]interface{}{
		"model": "grok-4.5", "stream": true,
		"input": []interface{}{
			map[string]interface{}{"type": "compaction", "encrypted_content": blob},
			map[string]interface{}{"type": "message", "role": "user", "content": "next question"},
		},
	})
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(string(body)))
	rec := httptest.NewRecorder()
	h.HandleResponses(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if received == nil {
		t.Fatal("upstream saw no request")
	}
	items := received["input"].([]interface{})
	for index, raw := range items {
		if item, ok := raw.(map[string]interface{}); ok && item["type"] == "compaction" {
			t.Fatalf("input[%d] still carries a compaction item: %#v", index, item)
		}
	}
	first := items[0].(map[string]interface{})
	if first["type"] != "message" || first["role"] != "user" {
		t.Fatalf("expanded item=%#v", first)
	}
	parts := first["content"].([]interface{})
	if text := parts[0].(map[string]interface{})["text"]; text != "Summary:\ncarried forward" {
		t.Fatalf("expanded text=%v", text)
	}
}

// A blob this gateway cannot open is a 400 that names the offending item. It must
// never be dropped silently or forwarded as an opaque blob.
func TestHandleResponsesRejectsUnreadableGatewayCompactionBlob(t *testing.T) {
	upstreamCalls := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamCalls++
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()
	h, _, cleanup := setupCompactionHandler(t, upstream)
	defer cleanup()

	body, _ := json.Marshal(map[string]interface{}{
		"model": "grok-4.5", "stream": false,
		"input": []interface{}{
			map[string]interface{}{"type": "message", "role": "user", "content": "hi"},
			map[string]interface{}{"type": "compaction", "encrypted_content": gatewayCompactionPrefix + "broken"},
		},
	})
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(string(body)))
	rec := httptest.NewRecorder()
	h.HandleResponses(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var payload map[string]interface{}
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode error body: %v", err)
	}
	errObj := payload["error"].(map[string]interface{})
	if errObj["code"] != "invalid_compaction_blob" {
		t.Fatalf("error=%#v", errObj)
	}
	if errObj["param"] != "input[1].encrypted_content" {
		t.Fatalf("param=%v", errObj["param"])
	}
	if upstreamCalls != 0 {
		t.Fatalf("upstream was called %d times for an unreadable blob", upstreamCalls)
	}
}

// Without a sealing key the feature is off, and a compaction trigger keeps the
// old behaviour: it is relayed to the upstream rather than answered locally.
func TestHandleResponsesRelaysCompactionTriggerWhenDisabled(t *testing.T) {
	var received map[string]interface{}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var decoded map[string]interface{}
		_ = json.Unmarshal(body, &decoded)
		received = decoded
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "event: response.created\ndata: {\"type\":\"response.created\"}\n\n")
		_, _ = io.WriteString(w, "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_3\",\"output\":[]}}\n\n")
	}))
	defer upstream.Close()

	h, _, cleanup := setupCompactionHandler(t, upstream)
	defer cleanup()
	h.SetCompactionCipher(nil)
	if h.GatewayCompactionEnabled() {
		t.Fatal("compaction reported as enabled without a cipher")
	}

	body, _ := json.Marshal(map[string]interface{}{
		"model": "grok-4.5", "stream": true,
		"input": []interface{}{map[string]interface{}{"type": "compaction_trigger"}},
	})
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(string(body)))
	rec := httptest.NewRecorder()
	h.HandleResponses(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if received == nil {
		t.Fatal("the trigger was not relayed upstream")
	}
	items := received["input"].([]interface{})
	if items[0].(map[string]interface{})["type"] != "compaction_trigger" {
		t.Fatalf("trigger was rewritten: %#v", items[0])
	}
	for _, raw := range items {
		if item, ok := raw.(map[string]interface{}); ok && item["content"] == gatewayCompactionPrompt {
			t.Fatal("the gateway ran a summary turn while the feature was disabled")
		}
	}
}

// asError is a tiny local alias so the test does not have to import errors just
// for one assertion.
func asError(err error, target interface{}) bool {
	switch typed := target.(type) {
	case **compactionBlobError:
		blobErr, ok := err.(*compactionBlobError)
		if ok {
			*typed = blobErr
		}
		return ok
	default:
		return false
	}
}

// A compaction_trigger turn with streaming enabled must come back as a synthetic
// SSE sequence, not as a buffered JSON body: Codex holds the connection open.
func TestHandleResponsesCompactionTriggerStreamsSyntheticEvents(t *testing.T) {
	upstream := compactionUpstream(t, compactionTestSummary, nil)
	defer upstream.Close()
	h, _, cleanup := setupCompactionHandler(t, upstream)
	defer cleanup()

	body, _ := json.Marshal(map[string]interface{}{
		"model": "grok-4.5", "stream": true,
		"input": []interface{}{map[string]interface{}{"type": "compaction_trigger"}},
	})
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(string(body)))
	rec := httptest.NewRecorder()
	h.HandleResponses(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("Content-Type"); !strings.Contains(got, "text/event-stream") {
		t.Fatalf("Content-Type=%q", got)
	}
	var events []string
	var blob string
	if err := consumeCompatibleSSE(strings.NewReader(rec.Body.String()), func(event compatibleSSEEvent) error {
		events = append(events, event.Event)
		if event.Event == "response.completed" {
			var payload map[string]interface{}
			if err := json.Unmarshal(event.Data(), &payload); err != nil {
				return err
			}
			response := payload["response"].(map[string]interface{})
			item := response["output"].([]interface{})[0].(map[string]interface{})
			blob, _ = item["encrypted_content"].(string)
		}
		return nil
	}); err != nil {
		t.Fatalf("consume response stream: %v", err)
	}
	want := []string{"response.created", "response.in_progress", "response.output_item.added", "keepalive", "response.output_item.done", "response.completed"}
	if strings.Join(events, ",") != strings.Join(want, ",") {
		t.Fatalf("events=%v want %v", events, want)
	}
	if !strings.HasPrefix(blob, gatewayCompactionPrefix) {
		t.Fatalf("streamed blob=%q is not gateway-owned", blob)
	}
}

// A summary the model refuses to produce is not charged to the client as an
// answer: the gateway reports a 502 compaction_failed instead of an empty blob.
func TestHandleResponsesCompactFailsOnDegenerateSummary(t *testing.T) {
	attempts := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts++
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_deg\",\"output\":[{\"type\":\"message\",\"content\":[{\"type\":\"output_text\",\"text\":\"too short\"}]}]}}\n\n")
	}))
	defer upstream.Close()
	h, _, cleanup := setupCompactionHandler(t, upstream)
	defer cleanup()

	body := `{"model":"grok-4.5","input":[{"type":"message","role":"user","content":"hello"}],"stream":false}`
	req := httptest.NewRequest(http.MethodPost, "/v1/responses/compact", strings.NewReader(body))
	rec := httptest.NewRecorder()
	// A degenerate summary is retryable, so this exercises the real retry loop;
	// only the pause is shrunk, because the production value is three seconds.
	previousPause := gatewayCompactionRetryPause
	gatewayCompactionRetryPause = time.Millisecond
	defer func() { gatewayCompactionRetryPause = previousPause }()
	h.HandleResponsesCompact(rec, req)

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var payload map[string]interface{}
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode error body: %v", err)
	}
	errObj, _ := payload["error"].(map[string]interface{})
	if errObj["code"] != "compaction_failed" {
		t.Fatalf("error=%#v", errObj)
	}
	if attempts != gatewayCompactionMaxAttempts {
		t.Fatalf("upstream attempts=%d want %d", attempts, gatewayCompactionMaxAttempts)
	}
}
