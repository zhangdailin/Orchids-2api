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
	for _, want := range []string{": keepalive", "id: event_a", "retry: 1000", "data: [DONE]"} {
		if !strings.Contains(recorder.Body.String(), want) || !bytes.Contains(capture, []byte(want)) {
			t.Fatal("SSE metadata lost", want)
		}
	}
	if strings.Count(recorder.Body.String(), "[DONE]") != 1 {
		t.Fatal("duplicate DONE")
	}
}

func TestGrok2apiNativeDoneWithoutTerminalFailsBeforeDone(t *testing.T) {
	recorder := httptest.NewRecorder()
	_, _, _ = copyNativeCLIResponseAndCaptureModel(recorder, strings.NewReader("data: [DONE]\n\n"), "text/event-stream", "grok-4.6")
	output := recorder.Body.String()
	if strings.Count(output, "data: [DONE]") != 1 || strings.Index(output, "event: response.failed") > strings.Index(output, "data: [DONE]") {
		t.Fatal(output)
	}
}
