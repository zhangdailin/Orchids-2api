package v0

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/goccy/go-json"

	"orchids-api/internal/config"
	"orchids-api/internal/debug"
	"orchids-api/internal/prompt"
	"orchids-api/internal/store"
	"orchids-api/internal/upstream"
	"orchids-api/internal/util"
)

const (
	defaultScopedUserURL = "https://v0.app/api/chat/scoped/user"
	defaultPlanInfoURL   = "https://v0.app/chat/api/plan-info"
	defaultScopesURL     = "https://v0.app/chat/api/scopes"
	defaultRateLimitURL  = "https://v0.app/chat/api/rate-limit"
	defaultSendURL       = "https://v0.app/chat/api/send"
	defaultModelID       = "v0-max"
)

var (
	ScopedUserURLForTest = defaultScopedUserURL
	PlanInfoURLForTest   = defaultPlanInfoURL
	ScopesURLForTest     = defaultScopesURL
	RateLimitURLForTest  = defaultRateLimitURL
	SendURLForTest       = defaultSendURL
	v0ChatURLPattern     = regexp.MustCompile(`https://v0\.app/chat/([A-Za-z0-9_-]+)`)
	v0FieldPattern       = regexp.MustCompile(`"(?:text|content|response|message|output|completion)"\s*:\s*"((?:[^"\\]|\\.)*)"`)
)

type Client struct {
	config           *config.Config
	account          *store.Account
	httpClient       *http.Client
	userSession      string
	sharedHTTPClient bool
}

type scopedUserResponse struct {
	OK    bool       `json:"ok"`
	Value ScopedUser `json:"value"`
}

type ScopedUser struct {
	ID            string   `json:"id"`
	Name          string   `json:"name"`
	Username      string   `json:"username"`
	Email         string   `json:"email"`
	Plan          string   `json:"plan"`
	V0Plan        string   `json:"v0plan"`
	RealV0Plan    string   `json:"realv0Plan"`
	Teams         []string `json:"teams"`
	TeamID        string   `json:"teamId"`
	TeamName      string   `json:"teamName"`
	Scope         string   `json:"scope"`
	DefaultTeamID string   `json:"defaultTeamId"`
	BillingStart  int64    `json:"billingCycleStart"`
	BillingEnd    int64    `json:"billingCycleEnd"`
}

type PlanInfo struct {
	Plan         string      `json:"plan"`
	RealPlan     string      `json:"realPlan"`
	Role         string      `json:"role"`
	BillingCycle BillingSpan `json:"billingCycle"`
	Balance      V0Balance   `json:"balance"`
	OnDemand     V0Balance   `json:"onDemand"`
}

type BillingSpan struct {
	Start int64 `json:"start"`
	End   int64 `json:"end"`
}

type V0Balance struct {
	Remaining float64 `json:"remaining"`
	Total     float64 `json:"total"`
}

type ScopeInfo struct {
	ID   string `json:"id"`
	Slug string `json:"slug"`
	Name string `json:"name"`
}

type RateLimitInfo struct {
	Remaining float64 `json:"remaining"`
	Reset     int64   `json:"reset"`
	Limit     float64 `json:"limit"`
}

type ModelChoice struct {
	ID   string `json:"id"`
	Name string `json:"name"`
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
		proxyFunc = util.ProxyFuncFromConfig(cfg)
		proxyKey = util.GenerateProxyKeyFromConfig(cfg)
	}

	return &Client{
		config:           cfg,
		account:          acc,
		httpClient:       util.GetSharedHTTPClient(proxyKey, timeout, proxyFunc),
		userSession:      resolveUserSession(acc),
		sharedHTTPClient: true,
	}
}

func resolveUserSession(acc *store.Account) string {
	if acc == nil {
		return ""
	}
	for _, value := range []string{acc.ClientCookie, acc.Token, acc.SessionCookie} {
		if token := extractUserSession(value); token != "" {
			return token
		}
	}
	return ""
}

func extractUserSession(raw string) string {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return ""
	}
	if strings.Contains(strings.ToLower(trimmed), "user_session=") {
		for _, part := range strings.Split(trimmed, ";") {
			part = strings.TrimSpace(part)
			if strings.HasPrefix(strings.ToLower(part), "user_session=") {
				return strings.TrimSpace(strings.TrimPrefix(part, "user_session="))
			}
		}
	}
	return strings.Trim(strings.TrimSpace(trimmed), "\"'")
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
	_, err := c.FetchScopedUser(ctx)
	return err
}

func (c *Client) FetchScopedUser(ctx context.Context) (*ScopedUser, error) {
	if c == nil {
		return nil, fmt.Errorf("v0 client is nil")
	}
	if strings.TrimSpace(c.userSession) == "" {
		return nil, fmt.Errorf("missing v0 user_session")
	}

	raw, err := c.doRequest(ctx, http.MethodGet, ScopedUserURLForTest, nil, "https://v0.app/chat", "application/json")
	if err != nil {
		return nil, fmt.Errorf("failed to fetch v0 scoped user: %w", err)
	}

	var parsed scopedUserResponse
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return nil, fmt.Errorf("failed to decode v0 scoped user response: %w", err)
	}
	if !parsed.OK {
		return nil, fmt.Errorf("v0 scoped user request was not accepted")
	}
	return &parsed.Value, nil
}

func (c *Client) FetchPlanInfo(ctx context.Context) (*PlanInfo, error) {
	if c == nil {
		return nil, fmt.Errorf("v0 client is nil")
	}
	if strings.TrimSpace(c.userSession) == "" {
		return nil, fmt.Errorf("missing v0 user_session")
	}

	raw, err := c.doRequest(ctx, http.MethodGet, PlanInfoURLForTest, nil, "https://v0.app/chat", "application/json")
	if err != nil {
		return nil, fmt.Errorf("failed to fetch v0 plan info: %w", err)
	}

	var info PlanInfo
	if err := json.Unmarshal(raw, &info); err != nil {
		return nil, fmt.Errorf("failed to decode v0 plan info response: %w", err)
	}
	return &info, nil
}

func (c *Client) FetchScopes(ctx context.Context) ([]ScopeInfo, error) {
	if c == nil {
		return nil, fmt.Errorf("v0 client is nil")
	}
	if strings.TrimSpace(c.userSession) == "" {
		return nil, fmt.Errorf("missing v0 user_session")
	}

	raw, err := c.doRequest(ctx, http.MethodGet, ScopesURLForTest, nil, "https://v0.app/chat", "application/json")
	if err != nil {
		return nil, fmt.Errorf("failed to fetch v0 scopes: %w", err)
	}

	var scopes []ScopeInfo
	if err := json.Unmarshal(raw, &scopes); err != nil {
		return nil, fmt.Errorf("failed to decode v0 scopes response: %w", err)
	}
	return scopes, nil
}

func (c *Client) FetchRateLimit(ctx context.Context, scopeSlug string) (*RateLimitInfo, error) {
	if c == nil {
		return nil, fmt.Errorf("v0 client is nil")
	}
	if strings.TrimSpace(c.userSession) == "" {
		return nil, fmt.Errorf("missing v0 user_session")
	}
	scopeSlug = strings.TrimSpace(scopeSlug)
	if scopeSlug == "" {
		return nil, fmt.Errorf("missing v0 scope slug")
	}

	raw, err := c.doRequest(ctx, http.MethodGet, RateLimitURLForTest+"?scope="+url.QueryEscape(scopeSlug), nil, "https://v0.app/chat", "application/json")
	if err != nil {
		return nil, fmt.Errorf("failed to fetch v0 rate limit: %w", err)
	}

	var info RateLimitInfo
	if err := json.Unmarshal(raw, &info); err != nil {
		return nil, fmt.Errorf("failed to decode v0 rate limit response: %w", err)
	}
	return &info, nil
}

func (c *Client) SendRequest(ctx context.Context, _ string, _ []interface{}, model string, onMessage func(upstream.SSEMessage), logger *debug.Logger) error {
	return c.SendRequestWithPayload(ctx, upstream.UpstreamRequest{Model: model}, onMessage, logger)
}

func (c *Client) SendRequestWithPayload(ctx context.Context, req upstream.UpstreamRequest, onMessage func(upstream.SSEMessage), logger *debug.Logger) error {
	if c == nil {
		return fmt.Errorf("v0 client is nil")
	}
	if strings.TrimSpace(c.userSession) == "" {
		return fmt.Errorf("missing v0 user_session")
	}

	timeoutMs := 180000
	if c.config != nil && c.config.RequestTimeout > 0 {
		timeoutMs = c.config.RequestTimeout * 1000
		if timeoutMs < 30000 {
			timeoutMs = 30000
		}
	}
	promptText := strings.TrimSpace(c.buildPrompt(req))
	if promptText == "" {
		promptText = "hello"
	}
	modelID := normalizeWebModel(req.Model)
	chatID := normalizeChatID(req.ChatSessionID)
	sendBody, referer, path, err := buildSendRequestBody(promptText, modelID, chatID, estimateMessageIndex(req.Messages))
	if err != nil {
		return fmt.Errorf("failed to build v0 send request: %w", err)
	}
	if logger != nil {
		logger.LogUpstreamRequest(SendURLForTest, map[string]string{"provider": "v0"}, sendBody)
	}
	raw, err := c.doRequest(ctx, http.MethodPost, SendURLForTest, sendBody, referer, "*/*")
	if err != nil {
		return err
	}
	if logger != nil {
		logger.LogUpstreamHTTPError(SendURLForTest, http.StatusOK, string(raw), nil)
	}
	reply := extractSendResponseText(raw, promptText)
	if reply == "" {
		return fmt.Errorf("v0 send request succeeded but assistant reply could not be parsed")
	}
	resolvedChatID := firstNonEmpty(extractSendResponseChatID(raw), extractChatIDFromPath(path), chatID)

	if onMessage != nil {
		if resolvedChatID != "" {
			onMessage(upstream.SSEMessage{
				Type:  "model.conversation_id",
				Event: map[string]interface{}{"id": resolvedChatID},
			})
		}
		if text := strings.TrimSpace(reply); text != "" {
			onMessage(upstream.SSEMessage{
				Type: "model.text-delta",
				Event: map[string]interface{}{
					"delta": text,
				},
			})
		}
		onMessage(upstream.SSEMessage{
			Type: "model.finish",
			Event: map[string]interface{}{
				"finishReason": "end_turn",
				"usage": map[string]int{
					"inputTokens":   estimateTokens(promptText),
					"outputTokens":  estimateTokens(reply),
					"input_tokens":  estimateTokens(promptText),
					"output_tokens": estimateTokens(reply),
				},
				"model": modelID,
			},
		})
	}
	return nil
}

func (c *Client) FetchDiscoveredModelChoices(ctx context.Context) ([]ModelChoice, string, error) {
	if c == nil {
		return nil, "", fmt.Errorf("v0 client is nil")
	}
	if strings.TrimSpace(c.userSession) == "" {
		return nil, "", fmt.Errorf("missing v0 user_session")
	}

	if _, scopedErr := c.FetchScopedUser(ctx); scopedErr != nil {
		return nil, "", scopedErr
	}
	return buildSeedModelChoices(), "v0_seed_fallback", nil
}

func normalizeDiscoveredModelChoices(items []ModelChoice) []ModelChoice {
	seen := make(map[string]struct{}, len(items))
	out := make([]ModelChoice, 0, len(items))
	for _, item := range items {
		id := normalizeWebModel(item.ID)
		name := strings.TrimSpace(item.Name)
		if name == "" {
			name = prettifyV0ModelName(id)
		}
		if id == "" {
			continue
		}
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		out = append(out, ModelChoice{ID: id, Name: name})
	}
	return out
}

func prettifyV0ModelName(modelID string) string {
	switch strings.ToLower(strings.TrimSpace(modelID)) {
	case "v0-auto":
		return "v0 Auto"
	case "v0-max":
		return "v0 Max"
	case "v0-mini":
		return "v0 Mini"
	case "v0-pro":
		return "v0 Pro"
	case "v0-max-fast":
		return "v0 Max Fast"
	default:
		return strings.TrimSpace(modelID)
	}
}

func buildSeedModelChoices() []ModelChoice {
	items := store.BuildV0SeedModels()
	out := make([]ModelChoice, 0, len(items))
	for _, item := range items {
		id := normalizeWebModel(item.ModelID)
		if id == "" {
			continue
		}
		out = append(out, ModelChoice{
			ID:   id,
			Name: firstNonEmpty(item.Name, prettifyV0ModelName(id), id),
		})
	}
	return out
}

func (c *Client) buildPrompt(req upstream.UpstreamRequest) string {
	if normalizeChatID(req.ChatSessionID) != "" {
		if latest := strings.TrimSpace(extractLatestUserMessageText(req.Messages)); latest != "" {
			return latest
		}
	}
	return flattenConversation(req.System, req.Messages)
}

func flattenConversation(system []prompt.SystemItem, messages []prompt.Message) string {
	parts := make([]string, 0, len(system)+len(messages))
	if sys := buildSystemText(system); sys != "" {
		parts = append(parts, "[system]\n"+sys)
	}
	for _, msg := range messages {
		if rendered := renderMessageForBridge(msg); rendered != "" {
			role := strings.TrimSpace(msg.Role)
			if role == "" {
				role = "user"
			}
			parts = append(parts, "["+role+"]\n"+rendered)
		}
	}
	return strings.TrimSpace(strings.Join(parts, "\n\n"))
}

func buildSystemText(system []prompt.SystemItem) string {
	parts := make([]string, 0, len(system))
	for _, item := range system {
		if text := strings.TrimSpace(item.Text); text != "" {
			parts = append(parts, text)
		}
	}
	return strings.TrimSpace(strings.Join(parts, "\n\n"))
}

func renderMessageForBridge(msg prompt.Message) string {
	if msg.Content.IsString() {
		return strings.TrimSpace(msg.Content.GetText())
	}

	blocks := msg.Content.GetBlocks()
	parts := make([]string, 0, len(blocks))
	for _, block := range blocks {
		switch block.Type {
		case "text":
			if text := strings.TrimSpace(block.Text); text != "" {
				parts = append(parts, text)
			}
		case "tool_use":
			raw, _ := json.Marshal(block.Input)
			if len(raw) == 0 || string(raw) == "null" {
				raw = []byte("{}")
			}
			parts = append(parts, fmt.Sprintf("[tool_use:%s] %s", firstNonEmpty(block.Name, block.ID, "tool"), string(raw)))
		case "tool_result":
			parts = append(parts, fmt.Sprintf("[tool_result:%s] %s", firstNonEmpty(block.ToolUseID, block.Name, "tool"), stringifyToolResult(block.Content)))
		}
	}
	return strings.TrimSpace(strings.Join(parts, "\n"))
}

func stringifyToolResult(v interface{}) string {
	switch x := v.(type) {
	case nil:
		return ""
	case string:
		return strings.TrimSpace(x)
	case []prompt.ContentBlock:
		out := make([]string, 0, len(x))
		for _, block := range x {
			if block.Type == "text" && strings.TrimSpace(block.Text) != "" {
				out = append(out, strings.TrimSpace(block.Text))
			}
		}
		return strings.Join(out, "\n")
	default:
		raw, err := json.Marshal(x)
		if err != nil {
			return ""
		}
		return string(raw)
	}
}

func extractLatestUserMessageText(messages []prompt.Message) string {
	for i := len(messages) - 1; i >= 0; i-- {
		msg := messages[i]
		if !strings.EqualFold(strings.TrimSpace(msg.Role), "user") {
			continue
		}
		if rendered := strings.TrimSpace(renderMessageForBridge(msg)); rendered != "" {
			return rendered
		}
	}
	return ""
}

func normalizeWebModel(model string) string {
	switch strings.ToLower(strings.TrimSpace(model)) {
	case "", "v0", "v0-max", "v0 max", "v0-1.5-md", "v0-1.5-lg", "v0-1.0-md":
		return defaultModelID
	case "v0-auto", "v0 auto":
		return "v0-auto"
	case "v0-mini", "v0 mini":
		return "v0-mini"
	case "v0-pro", "v0 pro":
		return "v0-pro"
	case "v0-max-fast", "v0 max fast":
		return "v0-max-fast"
	default:
		if strings.HasPrefix(strings.ToLower(strings.TrimSpace(model)), "v0-") {
			return strings.ToLower(strings.TrimSpace(model))
		}
		return defaultModelID
	}
}

func normalizeChatID(chatID string) string {
	chatID = strings.TrimSpace(chatID)
	if chatID == "" || strings.HasPrefix(chatID, "chat_") {
		return ""
	}
	return chatID
}

func estimateTokens(text string) int {
	if text == "" {
		return 0
	}
	return len(text) / 4
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if trimmed := strings.TrimSpace(value); trimmed != "" {
			return trimmed
		}
	}
	return ""
}

func (c *Client) doRequest(ctx context.Context, method, target string, body []byte, referer string, accept string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, method, target, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("failed to build v0 request: %w", err)
	}
	if accept == "" {
		accept = "application/json"
	}
	req.Header.Set("Accept", accept)
	req.Header.Set("Accept-Language", "zh-CN,zh;q=0.9,en;q=0.8")
	req.Header.Set("Origin", "https://v0.app")
	req.Header.Set("Referer", referer)
	req.Header.Set("Cookie", "user_session="+c.userSession)
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/146.0.0.0 Safari/537.36")
	if len(body) > 0 {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("v0 request failed: %w", err)
	}
	defer resp.Body.Close()

	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("v0 request error: status=%d, body=%s", resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	return raw, nil
}

func buildSendRequestBody(promptText, modelID, chatID string, index int) ([]byte, string, string, error) {
	if index <= 0 {
		index = 1
	}
	path := "/chat"
	referer := "https://v0.app/chat"
	genericRoute := "/chat"
	var chatValue interface{}
	if chatID != "" {
		path = "/chat/" + chatID
		referer = "https://v0.app/chat/" + chatID
		genericRoute = "/[id]"
		chatValue = chatID
	}
	metaRaw, err := json.Marshal(map[string]interface{}{
		"text":        promptText + "\n",
		"index":       index,
		"imageOnly":   false,
		"projectId":   nil,
		"messageMode": "build",
		"modelId":     modelID,
	})
	if err != nil {
		return nil, "", "", err
	}
	payload := map[string]interface{}{
		"action":         "SubmitNewUserMessage",
		"meta":           string(metaRaw),
		"referer":        referer,
		"error_code":     nil,
		"generic_route":  genericRoute,
		"call_layer":     "client",
		"chat_id":        chatValue,
		"message_id":     nil,
		"block_id":       nil,
		"domain":         "v0.app",
		"path":           path,
		"query_string":   nil,
		"browser_width":  1440,
		"browser_height": 900,
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return nil, "", "", err
	}
	return raw, referer, path, nil
}

func estimateMessageIndex(messages []prompt.Message) int {
	index := 1
	for _, msg := range messages {
		if strings.EqualFold(strings.TrimSpace(msg.Role), "user") && strings.TrimSpace(renderMessageForBridge(msg)) != "" {
			index++
		}
	}
	return index
}

func extractSendResponseText(raw []byte, promptText string) string {
	if len(raw) == 0 {
		return ""
	}
	var parsed interface{}
	if err := json.Unmarshal(raw, &parsed); err == nil {
		if text := extractFirstMeaningfulText(parsed, promptText); text != "" {
			return text
		}
	}
	matches := v0FieldPattern.FindAllStringSubmatch(string(raw), -1)
	best := ""
	for _, match := range matches {
		if len(match) < 2 {
			continue
		}
		var decoded string
		if err := json.Unmarshal([]byte(`"`+match[1]+`"`), &decoded); err != nil {
			decoded = match[1]
		}
		decoded = strings.TrimSpace(decoded)
		if isMeaningfulResponseText(decoded, promptText) && len(decoded) > len(best) {
			best = decoded
		}
	}
	if best != "" {
		return best
	}
	return fallbackMeaningfulResponseText(string(raw), promptText)
}

func extractFirstMeaningfulText(value interface{}, promptText string) string {
	best := ""
	var walk func(interface{})
	walk = func(node interface{}) {
		switch v := node.(type) {
		case string:
			text := strings.TrimSpace(v)
			if nested := extractMeaningfulTextFromNestedString(text, promptText); nested != "" && len(nested) > len(best) {
				best = nested
			}
			if !looksLikeStructuredPayload(text) && isMeaningfulResponseText(text, promptText) && len(text) > len(best) {
				best = text
			}
		case []interface{}:
			for _, item := range v {
				walk(item)
			}
		case map[string]interface{}:
			for key, item := range v {
				switch strings.ToLower(strings.TrimSpace(key)) {
				case "text", "content", "response", "message", "output", "completion":
					walk(item)
				default:
					walk(item)
				}
			}
		}
	}
	walk(value)
	return best
}

func extractMeaningfulTextFromNestedString(text, promptText string) string {
	text = strings.TrimSpace(text)
	if text == "" {
		return ""
	}
	if !(strings.HasPrefix(text, "{") || strings.HasPrefix(text, "[") || strings.HasPrefix(text, "\"{") || strings.HasPrefix(text, "\"[")) {
		return ""
	}
	unquoted := text
	if strings.HasPrefix(unquoted, "\"") {
		var decoded string
		if err := json.Unmarshal([]byte(unquoted), &decoded); err == nil {
			unquoted = strings.TrimSpace(decoded)
		}
	}
	var parsed interface{}
	if err := json.Unmarshal([]byte(unquoted), &parsed); err != nil {
		return ""
	}
	return extractFirstMeaningfulText(parsed, promptText)
}

func looksLikeStructuredPayload(text string) bool {
	text = strings.TrimSpace(text)
	return strings.HasPrefix(text, "{") || strings.HasPrefix(text, "[") || strings.HasPrefix(text, "\"{") || strings.HasPrefix(text, "\"[")
}

func isMeaningfulResponseText(text, promptText string) bool {
	text = strings.TrimSpace(text)
	if text == "" {
		return false
	}
	if strings.EqualFold(text, strings.TrimSpace(promptText)) {
		return false
	}
	switch strings.ToLower(text) {
	case "submitnewusermessage", "build", "client", "v0.app", "ok", "true", "success", "done":
		return false
	}
	return len([]rune(text)) >= 2
}

func fallbackMeaningfulResponseText(rawText, promptText string) string {
	rawText = strings.TrimSpace(rawText)
	if rawText == "" {
		return ""
	}
	lines := strings.Split(rawText, "\n")
	best := ""
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(strings.ToLower(line), "data:") {
			line = strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		}
		line = stripCommonResponseWrappers(line)
		if looksLikeStructuredPayload(line) {
			continue
		}
		if isMeaningfulResponseText(line, promptText) && len([]rune(line)) > len([]rune(best)) {
			best = line
		}
	}
	return best
}

func stripCommonResponseWrappers(text string) string {
	text = strings.TrimSpace(text)
	text = strings.Trim(text, "`")
	text = strings.Trim(text, "\"")
	text = strings.TrimSpace(text)
	if !utf8.ValidString(text) {
		return ""
	}
	text = strings.ReplaceAll(text, "\\n", "\n")
	text = strings.ReplaceAll(text, "\\t", "\t")
	text = strings.ReplaceAll(text, "\\\"", "\"")
	text = strings.TrimSpace(text)
	return text
}

func extractSendResponseChatID(raw []byte) string {
	if match := v0ChatURLPattern.FindSubmatch(raw); len(match) > 1 {
		return strings.TrimSpace(string(match[1]))
	}
	var parsed interface{}
	if err := json.Unmarshal(raw, &parsed); err == nil {
		if id := extractChatIDFromValue(parsed); id != "" {
			return id
		}
	}
	return ""
}

func extractChatIDFromValue(value interface{}) string {
	switch v := value.(type) {
	case map[string]interface{}:
		for key, item := range v {
			lower := strings.ToLower(strings.TrimSpace(key))
			switch lower {
			case "chatid", "chat_id", "id":
				if text, ok := item.(string); ok {
					if id := normalizeChatID(text); id != "" {
						return id
					}
				}
			case "referer", "url", "pathname", "path":
				if text, ok := item.(string); ok {
					if id := extractChatIDFromPath(text); id != "" {
						return id
					}
				}
			}
			if id := extractChatIDFromValue(item); id != "" {
				return id
			}
		}
	case []interface{}:
		for _, item := range v {
			if id := extractChatIDFromValue(item); id != "" {
				return id
			}
		}
	}
	return ""
}

func extractChatIDFromPath(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return ""
	}
	if strings.HasPrefix(value, "https://") || strings.HasPrefix(value, "http://") {
		if match := v0ChatURLPattern.FindStringSubmatch(value); len(match) > 1 {
			return strings.TrimSpace(match[1])
		}
	}
	parts := strings.Split(value, "/")
	for i := 0; i < len(parts)-1; i++ {
		if parts[i] == "chat" && strings.TrimSpace(parts[i+1]) != "" {
			return strings.TrimSpace(parts[i+1])
		}
	}
	return ""
}
