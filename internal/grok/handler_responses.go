package grok

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/goccy/go-json"
)

type captureResponseWriter struct {
	header http.Header
	body   bytes.Buffer
	code   int
}

func newCaptureResponseWriter() *captureResponseWriter {
	return &captureResponseWriter{header: make(http.Header), code: http.StatusOK}
}

func (w *captureResponseWriter) Header() http.Header {
	return w.header
}

func (w *captureResponseWriter) WriteHeader(code int) {
	if code != 0 {
		w.code = code
	}
}

func (w *captureResponseWriter) Write(p []byte) (int, error) {
	return w.body.Write(p)
}

func (w *captureResponseWriter) Flush() {}

type ResponsesCreateRequest struct {
	Model              string                   `json:"model"`
	Input              interface{}              `json:"input"`
	Instructions       string                   `json:"instructions,omitempty"`
	Stream             bool                     `json:"stream,omitempty"`
	StreamProvided     bool                     `json:"-"`
	Reasoning          map[string]interface{}   `json:"reasoning,omitempty"`
	Temperature        *float64                 `json:"temperature,omitempty"`
	TopP               *float64                 `json:"top_p,omitempty"`
	MaxOutputTokens    *int                     `json:"max_output_tokens,omitempty"`
	Tools              []map[string]interface{} `json:"tools,omitempty"`
	ToolChoice         interface{}              `json:"tool_choice,omitempty"`
	ParallelToolCalls  *bool                    `json:"parallel_tool_calls,omitempty"`
	PreviousResponseID string                   `json:"previous_response_id,omitempty"`
	Store              *bool                    `json:"store,omitempty"`
	Metadata           map[string]interface{}   `json:"metadata,omitempty"`
	Truncation         string                   `json:"truncation,omitempty"`
	Include            []string                 `json:"include,omitempty"`
	Background         *bool                    `json:"background,omitempty"`
}

func (r *ResponsesCreateRequest) UnmarshalJSON(data []byte) error {
	type rawResponsesCreateRequest struct {
		Model              interface{}              `json:"model"`
		Input              interface{}              `json:"input"`
		Instructions       interface{}              `json:"instructions,omitempty"`
		Stream             interface{}              `json:"stream,omitempty"`
		Reasoning          map[string]interface{}   `json:"reasoning,omitempty"`
		Temperature        interface{}              `json:"temperature,omitempty"`
		TopP               interface{}              `json:"top_p,omitempty"`
		MaxOutputTokens    interface{}              `json:"max_output_tokens,omitempty"`
		Tools              []map[string]interface{} `json:"tools,omitempty"`
		ToolChoice         interface{}              `json:"tool_choice,omitempty"`
		ParallelToolCalls  interface{}              `json:"parallel_tool_calls,omitempty"`
		PreviousResponseID interface{}              `json:"previous_response_id,omitempty"`
		Store              interface{}              `json:"store,omitempty"`
		Metadata           map[string]interface{}   `json:"metadata,omitempty"`
		Truncation         interface{}              `json:"truncation,omitempty"`
		Include            []string                 `json:"include,omitempty"`
		Background         interface{}              `json:"background,omitempty"`
	}

	var raw rawResponsesCreateRequest
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	stream, err := parseLooseBoolAny(raw.Stream)
	if err != nil {
		return err
	}
	temp, err := parseLooseFloatAny(raw.Temperature)
	if err != nil {
		return err
	}
	topP, err := parseLooseFloatAny(raw.TopP)
	if err != nil {
		return err
	}
	maxOutputTokens, err := parseLooseIntAny(raw.MaxOutputTokens)
	if err != nil {
		return err
	}
	var rawMap map[string]json.RawMessage
	_ = json.Unmarshal(data, &rawMap)
	_, streamProvided := rawMap["stream"]
	var parallel *bool
	if _, ok := rawMap["parallel_tool_calls"]; ok {
		v, err := parseLooseBoolAnyForField(raw.ParallelToolCalls, "parallel_tool_calls")
		if err != nil {
			return err
		}
		parallel = &v
	}
	var store *bool
	if _, ok := rawMap["store"]; ok {
		v, err := parseLooseBoolAnyForField(raw.Store, "store")
		if err != nil {
			return err
		}
		store = &v
	}
	var background *bool
	if _, ok := rawMap["background"]; ok {
		v, err := parseLooseBoolAnyForField(raw.Background, "background")
		if err != nil {
			return err
		}
		background = &v
	}
	var maxOutput *int
	if _, ok := rawMap["max_output_tokens"]; ok {
		maxOutput = &maxOutputTokens
	}

	r.Model = parseLooseStringAny(raw.Model)
	r.Input = raw.Input
	r.Instructions = parseLooseStringAny(raw.Instructions)
	r.Stream = stream
	r.StreamProvided = streamProvided
	r.Reasoning = raw.Reasoning
	r.Temperature = temp
	r.TopP = topP
	r.MaxOutputTokens = maxOutput
	r.Tools = raw.Tools
	r.ToolChoice = raw.ToolChoice
	r.ParallelToolCalls = parallel
	r.PreviousResponseID = parseLooseStringAny(raw.PreviousResponseID)
	r.Store = store
	r.Metadata = raw.Metadata
	r.Truncation = parseLooseStringAny(raw.Truncation)
	r.Include = raw.Include
	r.Background = background
	return nil
}

func (h *Handler) HandleResponses(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req ResponsesCreateRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid json", http.StatusBadRequest)
		return
	}
	h.applyDefaultResponsesStream(&req)
	chatReq, err := chatRequestFromResponses(req)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	raw, err := json.Marshal(chatReq)
	if err != nil {
		http.Error(w, "failed to build chat request", http.StatusInternalServerError)
		return
	}

	subReq := r.Clone(r.Context())
	subReq.Method = http.MethodPost
	subReq.URL.Path = "/v1/chat/completions"
	subReq.Header = make(http.Header)
	subReq.Header.Set("Content-Type", "application/json")
	subReq.Body = io.NopCloser(bytes.NewReader(raw))
	subReq.ContentLength = int64(len(raw))

	rec := newCaptureResponseWriter()
	h.HandleChatCompletions(rec, subReq)
	if rec.code < 200 || rec.code >= 300 {
		copyCapturedResponse(w, rec)
		return
	}
	if chatReq.Stream {
		writeResponsesStreamFromChat(w, req.Model, rec.body.String())
		return
	}
	var chat map[string]interface{}
	if err := json.Unmarshal(rec.body.Bytes(), &chat); err != nil {
		http.Error(w, "chat response parse error: "+err.Error(), http.StatusBadGateway)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(responsesObjectFromChat(req.Model, chat))
}

func (h *Handler) applyDefaultResponsesStream(req *ResponsesCreateRequest) {
	if req == nil || req.StreamProvided {
		return
	}
	req.Stream = h.defaultChatStream()
}

func chatRequestFromResponses(req ResponsesCreateRequest) (ChatCompletionsRequest, error) {
	model := normalizeModelID(req.Model)
	if strings.TrimSpace(model) == "" {
		return ChatCompletionsRequest{}, fmt.Errorf("model is required")
	}
	messages, err := responsesInputToMessages(req.Input)
	if err != nil {
		return ChatCompletionsRequest{}, err
	}
	if instructions := strings.TrimSpace(req.Instructions); instructions != "" {
		messages = append([]ChatMessage{{Role: "system", Content: instructions}}, messages...)
	}
	reasoningEffort := responsesReasoningEffort(req.Reasoning)
	out := ChatCompletionsRequest{
		Model:             model,
		Messages:          messages,
		Stream:            req.Stream,
		StreamProvided:    true,
		ReasoningEffort:   reasoningEffort,
		Temperature:       req.Temperature,
		TopP:              req.TopP,
		Tools:             responsesToolsToChatTools(req.Tools),
		ToolChoice:        responsesToolChoiceToChat(req.ToolChoice),
		ParallelToolCalls: req.ParallelToolCalls,
	}
	return out, nil
}

func responsesInputToMessages(input interface{}) ([]ChatMessage, error) {
	switch v := input.(type) {
	case nil:
		return nil, fmt.Errorf("input is required")
	case string:
		if strings.TrimSpace(v) == "" {
			return nil, fmt.Errorf("input is required")
		}
		return []ChatMessage{{Role: "user", Content: v}}, nil
	case []interface{}:
		messages := make([]ChatMessage, 0, len(v))
		for _, raw := range v {
			item, _ := raw.(map[string]interface{})
			if item == nil {
				continue
			}
			itemType := strings.ToLower(strings.TrimSpace(fmt.Sprint(item["type"])))
			if itemType == "" {
				if strings.TrimSpace(fmt.Sprint(item["role"])) != "" {
					itemType = "message"
				}
			}
			switch itemType {
			case "function_call":
				name := strings.TrimSpace(fmt.Sprint(item["name"]))
				if name == "" || name == "<nil>" {
					continue
				}
				args := "{}"
				if rawArgs := item["arguments"]; rawArgs != nil {
					switch x := rawArgs.(type) {
					case string:
						if strings.TrimSpace(x) != "" {
							args = strings.TrimSpace(x)
						}
					default:
						if buf, err := json.Marshal(x); err == nil {
							args = string(buf)
						}
					}
				}
				messages = append(messages, ChatMessage{
					Role:    "assistant",
					Content: nil,
					ToolCalls: []ToolCall{{
						ID:   strings.TrimSpace(fmt.Sprint(item["call_id"])),
						Type: "function",
						Function: map[string]interface{}{
							"name":      name,
							"arguments": args,
						},
					}},
				})
			case "function_call_output":
				messages = append(messages, ChatMessage{
					Role:       "tool",
					ToolCallID: strings.TrimSpace(fmt.Sprint(item["call_id"])),
					Content:    strings.TrimSpace(fmt.Sprint(item["output"])),
				})
			case "message":
				role := strings.TrimSpace(fmt.Sprint(item["role"]))
				if role == "" || role == "<nil>" {
					role = "user"
				}
				messages = append(messages, ChatMessage{
					Role:    role,
					Content: normalizeResponsesMessageContent(item["content"]),
				})
			}
		}
		if len(messages) == 0 {
			return nil, fmt.Errorf("input is required")
		}
		return messages, nil
	default:
		return nil, fmt.Errorf("input must be a string or an array")
	}
}

func normalizeResponsesMessageContent(content interface{}) interface{} {
	parts, ok := content.([]interface{})
	if !ok {
		return content
	}
	out := make([]interface{}, 0, len(parts))
	for _, raw := range parts {
		part, _ := raw.(map[string]interface{})
		if part == nil {
			continue
		}
		ptype := strings.ToLower(strings.TrimSpace(fmt.Sprint(part["type"])))
		switch ptype {
		case "input_text", "output_text":
			out = append(out, map[string]interface{}{"type": "text", "text": fmt.Sprint(part["text"])})
		case "input_image", "image":
			if url := responsesImageURL(part); url != "" {
				out = append(out, map[string]interface{}{"type": "image_url", "image_url": map[string]interface{}{"url": url}})
			}
		case "input_file", "file":
			if url := responsesFileURL(part); url != "" {
				out = append(out, map[string]interface{}{"type": "file", "file": map[string]interface{}{"url": url}})
			}
		default:
			out = append(out, part)
		}
	}
	return out
}

func responsesImageURL(part map[string]interface{}) string {
	for _, key := range []string{"image_url", "source"} {
		raw := part[key]
		switch v := raw.(type) {
		case string:
			if strings.TrimSpace(v) != "" {
				return strings.TrimSpace(v)
			}
		case map[string]interface{}:
			if s := strings.TrimSpace(fmt.Sprint(v["url"])); s != "" && s != "<nil>" {
				return s
			}
		}
	}
	return ""
}

func responsesFileURL(part map[string]interface{}) string {
	for _, key := range []string{"file", "file_url", "source"} {
		raw := part[key]
		switch v := raw.(type) {
		case string:
			if strings.TrimSpace(v) != "" {
				return strings.TrimSpace(v)
			}
		case map[string]interface{}:
			for _, nestedKey := range []string{"url", "file_url", "data"} {
				if s := strings.TrimSpace(fmt.Sprint(v[nestedKey])); s != "" && s != "<nil>" {
					return s
				}
			}
		}
	}
	if s := strings.TrimSpace(fmt.Sprint(part["file_id"])); s != "" && s != "<nil>" {
		return s
	}
	return ""
}

func responsesToolsToChatTools(tools []map[string]interface{}) []ToolDef {
	out := make([]ToolDef, 0, len(tools))
	for _, tool := range tools {
		if !strings.EqualFold(strings.TrimSpace(fmt.Sprint(tool["type"])), "function") {
			continue
		}
		if fn, _ := tool["function"].(map[string]interface{}); fn != nil {
			if strings.TrimSpace(fmt.Sprint(fn["name"])) != "" {
				out = append(out, ToolDef{Type: "function", Function: fn})
			}
			continue
		}
		name := strings.TrimSpace(fmt.Sprint(tool["name"]))
		if name == "" || name == "<nil>" {
			continue
		}
		out = append(out, ToolDef{Type: "function", Function: map[string]interface{}{
			"name":        name,
			"description": strings.TrimSpace(fmt.Sprint(tool["description"])),
			"parameters":  firstNonNil(tool["parameters"], map[string]interface{}{}),
		}})
	}
	return out
}

func responsesToolChoiceToChat(choice interface{}) interface{} {
	if choice == nil {
		return nil
	}
	m, _ := choice.(map[string]interface{})
	if m == nil || !strings.EqualFold(strings.TrimSpace(fmt.Sprint(m["type"])), "function") {
		return choice
	}
	if _, ok := m["function"].(map[string]interface{}); ok {
		return choice
	}
	name := strings.TrimSpace(fmt.Sprint(m["name"]))
	if name == "" || name == "<nil>" {
		return choice
	}
	return map[string]interface{}{"type": "function", "function": map[string]interface{}{"name": name}}
}

func responsesReasoningEffort(reasoning map[string]interface{}) *string {
	if len(reasoning) == 0 {
		return nil
	}
	if effort := strings.ToLower(strings.TrimSpace(fmt.Sprint(reasoning["effort"]))); effort != "" && effort != "<nil>" {
		return &effort
	}
	return nil
}

func firstNonNil(values ...interface{}) interface{} {
	for _, v := range values {
		if v != nil {
			return v
		}
	}
	return nil
}

func responsesObjectFromChat(model string, chat map[string]interface{}) map[string]interface{} {
	output := responsesOutputFromChat(chat)
	return map[string]interface{}{
		"id":                  "resp_" + randomHex(12),
		"object":              "response",
		"created_at":          time.Now().Unix(),
		"status":              "completed",
		"model":               firstNonEmpty(strings.TrimSpace(fmt.Sprint(chat["model"])), model),
		"output":              output,
		"parallel_tool_calls": true,
		"tool_choice":         "auto",
		"usage":               responsesUsageFromChat(chat["usage"]),
	}
}

func responsesOutputFromChat(chat map[string]interface{}) []interface{} {
	choices := interfaceSlice(chat["choices"])
	if len(choices) == 0 {
		return []interface{}{}
	}
	choice, _ := choices[0].(map[string]interface{})
	message, _ := choice["message"].(map[string]interface{})
	if message == nil {
		return []interface{}{}
	}
	out := make([]interface{}, 0, 1)
	for _, raw := range interfaceSlice(message["tool_calls"]) {
		call, _ := raw.(map[string]interface{})
		if item := responseFunctionCallItem(call); item != nil {
			out = append(out, item)
		}
	}
	text := strings.TrimSpace(fmt.Sprint(message["content"]))
	if text != "" && text != "<nil>" {
		part := map[string]interface{}{
			"type":        "output_text",
			"text":        text,
			"annotations": firstNonNil(message["annotations"], []interface{}{}),
		}
		out = append(out, map[string]interface{}{
			"id":      "msg_" + randomHex(12),
			"type":    "message",
			"status":  "completed",
			"role":    "assistant",
			"content": []interface{}{part},
		})
	}
	return out
}

func responseFunctionCallItem(call map[string]interface{}) map[string]interface{} {
	if call == nil {
		return nil
	}
	fn, _ := call["function"].(map[string]interface{})
	name := strings.TrimSpace(fmt.Sprint(fn["name"]))
	if name == "" || name == "<nil>" {
		return nil
	}
	args := strings.TrimSpace(fmt.Sprint(fn["arguments"]))
	if args == "" || args == "<nil>" {
		args = "{}"
	}
	return map[string]interface{}{
		"id":        "fc_" + randomHex(12),
		"type":      "function_call",
		"call_id":   firstNonEmpty(strings.TrimSpace(fmt.Sprint(call["id"])), "call_"+randomHex(12)),
		"name":      name,
		"arguments": args,
		"status":    "completed",
	}
}

func responsesUsageFromChat(raw interface{}) map[string]interface{} {
	usage, _ := raw.(map[string]interface{})
	if usage == nil {
		return map[string]interface{}{"input_tokens": 0, "output_tokens": 0, "total_tokens": 0}
	}
	input := intFromAny(firstNonNil(usage["prompt_tokens"], usage["input_tokens"]))
	output := intFromAny(firstNonNil(usage["completion_tokens"], usage["output_tokens"]))
	total := intFromAny(usage["total_tokens"])
	if total == 0 {
		total = input + output
	}
	return map[string]interface{}{
		"input_tokens":  input,
		"output_tokens": output,
		"total_tokens":  total,
	}
}

func copyCapturedResponse(w http.ResponseWriter, rec *captureResponseWriter) {
	for k, values := range rec.Header() {
		for _, v := range values {
			w.Header().Add(k, v)
		}
	}
	code := rec.code
	if code == 0 {
		code = http.StatusOK
	}
	w.WriteHeader(code)
	_, _ = w.Write(rec.body.Bytes())
}

func writeResponsesStreamFromChat(w http.ResponseWriter, model, raw string) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	id := "resp_" + randomHex(12)
	messageID := "msg_" + randomHex(12)
	startedMessage := false
	var text strings.Builder
	var output []interface{}
	var usage map[string]interface{}

	writeSSEJSON := func(event string, payload map[string]interface{}) {
		writeSSEBytes(w, event, mustJSON(payload))
	}
	writeSSEJSON("response.created", map[string]interface{}{
		"type": "response.created",
		"response": map[string]interface{}{
			"id": id, "object": "response", "created_at": time.Now().Unix(), "status": "in_progress", "model": model, "output": []interface{}{},
		},
	})

	scanner := bufio.NewScanner(strings.NewReader(raw))
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if data == "" || data == "[DONE]" {
			continue
		}
		var chunk map[string]interface{}
		if err := json.Unmarshal([]byte(data), &chunk); err != nil {
			continue
		}
		if u := responsesUsageFromChat(chunk["usage"]); intFromAny(u["total_tokens"]) > 0 {
			usage = u
		}
		choices := interfaceSlice(chunk["choices"])
		if len(choices) == 0 {
			continue
		}
		choice, _ := choices[0].(map[string]interface{})
		delta, _ := choice["delta"].(map[string]interface{})
		if content, ok := delta["content"].(string); ok && content != "" {
			if !startedMessage {
				startedMessage = true
				writeSSEJSON("response.output_item.added", map[string]interface{}{
					"type": "response.output_item.added", "output_index": len(output),
					"item": map[string]interface{}{"id": messageID, "type": "message", "role": "assistant", "content": []interface{}{}, "status": "in_progress"},
				})
				writeSSEJSON("response.content_part.added", map[string]interface{}{
					"type": "response.content_part.added", "item_id": messageID, "output_index": len(output), "content_index": 0,
					"part": map[string]interface{}{"type": "output_text", "text": "", "annotations": []interface{}{}},
				})
			}
			text.WriteString(content)
			writeSSEJSON("response.output_text.delta", map[string]interface{}{
				"type": "response.output_text.delta", "item_id": messageID, "output_index": len(output), "content_index": 0, "delta": content,
			})
		}
		for _, rawCall := range interfaceSlice(delta["tool_calls"]) {
			call, _ := rawCall.(map[string]interface{})
			item := responseFunctionCallItem(call)
			if item == nil {
				continue
			}
			outputIndex := len(output)
			output = append(output, item)
			writeSSEJSON("response.output_item.added", map[string]interface{}{
				"type": "response.output_item.added", "output_index": outputIndex,
				"item": map[string]interface{}{"id": item["id"], "type": "function_call", "call_id": item["call_id"], "name": item["name"], "arguments": "", "status": "in_progress"},
			})
			writeSSEJSON("response.function_call_arguments.delta", map[string]interface{}{
				"type": "response.function_call_arguments.delta", "item_id": item["id"], "output_index": outputIndex, "delta": item["arguments"],
			})
			writeSSEJSON("response.function_call_arguments.done", map[string]interface{}{
				"type": "response.function_call_arguments.done", "item_id": item["id"], "output_index": outputIndex, "arguments": item["arguments"],
			})
			writeSSEJSON("response.output_item.done", map[string]interface{}{
				"type": "response.output_item.done", "output_index": outputIndex, "item": item,
			})
		}
	}
	if startedMessage {
		outputIndex := len(output)
		fullText := text.String()
		part := map[string]interface{}{"type": "output_text", "text": fullText, "annotations": []interface{}{}}
		msg := map[string]interface{}{"id": messageID, "type": "message", "status": "completed", "role": "assistant", "content": []interface{}{part}}
		output = append(output, msg)
		writeSSEJSON("response.output_text.done", map[string]interface{}{
			"type": "response.output_text.done", "item_id": messageID, "output_index": outputIndex, "content_index": 0, "text": fullText,
		})
		writeSSEJSON("response.content_part.done", map[string]interface{}{
			"type": "response.content_part.done", "item_id": messageID, "output_index": outputIndex, "content_index": 0, "part": part,
		})
		writeSSEJSON("response.output_item.done", map[string]interface{}{
			"type": "response.output_item.done", "output_index": outputIndex, "item": msg,
		})
	}
	if usage == nil {
		usage = map[string]interface{}{"input_tokens": 0, "output_tokens": 0, "total_tokens": 0}
	}
	writeSSEJSON("response.completed", map[string]interface{}{
		"type": "response.completed",
		"response": map[string]interface{}{
			"id": id, "object": "response", "created_at": time.Now().Unix(), "status": "completed", "model": model, "output": output, "usage": usage,
		},
	})
	writeSSEBytes(w, "", []byte("[DONE]"))
}

func mustJSON(v interface{}) []byte {
	raw, err := json.Marshal(v)
	if err != nil {
		return []byte(`{}`)
	}
	return raw
}
