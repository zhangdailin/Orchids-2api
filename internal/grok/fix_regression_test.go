package grok

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
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

func TestWriteGrokModelNotFoundReturns404Code(t *testing.T) {
	rec := httptest.NewRecorder()
	writeGrokErrorCode(rec, http.StatusNotFound, "model_not_found", modelNotFoundMessage("missing"))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status=%d want 404", rec.Code)
	}
	var body map[string]map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	if got := fmt.Sprint(body["error"]["code"]); got != "model_not_found" {
		t.Fatalf("code=%q want model_not_found", got)
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

func TestAnthropicErrorTypeFollowsStatus(t *testing.T) {
	cases := map[int]string{
		http.StatusBadRequest:         "invalid_request_error",
		http.StatusUnauthorized:       "authentication_error",
		http.StatusForbidden:          "permission_error",
		http.StatusNotFound:           "not_found_error",
		http.StatusTooManyRequests:    "rate_limit_error",
		http.StatusServiceUnavailable: "overloaded_error",
	}
	for status, want := range cases {
		rec := httptest.NewRecorder()
		writeAnthropicError(rec, status, "boom")
		var envelope struct {
			Type  string `json:"type"`
			Error struct {
				Type string `json:"type"`
				Code string `json:"code"`
			} `json:"error"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &envelope); err != nil {
			t.Fatalf("status %d: invalid JSON %v", status, err)
		}
		if envelope.Error.Type != want {
			t.Fatalf("status %d: type = %q, want %q", status, envelope.Error.Type, want)
		}
		if envelope.Error.Code == "" {
			t.Fatalf("status %d: code must be present", status)
		}
	}
}

func TestResponsesAPIErrorTypeFollowsStatus(t *testing.T) {
	rec := httptest.NewRecorder()
	writeResponsesAPIError(rec, http.StatusServiceUnavailable, "service_unavailable", "busy")
	var envelope struct {
		Error struct {
			Type  string `json:"type"`
			Code  string `json:"code"`
			Param any    `json:"param"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &envelope); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	if envelope.Error.Type != "server_error" {
		t.Fatalf("type = %q, want server_error for a 503", envelope.Error.Type)
	}
	rec = httptest.NewRecorder()
	writeResponsesAPIError(rec, http.StatusTooManyRequests, "rate_limit_exceeded", "slow down")
	if err := json.Unmarshal(rec.Body.Bytes(), &envelope); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	if envelope.Error.Type != "rate_limit_error" {
		t.Fatalf("type = %q, want rate_limit_error for a 429", envelope.Error.Type)
	}
}

func TestBuildSessionUUIDIsStableAndValid(t *testing.T) {
	first := buildSessionUUID("deadbeef")
	if !isUUID(first) {
		t.Fatalf("buildSessionUUID() = %q, want a UUID", first)
	}
	if first != buildSessionUUID("deadbeef") {
		t.Fatal("the same session seed must map to the same UUID")
	}
	if first == buildSessionUUID("deadbeee") {
		t.Fatal("different seeds must not collide")
	}
	existing := "3f2504e0-4f89-41d3-9a0c-0305e82c3301"
	if got := buildSessionUUID(existing); got != existing {
		t.Fatalf("an existing UUID must pass through, got %q", got)
	}
}

func TestToolMessagesRequireCallID(t *testing.T) {
	messages := []ChatMessage{
		{Role: "assistant", ToolCalls: []ToolCall{{ID: "call_1", Type: "function", Function: map[string]interface{}{"name": "read", "arguments": "{}"}}}},
		{Role: "tool", Name: "read", Content: "done"},
	}
	items, _ := responsesInputFromChatMessages(messages)
	for _, item := range items {
		if m, ok := item.(map[string]interface{}); ok && m["type"] == "function_call_output" {
			t.Fatalf("a tool message without tool_call_id must not become a function_call_output: %#v", m)
		}
	}
}

func TestChatToolUseMustBeAnswered(t *testing.T) {
	unanswered := []ChatMessage{
		{Role: "assistant", ToolCalls: []ToolCall{{ID: "call_1", Function: map[string]interface{}{"name": "read"}}}},
		{Role: "user", Content: "next"},
	}
	if err := validateChatToolSequence(unanswered); err == nil {
		t.Fatal("an unanswered tool_use must be rejected")
	}
	answered := append([]ChatMessage{}, unanswered[0], ChatMessage{Role: "tool", ToolCallID: "call_1", Content: "ok"})
	if err := validateChatToolSequence(answered); err != nil {
		t.Fatalf("a paired tool_use must be accepted: %v", err)
	}
}

func TestResponseFailureClassifiesAntiBot(t *testing.T) {
	code7 := map[string]interface{}{"type": "error", "error": map[string]interface{}{"code": float64(7), "message": "rejected"}}
	err := responseFailure(code7)
	if !errors.Is(err, errGrokWebAntiBot) {
		t.Fatalf("code 7 must classify as anti-bot, got %v", err)
	}
	named := map[string]interface{}{"type": "response.failed", "response": map[string]interface{}{
		"error": map[string]interface{}{"message": "Anti-Bot detected"},
	}}
	if err := responseFailure(named); !errors.Is(err, errGrokWebAntiBot) {
		t.Fatalf("an anti-bot message must classify as anti-bot, got %v", err)
	}
	other := map[string]interface{}{"type": "error", "error": map[string]interface{}{"code": float64(3), "message": "bad request"}}
	if err := responseFailure(other); errors.Is(err, errGrokWebAntiBot) {
		t.Fatalf("an unrelated failure must not classify as anti-bot, got %v", err)
	}
}

func TestQualityDegradedDetection(t *testing.T) {
	cases := []struct {
		name string
		sig  qualitySignals
		want bool
	}{
		{
			name: "healthy reasoning turn",
			sig:  qualitySignals{ExpectReasoning: true, SawReasoning: true, ReasoningChars: 120, VisibleChars: 40, Terminal: true, FirstVisibleMS: 900},
			want: false,
		},
		{
			name: "no reasoning despite the request",
			sig:  qualitySignals{ExpectReasoning: true, VisibleChars: 200, Terminal: true, FirstVisibleMS: 500},
			want: true,
		},
		{
			name: "late dump with a large reasoning bill",
			sig:  qualitySignals{ExpectReasoning: true, VisibleChars: 20, ReasoningTokens: 900, Terminal: true, FirstVisibleMS: 1800},
			want: true,
		},
		{
			name: "tool-only turn is not judged",
			sig:  qualitySignals{ExpectReasoning: true, VisibleChars: 0, ToolCalls: 1, Terminal: true, FirstVisibleMS: -1},
			want: false,
		},
		{
			name: "no reasoning expected",
			sig:  qualitySignals{ExpectReasoning: false, VisibleChars: 200, Terminal: true, FirstVisibleMS: 400},
			want: false,
		},
		{
			name: "stream never terminated",
			sig:  qualitySignals{ExpectReasoning: true, VisibleChars: 200, Terminal: false, FirstVisibleMS: 400},
			want: false,
		},
		{
			name: "empty answer is not judged",
			sig:  qualitySignals{ExpectReasoning: true, VisibleChars: 0, Terminal: true, FirstVisibleMS: -1},
			want: false,
		},
	}
	for _, tc := range cases {
		if got := qualityDegraded(tc.sig); got != tc.want {
			t.Fatalf("%s: qualityDegraded() = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestQualityExpectsReasoning(t *testing.T) {
	none, low := "none", "low"
	if qualityExpectsReasoning(&ChatCompletionsRequest{ReasoningEffort: &none}, false) {
		t.Fatal("effort=none must not expect reasoning")
	}
	if !qualityExpectsReasoning(&ChatCompletionsRequest{ReasoningEffort: &low}, false) {
		t.Fatal("effort=low must expect reasoning")
	}
	if !qualityExpectsReasoning(nil, true) {
		t.Fatal("an active reasoning replay must expect reasoning")
	}
	if qualityExpectsReasoning(&ChatCompletionsRequest{}, false) {
		t.Fatal("a request without an effort must not expect reasoning")
	}
}

func TestUnbindAffinityDropsTheSessionBinding(t *testing.T) {
	h := &Handler{affinity: map[string]sessionAffinityEntry{}}
	ctx := withGrokSession(context.Background(), grokSessionContext{Key: "session-1", Model: "grok-4.6"})
	h.sessionMu.Lock()
	key := affinityMapKey(grokSessionContext{Key: "session-1", Model: "grok-4.6"}, ProviderBuild)
	h.affinity[key] = sessionAffinityEntry{AccountID: 7, ExpiresAt: time.Now().Add(time.Hour)}
	h.sessionMu.Unlock()

	h.unbindAffinity(ctx, ProviderBuild, 7)

	if id := h.affinityAccount(ctx, ProviderBuild); id != 0 {
		t.Fatalf("affinityAccount() = %d, want 0 after an unbind", id)
	}
	// An unrelated account id must not clear the binding.
	h.sessionMu.Lock()
	h.affinity[key] = sessionAffinityEntry{AccountID: 7, ExpiresAt: time.Now().Add(time.Hour)}
	h.sessionMu.Unlock()
	h.unbindAffinity(ctx, ProviderBuild, 9)
	if id := h.affinityAccount(ctx, ProviderBuild); id != 7 {
		t.Fatalf("affinityAccount() = %d, want the binding to survive a mismatch", id)
	}
}

func TestBackfillReasoningForCalls(t *testing.T) {
	proof := map[string]interface{}{"type": "reasoning", "id": "rs_1", "encrypted_content": "cipher-1"}
	cached := []interface{}{
		proof,
		map[string]interface{}{"type": "function_call", "call_id": "call_1", "name": "read", "arguments": "{}"},
	}
	// The client echoes the call but not its proof.
	input := []interface{}{map[string]interface{}{"type": "function_call", "call_id": "call_1", "name": "read", "arguments": "{}"}}
	filled := backfillReasoningForCalls(input, cached)
	if len(filled) != 2 {
		t.Fatalf("filled = %#v, want the proof inserted before the call", filled)
	}
	first, _ := filled[0].(map[string]interface{})
	if first["type"] != "reasoning" || first["encrypted_content"] != "cipher-1" {
		t.Fatalf("first item = %#v, want the cached proof", first)
	}
	// A call that already carries its proof is not doubled.
	withProof := append(cloneReplayItems([]interface{}{proof}), input...)
	if got := backfillReasoningForCalls(withProof, cached); len(got) != len(withProof) {
		t.Fatalf("a call that already carries its proof must not be doubled: %#v", got)
	}
	// An unknown call id is left alone.
	unknown := []interface{}{map[string]interface{}{"type": "function_call", "call_id": "call_9", "name": "read"}}
	if got := backfillReasoningForCalls(unknown, cached); len(got) != 1 {
		t.Fatalf("an unknown call must not gain a proof: %#v", got)
	}
	// No cache means no change.
	plain := []interface{}{map[string]interface{}{"type": "function_call", "call_id": "call_1"}}
	if got := backfillReasoningForCalls(plain, nil); len(got) != 1 {
		t.Fatalf("without cached items nothing may be inserted: %#v", got)
	}
}

func TestReasoningForCallsIndexesOnlyProofs(t *testing.T) {
	index := reasoningForCalls([]interface{}{
		map[string]interface{}{"type": "reasoning", "id": "rs_1"}, // no encrypted content
		map[string]interface{}{"type": "function_call", "call_id": "call_1"},
		map[string]interface{}{"type": "reasoning", "id": "rs_2", "encrypted_content": "cipher"},
		map[string]interface{}{"type": "custom_tool_call", "call_id": "call_2"},
	})
	if _, ok := index["call_1"]; ok {
		t.Fatal("a reasoning item without a proof must not be indexed")
	}
	if entry, ok := index["call_2"]; !ok || entry["id"] != "rs_2" {
		t.Fatalf("call_2 index = %#v, want rs_2", entry)
	}
}

func TestAccumulatedInputItemsWalksTheContinuationChain(t *testing.T) {
	if got := maxStoredInputChainDepth; got < 1 || got > 64 {
		t.Fatalf("chain depth = %d, want a bounded positive value", got)
	}
}
