package grok

import (
	"encoding/json"
	"errors"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestStreamConsoleChatSeparatesReasoningFromContent(t *testing.T) {
	recorder := httptest.NewRecorder()
	stream := strings.Join([]string{
		"event: response.reasoning_summary_text.delta",
		`data: {"type":"response.reasoning_summary_text.delta","delta":"plan first"}`,
		"",
		"event: response.output_text.delta",
		`data: {"type":"response.output_text.delta","delta":"final answer"}`,
		"",
	}, "\n")
	(&Handler{}).streamConsoleChat(recorder, &ChatCompletionsRequest{Model: "grok-4.3"}, strings.NewReader(stream))
	raw := recorder.Body.String()
	if !strings.Contains(raw, `"reasoning_content":"plan first"`) {
		t.Fatalf("reasoning delta missing: %q", raw)
	}
	if !strings.Contains(raw, `"content":"final answer"`) {
		t.Fatalf("visible content missing: %q", raw)
	}
	if strings.Contains(raw, `"content":"plan first"`) {
		t.Fatalf("reasoning leaked into visible content: %q", raw)
	}
}

func TestStreamConsoleChatStreamsFirstReasoningSourceWithoutDuplicates(t *testing.T) {
	recorder := httptest.NewRecorder()
	stream := strings.Join([]string{
		"event: response.reasoning_summary_text.delta",
		`data: {"type":"response.reasoning_summary_text.delta","delta":"duplicate summary"}`,
		"",
		"event: response.reasoning_text.delta",
		`data: {"type":"response.reasoning_text.delta","delta":"raw reasoning"}`,
		"",
		"event: response.output_text.delta",
		`data: {"type":"response.output_text.delta","delta":"answer"}`,
		"",
	}, "\n")
	(&Handler{}).streamConsoleChat(recorder, &ChatCompletionsRequest{Model: "grok-4.3"}, strings.NewReader(stream))
	raw := recorder.Body.String()
	if !strings.Contains(raw, `"reasoning_content":"duplicate summary"`) || strings.Contains(raw, "raw reasoning") {
		t.Fatalf("first reasoning source should stream immediately without later duplication: %q", raw)
	}
}

func TestCollectConsoleChatSeparatesReasoningFromContent(t *testing.T) {
	recorder := httptest.NewRecorder()
	body := `{"id":"resp_1","output":[{"id":"rs_1","type":"reasoning","content":[{"type":"reasoning_text","text":"private plan"}],"summary":[{"type":"summary_text","text":"duplicate summary"}]},{"id":"msg_1","type":"message","content":[{"type":"output_text","text":"public answer"}]}]}`
	(&Handler{}).collectConsoleChat(recorder, &ChatCompletionsRequest{Model: "grok-4.3"}, strings.NewReader(body))
	var response map[string]interface{}
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	choice := interfaceSlice(response["choices"])[0].(map[string]interface{})
	message := choice["message"].(map[string]interface{})
	if message["content"] != "public answer" || message["reasoning_content"] != "private plan" {
		t.Fatalf("message=%#v", message)
	}
}

type failingConsoleStreamReader struct {
	served bool
}

func (r *failingConsoleStreamReader) Read(dst []byte) (int, error) {
	if r.served {
		return 0, errors.New("upstream connection interrupted")
	}
	r.served = true
	return copy(dst, "data: {\"type\":\"response.output_text.delta\",\"delta\":\"hello\"}\n"), nil
}

func TestStreamConsoleChatReportsMalformedSSEData(t *testing.T) {
	recorder := httptest.NewRecorder()
	(&Handler{}).streamConsoleChat(recorder, &ChatCompletionsRequest{Model: "grok-4.3"}, strings.NewReader("data: {not-json}\n"))

	body := recorder.Body.String()
	if !strings.Contains(body, "event: error") || !strings.Contains(body, "console stream parse error") || !strings.Contains(body, "data: [DONE]") {
		t.Fatalf("stream=%q want explicit SSE parse error and terminator", body)
	}
}

func TestStreamConsoleChatReportsScannerError(t *testing.T) {
	recorder := httptest.NewRecorder()
	(&Handler{}).streamConsoleChat(recorder, &ChatCompletionsRequest{Model: "grok-4.3"}, &failingConsoleStreamReader{})

	body := recorder.Body.String()
	if !strings.Contains(body, "event: error") || !strings.Contains(body, "upstream connection interrupted") || !strings.Contains(body, "data: [DONE]") {
		t.Fatalf("stream=%q want explicit SSE read error and terminator", body)
	}
}
