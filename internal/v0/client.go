package v0

import (
	"bytes"
	"context"
	"crypto/rand"
	"fmt"
	"io"
	"net/http"
	"net/url"
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
	defaultChatURL       = "https://v0.app/chat/api/chat"
	defaultLeafURL       = "https://v0.app/chat/api/chat/leaf"
	defaultLatestChatURL = "https://v0.app/api/chat/chat/latest"
	defaultModelID       = "v0-max"
	v0ChatIDLength       = 11
	v0MessageIDLength    = 32
	v0PollInterval       = 1500 * time.Millisecond
	v0MinPollingWindow   = 30 * time.Second
)

var (
	ScopedUserURLForTest = defaultScopedUserURL
	PlanInfoURLForTest   = defaultPlanInfoURL
	ScopesURLForTest     = defaultScopesURL
	RateLimitURLForTest  = defaultRateLimitURL
	ChatURLForTest       = defaultChatURL
	LeafURLForTest       = defaultLeafURL
	LatestChatURLForTest = defaultLatestChatURL
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

type v0LatestResponse struct {
	OK    bool          `json:"ok"`
	Value v0LatestValue `json:"value"`
}

type v0LatestValue struct {
	NewMessages          []v0ChatMessage                `json:"newMessages"`
	ResumeUserMessageMap map[string]v0ResumeUserMessage `json:"resumeUserMessageMap"`
}

type v0ResumeUserMessage struct {
	ResponseMessageID string `json:"responseMessageId"`
	Type              string `json:"type"`
}

type v0ChatMessage struct {
	ID      string           `json:"id"`
	Role    string           `json:"role"`
	Content v0MessageContent `json:"content"`
}

type v0MessageContent struct {
	Type  string      `json:"type"`
	Value interface{} `json:"value"`
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

	promptText := strings.TrimSpace(c.buildPrompt(req))
	if promptText == "" {
		promptText = "hello"
	}
	modelID := normalizeWebModel(req.Model)
	chatID := normalizeChatID(req.ChatSessionID)
	isNewChat := chatID == ""
	if isNewChat {
		var err error
		chatID, err = generateRandomID(v0ChatIDLength)
		if err != nil {
			return fmt.Errorf("failed to generate v0 chat id: %w", err)
		}
	}
	userMessageID, err := generateRandomID(v0MessageIDLength)
	if err != nil {
		return fmt.Errorf("failed to generate v0 message id: %w", err)
	}

	teamSlug, err := c.resolveTeamSlug(ctx)
	if err != nil {
		return err
	}

	var parentID string
	if !isNewChat {
		parentID, _ = c.fetchLatestMessageID(ctx, chatID)
	}

	chatBody, referer, err := buildChatRequestBody(promptText, modelID, chatID, teamSlug, userMessageID, parentID, isNewChat)
	if err != nil {
		return fmt.Errorf("failed to build v0 chat request: %w", err)
	}
	if logger != nil {
		logger.LogUpstreamRequest(ChatURLForTest, map[string]string{"provider": "v0"}, chatBody)
	}
	raw, err := c.doRequest(ctx, http.MethodPost, ChatURLForTest, chatBody, referer, "*/*")
	if err != nil {
		return err
	}
	if logger != nil && len(raw) > 0 {
		logger.LogUpstreamHTTPError(ChatURLForTest, http.StatusOK, string(raw), nil)
	}

	reply, assistantMessageID, err := c.waitForAssistantReply(ctx, chatID, userMessageID)
	if err != nil {
		return err
	}
	if assistantMessageID != "" {
		_, _ = c.postLeaf(ctx, chatID, assistantMessageID)
	}

	if onMessage != nil {
		onMessage(upstream.SSEMessage{
			Type:  "model.conversation_id",
			Event: map[string]interface{}{"id": chatID},
		})
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
	return strings.TrimPrefix(chatID, "-")
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

	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("v0 request error: status=%d, body=%s", resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	return raw, nil
}

func buildChatRequestBody(promptText, modelID, chatID, teamSlug, userMessageID, parentID string, isNew bool) ([]byte, string, error) {
	payload := map[string]interface{}{
		"messageContent": map[string]interface{}{
			"version": 1,
			"parts": []map[string]string{
				{"type": "mdx", "content": promptText},
				{"type": "mdx", "content": "\n"},
			},
			"type": "parts",
		},
		"messageId": userMessageID,
		"chatId":    chatID,
		"isNew":     isNew,
		"team":      teamSlug,
		"modelConfiguration": map[string]interface{}{
			"modelId":          modelID,
			"imageGenerations": true,
			"thinking":         false,
		},
		"suggestedActionsEnabled":         true,
		"optimisticConnectedIntegrations": []interface{}{},
		"optimisticEnvVarKeys":            []interface{}{},
		"mcpServers":                      []interface{}{},
		"chatCreationTime":                time.Now().UnixMilli(),
	}
	if !isNew && parentID != "" {
		payload["parentId"] = parentID
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return nil, "", err
	}
	return raw, "https://v0.app/chat/" + chatID, nil
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

func generateRandomID(length int) (string, error) {
	const alphabet = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz"
	if length <= 0 {
		return "", fmt.Errorf("invalid id length")
	}
	buf := make([]byte, length)
	randBuf := make([]byte, length)
	if _, err := rand.Read(randBuf); err != nil {
		return "", err
	}
	for i := range buf {
		buf[i] = alphabet[int(randBuf[i])%len(alphabet)]
	}
	return string(buf), nil
}

func (c *Client) resolveTeamSlug(ctx context.Context) (string, error) {
	if c == nil {
		return "", fmt.Errorf("v0 client is nil")
	}
	candidates := make([]string, 0, 4)
	if c.account != nil {
		candidates = append(candidates, normalizeTeamSlug(c.account.ProjectID))
		candidates = append(candidates, normalizeTeamSlug(c.account.Name))
	}

	scopes, scopesErr := c.FetchScopes(ctx)
	scopeByID := make(map[string]string, len(scopes))
	for _, scope := range scopes {
		if slug := normalizeTeamSlug(scope.Slug); slug != "" {
			scopeByID[normalizeTeamSlug(scope.ID)] = slug
			scopeByID[normalizeTeamSlug(scope.Name)] = slug
			scopeByID[slug] = slug
		}
	}
	for _, candidate := range candidates {
		if slug := scopeByID[normalizeTeamSlug(candidate)]; slug != "" {
			return slug, nil
		}
		if candidate != "" && scopesErr != nil {
			return candidate, nil
		}
	}

	scopedUser, scopedErr := c.FetchScopedUser(ctx)
	if scopedUser != nil {
		for _, candidate := range []string{scopedUser.Scope, scopedUser.TeamID, scopedUser.DefaultTeamID, scopedUser.TeamName} {
			if slug := scopeByID[normalizeTeamSlug(candidate)]; slug != "" {
				return slug, nil
			}
			if normalized := normalizeTeamSlug(candidate); normalized != "" && len(scopes) == 0 {
				return normalized, nil
			}
		}
	}

	for _, scope := range scopes {
		if slug := normalizeTeamSlug(scope.Slug); slug != "" {
			return slug, nil
		}
	}

	if scopedErr != nil {
		return "", fmt.Errorf("failed to resolve v0 team slug: %w", scopedErr)
	}
	if scopesErr != nil {
		return "", fmt.Errorf("failed to resolve v0 team slug: %w", scopesErr)
	}
	return "", fmt.Errorf("failed to resolve v0 team slug")
}

func normalizeTeamSlug(value string) string {
	value = strings.TrimSpace(value)
	value = strings.TrimPrefix(value, "team:")
	value = strings.TrimPrefix(value, "project:")
	value = strings.TrimPrefix(value, "-")
	return strings.TrimSpace(value)
}

func (c *Client) fetchLatestMessageID(ctx context.Context, chatID string) (string, error) {
	latest, err := c.fetchLatestChatMessages(ctx, chatID, 0)
	if err != nil {
		return "", err
	}
	for i := len(latest.Value.NewMessages) - 1; i >= 0; i-- {
		if id := strings.TrimSpace(latest.Value.NewMessages[i].ID); id != "" {
			return id, nil
		}
	}
	return "", nil
}

func (c *Client) waitForAssistantReply(ctx context.Context, chatID, userMessageID string) (string, string, error) {
	deadline := time.Now().Add(v0MinPollingWindow)
	if dl, ok := ctx.Deadline(); ok && dl.Before(deadline) {
		deadline = dl
	}

	for {
		latest, err := c.fetchLatestChatMessages(ctx, chatID, 0)
		if err == nil {
			if reply, assistantID := extractAssistantReplyFromLatest(latest, userMessageID); reply != "" {
				return reply, assistantID, nil
			}
		}
		if time.Now().After(deadline) {
			break
		}
		select {
		case <-ctx.Done():
			return "", "", ctx.Err()
		case <-time.After(v0PollInterval):
		}
	}
	return "", "", fmt.Errorf("v0 assistant reply could not be parsed from latest chat state")
}

func (c *Client) fetchLatestChatMessages(ctx context.Context, chatID string, lastSyncedAt int64) (*v0LatestResponse, error) {
	queryURL := LatestChatURLForTest + "?chatId=" + url.QueryEscape(chatID) + "&lastSyncedAt=" + url.QueryEscape(fmt.Sprintf("%d", lastSyncedAt))
	raw, err := c.doRequest(ctx, http.MethodGet, queryURL, nil, "https://v0.app/chat/"+chatID, "application/json")
	if err != nil {
		return nil, err
	}
	var parsed v0LatestResponse
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return nil, fmt.Errorf("failed to decode v0 latest chat response: %w", err)
	}
	if !parsed.OK {
		return nil, fmt.Errorf("v0 latest chat request was not accepted")
	}
	return &parsed, nil
}

func extractAssistantReplyFromLatest(latest *v0LatestResponse, userMessageID string) (string, string) {
	if latest == nil {
		return "", ""
	}
	targetAssistantID := ""
	if resume, ok := latest.Value.ResumeUserMessageMap[userMessageID]; ok {
		targetAssistantID = strings.TrimSpace(resume.ResponseMessageID)
	}

	for _, msg := range latest.Value.NewMessages {
		if !strings.EqualFold(strings.TrimSpace(msg.Role), "assistant") {
			continue
		}
		if targetAssistantID != "" && strings.TrimSpace(msg.ID) != targetAssistantID {
			continue
		}
		if text := renderV0MessageContent(msg.Content); text != "" {
			return text, strings.TrimSpace(msg.ID)
		}
	}

	for i := len(latest.Value.NewMessages) - 1; i >= 0; i-- {
		msg := latest.Value.NewMessages[i]
		if !strings.EqualFold(strings.TrimSpace(msg.Role), "assistant") {
			continue
		}
		if text := renderV0MessageContent(msg.Content); text != "" {
			return text, strings.TrimSpace(msg.ID)
		}
	}
	return "", ""
}

func renderV0MessageContent(content v0MessageContent) string {
	if !strings.EqualFold(strings.TrimSpace(content.Type), "message-binary-format") {
		return ""
	}
	parts := make([]string, 0, 8)
	appendRenderedV0Node(&parts, content.Value)
	return cleanupRenderedV0Text(parts)
}

func appendRenderedV0Node(parts *[]string, node interface{}) {
	switch v := node.(type) {
	case string:
		if text := strings.TrimSpace(stripCommonResponseWrappers(v)); text != "" {
			*parts = append(*parts, text)
		}
	case []interface{}:
		appendRenderedV0Array(parts, v)
	case map[string]interface{}:
		for _, value := range v {
			appendRenderedV0Node(parts, value)
		}
	}
}

func appendRenderedV0Array(parts *[]string, items []interface{}) {
	if len(items) == 0 {
		return
	}
	tag, _ := items[0].(string)
	switch tag {
	case "AssistantMessageContentPart":
		return
	case "text":
		if len(items) > 2 {
			if text, ok := items[2].(string); ok {
				text = decodeV0TextNode(text)
				if strings.TrimSpace(text) != "" {
					*parts = append(*parts, text)
				}
			}
		}
		return
	case "br":
		*parts = append(*parts, "\n")
		return
	case "p", "li", "h1", "h2", "h3", "h4", "h5", "h6", "blockquote", "pre":
		start := len(*parts)
		for _, item := range items[1:] {
			appendRenderedV0Node(parts, item)
		}
		if len(*parts) > start {
			*parts = append(*parts, "\n\n")
		}
		return
	}

	start := 0
	if tag != "" {
		start = 1
	}
	for _, item := range items[start:] {
		appendRenderedV0Node(parts, item)
	}
}

func cleanupRenderedV0Text(parts []string) string {
	if len(parts) == 0 {
		return ""
	}
	var builder strings.Builder
	for _, part := range parts {
		builder.WriteString(part)
	}
	text := builder.String()
	text = strings.ReplaceAll(text, "\r\n", "\n")
	text = strings.ReplaceAll(text, "\r", "\n")
	lines := strings.Split(text, "\n")
	cleaned := make([]string, 0, len(lines))
	blank := false
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			if blank {
				continue
			}
			blank = true
			cleaned = append(cleaned, "")
			continue
		}
		blank = false
		cleaned = append(cleaned, line)
	}
	return strings.TrimSpace(strings.Join(cleaned, "\n"))
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
	return text
}

func decodeV0TextNode(text string) string {
	if !utf8.ValidString(text) {
		return ""
	}
	text = strings.ReplaceAll(text, "\\n", "\n")
	text = strings.ReplaceAll(text, "\\t", "\t")
	text = strings.ReplaceAll(text, "\\\"", "\"")
	return text
}

func (c *Client) postLeaf(ctx context.Context, chatID, messageID string) ([]byte, error) {
	payload, err := json.Marshal(map[string]string{
		"chatId":    chatID,
		"messageId": messageID,
	})
	if err != nil {
		return nil, err
	}
	return c.doRequest(ctx, http.MethodPost, LeafURLForTest, payload, "https://v0.app/chat/"+chatID, "application/json")
}
