package grok

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
)

func TestRefactorAliasMultilineKeepsMetadataAndRestoresNames(t *testing.T) {
	aliases := map[string]buildToolAliasIdentity{"crm__lookup": {Kind: "function", Namespace: "crm", Name: "lookup"}}
	input := "\uFEFF: keepalive\r\nid: ev_a\r\nretry: 1000\r\nevent: response.output_item.added\r\ndata: {\"type\":\"response.output_item.added\",\"item\":\r\ndata: {\"type\":\"function_call\",\"name\":\"crm__lookup\",\"id\":\"item_a\"}}\r\n\r\ndata: [DONE]\r\n\r\n"
	body := rewriteBuildToolAliasResponse(io.NopCloser(strings.NewReader(input)), "text/event-stream", aliases)
	defer body.Close()
	raw, err := io.ReadAll(body)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{": keepalive", "id: ev_a", "retry: 1000", "\"name\":\"lookup\"", "\"namespace\":\"crm\"", "data: [DONE]"} {
		if !strings.Contains(string(raw), want) {
			t.Fatal("lost", want, string(raw))
		}
	}
	if strings.Contains(string(raw), "crm__lookup") {
		t.Fatal("internal alias leaked")
	}
}

func TestRefactorSearchArgumentsSurviveItemDoneWithoutSnapshot(t *testing.T) {
	aliases := map[string]buildToolAliasIdentity{"tool_search": {Kind: "tool_search"}}
	added := map[string]interface{}{"id": "item_a", "call_id": "call_a", "name": "tool_search", "type": "function_call"}
	// The done item repeats the name but does not repeat arguments; it must not
	// replace the existing accumulator. Argument events can refer to call_id.
	stream := parityFrame("response.output_item.added", map[string]interface{}{"item": added}) +
		parityFrame("response.function_call_arguments.delta", map[string]interface{}{"call_id": "call_a", "delta": "{\"goal\":\"crm\"}"}) +
		parityFrame("response.output_item.done", map[string]interface{}{"item": added})
	body := rewriteBuildToolAliasResponse(io.NopCloser(strings.NewReader(stream)), "text/event-stream", aliases)
	defer body.Close()
	raw, err := io.ReadAll(body)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "response.function_call_arguments") || !strings.Contains(string(raw), "\"goal\":\"crm\"") {
		t.Fatal(string(raw))
	}
}

func TestRefactorAliasRejectsOversizedMultilineFrame(t *testing.T) {
	input := strings.Repeat("data: "+strings.Repeat("a", 64<<10)+"\n", 130)
	body := rewriteBuildToolAliasResponse(io.NopCloser(strings.NewReader(input)), "text/event-stream", nil)
	defer body.Close()
	if _, err := io.Copy(io.Discard, body); err == nil {
		t.Fatal("unbounded alias frame accepted")
	}
}

func TestRefactorReasoningReplayUsesProtocolItemsAndLatestSignature(t *testing.T) {
	response := map[string]interface{}{"output": []interface{}{
		map[string]interface{}{"type": "reasoning", "encrypted_content": "first"},
		map[string]interface{}{"type": "reasoning", "encrypted_content": "latest"},
	}}
	raw, _ := json.Marshal(response)
	if got := encryptedReasoningFromResponse(raw); got != "latest" {
		t.Fatal(got)
	}
	stream := "data: {\"type\":\"response.output_item.done\",\ndata: \"item\":{\"type\":\"reasoning\",\"encrypted_content\":\"early\"}}\n\n" +
		"event: response.completed\ndata: {\"response\":\ndata: " + string(raw) + "}\n\n"
	if got := encryptedReasoningFromResponse([]byte(stream)); got != "latest" {
		t.Fatal(got)
	}
	unrelated := []byte("{\"metadata\":{\"type\":\"reasoning\",\"encrypted_content\":\"not-a-reasoning-item\"}}")
	if got := encryptedReasoningFromResponse(unrelated); got != "" {
		t.Fatal("metadata replayed", got)
	}
}

func TestRefactorNativeUsageSurvivesFullCaptureOverflow(t *testing.T) {
	frame := parityFrame("response.in_progress", map[string]interface{}{"padding": strings.Repeat("a", 128<<10)})
	stream := strings.Repeat(frame, 70) + parityFrame("response.completed", map[string]interface{}{"response": map[string]interface{}{
		"id": "resp_a", "status": "completed", "usage": map[string]interface{}{"input_tokens": 100, "output_tokens": 10},
	}})
	id, captured, result := copyNativeCLIResponseAndCaptureModel(httptest.NewRecorder(), strings.NewReader(stream), "text/event-stream", "grok-4.6")
	if id != "resp_a" || len(captured) != upstreamMaxEventBytes || result.Err != nil || interfaceToInt(result.Usage["completion_tokens"]) != 10 {
		t.Fatal(id, len(captured), result)
	}
}

type refactorErrorReader struct{ err error }

func (r refactorErrorReader) Read([]byte) (int, error) { return 0, r.err }

func TestRefactorNativeJSONReadErrorIsNotAuditedAsSuccess(t *testing.T) {
	readErr := errors.New("synthetic transport failure")
	source := io.MultiReader(strings.NewReader("{\"id\":\"resp_a\",\"status\":\"completed\"}"), refactorErrorReader{readErr})
	_, _, result := copyNativeCLIResponseAndCaptureModel(httptest.NewRecorder(), source, "application/json", "grok-4.6")
	if !errors.Is(result.Err, readErr) || result.Finish != "error" {
		t.Fatal(result)
	}
}

func TestRefactorChatStreamFreezesCommittedHeaders(t *testing.T) {
	reader, writer := io.Pipe()
	defer reader.Close()
	defer writer.Close()
	stream := newStreamingChatWriter(writer)
	stream.Header().Set("X-Test", "initial")
	stream.WriteHeader(http.StatusBadRequest)
	stream.Header().Set("X-Test", "later")
	stream.WriteHeader(http.StatusOK)
	if stream.status != http.StatusBadRequest || stream.committedHeader.Get("X-Test") != "initial" {
		t.Fatal("committed response changed")
	}
}

func TestRefactorChatBridgePropagatesErrorAndClosesPipe(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader("{"))
	var saved io.Reader
	(&Handler{}).withChatStream(req, func(status int, header http.Header, reader io.Reader) {
		saved = reader
		body, err := io.ReadAll(reader)
		if err != nil || status != http.StatusBadRequest || !strings.Contains(string(body), "invalid json") {
			t.Fatal(status, err, string(body))
		}
	})
	if saved == nil {
		t.Fatal("bridge did not forward response")
	}
	if _, err := saved.Read(make([]byte, 1)); !errors.Is(err, io.ErrClosedPipe) {
		t.Fatal("bridge left reader open", err)
	}
}

func TestRefactorChatBridgeCancellationDoesNotWaitForHeaders(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader("{")).WithContext(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		(&Handler{}).withChatStream(req, func(_ int, _ http.Header, reader io.Reader) { _, _ = io.Copy(io.Discard, reader) })
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("canceled bridge stayed blocked")
	}
}
