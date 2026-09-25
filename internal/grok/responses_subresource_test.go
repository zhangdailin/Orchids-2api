package grok

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/goccy/go-json"

	"orchids-api/internal/store"
)

// brokenResponsesStore stands in for a response backend that is configured but
// unreachable, which is a different answer from "no such record".
type brokenResponsesStore struct{}

func (brokenResponsesStore) SaveStoredResponse(context.Context, *store.StoredResponse, time.Duration) error {
	return errors.New("store unreachable")
}

func (brokenResponsesStore) GetStoredResponse(context.Context, string, string) (*store.StoredResponse, error) {
	return nil, errors.New("store unreachable")
}

func (brokenResponsesStore) DeleteStoredResponse(context.Context, string, string) error {
	return errors.New("store unreachable")
}

func TestParseResponsesResourcePath(t *testing.T) {
	t.Parallel()

	cases := []struct {
		path       string
		wantID     string
		wantAction string
		wantOK     bool
	}{
		{"/v1/responses/resp_1", "resp_1", "", true},
		{"/v1/responses/resp_1/", "resp_1", "", true},
		{"/cline/v1/responses/resp_1/cancel", "resp_1", "cancel", true},
		{"/workbuddy/v1/responses/resp_1/input_items", "resp_1", "input_items", true},
		{"/v1/responses/resp%5F1/cancel", "resp_1", "cancel", true},
		{"/v1/responses/compact", "", "", false},
		{"/v1/responses/", "", "", false},
		{"/v1/responses", "", "", false},
		{"/something/else", "", "", false},
	}
	for _, tc := range cases {
		id, action, ok := parseResponsesResourcePath(tc.path)
		if ok != tc.wantOK || id != tc.wantID || action != tc.wantAction {
			t.Fatalf("parseResponsesResourcePath(%q) = (%q, %q, %v), want (%q, %q, %v)",
				tc.path, id, action, ok, tc.wantID, tc.wantAction, tc.wantOK)
		}
		wantAction := ""
		if tc.wantAction == responsesActionCancel || tc.wantAction == responsesActionInputItems {
			wantAction = tc.wantAction
		}
		if got := responsesSubResourceAction(tc.path); got != wantAction {
			t.Fatalf("responsesSubResourceAction(%q) = %q, want %q", tc.path, got, wantAction)
		}
	}
}

// TestResponsesInputItemsJSONNormalizesEveryInputShape pins the round trip a
// client performs: it reads the list and sends it back as the next turn's input.
// An item without an id or a status would be rejected on the way back, so the
// normalizer must supply both while leaving the client's own labels alone.
func TestResponsesInputItemsJSONNormalizesEveryInputShape(t *testing.T) {
	t.Parallel()

	var fromString []map[string]interface{}
	if err := json.Unmarshal(responsesInputItemsJSON("hello"), &fromString); err != nil {
		t.Fatalf("string input did not normalize: %v", err)
	}
	if len(fromString) != 1 {
		t.Fatalf("string input = %#v, want one message item", fromString)
	}
	if fromString[0]["type"] != "message" || fromString[0]["role"] != "user" {
		t.Fatalf("string input item = %#v", fromString[0])
	}
	if !strings.HasPrefix(interfaceString(fromString[0]["id"]), "msg_") {
		t.Fatalf("string input item id = %#v, want a msg_ id", fromString[0]["id"])
	}
	if fromString[0]["status"] != "completed" {
		t.Fatalf("string input item status = %#v, want completed", fromString[0]["status"])
	}

	var fromArray []map[string]interface{}
	raw := []interface{}{
		map[string]interface{}{"type": "message", "role": "user", "content": []interface{}{
			map[string]interface{}{"type": "input_text", "text": "first"},
		}},
		map[string]interface{}{"id": "fc_known", "type": "function_call", "call_id": "call_1", "name": "f"},
	}
	if err := json.Unmarshal(responsesInputItemsJSON(raw), &fromArray); err != nil {
		t.Fatalf("array input did not normalize: %v", err)
	}
	if len(fromArray) != 2 {
		t.Fatalf("array input = %#v, want two items", fromArray)
	}
	if fromArray[1]["id"] != "fc_known" {
		t.Fatalf("client id was rewritten: %#v", fromArray[1]["id"])
	}
	for i, item := range fromArray {
		if interfaceString(item["id"]) == "" || interfaceString(item["status"]) == "" {
			t.Fatalf("item %d is missing id/status: %#v", i, item)
		}
	}

	if got := responsesInputItemsJSON(nil); got != nil {
		t.Fatalf("nil input produced %s, want no stored items", got)
	}
}

func TestResponsesInputItemsServesPersistedItems(t *testing.T) {
	t.Parallel()

	opts := ResponsesBridgeOptions{Store: store.NewMemoryResponseStore(0)}
	id := "resp_items_" + randomHex(8)
	if err := opts.store().SaveStoredResponse(nil, &store.StoredResponse{ //nolint:staticcheck // nil ctx is fine for the in-process store
		ResponseID: id,
		OwnerHash:  "anonymous",
		Model:      "gpt-5.6-luna",
		Provider:   bridgedResponseProvider,
		Body:       []byte(`{"id":"` + id + `","object":"response","status":"completed"}`),
		InputItems: responsesInputItemsJSON("first turn"),
	}, 0); err != nil {
		t.Fatalf("SaveStoredResponse() error = %v", err)
	}

	handler := ResponsesInputItemsHandler(opts)
	rec := httptest.NewRecorder()
	handler(rec, httptest.NewRequest(http.MethodGet, "/workbuddy/v1/responses/"+id+"/input_items", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
	var payload struct {
		Object  string                   `json:"object"`
		Data    []map[string]interface{} `json:"data"`
		FirstID string                   `json:"first_id"`
		LastID  string                   `json:"last_id"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode input_items: %v (%s)", err, rec.Body.String())
	}
	if payload.Object != "list" || len(payload.Data) != 1 {
		t.Fatalf("payload = %#v, want a one-item list", payload)
	}
	content, _ := payload.Data[0]["content"].([]interface{})
	if len(content) != 1 {
		t.Fatalf("item content = %#v", payload.Data[0]["content"])
	}
	part, _ := content[0].(map[string]interface{})
	if part["text"] != "first turn" {
		t.Fatalf("item text = %#v, want the input the response was created from", part["text"])
	}
	if payload.FirstID == "" || payload.FirstID != payload.LastID {
		t.Fatalf("first_id/last_id = %q/%q, want the single item id", payload.FirstID, payload.LastID)
	}
}

func TestResponsesInputItemsUnknownResponseIs404(t *testing.T) {
	t.Parallel()

	handler := ResponsesInputItemsHandler(ResponsesBridgeOptions{Store: store.NewMemoryResponseStore(0)})
	rec := httptest.NewRecorder()
	handler(rec, httptest.NewRequest(http.MethodGet, "/v1/responses/resp_missing/input_items", nil))

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 (body=%s)", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "response_not_found") {
		t.Fatalf("body = %s, want the response_not_found envelope", rec.Body.String())
	}
}

func TestResponsesSubResourcesRejectWrongMethod(t *testing.T) {
	t.Parallel()

	opts := ResponsesBridgeOptions{Store: store.NewMemoryResponseStore(0)}
	cancel := ResponsesCancelHandler(opts)
	rec := httptest.NewRecorder()
	cancel(rec, httptest.NewRequest(http.MethodGet, "/v1/responses/resp_1/cancel", nil))
	if rec.Code != http.StatusMethodNotAllowed || !strings.Contains(rec.Header().Get("Allow"), "POST") {
		t.Fatalf("cancel GET: status=%d Allow=%q", rec.Code, rec.Header().Get("Allow"))
	}
	if !strings.Contains(rec.Body.String(), `"error"`) {
		t.Fatalf("cancel GET body = %s, want the Responses error envelope", rec.Body.String())
	}

	items := ResponsesInputItemsHandler(opts)
	rec = httptest.NewRecorder()
	items(rec, httptest.NewRequest(http.MethodPost, "/v1/responses/resp_1/input_items", nil))
	if rec.Code != http.StatusMethodNotAllowed || !strings.Contains(rec.Header().Get("Allow"), "GET") {
		t.Fatalf("input_items POST: status=%d Allow=%q", rec.Code, rec.Header().Get("Allow"))
	}
}

// TestResponsesCancelFlipsStoredStatus covers the whole observable contract:
// cancel answers with the response object carrying status=cancelled, the change
// is persisted, and a second cancel is a no-op that returns the same object.
func TestResponsesCancelFlipsStoredStatus(t *testing.T) {
	t.Parallel()

	opts := ResponsesBridgeOptions{Store: store.NewMemoryResponseStore(0)}
	id := "resp_cancel_" + randomHex(8)
	body := `{"id":"` + id + `","object":"response","status":"completed","output":[` +
		`{"id":"msg_1","type":"message","status":"completed","role":"assistant","content":[{"type":"output_text","text":"hi"}]}]}`
	if err := opts.store().SaveStoredResponse(nil, &store.StoredResponse{ //nolint:staticcheck // nil ctx is fine for the in-process store
		ResponseID: id, OwnerHash: "anonymous", Model: "gpt-5.6-luna",
		Provider: bridgedResponseProvider, ContentType: "application/json", Body: []byte(body),
	}, 0); err != nil {
		t.Fatalf("SaveStoredResponse() error = %v", err)
	}

	cancel := ResponsesCancelHandler(opts)
	first := httptest.NewRecorder()
	cancel(first, httptest.NewRequest(http.MethodPost, "/workbuddy/v1/responses/"+id+"/cancel", nil))
	if first.Code != http.StatusOK {
		t.Fatalf("cancel status = %d, body=%s", first.Code, first.Body.String())
	}
	var cancelled map[string]interface{}
	if err := json.Unmarshal(first.Body.Bytes(), &cancelled); err != nil {
		t.Fatalf("decode cancel body: %v (%s)", err, first.Body.String())
	}
	if cancelled["status"] != "cancelled" || cancelled["id"] != id {
		t.Fatalf("cancel body = %#v", cancelled)
	}
	output, _ := cancelled["output"].([]interface{})
	if len(output) != 1 {
		t.Fatalf("cancel body output = %#v", cancelled["output"])
	}

	record, err := opts.store().GetStoredResponse(nil, id, "anonymous") //nolint:staticcheck // see above
	if err != nil {
		t.Fatalf("GetStoredResponse() error = %v", err)
	}
	if !strings.Contains(string(record.Body), `"status":"cancelled"`) {
		t.Fatalf("cancel was not persisted: %s", record.Body)
	}

	second := httptest.NewRecorder()
	cancel(second, httptest.NewRequest(http.MethodPost, "/workbuddy/v1/responses/"+id+"/cancel", nil))
	if second.Code != http.StatusOK {
		t.Fatalf("second cancel status = %d, body=%s", second.Code, second.Body.String())
	}
	if second.Body.String() != first.Body.String() {
		t.Fatalf("cancel is not idempotent:\nfirst=%s\nsecond=%s", first.Body.String(), second.Body.String())
	}
}

func TestResponsesCancelUnknownResponseIs404(t *testing.T) {
	t.Parallel()

	cancel := ResponsesCancelHandler(ResponsesBridgeOptions{Store: store.NewMemoryResponseStore(0)})
	rec := httptest.NewRecorder()
	cancel(rec, httptest.NewRequest(http.MethodPost, "/v1/responses/resp_missing/cancel", nil))
	if rec.Code != http.StatusNotFound || !strings.Contains(rec.Body.String(), "response_not_found") {
		t.Fatalf("status = %d body = %s", rec.Code, rec.Body.String())
	}
}

// TestResponsesCancelAnswersBuildOwnershipRecords guards the build record case:
// ownership exists but the body lives upstream, so the gateway answers with the
// identity it knows rather than reporting the response as missing.
func TestResponsesCancelAnswersBuildOwnershipRecords(t *testing.T) {
	t.Parallel()

	opts := ResponsesBridgeOptions{Store: store.NewMemoryResponseStore(0)}
	id := "resp_build_" + randomHex(8)
	if err := opts.store().SaveStoredResponse(nil, &store.StoredResponse{ //nolint:staticcheck // nil ctx is fine for the in-process store
		ResponseID: id, OwnerHash: "anonymous", Model: "grok-4.6", Provider: ProviderBuild,
	}, 0); err != nil {
		t.Fatalf("SaveStoredResponse() error = %v", err)
	}

	rec := httptest.NewRecorder()
	ResponsesCancelHandler(opts)(rec, httptest.NewRequest(http.MethodPost, "/grok/v1/responses/"+id+"/cancel", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var decoded map[string]interface{}
	if err := json.Unmarshal(rec.Body.Bytes(), &decoded); err != nil {
		t.Fatalf("decode: %v (%s)", err, rec.Body.String())
	}
	if decoded["status"] != "cancelled" || decoded["id"] != id || decoded["model"] != "grok-4.6" {
		t.Fatalf("body = %#v", decoded)
	}
}

// TestResponsesBridgeForwardsTextFormatAndInclude is gap ②/③: the bridge used
// to drop these while the native Grok path forwarded them, so the same request
// produced structured output on one channel and free-form prose on the others.
func TestResponsesBridgeForwardsTextFormatAndInclude(t *testing.T) {
	t.Parallel()

	var mu sync.Mutex
	calls := []recordedChatCall{}
	bridge := ResponsesBridgeHandler(recordingChat(t, &calls, &mu), ResponsesBridgeOptions{})

	body := `{"model":"gpt-5.6-luna","input":"hi","stream":true,` +
		`"include":["reasoning.encrypted_content"],` +
		`"text":{"format":{"type":"json_schema","name":"answer","schema":{"type":"object"}}}}`
	req := httptest.NewRequest(http.MethodPost, "/qoder/v1/responses", strings.NewReader(body))
	rec := httptest.NewRecorder()
	bridge(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}

	mu.Lock()
	defer mu.Unlock()
	if len(calls) != 1 {
		t.Fatalf("inner chat calls = %d, want 1", len(calls))
	}
	inner := calls[0].body
	format, _ := inner["response_format"].(map[string]interface{})
	if format == nil || format["type"] != "json_schema" || format["name"] != "answer" {
		t.Fatalf("response_format = %#v, want the text.format object", inner["response_format"])
	}
	include, _ := inner["include"].([]interface{})
	if len(include) != 1 || include[0] != "reasoning.encrypted_content" {
		t.Fatalf("include = %#v", inner["include"])
	}
	text, _ := inner["text"].(map[string]interface{})
	if text == nil {
		t.Fatalf("text controls were dropped: %#v", inner)
	}
}

// TestResponsesBridgePrefersTextFormatOverResponseFormat pins precedence: when a
// migrated client sends both spellings, the field the Responses API defines wins.
func TestResponsesBridgePrefersTextFormatOverResponseFormat(t *testing.T) {
	t.Parallel()

	var mu sync.Mutex
	calls := []recordedChatCall{}
	bridge := ResponsesBridgeHandler(recordingChat(t, &calls, &mu), ResponsesBridgeOptions{})

	body := `{"model":"gpt-5.6-luna","input":"hi","stream":true,` +
		`"response_format":{"type":"text"},` +
		`"text":{"format":{"type":"json_object"}}}`
	rec := httptest.NewRecorder()
	bridge(rec, httptest.NewRequest(http.MethodPost, "/workbuddy/v1/responses", strings.NewReader(body)))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}

	mu.Lock()
	defer mu.Unlock()
	format, _ := calls[0].body["response_format"].(map[string]interface{})
	if format == nil || format["type"] != "json_object" {
		t.Fatalf("response_format = %#v, want text.format to win", calls[0].body["response_format"])
	}
}

// TestResponsesBridgeMemoryFallbackStoresResponses is gap ⑤: a gateway with no
// response backend must keep store=true, retrieval and continuation working
// in-process instead of answering response_store_unavailable.
func TestResponsesBridgeMemoryFallbackStoresResponses(t *testing.T) {
	t.Parallel()

	opts := ResponsesBridgeOptions{}
	chat := func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"id":"chatcmpl-1","object":"chat.completion","created":1,"model":"gpt-5.6-luna","choices":[{"index":0,"message":{"role":"assistant","content":"fallback-answer"},"finish_reason":"stop"}]}`)
	}
	bridge := ResponsesBridgeHandler(chat, opts)
	resource := ResponsesResourceHandler(opts)

	create := httptest.NewRecorder()
	bridge(create, httptest.NewRequest(http.MethodPost, "/workbuddy/v1/responses",
		strings.NewReader(`{"model":"gpt-5.6-luna","input":"hi","store":true}`)))
	if create.Code != http.StatusOK {
		t.Fatalf("create status=%d body=%s", create.Code, create.Body.String())
	}
	var created map[string]interface{}
	if err := json.Unmarshal(create.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode: %v (%s)", err, create.Body.String())
	}
	responseID, _ := created["id"].(string)
	if responseID == "" {
		t.Fatalf("no response id in %s", create.Body.String())
	}

	get := httptest.NewRecorder()
	resource(get, httptest.NewRequest(http.MethodGet, "/workbuddy/v1/responses/"+responseID, nil))
	if get.Code != http.StatusOK || !strings.Contains(get.Body.String(), "fallback-answer") {
		t.Fatalf("get status=%d body=%s", get.Code, get.Body.String())
	}

	// The in-process store must serve the items too, not just the body.
	items := httptest.NewRecorder()
	ResponsesInputItemsHandler(opts)(items, httptest.NewRequest(http.MethodGet, "/workbuddy/v1/responses/"+responseID+"/input_items", nil))
	if items.Code != http.StatusOK || !strings.Contains(items.Body.String(), "hi") {
		t.Fatalf("input_items status=%d body=%s", items.Code, items.Body.String())
	}
}

// TestResponsesSubResourcesRoundTripThroughRedis drives create -> input_items ->
// cancel -> get against the real Redis-backed store, so the new stored field is
// proven to survive the JSON path rather than only the in-process store. A
// sibling replica serving the next call reads the record through the same path.
func TestResponsesSubResourcesRoundTripThroughRedis(t *testing.T) {
	_, s, mini := setupValidationHandler(t)
	defer func() { _ = s.Close(); mini.Close() }()

	opts := ResponsesBridgeOptions{Store: s}
	chat := func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"id":"chatcmpl-2","object":"chat.completion","created":1,"model":"gpt-5.6-luna","choices":[{"index":0,"message":{"role":"assistant","content":"round-trip"},"finish_reason":"stop"}]}`)
	}

	create := httptest.NewRecorder()
	ResponsesBridgeHandler(chat, opts)(create, httptest.NewRequest(http.MethodPost, "/workbuddy/v1/responses",
		strings.NewReader(`{"model":"gpt-5.6-luna","input":"remember this turn","store":true}`)))
	if create.Code != http.StatusOK {
		t.Fatalf("create status=%d body=%s", create.Code, create.Body.String())
	}
	var created map[string]interface{}
	if err := json.Unmarshal(create.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode create: %v (%s)", err, create.Body.String())
	}
	responseID, _ := created["id"].(string)
	if responseID == "" {
		t.Fatalf("create carried no id: %s", create.Body.String())
	}

	items := httptest.NewRecorder()
	ResponsesInputItemsHandler(opts)(items, httptest.NewRequest(http.MethodGet, "/workbuddy/v1/responses/"+responseID+"/input_items", nil))
	if items.Code != http.StatusOK || !strings.Contains(items.Body.String(), "remember this turn") {
		t.Fatalf("input_items status=%d body=%s", items.Code, items.Body.String())
	}

	cancelled := httptest.NewRecorder()
	ResponsesCancelHandler(opts)(cancelled, httptest.NewRequest(http.MethodPost, "/workbuddy/v1/responses/"+responseID+"/cancel", nil))
	if cancelled.Code != http.StatusOK || !strings.Contains(cancelled.Body.String(), `"status":"cancelled"`) {
		t.Fatalf("cancel status=%d body=%s", cancelled.Code, cancelled.Body.String())
	}

	// The cancellation must be visible through a fresh read of the same store.
	get := httptest.NewRecorder()
	ResponsesResourceHandler(opts)(get, httptest.NewRequest(http.MethodGet, "/workbuddy/v1/responses/"+responseID, nil))
	if get.Code != http.StatusOK || !strings.Contains(get.Body.String(), `"status":"cancelled"`) {
		t.Fatalf("get after cancel status=%d body=%s", get.Code, get.Body.String())
	}
	// input_items must still be readable after the status rewrite.
	itemsAgain := httptest.NewRecorder()
	ResponsesInputItemsHandler(opts)(itemsAgain, httptest.NewRequest(http.MethodGet, "/workbuddy/v1/responses/"+responseID+"/input_items", nil))
	if itemsAgain.Code != http.StatusOK || !strings.Contains(itemsAgain.Body.String(), "remember this turn") {
		t.Fatalf("input_items after cancel status=%d body=%s", itemsAgain.Code, itemsAgain.Body.String())
	}
}

// TestResponsesUnifiedResourceHandsTheRecordToItsOwner is gap ①: GET/DELETE on
// the unified prefix must follow the stored record rather than always landing on
// Grok's handler, which used to accept records it did not write.
func TestResponsesUnifiedResourceHandsTheRecordToItsOwner(t *testing.T) {
	t.Parallel()

	// newStore seeds one record whose body is built from the id the helper
	// generates, so the caller never has to guess it.
	newStore := func(provider string, body func(id string) string) (ResponsesStore, string) {
		st := store.NewMemoryResponseStore(0)
		id := "resp_route_" + randomHex(8)
		payload := ""
		if body != nil {
			payload = body(id)
		}
		if err := st.SaveStoredResponse(nil, &store.StoredResponse{ //nolint:staticcheck // nil ctx is fine for the in-process store
			ResponseID: id, OwnerHash: "anonymous", Model: "gpt-5.6-luna",
			Provider: provider, ContentType: "application/json", Body: []byte(payload),
		}, 0); err != nil {
			t.Fatalf("SaveStoredResponse() error = %v", err)
		}
		return st, id
	}

	t.Run("bridged_record_goes_to_the_bridge", func(t *testing.T) {
		st, id := newStore(bridgedResponseProvider, func(id string) string {
			return `{"id":"` + id + `","object":"response","status":"completed","from":"bridge"}`
		})
		nativeCalls := 0
		handler := ResponsesUnifiedResource(func(w http.ResponseWriter, r *http.Request) { nativeCalls++ }, ResponsesBridgeOptions{Store: st})

		rec := httptest.NewRecorder()
		handler(rec, httptest.NewRequest(http.MethodGet, "/v1/responses/"+id, nil))
		if nativeCalls != 0 {
			t.Fatalf("the native handler served a bridged record (%d calls)", nativeCalls)
		}
		if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "bridge") {
			t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
		}
	})

	t.Run("build_record_stays_with_the_native_handler", func(t *testing.T) {
		st, id := newStore(ProviderBuild, nil)
		nativeCalls := 0
		handler := ResponsesUnifiedResource(func(w http.ResponseWriter, r *http.Request) {
			nativeCalls++
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, "native")
		}, ResponsesBridgeOptions{Store: st})

		rec := httptest.NewRecorder()
		handler(rec, httptest.NewRequest(http.MethodGet, "/v1/responses/"+id, nil))
		if nativeCalls != 1 {
			t.Fatalf("native handler calls = %d, want 1", nativeCalls)
		}
		if !strings.Contains(rec.Body.String(), "native") {
			t.Fatalf("body = %s", rec.Body.String())
		}
	})

	// An id that was never stored is answered by the dispatcher itself: the
	// store both handlers read is the same one, so "no such record" must not
	// become a backend failure just because the native handler is unconfigured.
	t.Run("unknown_record_is_answered_without_the_native_handler", func(t *testing.T) {
		nativeCalls := 0
		handler := ResponsesUnifiedResource(func(w http.ResponseWriter, r *http.Request) {
			nativeCalls++
			w.WriteHeader(http.StatusServiceUnavailable)
		}, ResponsesBridgeOptions{Store: store.NewMemoryResponseStore(0)})

		rec := httptest.NewRecorder()
		handler(rec, httptest.NewRequest(http.MethodGet, "/v1/responses/resp_absent", nil))
		if nativeCalls != 0 {
			t.Fatalf("native handler calls = %d, want 0: a miss needs no provider", nativeCalls)
		}
		if rec.Code != http.StatusNotFound || !strings.Contains(rec.Body.String(), "response_not_found") {
			t.Fatalf("status = %d body = %s", rec.Code, rec.Body.String())
		}
	})

	// A store that cannot be read is a different answer: the native handler owns
	// the response_store_unavailable envelope, so it must still be consulted.
	t.Run("unreadable_store_stays_with_the_native_handler", func(t *testing.T) {
		nativeCalls := 0
		handler := ResponsesUnifiedResource(func(w http.ResponseWriter, r *http.Request) {
			nativeCalls++
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = io.WriteString(w, "store unavailable")
		}, ResponsesBridgeOptions{Store: brokenResponsesStore{}})

		rec := httptest.NewRecorder()
		handler(rec, httptest.NewRequest(http.MethodGet, "/v1/responses/resp_any", nil))
		if nativeCalls != 1 {
			t.Fatalf("native handler calls = %d, want 1", nativeCalls)
		}
		if !strings.Contains(rec.Body.String(), "store unavailable") {
			t.Fatalf("body = %s", rec.Body.String())
		}
	})

	t.Run("sibling_actions_bypass_the_provider_decision", func(t *testing.T) {
		st, id := newStore(bridgedResponseProvider, func(id string) string {
			return `{"id":"` + id + `","object":"response","status":"completed"}`
		})
		nativeCalls := 0
		handler := ResponsesUnifiedResource(func(w http.ResponseWriter, r *http.Request) { nativeCalls++ }, ResponsesBridgeOptions{Store: st})

		rec := httptest.NewRecorder()
		handler(rec, httptest.NewRequest(http.MethodGet, "/v1/responses/"+id+"/input_items", nil))
		if nativeCalls != 0 {
			t.Fatalf("the provider decision ran for input_items (%d native calls)", nativeCalls)
		}
		if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"object":"list"`) {
			t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
		}
	})
}
