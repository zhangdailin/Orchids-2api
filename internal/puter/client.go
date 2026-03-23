package puter

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
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
	puterAPIURL         = defaultAPIURL
	toolCallPattern     = regexp.MustCompile(`(?s)<tool_call>\s*(\{.*?\})\s*</tool_call>`)
	toolFencePattern    = regexp.MustCompile("(?s)^```[a-zA-Z0-9_-]*\\s*(.*?)\\s*```$")
	tmpPathHintPattern  = regexp.MustCompile(`(?i)(/tmp/[^\s"'` + "`" + `]+)`)
	unixPathHintPattern = regexp.MustCompile(`(?i)(/[^\s"'` + "`" + `]+)`)
	winPathHintPattern  = regexp.MustCompile(`(?i)([a-z]:\\[^\r\n"'` + "`" + `]+)`)
)

type Client struct {
	config           *config.Config
	account          *store.Account
	httpClient       *http.Client
	authToken        string
	sharedHTTPClient bool
}

func NewFromAccount(acc *store.Account, cfg *config.Config) *Client {
	timeout := 5 * time.Minute
	if cfg != nil && cfg.RequestTimeout > 0 {
		timeout = time.Duration(cfg.RequestTimeout) * time.Second
		if timeout < 30*time.Second {
			timeout = 30 * time.Second
		}
	}

	proxyFunc := http.ProxyFromEnvironment
	proxyKey := "direct"
	if cfg != nil {
		proxyFunc = util.ProxyFunc(cfg.ProxyHTTP, cfg.ProxyHTTPS, cfg.ProxyUser, cfg.ProxyPass, cfg.ProxyBypass)
		proxyKey = util.GenerateProxyKey(cfg.ProxyHTTP, cfg.ProxyHTTPS, cfg.ProxyUser)
	}

	return &Client{
		config:           cfg,
		account:          acc,
		httpClient:       util.GetSharedHTTPClient(proxyKey, timeout, proxyFunc),
		authToken:        resolveAuthToken(acc),
		sharedHTTPClient: true,
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
	if c == nil || c.sharedHTTPClient || c.httpClient == nil || c.httpClient.Transport == nil {
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
			Messages: convertMessages(req.Messages, buildSystemPrompt(req.System, req.Workdir, req.Tools, req.NoTools, req.Messages)),
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

func buildSystemPrompt(system []prompt.SystemItem, workdir string, tools []interface{}, noTools bool, messages []prompt.Message) string {
	parts := []string{}
	if toolPrompt := buildPuterToolPrompt(workdir, tools, noTools, messages); toolPrompt != "" {
		parts = append(parts, toolPrompt)
	}
	for _, item := range system {
		if text := strings.TrimSpace(item.Text); text != "" {
			parts = append(parts, text)
		}
	}
	return strings.Join(parts, "\n\n")
}

func buildPuterToolPrompt(workdir string, tools []interface{}, noTools bool, messages []prompt.Message) string {
	parts := append([]string{}, buildPuterWorkspacePrompt(workdir)...)
	if noTools {
		parts = append(parts, "This turn must not make any tool calls. Answer directly using the existing context and prior tool results.")
		return strings.Join(parts, "\n")
	}

	toolPrompt := buildToolPrompt(tools)
	if toolPrompt != "" {
		parts = append(parts, toolPrompt)
	}
	parts = append(parts, buildPuterHistoryRecoveryPrompt(workdir, messages)...)
	return strings.Join(parts, "\n\n")
}

func buildPuterWorkspacePrompt(workdir string) []string {
	workdir = strings.TrimSpace(workdir)
	if workdir == "" {
		return nil
	}

	parts := make([]string, 0, 5)
	projectName := filepath.Base(filepath.Clean(workdir))
	if projectName != "" && projectName != "." && projectName != string(filepath.Separator) {
		parts = append(parts, "Current project directory name: "+projectName)
	}
	parts = append(parts, "The real local project working directory is `"+workdir+"`.")
	parts = append(parts, "If the user asks for the project directory, current path, or workspace path, answer with that real working directory directly.")
	parts = append(parts, "Treat the project root as `.` and prefer project-relative paths for Read, Write, Edit, Glob, Grep, and Bash.")
	parts = append(parts, "Do not assume the project is empty just because a sandbox path like `/tmp/...`, `/mnt/...`, or `~/...` fails.")
	return parts
}

func buildPuterHistoryRecoveryPrompt(workdir string, messages []prompt.Message) []string {
	invalidPath := detectRecentPuterInvalidPath(messages)
	if invalidPath == "" {
		return nil
	}

	parts := []string{
		"Recent history contains a failed external path access `" + invalidPath + "`. Treat it as a bad example and do not reuse that path.",
	}
	if strings.TrimSpace(workdir) != "" {
		parts = append(parts, "The real project directory is `"+workdir+"`.")
	}
	parts = append(parts, "If you need to inspect the project again, retry with `.` or project-relative files such as `README.md`, `go.mod`, or `package.json`.")
	return parts
}

func detectRecentPuterInvalidPath(messages []prompt.Message) string {
	for i := len(messages) - 1; i >= 0; i-- {
		for _, block := range puterBlocks(messages[i]) {
			if block.Type != "tool_result" {
				continue
			}
			content := strings.TrimSpace(stringifyToolResult(block.Content))
			if content == "" || !looksLikeMissingPath(content) {
				continue
			}
			if path := extractRecentPathHint(content); path != "" {
				return path
			}
		}
	}
	return ""
}

func looksLikeMissingPath(text string) bool {
	lower := strings.ToLower(strings.TrimSpace(text))
	if lower == "" {
		return false
	}
	for _, marker := range []string{
		"no such file or directory",
		"cannot access",
		"does not exist",
		"path does not exist",
		"enoent",
		"not found",
		"找不到指定的路径",
		"系统找不到指定的路径",
	} {
		if strings.Contains(lower, marker) {
			return true
		}
	}
	return false
}

func extractRecentPathHint(text string) string {
	if text == "" {
		return ""
	}
	for _, pattern := range []*regexp.Regexp{tmpPathHintPattern, unixPathHintPattern, winPathHintPattern} {
		if matches := pattern.FindStringSubmatch(text); len(matches) > 1 {
			return strings.TrimSpace(matches[1])
		}
	}
	return ""
}

func puterBlocks(msg prompt.Message) []prompt.ContentBlock {
	if msg.Content.IsString() {
		text := strings.TrimSpace(msg.Content.GetText())
		if text == "" {
			return nil
		}
		return []prompt.ContentBlock{{Type: "text", Text: text}}
	}
	return msg.Content.GetBlocks()
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
	if v == nil {
		return ""
	}
	if s, ok := v.(string); ok {
		s = strings.TrimSpace(s)
		if s == "<nil>" {
			return ""
		}
		return s
	}
	s := strings.TrimSpace(fmt.Sprint(v))
	if s == "<nil>" {
		return ""
	}
	return s
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
		line := normalizePuterStreamLine(scanner.Text())
		if line == "" {
			continue
		}

		var apiErr ErrorResponse
		if err := json.Unmarshal([]byte(line), &apiErr); err == nil {
			if apiErr.Error.Present() && (apiErr.Success == nil || !*apiErr.Success) {
				return "", formatPuterAPIError(apiErr.Error.AsPayload(), line)
			}
			if apiErr.Error.Present() && apiErr.Error.AsPayload() != nil {
				return "", formatPuterAPIError(apiErr.Error.AsPayload(), line)
			}
		}

		var chunk StreamChunk
		if err := json.Unmarshal([]byte(line), &chunk); err != nil {
			continue
		}
		if text := firstNonEmpty(chunk.Text, chunk.Delta, chunk.Message); text != "" {
			fullText.WriteString(text)
		}
	}
	return fullText.String(), scanner.Err()
}

func normalizePuterStreamLine(line string) string {
	line = strings.TrimSpace(line)
	if strings.HasPrefix(strings.ToLower(line), "data:") {
		line = strings.TrimSpace(line[5:])
	}
	if line == "[DONE]" {
		return ""
	}
	return line
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if trimmed := strings.TrimSpace(value); trimmed != "" {
			return value
		}
	}
	return ""
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
	if len(matches) > 0 {
		var calls []ParsedToolCall
		remaining := text
		for i, match := range matches {
			calls = append(calls, parseToolCallsFromJSON(match[1], i)...)
			remaining = strings.Replace(remaining, match[0], "", 1)
		}
		if len(calls) > 0 {
			return calls, strings.TrimSpace(remaining)
		}
	}

	trimmed := strings.TrimSpace(text)
	if trimmed == "" {
		return nil, ""
	}

	if calls := parseToolCallsFromJSON(trimmed, 0); len(calls) > 0 {
		return calls, ""
	}

	if fenced := stripPuterToolCodeFence(trimmed); fenced != trimmed {
		if calls := parseToolCallsFromJSON(fenced, 0); len(calls) > 0 {
			return calls, ""
		}
	}

	return nil, trimmed
}

func stripPuterToolCodeFence(text string) string {
	match := toolFencePattern.FindStringSubmatch(strings.TrimSpace(text))
	if len(match) < 2 {
		return strings.TrimSpace(text)
	}
	return strings.TrimSpace(match[1])
}

func parseToolCallsFromJSON(raw string, indexBase int) []ParsedToolCall {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}

	var value interface{}
	if err := json.Unmarshal([]byte(raw), &value); err != nil {
		return nil
	}

	calls := extractParsedToolCalls(value)
	for i := range calls {
		if strings.TrimSpace(calls[i].ID) == "" {
			calls[i].ID = fmt.Sprintf("toolu_%d_%d", time.Now().UnixNano(), indexBase+i)
		}
		if len(calls[i].Input) == 0 {
			calls[i].Input = json.RawMessage("{}")
		}
	}
	return calls
}

func extractParsedToolCalls(value interface{}) []ParsedToolCall {
	switch v := value.(type) {
	case []interface{}:
		var calls []ParsedToolCall
		for _, item := range v {
			calls = append(calls, extractParsedToolCalls(item)...)
		}
		return calls
	case map[string]interface{}:
		for _, key := range []string{"tool_calls", "toolCalls", "calls", "tool_call", "toolCall"} {
			if nested, ok := v[key]; ok {
				if calls := extractParsedToolCalls(nested); len(calls) > 0 {
					return calls
				}
			}
		}
		if call, ok := parseParsedToolCall(v); ok {
			return []ParsedToolCall{call}
		}
	}
	return nil
}

func parseParsedToolCall(payload map[string]interface{}) (ParsedToolCall, bool) {
	typeHint := strings.ToLower(strings.TrimSpace(asString(payload["type"])))
	id := strings.TrimSpace(asString(payload["id"]))

	if fn, ok := payload["function"].(map[string]interface{}); ok {
		name := strings.TrimSpace(asString(fn["name"]))
		if name == "" {
			return ParsedToolCall{}, false
		}
		input := normalizeParsedToolInput(firstNonNil(
			fn["input"],
			fn["arguments"],
			fn["parameters"],
			payload["input"],
			payload["arguments"],
			payload["parameters"],
			payload["params"],
		))
		return ParsedToolCall{Name: name, ID: id, Input: input}, true
	}

	name := strings.TrimSpace(asString(payload["name"]))
	if name == "" {
		name = strings.TrimSpace(asString(payload["tool"]))
	}
	if name == "" {
		name = strings.TrimSpace(asString(payload["toolName"]))
	}
	if name == "" {
		return ParsedToolCall{}, false
	}

	rawInput := firstNonNil(payload["input"], payload["arguments"], payload["parameters"], payload["params"])
	if rawInput == nil && typeHint != "tool_use" && typeHint != "tool_call" && typeHint != "function" {
		return ParsedToolCall{}, false
	}
	return ParsedToolCall{Name: name, ID: id, Input: normalizeParsedToolInput(rawInput)}, true
}

func normalizeParsedToolInput(value interface{}) json.RawMessage {
	switch v := value.(type) {
	case nil:
		return json.RawMessage("{}")
	case json.RawMessage:
		if len(v) == 0 {
			return json.RawMessage("{}")
		}
		return v
	case string:
		trimmed := strings.TrimSpace(v)
		if trimmed == "" {
			return json.RawMessage("{}")
		}
		if json.Valid([]byte(trimmed)) {
			return json.RawMessage(trimmed)
		}
	}

	raw, err := json.Marshal(value)
	if err != nil || len(raw) == 0 || string(raw) == "null" {
		return json.RawMessage("{}")
	}
	return raw
}

func firstNonNil(values ...interface{}) interface{} {
	for _, value := range values {
		if value != nil {
			return value
		}
	}
	return nil
}

func emitResponseEvents(onMessage func(upstream.SSEMessage), model, text string, toolCalls []ParsedToolCall, inputTokens, outputTokens int) error {
	if model == "" {
		model = defaultModelID
	}
	stopReason := "end_turn"
	if len(toolCalls) > 0 {
		stopReason = "tool_use"
	}
	if trimmed := strings.TrimSpace(text); trimmed != "" {
		onMessage(upstream.SSEMessage{
			Type: "model.text-delta",
			Event: map[string]interface{}{
				"delta": trimmed,
			},
		})
	}

	for _, call := range toolCalls {
		onMessage(upstream.SSEMessage{
			Type: "model.tool-call",
			Event: map[string]interface{}{
				"toolCallId": call.ID,
				"toolName":   call.Name,
				"input":      string(call.Input),
			},
		})
	}

	onMessage(upstream.SSEMessage{
		Type: "model.finish",
		Event: map[string]interface{}{
			"finishReason": stopReason,
			"usage": map[string]int{
				"inputTokens":   inputTokens,
				"outputTokens":  outputTokens,
				"input_tokens":  inputTokens,
				"output_tokens": outputTokens,
			},
		},
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
