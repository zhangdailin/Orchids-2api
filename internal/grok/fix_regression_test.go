package grok

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
)

// The client-facing error object has to be parseable by an OpenAI SDK: the
// handler used to answer text/plain, so resp.json()["error"] threw before the
// caller could read the reason.
func TestWriteGrokErrorReturnsOpenAIEnvelope(t *testing.T) {
	rec := httptest.NewRecorder()
	writeGrokError(rec, http.StatusBadRequest, "messages is required")
	if got := rec.Header().Get("Content-Type"); !strings.HasPrefix(got, "application/json") {
		t.Fatalf("Content-Type = %q, want application/json", got)
	}
	var body struct {
		Error struct {
			Message string `json:"message"`
			Type    string `json:"type"`
			Code    string `json:"code"`
			Param   any    `json:"param"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("error body is not JSON: %v (%s)", err, rec.Body.String())
	}
	if body.Error.Message != "messages is required" || body.Error.Code != "invalid_request" {
		t.Fatalf("unexpected error object: %+v", body.Error)
	}
	if body.Error.Type != "invalid_request_error" {
		t.Fatalf("type = %q, want invalid_request_error", body.Error.Type)
	}
}

func TestWriteGrokErrorStatusMapping(t *testing.T) {
	cases := map[int]string{
		http.StatusBadRequest:            "invalid_request_error",
		http.StatusUnauthorized:          "authentication_error",
		http.StatusTooManyRequests:       "rate_limit_error",
		http.StatusServiceUnavailable:    "server_error",
		http.StatusRequestEntityTooLarge: "invalid_request_error",
	}
	for status, wantType := range cases {
		rec := httptest.NewRecorder()
		writeGrokError(rec, status, "boom")
		if rec.Code != status {
			t.Fatalf("status = %d, want %d", rec.Code, status)
		}
		var body map[string]map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatalf("status %d: invalid JSON %v", status, err)
		}
		if got := fmt.Sprint(body["error"]["type"]); got != wantType {
			t.Fatalf("status %d: type = %q, want %q", status, got, wantType)
		}
	}
}

// An upstream failure must never hand the caller the upstream body, the egress
// node id, or the internal "status=… node=… body=…" shape.
func TestWriteGrokUpstreamErrorSanitizesInternalDetail(t *testing.T) {
	upstream := newUpstreamError(http.StatusUnauthorized, http.Header{"Retry-After": {"7"}},
		[]byte(`{"error":{"message":"account team=acme quota exhausted; upgrade at x.ai/pricing"}}`), "node-eu-3")
	rec := httptest.NewRecorder()
	writeGrokUpstreamError(rec, upstream)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503 (an upstream credential failure is the pool's problem)", rec.Code)
	}
	if got := rec.Header().Get("Retry-After"); got != "7" {
		t.Fatalf("Retry-After = %q, want 7", got)
	}
	body := rec.Body.String()
	for _, leak := range []string{"node-eu-3", "acme", "x.ai/pricing", "status=", "body="} {
		if strings.Contains(body, leak) {
			t.Fatalf("upstream detail %q leaked to the client: %s", leak, body)
		}
	}
}

// A local validation error keeps its own message, and its own 400.
func TestWriteGrokUpstreamErrorKeepsLocalValidationMessage(t *testing.T) {
	rec := httptest.NewRecorder()
	writeGrokUpstreamError(rec, errors.New("missing model"))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "missing model") {
		t.Fatalf("local message lost: %s", rec.Body.String())
	}
}

func TestStreamRepeatTrackerStopsRunawayOutput(t *testing.T) {
	tracker := &streamRepeatTracker{}
	event := map[string]interface{}{"type": "response.output_text.delta", "delta": "loop "}
	var err error
	for i := 0; i <= contentDoomLoopThreshold; i++ {
		if err = tracker.observe(event, ""); err != nil {
			break
		}
	}
	if err == nil {
		t.Fatalf("tracker did not stop after %d identical deltas", contentDoomLoopThreshold+1)
	}
	if !errors.Is(err, errGrokUpstreamOutputLoop) {
		t.Fatalf("error = %v, want errGrokUpstreamOutputLoop", err)
	}
}

func TestStreamRepeatTrackerAllowsLegitimateRepetition(t *testing.T) {
	tracker := &streamRepeatTracker{}
	// Markdown separators and table borders repeat the same single character.
	for i := 0; i < contentDoomLoopThreshold; i++ {
		if err := tracker.observe(map[string]interface{}{"type": "response.output_text.delta", "delta": "-"}, ""); err != nil {
			t.Fatalf("legitimate repetition rejected at %d: %v", i, err)
		}
	}
	// A different delta resets the run.
	if err := tracker.observe(map[string]interface{}{"type": "response.output_text.delta", "delta": "x"}, ""); err != nil {
		t.Fatalf("run reset rejected: %v", err)
	}
	if err := tracker.observe(map[string]interface{}{"type": "response.output_text.delta", "delta": "-"}, ""); err != nil {
		t.Fatalf("post-reset delta rejected: %v", err)
	}
}

func TestIsPrivateBuildControlEvent(t *testing.T) {
	if !isPrivateBuildControlEvent("response.doom_loop_check") {
		t.Fatal("doom loop control event must be treated as private")
	}
	if isPrivateBuildControlEvent("response.output_text.delta") {
		t.Fatal("generated delta must not be treated as private")
	}
}

func TestIsModelScopedRefusal(t *testing.T) {
	scoped := []string{
		"grok cli upstream status=403 body={\"error\":\"access to the chat endpoint is denied\"}",
		"grok upstream status=403 body=model is not available",
		"grok cli upstream status=403 body={\"message\":\"not available for model grok-4.6\"}",
	}
	for _, raw := range scoped {
		if !isModelScopedRefusal(errors.New(raw)) {
			t.Fatalf("isModelScopedRefusal(%q) = false, want true", raw)
		}
	}
	for _, raw := range []string{
		"grok upstream status=403 body=account banned",
		"grok upstream status=401 body=unauthorized",
		"grok upstream status=429 body=slow down",
	} {
		if isModelScopedRefusal(errors.New(raw)) {
			t.Fatalf("isModelScopedRefusal(%q) = true, want false", raw)
		}
	}
}

func TestModelScopedFreeQuotaRefusal(t *testing.T) {
	if !modelScopedFreeQuotaRefusal([]byte("You've used all the included free usage for model grok-4.6.")) {
		t.Fatal("model-scoped free usage refusal not detected")
	}
	if modelScopedFreeQuotaRefusal([]byte("subscription:free-usage-exhausted")) {
		t.Fatal("account-scoped refusal must not be treated as model-scoped")
	}
}

// B=3 cases: an integral argument serialized as a float must become an integer
// literal, guided by the tool schema (Codex's decoder rejects the float form).
func TestNormalizeFunctionArgumentsIntegralNumbers(t *testing.T) {
	schema := map[string]interface{}{
		"type": "object",
		"properties": map[string]interface{}{
			"timeout_ms": map[string]interface{}{"type": "integer"},
			"count":      map[string]interface{}{"type": "integer"},
			"ratio":      map[string]interface{}{"type": "number"},
			"nested": map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"limit": map[string]interface{}{"type": "integer"},
				},
			},
			"items": map[string]interface{}{
				"type":  "array",
				"items": map[string]interface{}{"type": "integer"},
			},
		},
	}
	raw := `{"timeout_ms":60000.0,"count":1e3,"ratio":1.5,"nested":{"limit":2.0},"items":[1.0,2e1]}`
	got, changed := normalizeFunctionArguments(raw, schema)
	if !changed {
		t.Fatalf("expected normalization, got %q", got)
	}
	var decoded map[string]interface{}
	decoder := json.NewDecoder(strings.NewReader(got))
	decoder.UseNumber()
	if err := decoder.Decode(&decoded); err != nil {
		t.Fatalf("normalized arguments are not JSON: %v (%s)", err, got)
	}
	check := func(path string, want string) {
		t.Helper()
		parts := strings.Split(path, ".")
		var current interface{} = decoded
		for _, part := range parts {
			asMap, ok := current.(map[string]interface{})
			if !ok {
				t.Fatalf("%s: path not an object in %s", path, got)
			}
			current = asMap[part]
		}
		if fmt.Sprint(current) != want {
			t.Fatalf("%s = %v, want %s (in %s)", path, current, want, got)
		}
	}
	// json.Number stringifies exactly as written, which is the point: the literal
	// must carry no fraction and no exponent.
	check("timeout_ms", "60000")
	check("count", "1000")
	check("nested.limit", "2")
	check("ratio", "1.5")
	if items, ok := decoded["items"].([]interface{}); !ok || fmt.Sprint(items[1]) != "20" {
		t.Fatalf("items = %v, want [1 20] (in %s)", decoded["items"], got)
	}
}

func TestNormalizeFunctionArgumentsLeavesNonIntegralAlone(t *testing.T) {
	schema := map[string]interface{}{
		"type":       "object",
		"properties": map[string]interface{}{"ratio": map[string]interface{}{"type": "number"}},
	}
	raw := `{"ratio":60000.0}`
	if got, changed := normalizeFunctionArguments(raw, schema); changed || got != raw {
		t.Fatalf("number-typed field must be untouched: %q (changed=%v)", got, changed)
	}
	// A payload that is not a single JSON value is also left alone.
	if got, changed := normalizeFunctionArguments(`{"a":1} trailing`, schema); changed || got != `{"a":1} trailing` {
		t.Fatalf("non-JSON payload must be untouched: %q (changed=%v)", got, changed)
	}
}

func TestNormalizeIntegralNumberBounds(t *testing.T) {
	if _, ok := normalizeIntegralNumber("1.0"); !ok {
		t.Fatal("1.0 should normalize to 1")
	}
	if _, ok := normalizeIntegralNumber("1.5"); ok {
		t.Fatal("1.5 is not an integer")
	}
	if _, ok := normalizeIntegralNumber("1e400"); ok {
		t.Fatal("1e400 does not fit an int64 and must be left alone")
	}
}

func TestAnthropicUsageCarriesCacheAndThinkingFields(t *testing.T) {
	usage := map[string]interface{}{
		"prompt_tokens":     100,
		"completion_tokens": 20,
		"prompt_tokens_details": map[string]interface{}{
			"cached_tokens":    30,
			"reasoning_tokens": 0,
		},
		"completion_tokens_details": map[string]interface{}{"reasoning_tokens": 7},
	}
	got := anthropicUsageFromOpenAI(usage)
	if got["input_tokens"] != 70 {
		t.Fatalf("input_tokens = %v, want 70 (100 - 30 cached)", got["input_tokens"])
	}
	if got["cache_read_input_tokens"] != 30 {
		t.Fatalf("cache_read_input_tokens = %v, want 30", got["cache_read_input_tokens"])
	}
	if _, ok := got["cache_creation_input_tokens"]; !ok {
		t.Fatal("cache_creation_input_tokens must be reported (0 is a value, not absence)")
	}
	details, ok := got["output_tokens_details"].(map[string]interface{})
	if !ok || details["thinking_tokens"] != 7 {
		t.Fatalf("output_tokens_details = %v, want thinking_tokens=7", got["output_tokens_details"])
	}
}

func TestAnthropicRefusalUsesDedicatedStopReason(t *testing.T) {
	chat := map[string]interface{}{
		"id": "chatcmpl_1",
		"choices": []interface{}{map[string]interface{}{
			"finish_reason": "stop",
			"message":       map[string]interface{}{"refusal": "I can't help with that"},
		}},
	}
	got := anthropicResponseFromChat("grok-4.6", chat)
	if got["stop_reason"] != "refusal" {
		t.Fatalf("stop_reason = %v, want refusal", got["stop_reason"])
	}
	if !strings.HasPrefix(fmt.Sprint(got["id"]), "msg_") {
		t.Fatalf("id = %v, want an Anthropic msg_ id, not a chatcmpl_ id", got["id"])
	}
}

func TestOpenAIFinishToAnthropicMapsRefusal(t *testing.T) {
	if got := openAIFinishToAnthropic("content_filter"); got != "refusal" {
		t.Fatalf("content_filter -> %q, want refusal", got)
	}
	if got := openAIFinishToAnthropic("stop"); got != "end_turn" {
		t.Fatalf("stop -> %q, want end_turn", got)
	}
}

func TestAnthropicMessageIDReshapesChatCompletionsID(t *testing.T) {
	if got := anthropicMessageID("chatcmpl_abc"); got != "msg_abc" {
		t.Fatalf("anthropicMessageID(chatcmpl_abc) = %q, want msg_abc", got)
	}
	if got := anthropicMessageID("msg_keep"); got != "msg_keep" {
		t.Fatalf("a msg_ id must be preserved, got %q", got)
	}
	if got := anthropicMessageID(""); !strings.HasPrefix(got, "msg_") || len(got) != len("msg_")+24 {
		t.Fatalf("empty id -> %q, want a generated msg_ id", got)
	}
}

func TestConsoleModelSemanticsPerModel(t *testing.T) {
	// grok-4.3 / grok-4.5: configurable effort, medium when the caller sends none.
	payload := map[string]interface{}{}
	normalizeConsoleReasoningEffort(payload, "console/grok-4.3")
	if got := payload["max_output_tokens"]; got != 1000000 {
		t.Fatalf("max_output_tokens = %v, want 1000000", got)
	}
	if reasoning, ok := payload["reasoning"].(map[string]interface{}); !ok || reasoning["effort"] != "medium" {
		t.Fatalf("reasoning = %v, want effort=medium", payload["reasoning"])
	}

	// Fixed-reasoning model: the effort must be dropped, the object kept.
	payload = map[string]interface{}{"reasoning": map[string]interface{}{"effort": "high", "summary": "concise"}}
	normalizeConsoleReasoningEffort(payload, "grok-4.20-0309-reasoning")
	reasoning, _ := payload["reasoning"].(map[string]interface{})
	if _, exists := reasoning["effort"]; exists {
		t.Fatalf("a fixed-reasoning model must not receive effort: %v", reasoning)
	}
	if reasoning["summary"] != "concise" {
		t.Fatalf("other reasoning controls must survive: %v", reasoning)
	}

	// Non-reasoning model: the whole object goes.
	payload = map[string]interface{}{"reasoning": map[string]interface{}{"effort": "low"}}
	normalizeConsoleReasoningEffort(payload, "grok-build-0.1")
	if _, exists := payload["reasoning"]; exists {
		t.Fatalf("a non-reasoning model must not receive reasoning: %v", payload)
	}
	if got := payload["max_output_tokens"]; got != 256000 {
		t.Fatalf("grok-build-0.1 max_output_tokens = %v, want 256000", got)
	}

	// An explicit effort is preserved.
	payload = map[string]interface{}{"reasoning": map[string]interface{}{"effort": "xhigh"}}
	normalizeConsoleReasoningEffort(payload, "grok-4.5")
	reasoning, _ = payload["reasoning"].(map[string]interface{})
	if reasoning["effort"] != "xhigh" {
		t.Fatalf("explicit effort = %v, want xhigh", reasoning["effort"])
	}

	// An unknown model is left alone rather than guessed at.
	payload = map[string]interface{}{}
	normalizeConsoleReasoningEffort(payload, "grok-unknown")
	if _, exists := payload["reasoning"]; exists {
		t.Fatalf("unknown model must not get an invented reasoning object: %v", payload)
	}
	if _, exists := payload["max_output_tokens"]; exists {
		t.Fatalf("unknown model must not get an invented output limit: %v", payload)
	}
}

func TestPrepareGrokSessionRecognizesAgentSessionHeaders(t *testing.T) {
	base := []ChatMessage{{Role: "user", Content: "hello"}}
	cases := map[string]string{
		"X-Claude-Code-Session-Id": "claude-sid",
		"X-Codex-Session-Id":       "codex-sid",
		"X-Codex-Conversation-Id":  "codex-conv",
		"X-Grok-Session-Id":        "grok-sid",
		"X-Session-Id":             "plain-sid",
	}
	for header, value := range cases {
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
		req.Header.Set(header, value)
		session := prepareGrokSession(req, "grok-4.6", "", base)
		if session.Key == "" {
			t.Fatalf("%s: no session key derived", header)
		}
		// An explicit client identity permits encrypted reasoning replay; the
		// message-prefix fallback is affinity-only.
		if !session.Replay {
			t.Fatalf("%s: session is not marked replay-capable", header)
		}
	}
	// Two different clients using the same identifier must not collide.
	reqA := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	reqA.Header.Set("X-Claude-Code-Session-Id", "shared")
	reqB := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	reqB.Header.Set("X-Codex-Session-Id", "shared")
	if a, b := prepareGrokSession(reqA, "grok-4.6", "", base), prepareGrokSession(reqB, "grok-4.6", "", base); a.Key == b.Key {
		t.Fatal("identical seeds from different clients collided")
	}
	// Without any client identity the fallback is affinity-only.
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	fallback := prepareGrokSession(req, "grok-4.6", "", base)
	if fallback.Replay {
		t.Fatal("a message-prefix fallback must not enable reasoning replay")
	}
}

func TestAnthropicUpstreamErrorDoesNotLeakUpstreamBody(t *testing.T) {
	const upstream = `{"error":"account team=acme-team quota exhausted; visit x.ai/pricing"}`
	rec := httptest.NewRecorder()
	writeAnthropicUpstreamError(rec, http.StatusBadRequest, upstream)
	body := rec.Body.String()
	for _, leak := range []string{"acme-team", "x.ai/pricing", "quota exhausted"} {
		if strings.Contains(body, leak) {
			t.Fatalf("upstream detail %q leaked to the client: %s", leak, body)
		}
	}
	var envelope struct {
		Type  string `json:"type"`
		Error struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &envelope); err != nil {
		t.Fatalf("envelope is not JSON: %v (%s)", err, body)
	}
	if envelope.Type != "error" || envelope.Error.Message == "" {
		t.Fatalf("unexpected Anthropic error envelope: %+v", envelope)
	}
}

func TestDetectMediaInputRejectsStillImageISOBrends(t *testing.T) {
	// A HEIC photo shares the "ftyp" signature with an mp4; it must not be
	// forwarded to the upstream as video/mp4.
	heic := append([]byte{0, 0, 0, 0x20}, []byte("ftypheic")...)
	heic = append(heic, make([]byte, 32)...)
	if _, _, err := detectMediaInput(heic, ""); err == nil {
		t.Fatal("a heic brand must not be accepted as a video input")
	}
	// A real mp4 brand is accepted.
	mp4 := append([]byte{0, 0, 0, 0x20}, []byte("ftypisom")...)
	mp4 = append(mp4, make([]byte, 32)...)
	kind, mimeType, err := detectMediaInput(mp4, "")
	if err != nil || kind != "video" || mimeType != "video/mp4" {
		t.Fatalf("mp4 brand rejected: kind=%q mime=%q err=%v", kind, mimeType, err)
	}
	// An explicit video declaration still wins.
	if kind, mimeType, err := detectMediaInput(heic, "video/quicktime"); err != nil || kind != "video" {
		t.Fatalf("declared video rejected: kind=%q mime=%q err=%v", kind, mimeType, err)
	}
}

func TestCachedFileETagIsStableAndWeak(t *testing.T) {
	info := &fakeFileInfo{size: 42}
	first := cachedFileETag(info)
	if !strings.HasPrefix(first, `W/"`) || first != cachedFileETag(info) {
		t.Fatalf("etag = %q, want a stable weak validator", first)
	}
	if cachedFileETag(nil) != "" {
		t.Fatal("etag of a missing file must be empty")
	}
}

type fakeFileInfo struct{ size int64 }

func (f *fakeFileInfo) Name() string       { return "x" }
func (f *fakeFileInfo) Size() int64        { return f.size }
func (f *fakeFileInfo) Mode() os.FileMode  { return 0o644 }
func (f *fakeFileInfo) ModTime() time.Time { return time.Unix(1700000000, 0) }
func (f *fakeFileInfo) IsDir() bool        { return false }
func (f *fakeFileInfo) Sys() interface{}   { return nil }

func TestCopyVoiceResponseHeadersFiltersUpstreamHeaders(t *testing.T) {
	source := http.Header{}
	source.Set("Content-Type", "text/html; charset=utf-8")
	source.Set("Content-Disposition", `attachment; filename="../../etc/passwd"`)
	source.Set("X-Request-Id", "req-1")
	source.Set("Retry-After", "30")
	source.Set("Set-Cookie", "sso=secret")
	destination := http.Header{}
	copyVoiceResponseHeaders(destination, source)
	if got := destination.Get("Content-Type"); got == "text/html; charset=utf-8" {
		t.Fatalf("upstream content type replayed verbatim: %q", got)
	}
	// A path-traversal filename must be stripped down to the bare disposition.
	if got := destination.Get("Content-Disposition"); strings.Contains(got, "..") || strings.Contains(got, "passwd") {
		t.Fatalf("upstream filename replayed: %q", got)
	}
	if got := destination.Get("Set-Cookie"); got != "" {
		t.Fatalf("upstream cookie forwarded: %q", got)
	}
	if got := destination.Get("Retry-After"); got != "30" {
		t.Fatalf("Retry-After = %q, want 30", got)
	}
	if got := destination.Get("X-Content-Type-Options"); got != "nosniff" {
		t.Fatalf("nosniff missing: %q", got)
	}
}

func TestCopyVoiceResponseHeadersKeepsAllowedAudioTypes(t *testing.T) {
	for _, allowed := range []string{"audio/mpeg", "application/json", "text/plain", "audio/wav"} {
		source := http.Header{}
		source.Set("Content-Type", allowed)
		destination := http.Header{}
		copyVoiceResponseHeaders(destination, source)
		if got := destination.Get("Content-Type"); got != allowed {
			t.Fatalf("%s: content type = %q, want it preserved", allowed, got)
		}
	}
}

func TestVideoContentURLIsReachable(t *testing.T) {
	if got := videoContentURL("abc"); got != "/v1/videos/abc/content" {
		t.Fatalf("videoContentURL() = %q, want /v1/videos/abc/content", got)
	}
}

func TestIdleTimeoutIsClassifiedSeparately(t *testing.T) {
	// The sentinel is exported so every plane can recognise the condition.
	if !errors.Is(errGrokSemanticIdle, ErrGrokSemanticIdle) {
		t.Fatal("the ported alias must resolve to the exported sentinel")
	}
	code, message := classifySynthesizedFailure("stream_read_error", "stream read error", errGrokSemanticIdle)
	if code != "upstream_stream_idle_timeout" {
		t.Fatalf("code = %q, want upstream_stream_idle_timeout", code)
	}
	if strings.Contains(message, "parse") {
		t.Fatalf("an idle timeout must not be described as a parse error: %q", message)
	}
	// Any other failure keeps its own classification.
	code, _ = classifySynthesizedFailure("stream_read_error", "stream read error", errors.New("boom"))
	if code != "stream_read_error" {
		t.Fatalf("code = %q, want stream_read_error", code)
	}
}

func TestImageResponseEntriesDeclareMimeAndRevisedPrompt(t *testing.T) {
	if got := imageMimeTypeForValue("b64_json", "aGVsbG8="); got != "image/png" {
		t.Fatalf("default b64 mime = %q, want image/png", got)
	}
	if got := imageMimeTypeForValue("b64_json", "data:image/webp;base64,AAAA"); got != "image/webp" {
		t.Fatalf("data-uri mime = %q, want image/webp", got)
	}
	if got := imageMimeTypeForValue("url", "https://cdn.example/a/b.JPEG"); got != "image/jpeg" {
		t.Fatalf("url mime = %q, want image/jpeg", got)
	}
}

func TestResponsesImagePartsCarryDefaultDetail(t *testing.T) {
	parts := responsesMessageParts([]interface{}{
		map[string]interface{}{"type": "image_url", "image_url": map[string]interface{}{"url": "https://example.com/a.png"}},
	}, false)
	if len(parts) != 1 {
		t.Fatalf("parts = %#v, want one image part", parts)
	}
	part, _ := parts[0].(map[string]interface{})
	if part["detail"] != "auto" {
		t.Fatalf("detail = %v, want auto", part["detail"])
	}
	// An explicit detail is preserved.
	parts = responsesMessageParts([]interface{}{
		map[string]interface{}{"type": "image_url", "detail": "high", "image_url": map[string]interface{}{"url": "https://example.com/a.png"}},
	}, false)
	part, _ = parts[0].(map[string]interface{})
	if part["detail"] != "high" {
		t.Fatalf("explicit detail = %v, want high", part["detail"])
	}
}
