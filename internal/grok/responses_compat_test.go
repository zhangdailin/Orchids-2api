package grok

import (
	"net/http/httptest"
	"strings"
	"testing"
)

func TestNativeResponsesCompatibilityPreservesCompleteSSEBytes(t *testing.T) {
	line := `event: response.completed
data: { "type":"response.completed", "id":"resp_ok", "response":{"id":"resp_ok","object":"response","created_at":42,"model":"grok-4.6","output":[{"type":"message","id":"msg_1","content":[{"type":"output_text","text":"ok","annotations":[]}]}]}}

`
	rec := httptest.NewRecorder()
	_, _, result := copyNativeCLIResponseAndCaptureModel(rec, strings.NewReader(line), "text/event-stream", "grok-4.6")
	if result.Err != nil {
		t.Fatal(result.Err)
	}
	// The native relay is byte-transparent: an event that already carries every
	// field reaches the client exactly as the upstream wrote it, with no added
	// frame (grok2api relays the same way).
	if rec.Body.String() != line {
		t.Fatalf("complete event bytes changed:\n got %q\nwant %q", rec.Body.String(), line)
	}
}

func TestNativeResponsesCompatibilitySupplementsStrictClientFields(t *testing.T) {
	stream := "event: response.output_item.added\ndata: {\"type\":\"response.output_item.added\",\"output_index\":0,\"item\":{\"type\":\"message\",\"content\":[{\"type\":\"output_text\",\"text\":\"ok\"}]}}\n\n" +
		"event: response.output_text.done\ndata: {\"type\":\"response.output_text.done\",\"output_index\":0,\"delta\":\"ok\"}\n\n" +
		"event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\"}}\n\n"
	rec := httptest.NewRecorder()
	_, _, result := copyNativeCLIResponseAndCaptureModel(rec, strings.NewReader(stream), "text/event-stream", "grok-4.6")
	if result.Err != nil {
		t.Fatal(result.Err)
	}
	got := rec.Body.String()
	for _, required := range []string{`"annotations":[]`, `"id":"item_1"`, `"item_id":"item_1"`, `"object":"response"`, `"model":"grok-4.6"`, `"output":[]`} {
		if !strings.Contains(got, required) {
			t.Fatalf("missing %s in %s", required, got)
		}
	}
}
