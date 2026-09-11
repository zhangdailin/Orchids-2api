package grok

import (
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/goccy/go-json"

	"orchids-api/internal/config"
)

func TestChatRequestFromResponses_ConvertsInputToolsAndReasoning(t *testing.T) {
	effort := map[string]interface{}{"effort": "high"}
	parallel := false
	req := ResponsesCreateRequest{
		Model:        "grok-4.20-0309",
		Instructions: "用中文回答",
		Input: []interface{}{
			map[string]interface{}{"type": "message", "role": "user", "content": []interface{}{
				map[string]interface{}{"type": "input_text", "text": "上海天气"},
				map[string]interface{}{"type": "input_file", "file_url": "https://example.com/a.pdf"},
			}},
			map[string]interface{}{"type": "function_call", "call_id": "call_1", "name": "get_weather", "arguments": `{"city":"Shanghai"}`},
			map[string]interface{}{"type": "function_call_output", "call_id": "call_1", "output": `{"temp":25}`},
		},
		Reasoning: effort,
		Tools: []map[string]interface{}{{
			"type":        "function",
			"name":        "get_weather",
			"description": "Get weather",
			"parameters":  map[string]interface{}{"type": "object"},
		}},
		ToolChoice:        map[string]interface{}{"type": "function", "name": "get_weather"},
		ParallelToolCalls: &parallel,
	}

	chatReq, err := chatRequestFromResponses(req)
	if err != nil {
		t.Fatalf("chatRequestFromResponses() error: %v", err)
	}
	if len(chatReq.Messages) != 4 {
		t.Fatalf("messages len=%d want 4: %#v", len(chatReq.Messages), chatReq.Messages)
	}
	if chatReq.Messages[0].Role != "system" || chatReq.Messages[0].Content != "用中文回答" {
		t.Fatalf("unexpected system message: %#v", chatReq.Messages[0])
	}
	if got := chatReq.Messages[1].Content.([]interface{})[0].(map[string]interface{})["type"]; got != "text" {
		t.Fatalf("content type=%#v want text", got)
	}
	if got := chatReq.Messages[1].Content.([]interface{})[1].(map[string]interface{})["type"]; got != "file" {
		t.Fatalf("file content type=%#v want file", got)
	}
	if len(chatReq.Messages[2].ToolCalls) != 1 {
		t.Fatalf("tool calls missing: %#v", chatReq.Messages[2])
	}
	if chatReq.Messages[3].Role != "tool" || chatReq.Messages[3].ToolCallID != "call_1" {
		t.Fatalf("tool output message mismatch: %#v", chatReq.Messages[3])
	}
	if chatReq.ReasoningEffort == nil || *chatReq.ReasoningEffort != "high" {
		t.Fatalf("reasoning_effort=%v want high", chatReq.ReasoningEffort)
	}
	if len(chatReq.Tools) != 1 || chatReq.Tools[0].Function["name"] != "get_weather" {
		t.Fatalf("tools mismatch: %#v", chatReq.Tools)
	}
	choice := chatReq.ToolChoice.(map[string]interface{})
	fn := choice["function"].(map[string]interface{})
	if fn["name"] != "get_weather" {
		t.Fatalf("tool_choice mismatch: %#v", choice)
	}
}

func TestResponsesCreateRequest_AcceptsCompatibilityFields(t *testing.T) {
	raw := []byte(`{
		"model":"grok-4.20-0309",
		"input":"hello",
		"max_output_tokens":"128",
		"previous_response_id":"resp_prev",
		"store":"false",
		"metadata":{"trace":"abc"},
		"truncation":"auto",
		"include":["message.output_text.logprobs"],
		"background":false
	}`)

	var req ResponsesCreateRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		t.Fatalf("json.Unmarshal() error: %v", err)
	}
	if req.StreamProvided {
		t.Fatal("stream should not be marked provided")
	}
	if req.MaxOutputTokens == nil || *req.MaxOutputTokens != 128 {
		t.Fatalf("max_output_tokens=%v want 128", req.MaxOutputTokens)
	}
	if req.PreviousResponseID != "resp_prev" || req.Truncation != "auto" {
		t.Fatalf("compat fields mismatch: %#v", req)
	}
	if req.Store == nil || *req.Store != false {
		t.Fatalf("store=%v want false", req.Store)
	}
	if req.Background == nil || *req.Background != false {
		t.Fatalf("background=%v want false", req.Background)
	}
	if len(req.Include) != 1 || req.Include[0] != "message.output_text.logprobs" {
		t.Fatalf("include mismatch: %#v", req.Include)
	}
}

func TestHandleResponses_AppliesDefaultStreamWhenOmitted(t *testing.T) {
	streamDefault := false
	h := &Handler{cfg: &config.Config{Stream: &streamDefault}}
	var decoded ResponsesCreateRequest
	if err := json.Unmarshal([]byte(`{"model":"grok-4.20-0309","input":"hello"}`), &decoded); err != nil {
		t.Fatalf("json.Unmarshal() error: %v", err)
	}
	if decoded.StreamProvided {
		t.Fatal("stream should be omitted before handler defaulting")
	}
	h.applyDefaultResponsesStream(&decoded)
	if decoded.Stream {
		t.Fatal("default stream should be false from config")
	}

	var provided ResponsesCreateRequest
	if err := json.Unmarshal([]byte(`{"model":"grok-4.20-0309","input":"hello","stream":true}`), &provided); err != nil {
		t.Fatalf("json.Unmarshal() provided error: %v", err)
	}
	h.applyDefaultResponsesStream(&provided)
	if !provided.Stream {
		t.Fatal("explicit stream=true should be preserved")
	}
}

func TestValidateResponsesCompatibility_RejectsIgnoredStatefulFields(t *testing.T) {
	store := true
	for _, req := range []ResponsesCreateRequest{
		{Store: &store, Stream: true},
		{Background: &store},
	} {
		if err := validateResponsesCompatibility(req); err == nil {
			t.Fatalf("expected unsupported-field error for %#v", req)
		}
	}
}

func TestValidateResponsesCompatibility_AcceptsRepresentableMetadataAndTruncation(t *testing.T) {
	if err := validateResponsesCompatibility(ResponsesCreateRequest{
		Metadata: map[string]interface{}{"trace": "value"}, Truncation: "auto",
		Include: []string{"reasoning.encrypted_content"},
	}); err != nil {
		t.Fatalf("validateResponsesCompatibility() error = %v", err)
	}
}

func TestChatRequestFromResponses_PreservesMaxOutputTokens(t *testing.T) {
	maxOutputTokens := 128
	chat, err := chatRequestFromResponses(ResponsesCreateRequest{
		Model: "grok-chat-fast", Input: "hello", MaxOutputTokens: &maxOutputTokens,
	})
	if err != nil {
		t.Fatalf("chatRequestFromResponses() error = %v", err)
	}
	if chat.MaxTokens == nil || *chat.MaxTokens != maxOutputTokens {
		t.Fatalf("MaxTokens=%v want %d", chat.MaxTokens, maxOutputTokens)
	}
}

func TestResponsesObjectFromChat_ConvertsMessageAndToolCalls(t *testing.T) {
	chat := map[string]interface{}{
		"model": "grok-4.20-0309",
		"choices": []interface{}{map[string]interface{}{
			"message": map[string]interface{}{
				"role":    "assistant",
				"content": "answer",
				"annotations": []interface{}{map[string]interface{}{
					"type": "url_citation",
					"url_citation": map[string]interface{}{
						"url": "https://example.com",
					},
				}},
				"tool_calls": []interface{}{map[string]interface{}{
					"id":   "call_1",
					"type": "function",
					"function": map[string]interface{}{
						"name":      "get_weather",
						"arguments": `{"city":"Shanghai"}`,
					},
				}},
			},
		}},
		"usage": map[string]interface{}{"prompt_tokens": float64(3), "completion_tokens": float64(4), "total_tokens": float64(7)},
	}

	resp := responsesObjectFromChat("grok-4.20-0309", chat)
	if resp["object"] != "response" || resp["status"] != "completed" {
		t.Fatalf("unexpected response metadata: %#v", resp)
	}
	output := resp["output"].([]interface{})
	if len(output) != 2 {
		t.Fatalf("output len=%d want function_call + message: %#v", len(output), output)
	}
	fc := output[0].(map[string]interface{})
	if fc["type"] != "function_call" || fc["call_id"] != "call_1" || fc["name"] != "get_weather" {
		t.Fatalf("function_call mismatch: %#v", fc)
	}
	msg := output[1].(map[string]interface{})
	content := msg["content"].([]interface{})[0].(map[string]interface{})
	if content["text"] != "answer" {
		t.Fatalf("message text mismatch: %#v", content)
	}
	usage := resp["usage"].(map[string]interface{})
	if usage["input_tokens"] != 3 || usage["output_tokens"] != 4 || usage["total_tokens"] != 7 {
		t.Fatalf("usage mismatch: %#v", usage)
	}
}

func TestWriteResponsesStreamFromChat_ConvertsToolCallChunk(t *testing.T) {
	var b strings.Builder
	chunk := map[string]interface{}{
		"id":     "chatcmpl_1",
		"object": "chat.completion.chunk",
		"choices": []interface{}{map[string]interface{}{
			"index": 0,
			"delta": map[string]interface{}{
				"tool_calls": []interface{}{map[string]interface{}{
					"index": 0,
					"id":    "call_1",
					"type":  "function",
					"function": map[string]interface{}{
						"name":      "get_weather",
						"arguments": `{"city":"Shanghai"}`,
					},
				}},
			},
			"finish_reason": "tool_calls",
		}},
		"usage": map[string]interface{}{"prompt_tokens": 3, "completion_tokens": 2, "total_tokens": 5},
	}
	raw, _ := json.Marshal(chunk)
	b.WriteString("data: ")
	b.Write(raw)
	b.WriteString("\n\n")
	b.WriteString("data: [DONE]\n\n")

	rec := httptest.NewRecorder()
	writeResponsesStreamFromChatReaderRequest(rec, ResponsesCreateRequest{Model: "grok-4.20-0309"}, strings.NewReader(b.String()))

	out := rec.Body.String()
	if !strings.Contains(out, "response.output_item.added") || !strings.Contains(out, "response.function_call_arguments.done") {
		t.Fatalf("expected function call response events, out=%q", out)
	}
	if !strings.Contains(out, `"call_id":"call_1"`) || !strings.Contains(out, `"name":"get_weather"`) {
		t.Fatalf("expected function call id/name, out=%q", out)
	}
	if !strings.Contains(out, `data: [DONE]`) {
		t.Fatalf("expected DONE, out=%q", out)
	}
}

func TestWriteResponsesStreamFromChatFailsEmptyAndPrematureStreams(t *testing.T) {
	for name, input := range map[string]string{
		"empty_done": "data: [DONE]\n\n",
		"premature":  "data: {\"choices\":[{\"delta\":{\"content\":\"partial\"}}]}\n\n",
		"error":      "data: {\"error\":{\"code\":\"rate_limit\",\"message\":\"slow down\"}}\n\n",
	} {
		t.Run(name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			writeResponsesStreamFromChatReaderRequest(recorder, ResponsesCreateRequest{Model: "grok-4.6"}, strings.NewReader(input))
			body := recorder.Body.String()
			if !strings.Contains(body, "event: response.failed") || strings.Contains(body, "event: response.completed") {
				t.Fatalf("body=%s", body)
			}
			if !strings.Contains(body, `"model":"grok-4.6"`) || !strings.Contains(body, "data: [DONE]") {
				t.Fatalf("failure terminal incomplete: %s", body)
			}
		})
	}
}

func TestCopyNativeCLIResponseAddsFailedTerminalOnPrematureEOF(t *testing.T) {
	recorder := httptest.NewRecorder()
	input := "event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_1\"}}\n\n"
	id, _, _ := copyNativeCLIResponseAndCaptureModel(recorder, strings.NewReader(input), "text/event-stream", "grok-4.6")
	if id != "resp_1" {
		t.Fatalf("id=%q", id)
	}
	body := recorder.Body.String()
	if !strings.Contains(body, "event: response.failed") || !strings.Contains(body, "upstream_stream_incomplete") || !strings.Contains(body, `"model":"grok-4.6"`) {
		t.Fatalf("body=%s", body)
	}
}

func TestCopyNativeCLIResponseKeepsValidTerminal(t *testing.T) {
	recorder := httptest.NewRecorder()
	input := "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_1\"}}\n\ndata: [DONE]\n\n"
	_, _, _ = copyNativeCLIResponseAndCaptureModel(recorder, strings.NewReader(input), "text/event-stream", "grok-4.6")
	if count := strings.Count(recorder.Body.String(), "response.failed"); count != 0 {
		t.Fatalf("unexpected failure: %s", recorder.Body.String())
	}
}

func TestResponsesObjectFromChatPreservesReasoningItem(t *testing.T) {
	chat := map[string]interface{}{
		"model": "grok-4.3",
		"choices": []interface{}{map[string]interface{}{"message": map[string]interface{}{
			"role": "assistant", "reasoning_content": "plan", "content": "answer",
		}}},
	}
	output := responsesObjectFromChat("grok-4.3", chat)["output"].([]interface{})
	if len(output) != 2 {
		t.Fatalf("output=%#v", output)
	}
	reasoning := output[0].(map[string]interface{})
	if reasoning["type"] != "reasoning" {
		t.Fatalf("reasoning item=%#v", reasoning)
	}
	summary := reasoning["summary"].([]interface{})[0].(map[string]interface{})
	if summary["text"] != "plan" {
		t.Fatalf("summary=%#v", summary)
	}
}

func TestWriteResponsesStreamFromChatPreservesReasoningEvents(t *testing.T) {
	raw := strings.Join([]string{
		`data: {"choices":[{"delta":{"reasoning_content":"plan"},"finish_reason":null}]}`,
		`data: {"choices":[{"delta":{"content":"answer"},"finish_reason":null}]}`,
		`data: [DONE]`,
	}, "\n\n")
	recorder := httptest.NewRecorder()
	writeResponsesStreamFromChatReaderRequest(recorder, ResponsesCreateRequest{Model: "grok-4.3"}, strings.NewReader(raw))
	out := recorder.Body.String()
	if !strings.Contains(out, `"type":"response.reasoning_summary_text.delta"`) || !strings.Contains(out, `"delta":"plan"`) {
		t.Fatalf("reasoning events missing: %q", out)
	}
	if !strings.Contains(out, `"type":"response.output_text.delta"`) || !strings.Contains(out, `"delta":"answer"`) {
		t.Fatalf("answer events missing: %q", out)
	}
}

// A Responses input_file must survive conversion into the chat layer, which
// validates the portable file shape. A nested url was rejected downstream, so
// the round trip has to carry the resolved reference as file_data.
//
// A bare file_id is intentionally not covered: the chat layer only accepts a URL
// or data URI, so an opaque asset id is not representable on this path.
func TestResponsesInputFileRoundTripsIntoChatMessages(t *testing.T) {
	for _, part := range []map[string]interface{}{
		{"type": "input_file", "file": map[string]interface{}{"data": "data:application/pdf;base64,QUFB"}},
		{"type": "input_file", "file_url": "https://example.com/a.pdf"},
		{"type": "input_file", "file_data": "data:application/pdf;base64,QUFB"},
		{"type": "input_file", "file": map[string]interface{}{"url": "https://example.com/b.pdf"}},
	} {
		input := []interface{}{map[string]interface{}{
			"type": "message", "role": "user",
			"content": []interface{}{map[string]interface{}{"type": "input_text", "text": "see attached"}, part},
		}}
		messages, err := responsesInputToMessages(input)
		if err != nil {
			t.Fatalf("%v: %v", part, err)
		}
		if err := validateChatMessages(messages); err != nil {
			t.Fatalf("%v: chat validation rejected the converted message: %v", part, err)
		}
		parts, ok := messages[0].Content.([]interface{})
		if !ok || len(parts) != 2 {
			t.Fatalf("%v: converted content=%#v", part, messages[0].Content)
		}
		filePart, _ := parts[1].(map[string]interface{})
		file, _ := filePart["file"].(map[string]interface{})
		if data, _ := file["file_data"].(string); data == "" {
			t.Fatalf("%v: file_data missing after conversion: %#v", part, filePart)
		}
	}
}
