package qoder

import (
	"errors"
	"io"
	"strings"
	"testing"

	"orchids-api/internal/upstream"
)

// envelope wraps one inner chunk the way the upstream does: a JSON object whose
// `body` field is a JSON string.
func envelope(inner string) string {
	return "data: " + `{"statusCodeValue":200,"body":` + jsonString(inner) + "}\n\n"
}

func jsonString(value string) string {
	var builder strings.Builder
	builder.WriteByte('"')
	for _, r := range value {
		switch r {
		case '"':
			builder.WriteString(`\"`)
		case '\\':
			builder.WriteString(`\\`)
		case '\n':
			builder.WriteString(`\n`)
		default:
			builder.WriteRune(r)
		}
	}
	builder.WriteByte('"')
	return builder.String()
}

// collectStream runs the parser and returns the events it produced.
func collectStream(t *testing.T, body string) ([]upstream.SSEMessage, streamResult, error) {
	t.Helper()
	var events []upstream.SSEMessage
	result, err := consumeStream(strings.NewReader(body), func(msg upstream.SSEMessage) {
		events = append(events, msg)
	})
	return events, result, err
}

// TestConsumeStreamDecodesWrappedChunks pins the double unwrapping: reading the
// envelope as the chunk yields an empty answer, which is indistinguishable from
// a model that returned nothing.
func TestConsumeStreamDecodesWrappedChunks(t *testing.T) {
	t.Parallel()

	body := envelope(`{"id":"1","choices":[{"index":0,"delta":{"role":"assistant","content":"Hel"}}]}`) +
		envelope(`{"id":"1","choices":[{"index":0,"delta":{"reasoning_content":"think"}}]}`) +
		envelope(`{"id":"1","choices":[{"index":0,"delta":{"content":"lo"},"finish_reason":"stop"}]}`) +
		"event:finish\ndata: {}\n\n"

	events, result, err := collectStream(t, body)
	if err != nil {
		t.Fatalf("consumeStream() error = %v", err)
	}

	var text strings.Builder
	var reasoning strings.Builder
	for _, event := range events {
		switch event.Type {
		case "model.text-delta":
			text.WriteString(event.Event["delta"].(string))
		case "model.reasoning-delta":
			reasoning.WriteString(event.Event["delta"].(string))
		}
	}
	if text.String() != "Hello" {
		t.Fatalf("text = %q, want Hello", text.String())
	}
	if reasoning.String() != "think" {
		t.Fatalf("reasoning = %q, want think", reasoning.String())
	}
	if got := result.FinishReason(); got != "end_turn" {
		t.Fatalf("FinishReason() = %q, want end_turn", got)
	}
	if !result.SawMeaningfulEvent {
		t.Fatal("SawMeaningfulEvent = false")
	}
}

// TestConsumeStreamRequiresTerminator proves a premature EOF is reported as a
// truncation instead of as a successful short answer. Silently accepting it
// would hand the client a cut-off response with no error.
func TestConsumeStreamRequiresTerminator(t *testing.T) {
	t.Parallel()

	body := envelope(`{"id":"1","choices":[{"index":0,"delta":{"content":"partial"}}]}`)
	_, result, err := collectStream(t, body)
	if !errors.Is(err, ErrStreamTruncated) {
		t.Fatalf("error = %v, want ErrStreamTruncated", err)
	}
	if !result.SawMeaningfulEvent {
		t.Fatal("SawMeaningfulEvent = false, want the partial content to be recorded")
	}
}

// TestConsumeStreamAcceptsBareDone covers the alternative terminator.
func TestConsumeStreamAcceptsBareDone(t *testing.T) {
	t.Parallel()

	body := envelope(`{"id":"1","choices":[{"index":0,"delta":{"content":"ok"}}]}`) + "data: [DONE]\n\n"
	if _, _, err := collectStream(t, body); err != nil {
		t.Fatalf("consumeStream() error = %v, want success for a bare [DONE]", err)
	}
}

// TestConsumeStreamAcceptsDoneInsideEnvelope covers a terminator nested in the
// wrapper, which is how some gateway builds signal completion.
func TestConsumeStreamAcceptsDoneInsideEnvelope(t *testing.T) {
	t.Parallel()

	body := envelope(`[DONE]`)
	if _, _, err := collectStream(t, body); err != nil {
		t.Fatalf("consumeStream() error = %v, want success for an enveloped [DONE]", err)
	}
}

// TestConsumeStreamReportsErrorEnvelope proves an upstream failure is surfaced
// rather than being swallowed as an empty stream.
func TestConsumeStreamReportsErrorEnvelope(t *testing.T) {
	t.Parallel()

	body := "data: " + `{"statusCodeValue":500,"body":"{\"message\":\"model overloaded\"}"}` + "\n\n"
	_, _, err := collectStream(t, body)
	if err == nil {
		t.Fatal("consumeStream() error = nil for an error envelope")
	}
	if !strings.Contains(err.Error(), "model overloaded") {
		t.Fatalf("error = %v, want the upstream message", err)
	}
}

// TestConsumeStreamClassifiesBusyCode proves business code 10605 is reported as
// a queue refusal under a 401, because refreshing the token cannot fix it and
// the retry policy differs.
func TestConsumeStreamClassifiesBusyCode(t *testing.T) {
	t.Parallel()

	body := "data: " + `{"statusCodeValue":401,"body":"{\"code\":\"10605\",\"message\":\"queue full\"}"}` + "\n\n"
	_, _, err := collectStream(t, body)
	if !errors.Is(err, ErrBusy) {
		t.Fatalf("error = %v, want ErrBusy", err)
	}
}

// TestConsumeStreamClassifiesUnauthorizedEnvelope proves a genuine auth failure
// is distinguished from a busy verdict.
func TestConsumeStreamClassifiesUnauthorizedEnvelope(t *testing.T) {
	t.Parallel()

	body := "data: " + `{"statusCodeValue":403,"body":"{\"message\":\"login expired\"}"}` + "\n\n"
	_, _, err := collectStream(t, body)
	if !errors.Is(err, errUpstreamUnauthorized) {
		t.Fatalf("error = %v, want errUpstreamUnauthorized", err)
	}
}

// TestConsumeStreamIgnoresMalformedFrames proves one bad frame does not discard
// an otherwise good answer.
func TestConsumeStreamIgnoresMalformedFrames(t *testing.T) {
	t.Parallel()

	body := "data: not-json\n\n" +
		envelope(`{"id":"1","choices":[{"index":0,"delta":{"content":"kept"},"finish_reason":"stop"}]}`) +
		"event:finish\ndata: {}\n\n"

	events, _, err := collectStream(t, body)
	if err != nil {
		t.Fatalf("consumeStream() error = %v", err)
	}
	if len(events) != 1 || events[0].Event["delta"] != "kept" {
		t.Fatalf("events = %+v, want the well-formed delta only", events)
	}
}

// TestConsumeStreamUsageSurvivesASharedFrame proves usage is captured even when
// it rides along with the final content frame, which is how the gateway reports
// the last chunk.
func TestConsumeStreamUsageSurvivesASharedFrame(t *testing.T) {
	t.Parallel()

	body := envelope(`{"id":"1","choices":[{"index":0,"delta":{"content":"final"},"finish_reason":"stop"}],"usage":{"prompt_tokens":11,"completion_tokens":7,"total_tokens":18,"prompt_tokens_details":{"cached_tokens":3,"cacheable_tokens":5},"completion_tokens_details":{"reasoning_tokens":2},"credits":0.25,"original_credits":0.5}}`) +
		"event:finish\ndata: {}\n\n"

	events, result, err := collectStream(t, body)
	if err != nil {
		t.Fatalf("consumeStream() error = %v", err)
	}
	var sawUsage bool
	var sawText bool
	for _, event := range events {
		switch event.Type {
		case "model.tokens-used":
			sawUsage = true
		case "model.text-delta":
			sawText = true
		}
	}
	if !sawText {
		t.Fatal("the content in a shared usage frame was dropped")
	}
	if !sawUsage {
		t.Fatal("usage was not emitted")
	}
	if got := result.Usage["inputTokens"]; got != 11 {
		t.Fatalf("inputTokens = %v, want 11", got)
	}
	if got := result.Usage["outputTokens"]; got != 7 {
		t.Fatalf("outputTokens = %v, want 7", got)
	}
	if got := result.Usage["cacheReadTokens"]; got != 3 {
		t.Fatalf("cacheReadTokens = %v, want 3", got)
	}
	if got := result.Usage["cacheWriteTokens"]; got != 5 {
		t.Fatalf("cacheWriteTokens = %v, want 5", got)
	}
	if got := result.Usage["credits"]; got != 0.25 {
		t.Fatalf("credits = %v, want 0.25", got)
	}
}

// TestConsumeStreamToolCallAccumulatesArguments proves split argument deltas are
// reassembled. Emitting on the first delta loses every later fragment and
// produces invalid JSON.
func TestConsumeStreamToolCallAccumulatesArguments(t *testing.T) {
	t.Parallel()

	body := envelope(`{"id":"1","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"search","arguments":"{\"q\":"}}]}}]}`) +
		envelope(`{"id":"1","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"cats\"}"}}]}}]}`) +
		envelope(`{"id":"1","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`) +
		"event:finish\ndata: {}\n\n"

	events, result, err := collectStream(t, body)
	if err != nil {
		t.Fatalf("consumeStream() error = %v", err)
	}
	var call map[string]interface{}
	for _, event := range events {
		if event.Type == "model.tool-call" {
			call = event.Event
		}
	}
	if call == nil {
		t.Fatalf("no tool call was emitted; events = %+v", events)
	}
	if call["toolName"] != "search" {
		t.Fatalf("toolName = %v, want search", call["toolName"])
	}
	if call["input"] != `{"q":"cats"}` {
		t.Fatalf("input = %v, want the reassembled arguments", call["input"])
	}
	if got := result.FinishReason(); got != "tool_use" {
		t.Fatalf("FinishReason() = %q, want tool_use", got)
	}
	if result.ToolCallCount != 1 {
		t.Fatalf("ToolCallCount = %d, want 1", result.ToolCallCount)
	}
}

// TestToolCallAccumulatorOpensNewCallOnIDChange covers parallel calls, which the
// upstream sometimes reports by reusing one index and changing only the id.
//
// One index holds one call, so the newest id wins and the displaced call is
// counted rather than emitted with its truncated arguments.
func TestToolCallAccumulatorOpensNewCallOnIDChange(t *testing.T) {
	t.Parallel()

	accumulator := newToolCallAccumulator()
	accumulator.add(0, "call_a", "first", `{"a":1}`)
	first := accumulator.completeAll()
	if len(first) != 1 || first[0].ID != "call_a" {
		t.Fatalf("first flush = %+v, want the first call", first)
	}

	// The same index is reused for a different call.
	accumulator.add(0, "call_b", "second", `{"b":2}`)
	second := accumulator.completeAll()
	if len(second) != 1 || second[0].ID != "call_b" || second[0].Name != "second" {
		t.Fatalf("second flush = %+v, want the reused-index call", second)
	}

	// A repeated id must not emit again.
	accumulator.add(0, "call_b", "", ``)
	if got := accumulator.completeAll(); len(got) != 0 {
		t.Fatalf("third flush = %+v, want nothing", got)
	}
}

// TestToolCallAccumulatorCountsDisplacedCalls proves an index collision while a
// call is still open is reported rather than silently swallowed.
func TestToolCallAccumulatorCountsDisplacedCalls(t *testing.T) {
	t.Parallel()

	accumulator := newToolCallAccumulator()
	accumulator.add(1, "call_c", "third", `{"c":3}`)
	accumulator.add(1, "call_d", "fourth", `{"d":4}`)
	flushed := accumulator.completeAll()
	if len(flushed) != 1 || flushed[0].ID != "call_d" {
		t.Fatalf("flush = %+v, want only the newest call at the index", flushed)
	}
	if accumulator.dropped != 1 {
		t.Fatalf("dropped = %d, want 1", accumulator.dropped)
	}
	if got := accumulator.completeAll(); len(got) != 0 {
		t.Fatalf("re-flush = %+v, want nothing", got)
	}
}

// TestNormalizeToolInputUnwrapsNestedArguments proves the OpenAI argument
// wrapping is removed before the tool dispatcher sees the input.
func TestNormalizeToolInputUnwrapsNestedArguments(t *testing.T) {
	t.Parallel()

	cases := map[string]string{
		`{"arguments":"{\"a\":1}"}`: `{"a":1}`,
		`"{\"a\":1}"`:               `{"a":1}`,
		``:                          `{}`,
		`null`:                      `{}`,
		`{"a":1}`:                   `{"a":1}`,
	}
	for input, want := range cases {
		if got := normalizeToolInput(input); got != want {
			t.Errorf("normalizeToolInput(%q) = %q, want %q", input, got, want)
		}
	}
}

// TestConsumeStreamRejectsEventError proves an explicit error event terminates
// with an error rather than an empty success.
func TestConsumeStreamRejectsEventError(t *testing.T) {
	t.Parallel()

	body := "event: error\ndata: {}\n\n"
	if _, _, err := collectStream(t, body); err == nil {
		t.Fatal("consumeStream() error = nil for an error event")
	}
}

// TestReadSSEHandlesMultilineData proves a frame split across several data lines
// is joined with newlines, which is how long JSON payloads arrive.
func TestReadSSEHandlesMultilineData(t *testing.T) {
	t.Parallel()

	var frames []sseFrame
	err := readSSE(strings.NewReader("event: message\ndata: line1\ndata: line2\n\n"), func(frame sseFrame) bool {
		frames = append(frames, frame)
		return true
	})
	if err != nil {
		t.Fatalf("readSSE() error = %v", err)
	}
	if len(frames) != 1 {
		t.Fatalf("frames = %d, want 1", len(frames))
	}
	if frames[0].event != "message" || frames[0].data != "line1\nline2" {
		t.Fatalf("frame = %+v, want the joined data", frames[0])
	}
}

// TestReadSSEPropagatesReadErrors proves a transport failure is visible.
func TestReadSSEPropagatesReadErrors(t *testing.T) {
	t.Parallel()

	err := readSSE(io.MultiReader(strings.NewReader("data: x\n"), errReader{}), func(sseFrame) bool { return true })
	if err == nil {
		t.Fatal("readSSE() error = nil for a failing reader")
	}
}

type errReader struct{}

func (errReader) Read([]byte) (int, error) { return 0, io.ErrUnexpectedEOF }

// TestFinishReasonMapping pins the stop-reason translation.
func TestFinishReasonMapping(t *testing.T) {
	t.Parallel()

	cases := []struct {
		in   string
		want string
	}{
		{"stop", "end_turn"},
		{"tool_calls", "tool_use"},
		{"length", "max_tokens"},
		{"content_filter", "refusal"},
		{"", "end_turn"},
	}
	for _, tc := range cases {
		if got := (streamResult{FinishReasonValue: tc.in}).FinishReason(); got != tc.want {
			t.Errorf("FinishReason(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
	if got := (streamResult{ToolCallCount: 1}).FinishReason(); got != "tool_use" {
		t.Errorf("FinishReason with a tool call = %q, want tool_use", got)
	}
}
