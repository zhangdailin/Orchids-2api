package grok

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"sync"
	"testing"

	"github.com/goccy/go-json"
)

func TestResponsesChatPathMapsTheChannelPrefix(t *testing.T) {
	t.Parallel()

	cases := map[string]string{
		"/workbuddy/v1/responses":         "/workbuddy/v1/chat/completions",
		"/workbuddy/v1/responses/compact": "/workbuddy/v1/chat/completions",
		"/warp/v1/responses/":             "/warp/v1/chat/completions",
		"/v1/responses":                   "/v1/chat/completions",
		"  /qoder/v1/responses  ":         "/qoder/v1/chat/completions",
		"/something/else":                 "/v1/chat/completions",
	}
	for path, want := range cases {
		if got := responsesChatPath(path); got != want {
			t.Fatalf("responsesChatPath(%q) = %q, want %q", path, got, want)
		}
	}
}

type recordedChatCall struct {
	path string
	body map[string]interface{}
}

// recordingChat captures the inner chat request and replies with a complete
// chat-completions SSE stream, which is what the shared handler emits.
func recordingChat(t *testing.T, calls *[]recordedChatCall, mu *sync.Mutex) http.HandlerFunc {
	t.Helper()
	return func(w http.ResponseWriter, r *http.Request) {
		raw, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("inner chat body: %v", err)
		}
		var decoded map[string]interface{}
		_ = json.Unmarshal(raw, &decoded)
		mu.Lock()
		*calls = append(*calls, recordedChatCall{path: r.URL.Path, body: decoded})
		mu.Unlock()

		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		for _, frame := range []string{
			`data: {"id":"chatcmpl-1","object":"chat.completion.chunk","created":1,"model":"gpt-5.6-luna","choices":[{"index":0,"delta":{"role":"assistant"}}]}`,
			`data: {"id":"chatcmpl-1","object":"chat.completion.chunk","created":1,"model":"gpt-5.6-luna","choices":[{"index":0,"delta":{"content":"hello"}}]}`,
			`data: {"id":"chatcmpl-1","object":"chat.completion.chunk","created":1,"model":"gpt-5.6-luna","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`,
			`data: [DONE]`,
		} {
			_, _ = io.WriteString(w, frame+"\n\n")
		}
	}
}

func TestResponsesBridgeStreamsChatAsResponses(t *testing.T) {
	t.Parallel()

	var mu sync.Mutex
	calls := []recordedChatCall{}
	bridge := ResponsesBridgeHandler(recordingChat(t, &calls, &mu), ResponsesBridgeOptions{})

	req := httptest.NewRequest(http.MethodPost, "/workbuddy/v1/responses",
		strings.NewReader(`{"model":"gpt-5.6-luna","instructions":"be brief","input":"say hi","stream":true}`))
	rec := httptest.NewRecorder()
	bridge(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	out := rec.Body.String()
	for _, want := range []string{"event: response.created", "response.output_text.delta", `"hello"`, "event: response.completed"} {
		if !strings.Contains(out, want) {
			t.Fatalf("stream is missing %q: %s", want, out)
		}
	}

	mu.Lock()
	defer mu.Unlock()
	if len(calls) != 1 {
		t.Fatalf("inner chat calls = %d, want 1", len(calls))
	}
	if calls[0].path != "/workbuddy/v1/chat/completions" {
		t.Fatalf("inner chat path = %q, want the same channel prefix", calls[0].path)
	}
	messages, _ := calls[0].body["messages"].([]interface{})
	if len(messages) != 2 {
		t.Fatalf("inner messages = %#v, want instructions plus the input turn", calls[0].body["messages"])
	}
	first, _ := messages[0].(map[string]interface{})
	if first["role"] != "system" || first["content"] != "be brief" {
		t.Fatalf("first inner message = %#v, want the instructions as a system turn", first)
	}
	if stream, _ := calls[0].body["stream"].(bool); !stream {
		t.Fatalf("inner stream = %#v, want true", calls[0].body["stream"])
	}
}

func TestResponsesBridgeNonStreamReturnsAResponseObject(t *testing.T) {
	t.Parallel()

	chat := func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"id":"chatcmpl-2","object":"chat.completion","created":1,"model":"gpt-5.6-luna","choices":[{"index":0,"message":{"role":"assistant","content":"hello"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`)
	}
	bridge := ResponsesBridgeHandler(chat, ResponsesBridgeOptions{})

	req := httptest.NewRequest(http.MethodPost, "/puter/v1/responses",
		strings.NewReader(`{"model":"gpt-5.6-luna","input":"say hi"}`))
	rec := httptest.NewRecorder()
	bridge(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var decoded map[string]interface{}
	if err := json.Unmarshal(rec.Body.Bytes(), &decoded); err != nil {
		t.Fatalf("decode response object: %v (%s)", err, rec.Body.String())
	}
	if decoded["object"] != "response" || decoded["model"] != "gpt-5.6-luna" {
		t.Fatalf("response object = %#v", decoded)
	}
	if !strings.Contains(rec.Body.String(), "hello") {
		t.Fatalf("response is missing the assistant text: %s", rec.Body.String())
	}
}

func TestResponsesBridgeForwardsChatErrors(t *testing.T) {
	t.Parallel()

	chat := func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, `{"error":{"message":"model not found","type":"invalid_request_error"}}`)
	}
	bridge := ResponsesBridgeHandler(chat, ResponsesBridgeOptions{})

	req := httptest.NewRequest(http.MethodPost, "/warp/v1/responses",
		strings.NewReader(`{"model":"does-not-exist","input":"hi","stream":true}`))
	rec := httptest.NewRecorder()
	bridge(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want the inner 400", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "model not found") {
		t.Fatalf("error body was not forwarded: %s", rec.Body.String())
	}
}

func TestResponsesBridgeRejectsInvalidRequests(t *testing.T) {
	t.Parallel()

	bridge := ResponsesBridgeHandler(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("inner chat handler must not run for an invalid request")
	}, ResponsesBridgeOptions{})

	for name, body := range map[string]string{
		"missing_model": `{"input":"hi"}`,
		"missing_input": `{"model":"gpt-5.6-luna"}`,
		"broken_json":   `{"model":`,
		"background":    `{"model":"gpt-5.6-luna","input":"hi","background":true}`,
	} {
		t.Run(name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/workbuddy/v1/responses", strings.NewReader(body))
			rec := httptest.NewRecorder()
			bridge(rec, req)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("%s: status = %d, want 400 (body=%s)", name, rec.Code, rec.Body.String())
			}
		})
	}
}

func TestResponsesBridgeRejectsNonPost(t *testing.T) {
	t.Parallel()

	bridge := ResponsesBridgeHandler(func(w http.ResponseWriter, r *http.Request) {}, ResponsesBridgeOptions{})
	rec := httptest.NewRecorder()
	bridge(rec, httptest.NewRequest(http.MethodGet, "/workbuddy/v1/responses", nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405", rec.Code)
	}
}

func TestResponsesChannelSubpathServesCompactAndTrailingSlash(t *testing.T) {
	t.Parallel()

	var mu sync.Mutex
	calls := []recordedChatCall{}
	handler := ResponsesChannelSubpath(recordingChat(t, &calls, &mu), ResponsesBridgeOptions{})

	for name, target := range map[string]string{
		"trailing_slash": "/warp/v1/responses/",
		"compact":        "/workbuddy/v1/responses/compact",
	} {
		t.Run(name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, target, strings.NewReader(`{"model":"gpt-5.6-luna","input":"summarise the thread","stream":true}`))
			rec := httptest.NewRecorder()
			handler(rec, req)
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200 (body=%s)", rec.Code, rec.Body.String())
			}
			if !strings.Contains(rec.Body.String(), "event: response.completed") {
				t.Fatalf("missing completion event: %s", rec.Body.String())
			}
		})
	}

	mu.Lock()
	defer mu.Unlock()
	if len(calls) != 2 {
		t.Fatalf("inner chat calls = %d, want 2", len(calls))
	}
	// The sub-tests run in map order, so compare the set of paths.
	paths := map[string]bool{}
	for _, call := range calls {
		paths[call.path] = true
	}
	if !paths["/warp/v1/chat/completions"] || !paths["/workbuddy/v1/chat/completions"] {
		t.Fatalf("inner chat paths = %v, want the same channel prefix as the request", paths)
	}
}

// The chat-only channels keep no response store, so /responses/{id} must answer
// with the Responses error envelope instead of Go's plain-text 404.
func TestResponsesChannelSubpathReportsUnstoredResponses(t *testing.T) {
	t.Parallel()

	handler := ResponsesChannelSubpath(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("the create handler must not serve a resource path")
	}, ResponsesBridgeOptions{})

	for _, method := range []string{http.MethodGet, http.MethodDelete} {
		req := httptest.NewRequest(method, "/warp/v1/responses/resp_123", nil)
		rec := httptest.NewRecorder()
		handler(rec, req)
		if rec.Code != http.StatusNotFound {
			t.Fatalf("%s status = %d, want 404", method, rec.Code)
		}
		if !strings.Contains(rec.Body.String(), "response_not_found") {
			t.Fatalf("%s body = %s, want a response_not_found error", method, rec.Body.String())
		}
	}

	put := httptest.NewRequest(http.MethodPut, "/warp/v1/responses/resp_123", nil)
	rec := httptest.NewRecorder()
	handler(rec, put)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("PUT status = %d, want 405", rec.Code)
	}
	if allow := rec.Header().Get("Allow"); !strings.Contains(allow, "GET") || !strings.Contains(allow, "DELETE") {
		t.Fatalf("Allow = %q, want GET and DELETE", allow)
	}
}

func TestResponsesBridgeStoresAndServesResponses(t *testing.T) {
	_, s, mini := setupValidationHandler(t)
	defer func() { _ = s.Close(); mini.Close() }()

	var mu sync.Mutex
	var chatBodies []map[string]interface{}
	chat := func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var decoded map[string]interface{}
		_ = json.Unmarshal(raw, &decoded)
		mu.Lock()
		chatBodies = append(chatBodies, decoded)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"id":"chatcmpl-9","object":"chat.completion","created":1,"model":"gpt-5.6-luna","choices":[{"index":0,"message":{"role":"assistant","content":"stored-answer"},"finish_reason":"stop"}]}`)
	}
	opts := ResponsesBridgeOptions{Store: s}
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
		t.Fatalf("decode created response: %v (%s)", err, create.Body.String())
	}
	responseID, _ := created["id"].(string)
	if !strings.HasPrefix(responseID, "resp_") {
		t.Fatalf("response id = %q, want a resp_ id", responseID)
	}

	get := httptest.NewRecorder()
	resource(get, httptest.NewRequest(http.MethodGet, "/workbuddy/v1/responses/"+responseID, nil))
	if get.Code != http.StatusOK || !strings.Contains(get.Body.String(), "stored-answer") {
		t.Fatalf("get status=%d body=%s", get.Code, get.Body.String())
	}

	// A continuation must replay the stored conversation upstream.
	continuation := httptest.NewRecorder()
	bridge(continuation, httptest.NewRequest(http.MethodPost, "/workbuddy/v1/responses",
		strings.NewReader(`{"model":"gpt-5.6-luna","input":"again","previous_response_id":"`+responseID+`"}`)))
	if continuation.Code != http.StatusOK {
		t.Fatalf("continuation status=%d body=%s", continuation.Code, continuation.Body.String())
	}
	mu.Lock()
	last := chatBodies[len(chatBodies)-1]
	mu.Unlock()
	if !strings.Contains(fmt.Sprint(last["messages"]), "stored-answer") {
		t.Fatalf("continuation did not replay the stored output: %#v", last["messages"])
	}

	deleted := httptest.NewRecorder()
	resource(deleted, httptest.NewRequest(http.MethodDelete, "/workbuddy/v1/responses/"+responseID, nil))
	if deleted.Code != http.StatusOK || !strings.Contains(deleted.Body.String(), `"deleted":true`) {
		t.Fatalf("delete status=%d body=%s", deleted.Code, deleted.Body.String())
	}
	gone := httptest.NewRecorder()
	resource(gone, httptest.NewRequest(http.MethodGet, "/workbuddy/v1/responses/"+responseID, nil))
	if gone.Code != http.StatusNotFound {
		t.Fatalf("after delete status=%d body=%s", gone.Code, gone.Body.String())
	}
}

func TestResponsesBridgeStoresStreamedResponse(t *testing.T) {
	_, s, mini := setupValidationHandler(t)
	defer func() { _ = s.Close(); mini.Close() }()

	var mu sync.Mutex
	calls := []recordedChatCall{}
	opts := ResponsesBridgeOptions{Store: s}
	bridge := ResponsesBridgeHandler(recordingChat(t, &calls, &mu), opts)
	resource := ResponsesResourceHandler(opts)

	stream := httptest.NewRecorder()
	bridge(stream, httptest.NewRequest(http.MethodPost, "/warp/v1/responses",
		strings.NewReader(`{"model":"gpt-5-6-sol-low","input":"hi","stream":true,"store":true}`)))
	if stream.Code != http.StatusOK || !strings.Contains(stream.Body.String(), "event: response.completed") {
		t.Fatalf("stream status=%d body=%s", stream.Code, stream.Body.String())
	}
	match := regexp.MustCompile(`"id":"(resp_[0-9a-f]+)"`).FindStringSubmatch(stream.Body.String())
	if len(match) < 2 {
		t.Fatalf("stream carries no response id: %s", stream.Body.String())
	}

	get := httptest.NewRecorder()
	resource(get, httptest.NewRequest(http.MethodGet, "/warp/v1/responses/"+match[1], nil))
	if get.Code != http.StatusOK || !strings.Contains(get.Body.String(), "hello") {
		t.Fatalf("stored streamed response status=%d body=%s", get.Code, get.Body.String())
	}
}

func TestResponsesDispatcherRoutesByModel(t *testing.T) {
	t.Parallel()

	nativeCalls := 0
	bridgedCalls := 0
	native := func(w http.ResponseWriter, r *http.Request) { nativeCalls++; _, _ = io.WriteString(w, "native") }
	bridged := func(w http.ResponseWriter, r *http.Request) { bridgedCalls++; _, _ = io.WriteString(w, "bridged") }
	dispatch := ModelDispatcher(native, bridged, func(model string) bool {
		return strings.HasPrefix(strings.ToLower(model), "grok-")
	})

	call := func(method, target, body string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		dispatch(rec, httptest.NewRequest(method, target, strings.NewReader(body)))
		return rec
	}

	if rec := call(http.MethodPost, "/v1/responses", `{"model":"grok-4.6","input":"hi"}`); rec.Body.String() != "native" {
		t.Fatalf("grok model routed to %q, want native", rec.Body.String())
	}
	if rec := call(http.MethodPost, "/v1/responses", `{"model":"gpt-5.6-luna","input":"hi"}`); rec.Body.String() != "bridged" {
		t.Fatalf("non-grok model routed to %q, want bridged", rec.Body.String())
	}
	if rec := call(http.MethodGet, "/v1/responses/resp_1", ""); rec.Body.String() != "native" {
		t.Fatalf("resource request routed to %q, want the native handler", rec.Body.String())
	}
	if nativeCalls != 2 || bridgedCalls != 1 {
		t.Fatalf("native=%d bridged=%d, want 2/1", nativeCalls, bridgedCalls)
	}
}
