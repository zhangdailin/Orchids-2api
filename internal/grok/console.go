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

type consoleInputItem struct {
	Role    string                `json:"role"`
	Content []consoleContentBlock `json:"content"`
}

func consoleInputFromMessages(messages []ChatMessage) ([]consoleInputItem, string) {
	items := make([]consoleInputItem, 0, len(messages))
	var instructions strings.Builder
	for _, msg := range messages {
		role := strings.ToLower(strings.TrimSpace(msg.Role))
		text := strings.TrimSpace(chatMessageContentText(msg.Content))
		if text == "" {
			continue
		}
		if role == "system" || role == "developer" {
			if instructions.Len() > 0 {
				instructions.WriteString("\n\n")
			}
			instructions.WriteString(text)
			continue
		}
		contentType := "input_text"
		if role == "assistant" {
			contentType = "output_text"
		}
		if role != "assistant" {
			role = "user"
		}
		items = append(items, consoleInputItem{
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
	return payload, nil
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

func consoleUsage(v map[string]interface{}) map[string]interface{} {
	raw, ok := v["usage"].(map[string]interface{})
	if !ok {
		return nil
	}
	prompt := intFromAny(raw["input_tokens"])
	completion := intFromAny(raw["output_tokens"])
	if prompt == 0 {
		prompt = intFromAny(raw["prompt_tokens"])
	}
	if completion == 0 {
		completion = intFromAny(raw["completion_tokens"])
	}
	total := intFromAny(raw["total_tokens"])
	if total == 0 {
		total = prompt + completion
	}
	return map[string]interface{}{
		"prompt_tokens":     prompt,
		"completion_tokens": completion,
		"total_tokens":      total,
	}
}

func intFromAny(v interface{}) int {
	switch n := v.(type) {
	case float64:
		return int(n)
	case int:
		return n
	case int64:
		return int(n)
	default:
		return 0
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
	text := strings.TrimSpace(consoleExtractText(raw["output"]))
	if text == "" {
		text = strings.TrimSpace(consoleExtractText(raw))
	}
	resp := map[string]interface{}{
		"id":                 firstNonEmpty(fmt.Sprint(raw["id"]), "chatcmpl_"+randomHex(8)),
		"object":             "chat.completion",
		"created":            time.Now().Unix(),
		"model":              req.Model,
		"service_tier":       nil,
		"system_fingerprint": "",
		"choices": []map[string]interface{}{{
			"index": 0,
			"message": map[string]interface{}{
				"role":        "assistant",
				"content":     text,
				"refusal":     nil,
				"annotations": []interface{}{},
			},
			"finish_reason": "stop",
		}},
		"usage": firstUsage(consoleUsage(raw), buildChatUsagePayload(req, text, nil)),
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}

func firstUsage(a, b map[string]interface{}) map[string]interface{} {
	if len(a) > 0 {
		return a
	}
	return b
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
	usage := buildChatUsagePayload(req, final.String(), nil)
	raw = appendChatCompletionChunkWithUsage(nil, id, time.Now().Unix(), req.Model, fingerprint, "", "", "stop", true, usage)
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
