package grok

import (
	"bytes"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/iotest"
)

func TestGrok2apiNativeOutcomeStatusAndEOF(t *testing.T) {
	for _, test := range []struct {
		status, finish string
		fail           bool
	}{
		{"completed", "stop", false}, {"incomplete", "length", false},
		{"failed", "error", true}, {"cancelled", "error", true},
		{"in_progress", "error", true}, {"queued", "error", true},
	} {
		t.Run(test.status, func(t *testing.T) {
			response := map[string]interface{}{"status": test.status}
			raw := strings.TrimRight(parityFrame("response.completed", map[string]interface{}{"response": response}), "\n")
			// Byte-sized reads and no final LF exercise the shared production decoder.
			_, _, result := copyNativeCLIResponseAndCaptureModel(httptest.NewRecorder(), iotest.OneByteReader(strings.NewReader(raw)), "text/event-stream", "grok-4.6")
			if result.Finish != test.finish || (result.Err != nil) != test.fail {
				t.Fatalf("%+v", result)
			}
		})
	}
	for _, status := range []string{"queued", "in_progress"} {
		_, _, result := copyNativeCLIResponseAndCaptureModel(httptest.NewRecorder(), strings.NewReader("{\"status\":\""+status+"\"}"), "application/json", "grok-4.6")
		if result.Finish != status || result.Err != nil {
			t.Fatalf("async JSON mislabeled: %+v", result)
		}
	}
}

type grok2apiShortWriter struct{ http.ResponseWriter }

func (w grok2apiShortWriter) Write(p []byte) (int, error) { return len(p) - 1, nil }

func TestGrok2apiNativeOutcomeShortWriteAndStickyFailure(t *testing.T) {
	_, _, result := copyNativeCLIResponseAndCaptureModel(grok2apiShortWriter{httptest.NewRecorder()}, strings.NewReader(parityTerminal("response.completed")), "text/event-stream", "grok-4.6")
	if !errors.Is(result.Err, io.ErrShortWrite) || result.Finish != "error" {
		t.Fatal(result)
	}
	stream := parityFrame("response.failed", map[string]interface{}{"response": map[string]interface{}{"error": map[string]interface{}{"message": "failed first"}}}) + parityTerminal("response.completed")
	_, _, result = copyNativeCLIResponseAndCaptureModel(httptest.NewRecorder(), strings.NewReader(stream), "text/event-stream", "grok-4.6")
	if result.Err == nil || result.Finish != "error" {
		t.Fatal(result)
	}
}

func TestGrok2apiNativeAuditBoundsWholeMultilineFrame(t *testing.T) {
	line := "data: " + strings.Repeat("a", 64<<10) + "\n"
	recorder := httptest.NewRecorder()
	_, capture, result := copyNativeCLIResponseAndCaptureModel(recorder, strings.NewReader(strings.Repeat(line, 130)), "text/event-stream", "grok-4.6")
	if result.Err == nil || len(capture) > upstreamMaxEventBytes || !strings.Contains(recorder.Body.String(), "response.failed") {
		t.Fatal("multiline audit accumulation is unbounded", result)
	}
}

type grok2apiUnexpectedReader struct{ reads int }

func (r *grok2apiUnexpectedReader) Read([]byte) (int, error) {
	r.reads++
	return 0, errors.New("read after logical terminal")
}

func TestGrok2apiNativeSSEFramingAndLogicalTerminal(t *testing.T) {
	input := "\uFEFF: keepalive\r\nid: event_a\r\nretry: 1000\r\nevent: response.completed\r\ndata: {\"response\":\r\ndata: {\"id\":\"resp_a\",\"status\":\"completed\"}}\r\n\r\n"
	tail := &grok2apiUnexpectedReader{}
	recorder := httptest.NewRecorder()
	id, capture, result := copyNativeCLIResponseAndCaptureModel(recorder, io.MultiReader(strings.NewReader(input), tail), "text/event-stream", "grok-4.6")
	if id != "resp_a" || tail.reads != 0 || result.Err != nil || result.Finish != "stop" {
		t.Fatal(id, tail.reads, result)
	}
	for _, want := range []string{": keepalive", "id: event_a", "retry: 1000"} {
		if !strings.Contains(recorder.Body.String(), want) || !bytes.Contains(capture, []byte(want)) {
			t.Fatal("SSE metadata lost", want)
		}
	}
	// grok2api relays the native Responses stream: no added [DONE], and the
	// upstream's own framing (CRLF, multi-line data) is what the client receives.
	if strings.Contains(recorder.Body.String(), "data: [DONE]") {
		t.Fatal("the relay appended a Responses-invalid [DONE] frame")
	}

	// A frame that already carries every field the strict clients need is relayed
	// byte-for-byte, CRLF included: supplementation is what makes a frame differ,
	// never the relay itself.
	complete := "event: response.completed\r\ndata: { \"type\":\"response.completed\", \"id\":\"resp_ok\", \"response\":{\"id\":\"resp_ok\",\"object\":\"response\",\"created_at\":42,\"model\":\"grok-4.6\",\"output\":[{\"type\":\"message\",\"id\":\"msg_1\",\"content\":[{\"type\":\"output_text\",\"text\":\"ok\",\"annotations\":[]}]}]}}\r\n\r\n"
	verbatim := httptest.NewRecorder()
	_, _, verbatimResult := copyNativeCLIResponseAndCaptureModel(verbatim, strings.NewReader(complete), "text/event-stream", "grok-4.6")
	if verbatimResult.Err != nil {
		t.Fatal(verbatimResult.Err)
	}
	if verbatim.Body.String() != complete {
		t.Fatalf("complete frame was rewritten:\n got %q\nwant %q", verbatim.Body.String(), complete)
	}
}

// An upstream [DONE] without a terminal response event is still a failure the
// client has to see; the relay reports it instead of inventing a completion.
func TestGrok2apiNativeDoneWithoutTerminalSynthesizesFailure(t *testing.T) {
	recorder := httptest.NewRecorder()
	_, _, _ = copyNativeCLIResponseAndCaptureModel(recorder, strings.NewReader("data: [DONE]\n\n"), "text/event-stream", "grok-4.6")
	output := recorder.Body.String()
	if !strings.Contains(output, "event: response.failed") {
		t.Fatalf("no synthesized failure: %s", output)
	}
	if strings.Contains(output, "data: [DONE]") {
		t.Fatalf("the relay relayed or re-added [DONE]: %s", output)
	}
	if !strings.Contains(output, "upstream_terminal_missing") {
		t.Fatalf("failure reason lost: %s", output)
	}
}
