package grok

import (
	"bytes"
	"github.com/goccy/go-json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestStreamMessageDelta(t *testing.T) {
	tests := []struct {
		name     string
		previous string
		current  string
		want     string
	}{
		{name: "initial", previous: "", current: "你好", want: "你好"},
		{name: "append", previous: "你", current: "你好！", want: "好！"},
		{name: "rewrite prefix", previous: "你！rok，", current: "你好！我是 Grok，xAI AI 助手。", want: "好！我是 Grok，xAI AI 助手。"},
		{name: "shrink", previous: "你好世界", current: "你好", want: ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := streamMessageDelta(tt.previous, tt.current); got != tt.want {
				t.Fatalf("streamMessageDelta(%q,%q)=%q want=%q", tt.previous, tt.current, got, tt.want)
			}
		})
	}
}

func TestCollectChat_EmitsOpenAIParityMetadata(t *testing.T) {
	h := &Handler{}
	rec := httptest.NewRecorder()

	body := strings.NewReader(
		`{"result":{"response":{"llmInfo":{"modelHash":"hash-1"}}}}` +
			`{"result":{"response":{"modelResponse":{"responseId":"resp_123","message":"hello","metadata":{"llm_info":{"modelHash":"hash-2"}}}}}}`,
	)

	h.collectChat(rec, &ChatCompletionsRequest{Messages: []ChatMessage{{Role: "user", Content: "hello world"}}}, "grok-420", ModelSpec{ID: "grok-420"}, "", "", false, nil, nil, body, nil)

	var obj map[string]interface{}
	if err := json.Unmarshal(rec.Body.Bytes(), &obj); err != nil {
		t.Fatalf("json.Unmarshal() error=%v body=%q", err, rec.Body.String())
	}
	if got, _ := obj["id"].(string); got != "resp_123" {
		t.Fatalf("id=%q want=%q", got, "resp_123")
	}
	if got, _ := obj["system_fingerprint"].(string); got != "hash-1" && got != "hash-2" {
		t.Fatalf("system_fingerprint=%q want non-empty upstream hash", got)
	}
	choices, ok := obj["choices"].([]interface{})
	if !ok || len(choices) == 0 {
		t.Fatalf("choices missing: %#v", obj["choices"])
	}
	choice, _ := choices[0].(map[string]interface{})
	message, _ := choice["message"].(map[string]interface{})
	if _, ok := message["refusal"]; !ok {
		t.Fatalf("message.refusal missing: %#v", message)
	}
	if _, ok := message["annotations"].([]interface{}); !ok {
		t.Fatalf("message.annotations missing: %#v", message)
	}
	usage, ok := obj["usage"].(map[string]interface{})
	if !ok {
		t.Fatalf("usage missing: %#v", obj["usage"])
	}
	if _, ok := usage["prompt_tokens_details"].(map[string]interface{}); !ok {
		t.Fatalf("prompt_tokens_details missing: %#v", usage)
	}
	if _, ok := usage["completion_tokens_details"].(map[string]interface{}); !ok {
		t.Fatalf("completion_tokens_details missing: %#v", usage)
	}
	if got, _ := usage["prompt_tokens"].(float64); got <= 0 {
		t.Fatalf("expected positive prompt_tokens, usage=%#v", usage)
	}
	if got, _ := usage["completion_tokens"].(float64); got <= 0 {
		t.Fatalf("expected positive completion_tokens, usage=%#v", usage)
	}
}

func TestCollectChat_PrependsPublicBaseForCachedVideoURL(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "video/mp4")
		_, _ = w.Write(bytes.Repeat([]byte{1, 2, 3, 4}, 1024))
	}))
	defer server.Close()

	h := &Handler{client: New(nil)}
	rec := httptest.NewRecorder()
	body := strings.NewReader(
		`{"result":{"response":{"streamingVideoGenerationResponse":{"progress":100,"videoUrl":"` + server.URL + `/demo.mp4"}}}}`,
	)

	h.collectChat(rec, &ChatCompletionsRequest{Messages: []ChatMessage{{Role: "user", Content: "make a video"}}}, "grok-420", ModelSpec{ID: "grok-420", IsVideo: true}, "", "https://example.com", false, nil, nil, body, nil)

	var obj map[string]interface{}
	if err := json.Unmarshal(rec.Body.Bytes(), &obj); err != nil {
		t.Fatalf("json.Unmarshal() error=%v body=%q", err, rec.Body.String())
	}
	choices, _ := obj["choices"].([]interface{})
	choice, _ := choices[0].(map[string]interface{})
	message, _ := choice["message"].(map[string]interface{})
	content, _ := message["content"].(string)
	if !strings.Contains(content, "https://example.com/grok/v1/files/video/") {
		t.Fatalf("expected public base prefixed cached video url, content=%q", content)
	}
}

func TestCollapseDuplicatedLongChunk(t *testing.T) {
	dup := "Hi! How can I help you today?Hi! How can I help you today?"
	if got := collapseDuplicatedLongChunk(dup); got != "Hi! How can I help you today?" {
		t.Fatalf("collapseDuplicatedLongChunk()=%q", got)
	}

	shortDup := "haha" + "haha"
	if got := collapseDuplicatedLongChunk(shortDup); got != shortDup {
		t.Fatalf("short duplicated chunk should not collapse, got=%q", got)
	}
}

func TestStreamChat_DedupsGreetingRepeat(t *testing.T) {
	h := &Handler{}
	rec := httptest.NewRecorder()

	dup := "Hi! How can I help you today?Hi! How can I help you today?"
	body := strings.NewReader(
		`{"result":{"response":{"token":"` + dup + `"}}}` +
			`{"result":{"response":{"token":"` + dup + `"}}}`,
	)

	h.streamChat(rec, &ChatCompletionsRequest{Messages: []ChatMessage{{Role: "user", Content: "Hi"}}}, "grok-420", ModelSpec{ID: "grok-420"}, "", "", true, nil, nil, body, nil)
	contents := extractStreamTextContents(t, rec.Body.String())
	combined := strings.Join(contents, "")
	if strings.Count(combined, "Hi! How can I help you today?") != 1 {
		t.Fatalf("expected greeting once, combined=%q raw=%q", combined, rec.Body.String())
	}
	if strings.Contains(combined, dup) {
		t.Fatalf("unexpected duplicated greeting in stream, combined=%q", combined)
	}
}

func TestStreamChat_PrefersModelResponseOverNoisyTokens(t *testing.T) {
	h := &Handler{}
	rec := httptest.NewRecorder()

	body := strings.NewReader(
		`{"result":{"response":{"token":"你！rok，"}}}` +
			`{"result":{"response":{"modelResponse":{"message":"你好！我是 Grok，xAI AI 助手。"}}}}` +
			`{"result":{"response":{"modelResponse":{"message":"你好！我是 Grok，xAI AI 助手，不是之前提到的那个。"}}}}`,
	)

	h.streamChat(rec, &ChatCompletionsRequest{Messages: []ChatMessage{{Role: "user", Content: "你好"}}}, "grok-420", ModelSpec{ID: "grok-420"}, "", "", false, nil, nil, body, nil)
	combined := strings.Join(extractStreamTextContents(t, rec.Body.String()), "")
	if strings.Contains(combined, "你！rok，") {
		t.Fatalf("unexpected noisy token leak, combined=%q raw=%q", combined, rec.Body.String())
	}
	if !strings.Contains(combined, "你好！我是 Grok，xAI AI 助手，不是之前提到的那个。") {
		t.Fatalf("expected final modelResponse text, combined=%q raw=%q", combined, rec.Body.String())
	}
	finalContent := extractStreamFinalMessageContent(t, rec.Body.String())
	if finalContent != "你好！我是 Grok，xAI AI 助手，不是之前提到的那个。" {
		t.Fatalf("expected final snapshot message, got=%q raw=%q", finalContent, rec.Body.String())
	}
}

func TestStreamChat_FallsBackToTokenWhenModelResponseMissing(t *testing.T) {
	h := &Handler{}
	rec := httptest.NewRecorder()

	body := strings.NewReader(
		`{"result":{"response":{"token":"你好"}}}` +
			`{"result":{"response":{"token":"！我是 Grok。"}}}`,
	)

	h.streamChat(rec, &ChatCompletionsRequest{Messages: []ChatMessage{{Role: "user", Content: "你好"}}}, "grok-420", ModelSpec{ID: "grok-420"}, "", "", false, nil, nil, body, nil)
	combined := strings.Join(extractStreamTextContents(t, rec.Body.String()), "")
	if !strings.Contains(combined, "你好！我是 Grok。") {
		t.Fatalf("expected token fallback text, combined=%q raw=%q", combined, rec.Body.String())
	}
	finalContent := extractStreamFinalMessageContent(t, rec.Body.String())
	if finalContent != "你好！我是 Grok。" {
		t.Fatalf("expected token fallback final snapshot, got=%q raw=%q", finalContent, rec.Body.String())
	}
}

func TestStreamChat_RewriteFallsBackToFinalSnapshot(t *testing.T) {
	h := &Handler{}
	rec := httptest.NewRecorder()

	body := strings.NewReader(
		`{"result":{"response":{"modelResponse":{"message":"The answer is 4"}}}}` +
			`{"result":{"response":{"modelResponse":{"message":"The answer is 42"}}}}` +
			`{"result":{"response":{"modelResponse":{"message":"The result is 42"}}}}`,
	)

	h.streamChat(rec, &ChatCompletionsRequest{Messages: []ChatMessage{{Role: "user", Content: "answer?"}}}, "grok-420", ModelSpec{ID: "grok-420"}, "", "", false, nil, nil, body, nil)
	combined := strings.Join(extractStreamTextContents(t, rec.Body.String()), "")
	if strings.Contains(combined, "The answer is 4The result is 42") {
		t.Fatalf("expected rewrite suffix to stay out of delta stream, combined=%q raw=%q", combined, rec.Body.String())
	}
	finalContent := extractStreamFinalMessageContent(t, rec.Body.String())
	if finalContent != "The result is 42" {
		t.Fatalf("expected final snapshot to carry rewritten text, got=%q raw=%q", finalContent, rec.Body.String())
	}
}

func TestStreamChat_EmitsToolCallsChunk(t *testing.T) {
	h := &Handler{}
	rec := httptest.NewRecorder()

	body := strings.NewReader(
		`{"result":{"response":{"modelResponse":{"message":"<tool_call>{\"name\":\"weather\",\"arguments\":{\"city\":\"shanghai\"}}</tool_call>"}}}}`,
	)

	h.streamChat(rec, &ChatCompletionsRequest{Messages: []ChatMessage{{Role: "user", Content: "weather?"}}}, "grok-420", ModelSpec{ID: "grok-420"}, "", "", true, []ToolDef{{
		Type: "function",
		Function: map[string]interface{}{
			"name": "weather",
		},
	}}, "required", body, nil)

	raw := rec.Body.String()
	if strings.Contains(raw, "<tool_call>") {
		t.Fatalf("raw tool markup leaked: %q", raw)
	}
	if !strings.Contains(raw, `"tool_calls"`) {
		t.Fatalf("expected tool_calls chunk, raw=%q", raw)
	}
	if !strings.Contains(raw, `"finish_reason":"tool_calls"`) {
		t.Fatalf("expected tool_calls finish reason, raw=%q", raw)
	}
}

func TestStreamChat_EmitsToolCallsBeforeDone(t *testing.T) {
	h := &Handler{}
	rec := httptest.NewRecorder()

	body := strings.NewReader(
		`{"result":{"response":{"modelResponse":{"message":"checking\n<tool_call>{\"name\":\"weather\",\"arguments\":{\"city\":\"shanghai\"}}</tool_call>"}}}}` +
			`{"result":{"response":{"token":"ignored"}}}`,
	)

	h.streamChat(rec, &ChatCompletionsRequest{Messages: []ChatMessage{{Role: "user", Content: "weather?"}}}, "grok-420", ModelSpec{ID: "grok-420"}, "", "", true, []ToolDef{{
		Type: "function",
		Function: map[string]interface{}{
			"name": "weather",
		},
	}}, "required", body, nil)

	raw := rec.Body.String()
	idxTool := strings.Index(raw, `"tool_calls"`)
	idxDone := strings.LastIndex(raw, `[DONE]`)
	if idxTool < 0 || idxDone < 0 {
		t.Fatalf("expected tool_calls and DONE, raw=%q", raw)
	}
	if idxTool > idxDone {
		t.Fatalf("tool_calls should be emitted before DONE, raw=%q", raw)
	}
}

func TestStreamChat_ParsesSplitToolCallsFromTokenStream(t *testing.T) {
	h := &Handler{}
	rec := httptest.NewRecorder()

	body := strings.NewReader(
		`{"result":{"response":{"token":"before <tool_"}}}` +
			`{"result":{"response":{"token":"call>{\"name\":\"weather\","}}}` +
			`{"result":{"response":{"token":"\"arguments\":{\"city\":\"shanghai\"}}</tool_call> after"}}}`,
	)

	h.streamChat(rec, &ChatCompletionsRequest{Messages: []ChatMessage{{Role: "user", Content: "weather?"}}}, "grok-420", ModelSpec{ID: "grok-420"}, "", "", true, []ToolDef{{
		Type: "function",
		Function: map[string]interface{}{
			"name": "weather",
		},
	}}, "required", body, nil)

	raw := rec.Body.String()
	if strings.Contains(raw, "<tool_call>") {
		t.Fatalf("raw tool markup leaked: %q", raw)
	}
	if !strings.Contains(raw, `"tool_calls"`) {
		t.Fatalf("expected streamed tool_calls chunk, raw=%q", raw)
	}
	if !strings.Contains(raw, `"index":0`) {
		t.Fatalf("expected indexed tool_calls chunk, raw=%q", raw)
	}
	if !strings.Contains(raw, `"content":"before `) {
		t.Fatalf("expected text before tool call to stream, raw=%q", raw)
	}
	if !strings.Contains(raw, `"content":" after"`) {
		t.Fatalf("expected trailing text after tool call to stream, raw=%q", raw)
	}
}

func TestStreamChat_EmitsSystemFingerprint(t *testing.T) {
	h := &Handler{}
	rec := httptest.NewRecorder()

	body := strings.NewReader(
		`{"result":{"response":{"llmInfo":{"modelHash":"hash-123"},"token":"hello"}}}`,
	)

	h.streamChat(rec, &ChatCompletionsRequest{Messages: []ChatMessage{{Role: "user", Content: "hello"}}}, "grok-420", ModelSpec{ID: "grok-420"}, "", "", false, nil, nil, body, nil)
	raw := rec.Body.String()
	if !strings.Contains(raw, `"system_fingerprint":"hash-123"`) {
		t.Fatalf("expected stream fingerprint, raw=%q", raw)
	}
	if !strings.Contains(raw, `"role":"assistant","content":""`) {
		t.Fatalf("expected assistant role chunk with empty content, raw=%q", raw)
	}
}

func TestStreamChat_AcceptsAlternateUpstreamEventShape(t *testing.T) {
	h := &Handler{}
	rec := httptest.NewRecorder()

	body := strings.NewReader(
		`{"result":{"response":{"llm_info":{"model_hash":"hash-alt"},"response_id":"resp_alt","delta":"hello "}}}` +
			`{"result":{"response":{"model_response":{"response_id":"resp_alt_2","text":"hello world"}}}}`,
	)

	h.streamChat(rec, &ChatCompletionsRequest{Messages: []ChatMessage{{Role: "user", Content: "hello"}}}, "grok-420", ModelSpec{ID: "grok-420"}, "", "", false, nil, nil, body, nil)
	raw := rec.Body.String()
	if !strings.Contains(raw, `"system_fingerprint":"hash-alt"`) {
		t.Fatalf("expected alternate fingerprint to be accepted, raw=%q", raw)
	}
	if !strings.Contains(raw, `"id":"resp_alt"`) && !strings.Contains(raw, `"id":"resp_alt_2"`) {
		t.Fatalf("expected alternate response id to be accepted, raw=%q", raw)
	}
	if !strings.Contains(raw, `"content":"hello world"`) {
		t.Fatalf("expected alternate text shape to stream, raw=%q", raw)
	}
}

func TestStreamChat_ParseErrorUsesSSEErrorEvent(t *testing.T) {
	h := &Handler{}
	rec := httptest.NewRecorder()

	body := strings.NewReader(`{"result":{"response":{"token":"hello"}}`)

	h.streamChat(rec, &ChatCompletionsRequest{Messages: []ChatMessage{{Role: "user", Content: "hello"}}}, "grok-420", ModelSpec{ID: "grok-420"}, "", "", false, nil, nil, body, nil)
	raw := rec.Body.String()
	if !strings.Contains(raw, "event: error") {
		t.Fatalf("expected SSE error event, raw=%q", raw)
	}
	if !strings.Contains(raw, `"code":"stream_error"`) {
		t.Fatalf("expected stream_error code, raw=%q", raw)
	}
	if !strings.Contains(raw, "[DONE]") {
		t.Fatalf("expected DONE after error, raw=%q", raw)
	}
}

func TestStreamChat_PrependsPublicBaseForCachedVideoURL(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "video/mp4")
		_, _ = w.Write(bytes.Repeat([]byte{5, 6, 7, 8}, 1024))
	}))
	defer server.Close()

	h := &Handler{client: New(nil)}
	rec := httptest.NewRecorder()
	body := strings.NewReader(
		`{"result":{"response":{"streamingVideoGenerationResponse":{"progress":100,"videoUrl":"` + server.URL + `/demo.mp4"}}}}`,
	)

	h.streamChat(rec, &ChatCompletionsRequest{Messages: []ChatMessage{{Role: "user", Content: "make a video"}}}, "grok-420", ModelSpec{ID: "grok-420", IsVideo: true}, "", "https://example.com", false, nil, nil, body, nil)
	combined := strings.Join(extractStreamTextContents(t, rec.Body.String()), "")
	if !strings.Contains(combined, "https://example.com/grok/v1/files/video/") {
		t.Fatalf("expected public base prefixed cached video url, combined=%q raw=%q", combined, rec.Body.String())
	}
}

func TestStreamMarkupFilter_CaseInsensitiveToolCard(t *testing.T) {
	filter := &streamMarkupFilter{}
	in := `<XAI:TOOL_USAGE_CARD><XAI:TOOL_NAME>web_search</XAI:TOOL_NAME><XAI:TOOL_ARGS>{"query":"hello"}</XAI:TOOL_ARGS></XAI:TOOL_USAGE_CARD>`
	out := filter.feed(in) + filter.flush()
	if !strings.Contains(out, "[WebSearch] hello") {
		t.Fatalf("expected parsed tool card text, got=%q", out)
	}
}

func TestIndexFoldASCII(t *testing.T) {
	if got := indexFoldASCII("abc<GROK:RENDER>x", "<grok:render"); got != 3 {
		t.Fatalf("indexFoldASCII case-insensitive mismatch got=%d want=3", got)
	}
	if got := indexFoldASCII("abc", "xyz"); got != -1 {
		t.Fatalf("indexFoldASCII should return -1, got=%d", got)
	}
}

func TestStreamTextImageRefCollector_CrossChunkAndDedup(t *testing.T) {
	c := newStreamTextImageRefCollector()
	c.feed("prefix https://assets.grok.com/users/u-1/generated/a1/image.p")
	c.feed("ng?x=1 middle users/u-2/generated/a2/image.we")
	c.feed("bp suffix https://assets.grok.com/users/u-1/generated/a1/image.png?x=1")

	out := make([]string, 0, 4)
	c.emit(func(u string) {
		out = append(out, u)
	})

	wantDirect := "https://assets.grok.com/users/u-1/generated/a1/image.png?x=1"
	wantAsset := "https://assets.grok.com/users/u-2/generated/a2/image.webp"

	hasDirect := false
	hasAsset := false
	directCount := 0
	for _, u := range out {
		if u == wantDirect {
			hasDirect = true
			directCount++
		}
		if u == wantAsset {
			hasAsset = true
		}
	}

	if !hasDirect {
		t.Fatalf("missing direct url, out=%v", out)
	}
	if !hasAsset {
		t.Fatalf("missing asset url, out=%v", out)
	}
	if directCount != 1 {
		t.Fatalf("direct url should be deduped, count=%d out=%v", directCount, out)
	}
}

func extractStreamTextContents(t *testing.T, raw string) []string {
	t.Helper()
	lines := strings.Split(raw, "\n")
	out := make([]string, 0, 4)
	for _, line := range lines {
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		payload := strings.TrimPrefix(line, "data: ")
		if payload == "[DONE]" || strings.TrimSpace(payload) == "" {
			continue
		}
		var obj map[string]interface{}
		if err := json.Unmarshal([]byte(payload), &obj); err != nil {
			continue
		}
		choices, ok := obj["choices"].([]interface{})
		if !ok || len(choices) == 0 {
			continue
		}
		choice, ok := choices[0].(map[string]interface{})
		if !ok {
			continue
		}
		delta, ok := choice["delta"].(map[string]interface{})
		if !ok {
			continue
		}
		content, _ := delta["content"].(string)
		if strings.TrimSpace(content) != "" {
			out = append(out, content)
		}
	}
	return out
}

func extractStreamFinalMessageContent(t *testing.T, raw string) string {
	t.Helper()
	lines := strings.Split(raw, "\n")
	for _, line := range lines {
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		payload := strings.TrimPrefix(line, "data: ")
		if payload == "[DONE]" || strings.TrimSpace(payload) == "" {
			continue
		}
		var obj map[string]interface{}
		if err := json.Unmarshal([]byte(payload), &obj); err != nil {
			continue
		}
		choices, ok := obj["choices"].([]interface{})
		if !ok || len(choices) == 0 {
			continue
		}
		choice, ok := choices[0].(map[string]interface{})
		if !ok {
			continue
		}
		message, ok := choice["message"].(map[string]interface{})
		if !ok {
			continue
		}
		if content, _ := message["content"].(string); strings.TrimSpace(content) != "" {
			return content
		}
	}
	return ""
}

func TestAppendChatCompletionChunkMatchesMapEncoding(t *testing.T) {
	tests := []struct {
		name      string
		role      string
		content   string
		finish    string
		hasFinish bool
	}{
		{name: "role", role: "assistant"},
		{name: "content", content: "hello world"},
		{name: "stop", finish: "stop", hasFinish: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			raw := appendChatCompletionChunk(make([]byte, 0, 256), "chatcmpl_1", 123, "grok-4", "fp-1", tt.role, tt.content, tt.finish, tt.hasFinish)
			var got map[string]interface{}
			if err := json.Unmarshal(raw, &got); err != nil {
				t.Fatalf("unmarshal got: %v", err)
			}

			delta := map[string]interface{}{}
			if tt.role != "" {
				delta["role"] = tt.role
				delta["content"] = ""
			}
			if tt.content != "" {
				delta["content"] = tt.content
			}
			finish := interface{}(nil)
			if tt.hasFinish {
				finish = tt.finish
			}
			wantRaw := encodeJSONBytes(map[string]interface{}{
				"id":                 "chatcmpl_1",
				"object":             "chat.completion.chunk",
				"created":            int64(123),
				"model":              "grok-4",
				"service_tier":       nil,
				"system_fingerprint": "fp-1",
				"choices": []map[string]interface{}{{
					"index":         0,
					"delta":         delta,
					"logprobs":      nil,
					"finish_reason": finish,
				}},
			})
			if tt.hasFinish {
				var wantObj map[string]interface{}
				if err := json.Unmarshal(wantRaw, &wantObj); err != nil {
					t.Fatalf("unmarshal wantRaw: %v", err)
				}
				wantObj["usage"] = map[string]interface{}{
					"prompt_tokens":     float64(0),
					"completion_tokens": float64(0),
					"total_tokens":      float64(0),
					"prompt_tokens_details": map[string]interface{}{
						"cached_tokens": float64(0),
						"text_tokens":   float64(0),
						"audio_tokens":  float64(0),
						"image_tokens":  float64(0),
					},
					"completion_tokens_details": map[string]interface{}{
						"text_tokens":      float64(0),
						"audio_tokens":     float64(0),
						"reasoning_tokens": float64(0),
					},
				}
				wantRaw = encodeJSONBytes(wantObj)
			}
			var want map[string]interface{}
			if err := json.Unmarshal(wantRaw, &want); err != nil {
				t.Fatalf("unmarshal want: %v", err)
			}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("got=%#v want=%#v", got, want)
			}
		})
	}
}

func TestAppendChatCompletionSnapshotChunk_FinalIncludesMessage(t *testing.T) {
	raw := appendChatCompletionSnapshotChunkWithUsage(make([]byte, 0, 256), "chatcmpl_1", 123, "grok-4", "fp-1", "hello world", "stop", true, nil)
	var got map[string]interface{}
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshal got: %v", err)
	}
	choices, ok := got["choices"].([]interface{})
	if !ok || len(choices) == 0 {
		t.Fatalf("choices missing: %#v", got)
	}
	choice, _ := choices[0].(map[string]interface{})
	delta, _ := choice["delta"].(map[string]interface{})
	if len(delta) != 0 {
		t.Fatalf("expected empty delta, got=%#v", delta)
	}
	message, _ := choice["message"].(map[string]interface{})
	if content, _ := message["content"].(string); content != "hello world" {
		t.Fatalf("message.content=%q want=%q", content, "hello world")
	}
	if finish, _ := choice["finish_reason"].(string); finish != "stop" {
		t.Fatalf("finish_reason=%q want=stop", finish)
	}
	if _, ok := got["usage"].(map[string]interface{}); !ok {
		t.Fatalf("usage missing: %#v", got)
	}
}

func TestAppendChatCompletionToolCallsChunk_FinalIncludesUsage(t *testing.T) {
	raw := appendChatCompletionToolCallsChunk(make([]byte, 0, 512), "chatcmpl_1", 123, "grok-4", "fp-1", []map[string]interface{}{{
		"index": 0,
		"id":    "call_1",
		"type":  "function",
		"function": map[string]interface{}{
			"name":      "weather",
			"arguments": `{"city":"shanghai"}`,
		},
	}}, "tool_calls", true)
	var got map[string]interface{}
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshal got: %v", err)
	}
	if _, ok := got["usage"].(map[string]interface{}); !ok {
		t.Fatalf("usage missing: %#v", got)
	}
	if _, ok := got["service_tier"]; !ok {
		t.Fatalf("service_tier missing: %#v", got)
	}
}

func TestBuildChatUsagePayload_TracksPromptAndCompletionTokens(t *testing.T) {
	req := &ChatCompletionsRequest{
		Messages: []ChatMessage{
			{Role: "system", Content: "you are helpful"},
			{Role: "user", Content: []interface{}{
				map[string]interface{}{"type": "text", "text": "describe this"},
				map[string]interface{}{"type": "image_url", "image_url": map[string]interface{}{"url": "https://example.com/a.png"}},
			}},
		},
	}
	usage := buildChatUsagePayload(req, "here is the answer", nil)
	if got, _ := usage["prompt_tokens"].(int); got <= 0 {
		t.Fatalf("expected prompt tokens > 0, usage=%#v", usage)
	}
	promptDetails, _ := usage["prompt_tokens_details"].(map[string]interface{})
	if got, _ := promptDetails["image_tokens"].(int); got <= 0 {
		t.Fatalf("expected image prompt tokens > 0, usage=%#v", usage)
	}
	if got, _ := usage["completion_tokens"].(int); got <= 0 {
		t.Fatalf("expected completion tokens > 0, usage=%#v", usage)
	}
}

func BenchmarkAppendChatCompletionChunk_Content(b *testing.B) {
	buf := make([]byte, 0, 256)
	created := time.Now().Unix()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		raw := appendChatCompletionChunk(buf[:0], "chatcmpl_1", created, "grok-4", "fp-1", "", "hello world", "", false)
		buf = raw[:0]
	}
}

func BenchmarkEncodeChatCompletionChunk_Map(b *testing.B) {
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_ = encodeJSONBytes(map[string]interface{}{
			"id":                 "chatcmpl_1",
			"object":             "chat.completion.chunk",
			"created":            int64(123),
			"model":              "grok-4",
			"system_fingerprint": "fp-1",
			"choices": []map[string]interface{}{{
				"index":         0,
				"delta":         map[string]interface{}{"content": "hello world"},
				"logprobs":      nil,
				"finish_reason": nil,
			}},
		})
	}
}
