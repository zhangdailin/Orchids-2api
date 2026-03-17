package puter

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/goccy/go-json"

	"orchids-api/internal/config"
	"orchids-api/internal/debug"
	"orchids-api/internal/prompt"
	"orchids-api/internal/store"
	"orchids-api/internal/upstream"
	"orchids-api/internal/util"
)

const (
	defaultAPIURL   = "https://api.puter.com/drivers/call"
	defaultModelID  = "claude-opus-4-5"
	defaultMethod   = "complete"
	defaultIface    = "puter-chat-completion"
	defaultToolHint = "When you need to use a tool, output it in this exact format: <tool_call>{\"name\":\"tool_name\",\"input\":{\"param\":\"value\"}}</tool_call>"
)

var (
	puterAPIURL     = defaultAPIURL
	toolCallPattern = regexp.MustCompile(`(?s)<tool_call>\s*(\{.*?\})\s*</tool_call>`)
)

type Client struct {
	config     *config.Config
	account    *store.Account
	httpClient *http.Client
	authToken  string
}

func NewFromAccount(acc *store.Account, cfg *config.Config) *Client {
	timeout := 5 * time.Minute
	if cfg != nil && cfg.RequestTimeout > 0 {
		timeout = time.Duration(cfg.RequestTimeout) * time.Second
		if timeout < 30*time.Second {
			timeout = 30 * time.Second
		}
	}

	transport := http.DefaultTransport.(*http.Transport).Clone()
	if cfg != nil {
		transport.Proxy = util.ProxyFunc(cfg.ProxyHTTP, cfg.ProxyHTTPS, cfg.ProxyUser, cfg.ProxyPass, cfg.ProxyBypass)
	}

	return &Client{
		config:     cfg,
		account:    acc,
		httpClient: &http.Client{Timeout: timeout, Transport: transport},
		authToken:  resolveAuthToken(acc),
	}
}

func resolveAuthToken(acc *store.Account) string {
	if acc == nil {
		return ""
	}
	for _, value := range []string{acc.ClientCookie, acc.Token, acc.SessionCookie} {
		if trimmed := strings.TrimSpace(value); trimmed != "" {
			return trimmed
		}
	}
	return ""
}

func (c *Client) Close() {
	if c == nil || c.httpClient == nil || c.httpClient.Transport == nil {
		return
	}
	if closer, ok := c.httpClient.Transport.(interface{ CloseIdleConnections() }); ok {
		closer.CloseIdleConnections()
	}
}

func (c *Client) VerifyAuthToken(ctx context.Context) error {
	if c == nil {
		return fmt.Errorf("puter client is nil")
	}
	req := upstream.UpstreamRequest{
		Model: defaultModelID,
		Messages: []prompt.Message{
			{Role: "user", Content: prompt.MessageContent{Text: "ping"}},
		},
		System: nil,
		Tools:  nil,
	}
	return c.SendRequestWithPayload(ctx, req, nil, nil)
}

func (c *Client) SendRequest(ctx context.Context, _ string, _ []interface{}, model string, onMessage func(upstream.SSEMessage), logger *debug.Logger) error {
	return c.SendRequestWithPayload(ctx, upstream.UpstreamRequest{Model: model}, onMessage, logger)
}

func (c *Client) SendRequestWithPayload(ctx context.Context, req upstream.UpstreamRequest, onMessage func(upstream.SSEMessage), logger *debug.Logger) error {
	if c == nil {
		return fmt.Errorf("puter client is nil")
	}
	if strings.TrimSpace(c.authToken) == "" {
		return fmt.Errorf("missing puter auth token")
	}

	puterReq := c.buildRequest(req)
	body, err := json.Marshal(puterReq)
	if err != nil {
		return fmt.Errorf("failed to marshal puter request: %w", err)
	}
	if logger != nil {
		logger.LogUpstreamRequest(puterAPIURL, map[string]string{"provider": "puter"}, body)
	}

	reqCtx, cancel := util.WithDefaultTimeout(ctx, 5*time.Minute)
	defer cancel()

	httpReq, err := http.NewRequestWithContext(reqCtx, http.MethodPost, puterAPIURL, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("failed to create puter request: %w", err)
	}
	c.applyHeaders(httpReq)

	resp, err := c.httpClient.Do(httpReq)
	if err != nil {
		return fmt.Errorf("failed to send puter request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 8192))
		return fmt.Errorf("puter API error: status=%d, body=%s", resp.StatusCode, strings.TrimSpace(string(raw)))
	}

	fullText, err := readStreamText(resp.Body)
	if err != nil {
		return err
	}

	toolCalls, remainingText := parseToolCalls(fullText)
	if onMessage == nil {
		return nil
	}
	return emitResponseEvents(onMessage, strings.TrimSpace(req.Model), remainingText, toolCalls, estimateInputTokens(req.Messages, req.System), estimateOutputTokens(fullText))
}

func (c *Client) buildRequest(req upstream.UpstreamRequest) *Request {
	modelID := strings.TrimSpace(req.Model)
	if modelID == "" {
		modelID = defaultModelID
	}
	return &Request{
		Interface: defaultIface,
		Driver:    driverForModel(modelID),
		TestMode:  false,
		Method:    defaultMethod,
		Args: RequestArgs{
			Messages: convertMessages(req.Messages, buildSystemPrompt(req.System, req.Tools)),
			Model:    modelID,
			Stream:   true,
		},
		AuthToken: c.authToken,
	}
}

func (c *Client) applyHeaders(req *http.Request) {
	req.Header.Set("Accept", "*/*")
	req.Header.Set("Accept-Encoding", "gzip, deflate, br, zstd")
	req.Header.Set("Accept-Language", "zh-CN,zh;q=0.9,en;q=0.8")
	req.Header.Set("Cache-Control", "no-cache")
	req.Header.Set("Content-Type", "text/plain;actually=json")
	req.Header.Set("Origin", "https://docs.puter.com")
	req.Header.Set("Pragma", "no-cache")
	req.Header.Set("Referer", "https://docs.puter.com/")
	req.Header.Set("Sec-Fetch-Dest", "empty")
	req.Header.Set("Sec-Fetch-Mode", "cors")
	req.Header.Set("Sec-Fetch-Site", "same-site")
	req.Header.Set("User-Agent", "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/142.0.0.0 Safari/537.36")
	req.Header.Set("sec-ch-ua", `"Chromium";v="142", "Google Chrome";v="142", "Not_A Brand";v="99"`)
	req.Header.Set("sec-ch-ua-mobile", "?0")
	req.Header.Set("sec-ch-ua-platform", `"macOS"`)
}

func driverForModel(modelID string) string {
	modelID = strings.ToLower(strings.TrimSpace(modelID))
	switch {
	case strings.HasPrefix(modelID, "claude-"):
		return "claude"
	case strings.HasPrefix(modelID, "gpt-"), strings.HasPrefix(modelID, "o1"), strings.HasPrefix(modelID, "o3"), strings.HasPrefix(modelID, "o4"):
		return "openai"
	case strings.HasPrefix(modelID, "gemini-"):
		return "google"
	case strings.HasPrefix(modelID, "grok-"):
		return "x-ai"
	case strings.HasPrefix(modelID, "deepseek-"):
		return "deepseek"
	case strings.HasPrefix(modelID, "mistral"), strings.HasPrefix(modelID, "ministral"), strings.HasPrefix(modelID, "codestral"), strings.HasPrefix(modelID, "pixtral"), strings.HasPrefix(modelID, "magistral"), strings.HasPrefix(modelID, "devstral"):
		return "mistral"
	case strings.HasPrefix(modelID, "openrouter:"):
		return "openrouter"
	case strings.HasPrefix(modelID, "togetherai:"):
		return "togetherai"
	default:
		return "claude"
	}
}

func buildSystemPrompt(system []prompt.SystemItem, tools []interface{}) string {
	parts := make([]string, 0, len(system)+2)
	for _, item := range system {
		if text := strings.TrimSpace(item.Text); text != "" {
			parts = append(parts, text)
		}
	}

	toolPrompt := buildToolPrompt(tools)
	if toolPrompt != "" {
		parts = append(parts, toolPrompt)
	}
	return strings.Join(parts, "\n\n")
}

func buildToolPrompt(tools []interface{}) string {
	if len(tools) == 0 {
		return ""
	}
	var sections []string
	for _, tool := range tools {
		name, desc, schema := extractToolDefinition(tool)
		if name == "" {
			continue
		}
		section := "## " + name
		if desc != "" {
			section += "\n" + desc
		}
		if schema != "" {
			section += "\nInput schema: " + schema
		}
		sections = append(sections, section)
	}
	if len(sections) == 0 {
		return ""
	}
	return "# Tools\n\n" + defaultToolHint + "\n\nAvailable tools:\n\n" + strings.Join(sections, "\n\n")
}

func extractToolDefinition(tool interface{}) (string, string, string) {
	switch t := tool.(type) {
	case map[string]interface{}:
		if fn, ok := t["function"].(map[string]interface{}); ok {
			return asString(fn["name"]), asString(fn["description"]), marshalCompactJSON(fn["parameters"])
		}
		return asString(t["name"]), asString(t["description"]), marshalCompactJSON(t["input_schema"])
	default:
		raw, err := json.Marshal(t)
		if err != nil {
			return "", "", ""
		}
		var decoded map[string]interface{}
		if err := json.Unmarshal(raw, &decoded); err != nil {
			return "", "", ""
		}
		return extractToolDefinition(decoded)
	}
}

func asString(v interface{}) string {
	if s, ok := v.(string); ok {
		return strings.TrimSpace(s)
	}
	return strings.TrimSpace(fmt.Sprint(v))
}

func marshalCompactJSON(v interface{}) string {
	if v == nil {
		return ""
	}
	raw, err := json.Marshal(v)
	if err != nil || len(raw) == 0 || string(raw) == "null" {
		return ""
	}
	return string(raw)
}

func convertMessages(messages []prompt.Message, systemPrompt string) []Message {
	var out []Message
	if strings.TrimSpace(systemPrompt) != "" {
		out = append(out, Message{Role: "system", Content: systemPrompt})
	}

	for _, msg := range messages {
		content := extractMessageText(msg)
		if strings.TrimSpace(content) == "" {
			continue
		}
		role := strings.TrimSpace(msg.Role)
		if role == "" {
			role = "user"
		}
		out = append(out, Message{
			Role:    role,
			Content: content,
		})
	}

	for len(out) > 0 && out[0].Role != "system" && out[0].Role != "user" {
		out = out[1:]
	}
	return out
}

func extractMessageText(msg prompt.Message) string {
	if msg.Content.IsString() {
		return strings.TrimSpace(msg.Content.GetText())
	}

	var parts []string
	for _, block := range msg.Content.GetBlocks() {
		switch block.Type {
		case "text":
			if text := strings.TrimSpace(block.Text); text != "" {
				parts = append(parts, text)
			}
		case "tool_use":
			input := marshalCompactJSON(block.Input)
			if input == "" {
				input = "{}"
			}
			call := map[string]interface{}{
				"name":  block.Name,
				"id":    block.ID,
				"input": json.RawMessage(input),
			}
			raw, err := json.Marshal(call)
			if err == nil {
				parts = append(parts, "<tool_call>\n"+string(raw)+"\n</tool_call>")
			}
		case "tool_result":
			content := stringifyToolResult(block.Content)
			if content != "" {
				parts = append(parts, fmt.Sprintf("<tool_result id=\"%s\">\n%s\n</tool_result>", block.ToolUseID, content))
			}
		}
	}
	return strings.TrimSpace(strings.Join(parts, "\n\n"))
}

func stringifyToolResult(v interface{}) string {
	switch x := v.(type) {
	case nil:
		return ""
	case string:
		return x
	case []prompt.ContentBlock:
		var parts []string
		for _, block := range x {
			if block.Type == "text" && strings.TrimSpace(block.Text) != "" {
				parts = append(parts, block.Text)
			}
		}
		return strings.Join(parts, "\n")
	default:
		raw, err := json.Marshal(x)
		if err != nil {
			return ""
		}
		return string(raw)
	}
}

func readStreamText(body io.Reader) (string, error) {
	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 0, 64*1024), 2*1024*1024)

	var fullText strings.Builder
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}

		var apiErr ErrorResponse
		if err := json.Unmarshal([]byte(line), &apiErr); err == nil && apiErr.Error != nil {
			return "", formatPuterAPIError(apiErr.Error, line)
		}

		var chunk StreamChunk
		if err := json.Unmarshal([]byte(line), &chunk); err != nil {
			continue
		}
		if chunk.Text != "" {
			fullText.WriteString(chunk.Text)
		}
	}
	return fullText.String(), scanner.Err()
}

func formatPuterAPIError(apiErr *ErrorPayload, raw string) error {
	if apiErr == nil {
		return fmt.Errorf("puter API error: %s", strings.TrimSpace(raw))
	}

	parts := make([]string, 0, 4)
	if code := strings.TrimSpace(apiErr.Code); code != "" {
		parts = append(parts, "code="+code)
	}
	if iface := strings.TrimSpace(apiErr.Iface); iface != "" {
		parts = append(parts, "iface="+iface)
	}
	if status := apiErr.Status; status > 0 {
		parts = append(parts, fmt.Sprintf("status=%d", status))
	}
	if msg := strings.TrimSpace(apiErr.Message); msg != "" {
		parts = append(parts, "message="+msg)
	}
	if len(parts) == 0 {
		return fmt.Errorf("puter API error: %s", strings.TrimSpace(raw))
	}
	return fmt.Errorf("puter API error: %s", strings.Join(parts, ", "))
}

func parseToolCalls(text string) ([]ParsedToolCall, string) {
	matches := toolCallPattern.FindAllStringSubmatch(text, -1)
	if len(matches) == 0 {
		return nil, strings.TrimSpace(text)
	}

	var calls []ParsedToolCall
	remaining := text
	for i, match := range matches {
		var call ParsedToolCall
		if err := json.Unmarshal([]byte(match[1]), &call); err == nil && strings.TrimSpace(call.Name) != "" {
			if call.ID == "" {
				call.ID = fmt.Sprintf("toolu_%d_%d", time.Now().UnixNano(), i)
			}
			if len(call.Input) == 0 {
				call.Input = json.RawMessage("{}")
			}
			calls = append(calls, call)
		}
		remaining = strings.Replace(remaining, match[0], "", 1)
	}
	return calls, strings.TrimSpace(remaining)
}

func emitResponseEvents(onMessage func(upstream.SSEMessage), model, text string, toolCalls []ParsedToolCall, inputTokens, outputTokens int) error {
	if model == "" {
		model = defaultModelID
	}
	msgID := fmt.Sprintf("msg_%d", time.Now().UnixMilli())

	emit := func(event string, payload map[string]interface{}) {
		onMessage(upstream.SSEMessage{Type: event, Event: payload})
	}

	emit("message_start", map[string]interface{}{
		"type": "message_start",
		"message": map[string]interface{}{
			"id":            msgID,
			"type":          "message",
			"role":          "assistant",
			"content":       []interface{}{},
			"model":         model,
			"stop_reason":   nil,
			"stop_sequence": nil,
			"usage": map[string]int{
				"input_tokens":  inputTokens,
				"output_tokens": 0,
			},
		},
	})

	blockIndex := 0
	if text != "" || len(toolCalls) == 0 {
		emit("content_block_start", map[string]interface{}{
			"type":  "content_block_start",
			"index": blockIndex,
			"content_block": map[string]interface{}{
				"type": "text",
				"text": "",
			},
		})
		if text != "" {
			emit("content_block_delta", map[string]interface{}{
				"type":  "content_block_delta",
				"index": blockIndex,
				"delta": map[string]interface{}{
					"type": "text_delta",
					"text": text,
				},
			})
		}
		emit("content_block_stop", map[string]interface{}{
			"type":  "content_block_stop",
			"index": blockIndex,
		})
		blockIndex++
	}

	for _, call := range toolCalls {
		emit("content_block_start", map[string]interface{}{
			"type":  "content_block_start",
			"index": blockIndex,
			"content_block": map[string]interface{}{
				"type":  "tool_use",
				"id":    call.ID,
				"name":  call.Name,
				"input": map[string]interface{}{},
			},
		})
		emit("content_block_delta", map[string]interface{}{
			"type":  "content_block_delta",
			"index": blockIndex,
			"delta": map[string]interface{}{
				"type":         "input_json_delta",
				"partial_json": string(call.Input),
			},
		})
		emit("content_block_stop", map[string]interface{}{
			"type":  "content_block_stop",
			"index": blockIndex,
		})
		blockIndex++
	}

	stopReason := "end_turn"
	if len(toolCalls) > 0 {
		stopReason = "tool_use"
	}
	emit("message_delta", map[string]interface{}{
		"type": "message_delta",
		"delta": map[string]interface{}{
			"stop_reason":   stopReason,
			"stop_sequence": nil,
		},
		"usage": map[string]int{
			"output_tokens": outputTokens,
		},
	})
	emit("message_stop", map[string]interface{}{
		"type": "message_stop",
	})
	return nil
}

func estimateInputTokens(messages []prompt.Message, system []prompt.SystemItem) int {
	total := 0
	for _, item := range system {
		total += len(item.Text)
	}
	for _, msg := range messages {
		total += len(msg.ExtractText())
	}
	if total <= 0 {
		return 0
	}
	return total / 4
}

func estimateOutputTokens(text string) int {
	if text == "" {
		return 0
	}
	return len(text) / 4
}
