package grok

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/goccy/go-json"
)

const consoleResponsesURL = "https://console.x.ai/v1/responses"

type consoleContentBlock struct {
	Type string `json:"type"`
	Text string `json:"text,omitempty"`
	URL  string `json:"image_url,omitempty"`
}

type consoleFunctionCallItem struct {
	Type      string `json:"type"`
	CallID    string `json:"call_id"`
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

type consoleFunctionCallOutputItem struct {
	Type   string `json:"type"`
	CallID string `json:"call_id"`
	Output string `json:"output"`
}

type consoleMessageItem struct {
	Role    string                `json:"role"`
	Content []consoleContentBlock `json:"content"`
}

func consoleInputFromMessages(messages []ChatMessage) ([]interface{}, string) {
	items := make([]interface{}, 0, len(messages))
	var instructions strings.Builder
	for _, msg := range messages {
		role := strings.ToLower(strings.TrimSpace(msg.Role))
		text := strings.TrimSpace(chatMessageContentText(msg.Content))
		if role == "system" || role == "developer" {
			if instructions.Len() > 0 {
				instructions.WriteString("\n\n")
			}
			instructions.WriteString(text)
			continue
		}
		if role == "tool" {
			callID := strings.TrimSpace(msg.ToolCallID)
			if callID == "" {
				callID = strings.TrimSpace(msg.Name)
			}
			items = append(items, consoleFunctionCallOutputItem{
				Type:   "function_call_output",
				CallID: callID,
				Output: text,
			})
			continue
		}
		if role == "assistant" && len(msg.ToolCalls) > 0 {
			for _, tc := range msg.ToolCalls {
				name := strings.TrimSpace(fmt.Sprint(tc.Function["name"]))
				if name == "" {
					continue
				}
				args := "{}"
				if raw := tc.Function["arguments"]; raw != nil {
					switch v := raw.(type) {
					case string:
						if strings.TrimSpace(v) != "" {
							args = strings.TrimSpace(v)
						}
					default:
						if buf, err := json.Marshal(v); err == nil {
							args = string(buf)
						}
					}
				}
				callID := strings.TrimSpace(tc.ID)
				if callID == "" {
					callID = "call_" + randomHex(12)
				}
				items = append(items, consoleFunctionCallItem{
					Type:      "function_call",
					CallID:    callID,
					Name:      name,
					Arguments: args,
				})
			}
		}
		if text == "" {
			continue
		}
		contentType := "input_text"
		if role == "assistant" {
			contentType = "output_text"
		}
		if role != "assistant" {
			role = "user"
		}
		items = append(items, consoleMessageItem{
			Role: role,
			Content: []consoleContentBlock{{
				Type: contentType,
				Text: text,
			}},
		})
	}
	return items, strings.TrimSpace(instructions.String())
}

func chatMessageContentText(content interface{}) string {
	switch v := content.(type) {
	case nil:
		return ""
	case string:
		return v
	case []interface{}:
		var b strings.Builder
		for _, part := range v {
			m, ok := part.(map[string]interface{})
			if !ok {
				continue
			}
			t := strings.ToLower(strings.TrimSpace(fmt.Sprint(m["type"])))
			switch t {
			case "text", "input_text":
				if s := strings.TrimSpace(fmt.Sprint(m["text"])); s != "" {
					if b.Len() > 0 {
						b.WriteByte('\n')
					}
					b.WriteString(s)
				}
			}
		}
		return b.String()
	default:
		return strings.TrimSpace(fmt.Sprint(v))
	}
}

func (c *Client) consoleHeaders(token string) http.Header {
	h := c.headers(token)
	h.Set("Origin", "https://console.x.ai")
	h.Set("Referer", "https://console.x.ai/")
	h.Set("Accept", "*/*")
	return h
}

func (h *Handler) consolePayload(spec ModelSpec, req *ChatCompletionsRequest) (map[string]interface{}, error) {
	input, instructions := consoleInputFromMessages(req.Messages)
	if len(input) == 0 && instructions == "" {
		return nil, fmt.Errorf("empty message")
	}
	payload := map[string]interface{}{
		"model": spec.ConsoleModel,
		"input": input,
	}
	if instructions != "" {
		payload["instructions"] = instructions
	}
	if req.Stream {
		payload["stream"] = true
	}
	if req.Temperature != nil {
		payload["temperature"] = *req.Temperature
	}
	if req.TopP != nil {
		payload["top_p"] = *req.TopP
	}
	if req.ReasoningEffort != nil {
		if effort := strings.ToLower(strings.TrimSpace(*req.ReasoningEffort)); effort != "" {
			payload["reasoning"] = map[string]interface{}{"effort": effort}
		}
	}
	tools := injectConsoleSearchTools(consoleToolsFromOpenAI(req.Tools))
	if len(tools) > 0 {
		payload["tools"] = tools
		if choice := consoleToolChoiceFromOpenAI(req.ToolChoice); choice != nil {
			payload["tool_choice"] = choice
		}
	}
	return payload, nil
}

func consoleToolsFromOpenAI(tools []ToolDef) []map[string]interface{} {
	if len(tools) == 0 {
		return nil
	}
	out := make([]map[string]interface{}, 0, len(tools))
	for _, tool := range tools {
		if !strings.EqualFold(strings.TrimSpace(tool.Type), "function") {
			continue
		}
		name := strings.TrimSpace(fmt.Sprint(tool.Function["name"]))
		if name == "" {
			continue
		}
		item := map[string]interface{}{
			"type":        "function",
			"name":        name,
			"description": strings.TrimSpace(fmt.Sprint(tool.Function["description"])),
			"parameters":  map[string]interface{}{},
		}
		if params, ok := tool.Function["parameters"]; ok && params != nil {
			item["parameters"] = params
		}
		out = append(out, item)
	}
	return out
}

func consoleToolChoiceFromOpenAI(choice interface{}) interface{} {
	switch v := choice.(type) {
	case nil:
		return nil
	case string:
		c := strings.ToLower(strings.TrimSpace(v))
		if c == "" {
			return nil
		}
		return c
	case map[string]interface{}:
		if !strings.EqualFold(strings.TrimSpace(fmt.Sprint(v["type"])), "function") {
			return v
		}
		fn, _ := v["function"].(map[string]interface{})
		name := strings.TrimSpace(fmt.Sprint(fn["name"]))
		if name == "" {
			return v
		}
		return map[string]interface{}{
			"type": "function",
			"name": name,
		}
	default:
		return v
	}
}

func injectConsoleSearchTools(tools []map[string]interface{}) []map[string]interface{} {
	out := make([]map[string]interface{}, 0, len(tools)+2)
	hasWebSearch := false
	hasXSearch := false
	for _, tool := range tools {
		if tool == nil {
			continue
		}
		copied := make(map[string]interface{}, len(tool))
		for k, v := range tool {
			copied[k] = v
		}
		switch strings.ToLower(strings.TrimSpace(fmt.Sprint(copied["type"]))) {
		case "web_search":
			hasWebSearch = true
		case "x_search":
			hasXSearch = true
		}
		out = append(out, copied)
	}
	if !hasWebSearch {
		out = append(out, map[string]interface{}{"type": "web_search"})
	}
	if !hasXSearch {
		out = append(out, map[string]interface{}{"type": "x_search"})
	}
	return out
}

func (h *Handler) doConsole(ctx context.Context, token string, payload map[string]interface{}) (*http.Response, error) {
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	return h.client.doRequestWith429Retry(ctx, consoleResponsesURL, http.MethodPost, body, h.client.consoleHeaders(token), http.StatusOK, false, true)
}

func shouldServeConsoleChat(spec ModelSpec, attachments []AttachmentInput) bool {
	return strings.TrimSpace(spec.ConsoleModel) != "" && len(attachments) == 0
}

type ConsoleProbeResult struct {
	RequestedModel string
	CanonicalModel string
	OK             bool
	Status         int
	Error          string
}

func (c *Client) ProbeConsoleModel(ctx context.Context, token string, modelID string) ConsoleProbeResult {
	modelID = strings.TrimSpace(modelID)
	result := ConsoleProbeResult{RequestedModel: modelID}
	if modelID == "" {
		result.Error = "empty model"
		return result
	}
	payload := map[string]interface{}{
		"model": modelID,
		"input": "Reply with exactly: ok",
	}
	body, err := json.Marshal(payload)
	if err != nil {
		result.Error = err.Error()
		return result
	}
	resp, err := c.doRequestWith429Retry(ctx, consoleResponsesURL, http.MethodPost, body, c.consoleHeaders(token), http.StatusOK, false, false)
	if err != nil {
		result.Error = err.Error()
		if status := parseUpstreamStatus(err); status > 0 {
			result.Status = status
		}
		return result
	}
	defer resp.Body.Close()
	result.Status = resp.StatusCode
	var raw map[string]interface{}
	if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
		result.Error = "decode response: " + err.Error()
		return result
	}
	result.CanonicalModel = strings.TrimSpace(fmt.Sprint(raw["model"]))
	if result.CanonicalModel == "" || result.CanonicalModel == "<nil>" {
		result.Error = "missing canonical model"
		return result
	}
	result.OK = true
	return result
}

func consoleExtractText(v interface{}) string {
	switch x := v.(type) {
	case nil:
		return ""
	case string:
		return x
	case map[string]interface{}:
		if t := strings.TrimSpace(fmt.Sprint(x["type"])); t == "output_text" || t == "text" || t == "message" {
			if raw := x["text"]; raw != nil {
				if s := strings.TrimSpace(fmt.Sprint(raw)); s != "" && s != "<nil>" {
					return s
				}
			}
			if raw := x["content"]; raw != nil {
				if s := strings.TrimSpace(consoleExtractText(raw)); s != "" {
					return s
				}
			}
			if raw := x["summary"]; raw != nil {
				if s := strings.TrimSpace(consoleExtractText(raw)); s != "" {
					return s
				}
			}
		}
		if t := strings.TrimSpace(fmt.Sprint(x["type"])); t == "message" || t == "response.output_message" {
			if raw := x["content"]; raw != nil {
				if s := strings.TrimSpace(consoleExtractText(raw)); s != "" {
					return s
				}
			}
		}
		for _, key := range []string{"output_text", "content", "output", "text", "message"} {
			if raw := x[key]; raw != nil {
				if s := consoleExtractText(raw); strings.TrimSpace(s) != "" {
					return s
				}
			}
		}
	case []interface{}:
		var b strings.Builder
		for _, item := range x {
			if s := strings.TrimSpace(consoleExtractText(item)); s != "" {
				if b.Len() > 0 {
					b.WriteByte('\n')
				}
				b.WriteString(s)
			}
		}
		return b.String()
	}
	return ""
}

func consoleExtractMessageText(v interface{}) string {
	switch x := v.(type) {
	case map[string]interface{}:
		if output, ok := x["output"].([]interface{}); ok {
			for _, item := range output {
				m, ok := item.(map[string]interface{})
				if !ok {
					continue
				}
				t := strings.ToLower(strings.TrimSpace(fmt.Sprint(m["type"])))
				if t != "message" && t != "response.output_message" {
					continue
				}
				if s := strings.TrimSpace(consoleExtractText(m["content"])); s != "" {
					return s
				}
			}
		}
	}
	return strings.TrimSpace(consoleExtractText(v))
}

func consoleFlatAnnotations(v interface{}) []map[string]interface{} {
	seen := map[string]struct{}{}
	out := make([]map[string]interface{}, 0)
	add := func(url, title string, start, end int) {
		url = strings.TrimSpace(url)
		if url == "" {
			return
		}
		key := url + "\x00" + title
		if _, ok := seen[key]; ok {
			return
		}
		seen[key] = struct{}{}
		out = append(out, map[string]interface{}{
			"url":         url,
			"title":       strings.TrimSpace(title),
			"start_index": start,
			"end_index":   end,
		})
	}
	var walk func(interface{})
	walk = func(raw interface{}) {
		switch x := raw.(type) {
		case map[string]interface{}:
			t := strings.ToLower(strings.TrimSpace(fmt.Sprint(x["type"])))
			if t == "url_citation" || (x["url"] != nil && (x["title"] != nil || x["start_index"] != nil || x["end_index"] != nil)) {
				add(fmt.Sprint(x["url"]), fmt.Sprint(x["title"]), interfaceToInt(x["start_index"]), interfaceToInt(x["end_index"]))
			}
			if t == "web_search_call" {
				if action, _ := x["action"].(map[string]interface{}); action != nil {
					for _, src := range interfaceSlice(action["sources"]) {
						if m, _ := src.(map[string]interface{}); m != nil {
							add(fmt.Sprint(m["url"]), fmt.Sprint(m["title"]), 0, 0)
						}
					}
					if strings.EqualFold(strings.TrimSpace(fmt.Sprint(action["type"])), "open_page") {
						add(fmt.Sprint(action["url"]), "", 0, 0)
					}
				}
			}
			for _, key := range []string{"annotation", "annotations", "content", "output", "item"} {
				if child, ok := x[key]; ok {
					walk(child)
				}
			}
		case []interface{}:
			for _, item := range x {
				walk(item)
			}
		}
	}
	walk(v)
	return out
}

func consoleChatAnnotations(flat []map[string]interface{}) []interface{} {
	if len(flat) == 0 {
		return []interface{}{}
	}
	out := make([]interface{}, 0, len(flat))
	for _, ann := range flat {
		out = append(out, map[string]interface{}{
			"type": "url_citation",
			"url_citation": map[string]interface{}{
				"url":         ann["url"],
				"title":       ann["title"],
				"start_index": ann["start_index"],
				"end_index":   ann["end_index"],
			},
		})
	}
	return out
}

func appendUniqueConsoleAnnotations(dst []map[string]interface{}, src []map[string]interface{}) []map[string]interface{} {
	if len(src) == 0 {
		return dst
	}
	seen := make(map[string]struct{}, len(dst)+len(src))
	for _, ann := range dst {
		seen[fmt.Sprint(ann["url"])+"\x00"+fmt.Sprint(ann["title"])] = struct{}{}
	}
	for _, ann := range src {
		key := fmt.Sprint(ann["url"]) + "\x00" + fmt.Sprint(ann["title"])
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		dst = append(dst, ann)
	}
	return dst
}

func interfaceSlice(v interface{}) []interface{} {
	switch x := v.(type) {
	case []interface{}:
		return x
	default:
		return nil
	}
}

func consoleUsage(v map[string]interface{}) map[string]interface{} {
	raw, ok := v["usage"].(map[string]interface{})
	if !ok {
		return nil
	}
	prompt := interfaceToInt(raw["input_tokens"])
	completion := interfaceToInt(raw["output_tokens"])
	if prompt == 0 {
		prompt = interfaceToInt(raw["prompt_tokens"])
	}
	if completion == 0 {
		completion = interfaceToInt(raw["completion_tokens"])
	}
	total := interfaceToInt(raw["total_tokens"])
	if total == 0 {
		total = prompt + completion
	}
	reasoning := 0
	if details, _ := raw["output_tokens_details"].(map[string]interface{}); details != nil {
		reasoning = interfaceToInt(details["reasoning_tokens"])
	}
	if reasoning == 0 {
		reasoning = interfaceToInt(raw["reasoning_tokens"])
	}
	return map[string]interface{}{
		"prompt_tokens":     prompt,
		"completion_tokens": completion,
		"total_tokens":      total,
		"prompt_tokens_details": map[string]interface{}{
			"cached_tokens": 0,
			"text_tokens":   prompt,
			"audio_tokens":  0,
			"image_tokens":  0,
		},
		"completion_tokens_details": map[string]interface{}{
			"text_tokens":      max(completion-reasoning, 0),
			"audio_tokens":     0,
			"reasoning_tokens": reasoning,
		},
	}
}

func (h *Handler) serveConsoleChat(ctx context.Context, w http.ResponseWriter, req *ChatCompletionsRequest, spec ModelSpec, sess *chatAccountSession) {
	payload, err := h.consolePayload(spec, req)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	resp, err := h.doConsole(ctx, sess.token, payload)
	if err != nil {
		h.markAccountStatus(ctx, sess.acc, err)
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()
	h.syncGrokQuota(sess.acc, resp.Header)
	if req.Stream {
		h.streamConsoleChat(w, req, resp.Body)
		return
	}
	h.collectConsoleChat(w, req, resp.Body)
}

func (h *Handler) collectConsoleChat(w http.ResponseWriter, req *ChatCompletionsRequest, body io.Reader) {
	var raw map[string]interface{}
	if err := json.NewDecoder(body).Decode(&raw); err != nil {
		http.Error(w, "console response parse error: "+err.Error(), http.StatusBadGateway)
		return
	}
	text := consoleExtractMessageText(raw)
	annotations := consoleChatAnnotations(consoleFlatAnnotations(raw))
	toolCalls := consoleToolCallsFromOutput(raw)
	message := map[string]interface{}{
		"role":        "assistant",
		"content":     text,
		"refusal":     nil,
		"annotations": annotations,
	}
	finishReason := "stop"
	if len(toolCalls) > 0 {
		message["tool_calls"] = toolCalls
		finishReason = "tool_calls"
		if strings.TrimSpace(text) == "" {
			message["content"] = nil
		}
	}
	resp := map[string]interface{}{
		"id":                 firstNonEmpty(fmt.Sprint(raw["id"]), "chatcmpl_"+randomHex(8)),
		"object":             "chat.completion",
		"created":            time.Now().Unix(),
		"model":              req.Model,
		"service_tier":       nil,
		"system_fingerprint": "",
		"choices": []map[string]interface{}{{
			"index":         0,
			"message":       message,
			"finish_reason": finishReason,
		}},
		"usage": firstUsage(consoleUsage(raw), buildChatUsagePayload(req, text, toolCalls)),
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}

func consoleToolCallsFromOutput(raw map[string]interface{}) []map[string]interface{} {
	if raw == nil {
		return nil
	}
	var out []map[string]interface{}
	for _, item := range interfaceSlice(raw["output"]) {
		if tc := consoleToolCallFromItem(item); tc != nil {
			out = append(out, tc)
		}
	}
	return out
}

func consoleToolCallFromItem(raw interface{}) map[string]interface{} {
	item, _ := raw.(map[string]interface{})
	if item == nil {
		return nil
	}
	if !strings.EqualFold(strings.TrimSpace(fmt.Sprint(item["type"])), "function_call") {
		return nil
	}
	name := strings.TrimSpace(fmt.Sprint(item["name"]))
	if name == "" || name == "<nil>" {
		return nil
	}
	callID := strings.TrimSpace(fmt.Sprint(item["call_id"]))
	if callID == "" || callID == "<nil>" {
		callID = strings.TrimSpace(fmt.Sprint(item["id"]))
	}
	if callID == "" || callID == "<nil>" {
		callID = "call_" + randomHex(12)
	}
	arguments := "{}"
	if rawArgs, ok := item["arguments"]; ok && rawArgs != nil {
		switch v := rawArgs.(type) {
		case string:
			if strings.TrimSpace(v) != "" {
				arguments = strings.TrimSpace(v)
			}
		default:
			if buf, err := json.Marshal(v); err == nil {
				arguments = string(buf)
			}
		}
	}
	return map[string]interface{}{
		"id":   callID,
		"type": "function",
		"function": map[string]interface{}{
			"name":      name,
			"arguments": arguments,
		},
	}
}

type consoleStreamToolCall struct {
	ID        string
	Name      string
	Arguments strings.Builder
}

func (tc *consoleStreamToolCall) openAIToolCall(index int) map[string]interface{} {
	id := strings.TrimSpace(tc.ID)
	if id == "" {
		id = "call_" + randomHex(12)
	}
	args := strings.TrimSpace(tc.Arguments.String())
	if args == "" {
		args = "{}"
	}
	return map[string]interface{}{
		"index": index,
		"id":    id,
		"type":  "function",
		"function": map[string]interface{}{
			"name":      strings.TrimSpace(tc.Name),
			"arguments": args,
		},
	}
}

func firstUsage(a, b map[string]interface{}) map[string]interface{} {
	if len(a) > 0 {
		return a
	}
	return b
}

func consoleUsageFromStreamEvent(ev map[string]interface{}) map[string]interface{} {
	if ev == nil {
		return nil
	}
	if resp, _ := ev["response"].(map[string]interface{}); resp != nil {
		if usage := consoleUsage(resp); len(usage) > 0 {
			return usage
		}
	}
	return consoleUsage(ev)
}

func appendConsoleFinalChunk(dst []byte, id string, created int64, model, fingerprint, finish string, annotations []interface{}, usage map[string]interface{}) []byte {
	delta := map[string]interface{}{}
	if len(annotations) > 0 {
		delta["annotations"] = annotations
	}
	chunk := map[string]interface{}{
		"id":                 id,
		"object":             "chat.completion.chunk",
		"created":            created,
		"model":              model,
		"service_tier":       nil,
		"system_fingerprint": fingerprint,
		"choices": []map[string]interface{}{{
			"index":         0,
			"delta":         delta,
			"logprobs":      nil,
			"finish_reason": finish,
		}},
		"usage": usage,
	}
	raw, err := json.Marshal(chunk)
	if err != nil {
		return appendChatCompletionChunkWithUsage(dst, id, created, model, fingerprint, "", "", finish, true, usage)
	}
	return append(dst, raw...)
}

func (h *Handler) streamConsoleChat(w http.ResponseWriter, req *ChatCompletionsRequest, body io.Reader) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	flusher, _ := w.(http.Flusher)
	id := "chatcmpl_" + randomHex(8)
	fingerprint := ""
	raw := appendChatCompletionChunk(nil, id, time.Now().Unix(), req.Model, fingerprint, "assistant", "", "", false)
	writeSSEBytes(w, "", raw)
	if flusher != nil {
		flusher.Flush()
	}
	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	var event string
	var final strings.Builder
	var annotations []map[string]interface{}
	var finalUsage map[string]interface{}
	var toolCalls []*consoleStreamToolCall
	var activeToolCall *consoleStreamToolCall
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "event:") {
			event = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
			continue
		}
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if data == "" || data == "[DONE]" {
			continue
		}
		var ev map[string]interface{}
		if err := json.Unmarshal([]byte(data), &ev); err != nil {
			continue
		}
		annotations = appendUniqueConsoleAnnotations(annotations, consoleFlatAnnotations(ev))
		if usage := consoleUsageFromStreamEvent(ev); len(usage) > 0 {
			finalUsage = usage
		}
		eventLower := strings.ToLower(strings.TrimSpace(event))
		if eventLower == "" {
			eventLower = strings.ToLower(strings.TrimSpace(fmt.Sprint(ev["type"])))
		}
		if item, _ := ev["item"].(map[string]interface{}); item != nil && strings.EqualFold(strings.TrimSpace(fmt.Sprint(item["type"])), "function_call") {
			name := strings.TrimSpace(fmt.Sprint(item["name"]))
			if name != "" && name != "<nil>" {
				tc := &consoleStreamToolCall{
					ID:   strings.TrimSpace(fmt.Sprint(item["call_id"])),
					Name: name,
				}
				if tc.ID == "" || tc.ID == "<nil>" {
					tc.ID = strings.TrimSpace(fmt.Sprint(item["id"]))
				}
				if args, ok := item["arguments"]; ok && args != nil {
					switch v := args.(type) {
					case string:
						tc.Arguments.WriteString(v)
					default:
						if buf, err := json.Marshal(v); err == nil {
							tc.Arguments.Write(buf)
						}
					}
				}
				toolCalls = append(toolCalls, tc)
				activeToolCall = tc
			}
			continue
		}
		if strings.Contains(eventLower, "function_call_arguments") {
			if activeToolCall == nil && len(toolCalls) > 0 {
				activeToolCall = toolCalls[len(toolCalls)-1]
			}
			if activeToolCall != nil {
				if strings.Contains(eventLower, ".delta") {
					if delta := strings.TrimSpace(fmt.Sprint(ev["delta"])); delta != "" && delta != "<nil>" {
						activeToolCall.Arguments.WriteString(delta)
					}
				}
				if strings.Contains(eventLower, ".done") {
					if args := strings.TrimSpace(fmt.Sprint(ev["arguments"])); args != "" && args != "<nil>" {
						activeToolCall.Arguments.Reset()
						activeToolCall.Arguments.WriteString(args)
					}
				}
			}
			continue
		}
		content := consoleDeltaText(event, ev)
		if content == "" {
			continue
		}
		final.WriteString(content)
		raw = appendChatCompletionChunk(nil, id, time.Now().Unix(), req.Model, fingerprint, "", content, "", false)
		writeSSEBytes(w, "", raw)
		if flusher != nil {
			flusher.Flush()
		}
	}
	indexedToolCalls := make([]map[string]interface{}, 0, len(toolCalls))
	for _, tc := range toolCalls {
		if tc == nil || strings.TrimSpace(tc.Name) == "" {
			continue
		}
		indexedToolCalls = append(indexedToolCalls, tc.openAIToolCall(len(indexedToolCalls)))
	}
	usage := firstUsage(finalUsage, buildChatUsagePayload(req, final.String(), indexedToolCalls))
	if len(indexedToolCalls) > 0 {
		raw = appendChatCompletionToolCallsChunkWithUsage(nil, id, time.Now().Unix(), req.Model, fingerprint, indexedToolCalls, "tool_calls", true, usage)
		writeSSEBytes(w, "", raw)
		writeSSEBytes(w, "", []byte("[DONE]"))
		if flusher != nil {
			flusher.Flush()
		}
		return
	}
	raw = appendConsoleFinalChunk(nil, id, time.Now().Unix(), req.Model, fingerprint, "stop", consoleChatAnnotations(annotations), usage)
	writeSSEBytes(w, "", raw)
	writeSSEBytes(w, "", []byte("[DONE]"))
	if flusher != nil {
		flusher.Flush()
	}
}

func consoleDeltaText(event string, ev map[string]interface{}) string {
	event = strings.ToLower(strings.TrimSpace(event))
	if !strings.Contains(event, "delta") {
		return ""
	}
	for _, key := range []string{"delta", "text"} {
		raw, ok := ev[key]
		if !ok || raw == nil {
			continue
		}
		s, ok := raw.(string)
		if !ok {
			s = fmt.Sprint(raw)
		}
		if s != "" && s != "<nil>" {
			return s
		}
	}
	if strings.Contains(event, "output_text") {
		return consoleExtractText(ev)
	}
	if bytes.Contains([]byte(event), []byte("completed")) {
		return ""
	}
	return ""
}
