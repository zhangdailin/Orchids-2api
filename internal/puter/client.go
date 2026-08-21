package puter

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/goccy/go-json"

	"orchids-api/internal/config"
	"orchids-api/internal/debug"
	"orchids-api/internal/modelpolicy"
	"orchids-api/internal/prompt"
	"orchids-api/internal/store"
	"orchids-api/internal/upstream"
	"orchids-api/internal/util"
)

const (
	defaultAPIURL           = "https://api.puter.com/drivers/call"
	defaultMeteringUsageURL = "https://api.puter.com/metering/usage"
	defaultModelID          = modelpolicy.DefaultPuterModelID
	defaultMethod           = "complete"
	defaultIface            = "puter-chat-completion"
)

var (
	puterAPIURL           = defaultAPIURL
	puterMeteringUsageURL = defaultMeteringUsageURL
)

type Client struct {
	httpClient     *http.Client
	authToken      string
	requestTimeout time.Duration
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
		httpClient:     util.GetSharedHTTPClient(proxyKey, timeout, proxyFunc),
		authToken:      ResolveAuthToken(acc),
		requestTimeout: timeout,
	}
}

func ResolveAuthToken(acc *store.Account) string {
	if acc == nil {
		return ""
	}
	for _, value := range []string{acc.ClientCookie, acc.Token, acc.SessionCookie} {
		if token := extractAuthToken(value); token != "" {
			return token
		}
	}
	return ""
}

func extractAuthToken(value string) string {
	trimmed := strings.Trim(strings.TrimSpace(value), `"'`)
	if trimmed == "" {
		return ""
	}
	if !strings.Contains(trimmed, "=") {
		return trimmed
	}
	for _, part := range strings.Split(trimmed, ";") {
		key, val, ok := strings.Cut(strings.TrimSpace(part), "=")
		if !ok {
			continue
		}
		switch strings.ToLower(strings.TrimSpace(key)) {
		case "auth_token", "puter_auth_token", "token", "auth":
			if token := strings.Trim(strings.TrimSpace(val), `"'`); token != "" {
				return token
			}
		}
	}
	return trimmed
}

// Close satisfies the shared upstream client lifecycle. The HTTP transport is
// process-wide, so an individual Puter client owns no resources to close.
func (c *Client) Close() {}

func (c *Client) VerifyAuthToken(ctx context.Context) error {
	return c.VerifyModel(ctx, defaultModelID)
}

func (c *Client) VerifyModel(ctx context.Context, modelID string) error {
	if c == nil {
		return fmt.Errorf("puter client is nil")
	}
	if strings.TrimSpace(c.authToken) == "" {
		return fmt.Errorf("missing puter auth token")
	}
	modelID = strings.TrimSpace(modelID)
	if modelID == "" {
		modelID = defaultModelID
	}

	puterReq, err := c.buildRequest(upstream.UpstreamRequest{
		Model: modelID,
		Messages: []prompt.Message{
			{Role: "user", Content: prompt.MessageContent{Text: "Reply only OK."}},
		},
	}, true)
	if err != nil {
		return err
	}
	body, err := json.Marshal(puterReq)
	if err != nil {
		return fmt.Errorf("failed to marshal puter verify request: %w", err)
	}

	reqCtx, cancel := util.WithDefaultTimeout(ctx, 45*time.Second)
	defer cancel()
	resp, err := c.doChatRequest(reqCtx, body)
	if err != nil {
		return fmt.Errorf("puter verify request failed: %w", err)
	}
	defer resp.Body.Close()

	result, err := consumePuterStream(resp.Body, nil)
	if err != nil {
		return err
	}
	if !result.SawMeaningfulEvent {
		return fmt.Errorf("puter verify request returned no usable stream events")
	}
	return nil
}

func (c *Client) FetchMonthlyUsage(ctx context.Context) (*MonthlyUsage, error) {
	if c == nil {
		return nil, fmt.Errorf("puter client is nil")
	}
	if strings.TrimSpace(c.authToken) == "" {
		return nil, fmt.Errorf("missing puter auth token")
	}

	reqCtx, cancel := util.WithDefaultTimeout(ctx, 20*time.Second)
	defer cancel()
	httpReq, err := http.NewRequestWithContext(reqCtx, http.MethodGet, puterMeteringUsageURL, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create puter usage request: %w", err)
	}
	c.applyUsageHeaders(httpReq)

	resp, err := c.httpClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("failed to send puter usage request: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 8192))
		return nil, fmt.Errorf("puter usage API error: status=%d, body=%s", resp.StatusCode, strings.TrimSpace(string(raw)))
	}

	var usage MonthlyUsage
	if err := json.NewDecoder(io.LimitReader(resp.Body, 8<<20)).Decode(&usage); err != nil {
		return nil, fmt.Errorf("failed to decode puter usage response: %w", err)
	}
	return &usage, nil
}

func (c *Client) SendRequestWithPayload(ctx context.Context, req upstream.UpstreamRequest, onMessage func(upstream.SSEMessage), logger *debug.Logger) error {
	if c == nil {
		return fmt.Errorf("puter client is nil")
	}
	if strings.TrimSpace(c.authToken) == "" {
		return fmt.Errorf("missing puter auth token")
	}

	puterReq, err := c.buildRequest(req, false)
	if err != nil {
		return err
	}
	body, err := json.Marshal(puterReq)
	if err != nil {
		return fmt.Errorf("failed to marshal puter request: %w", err)
	}
	if logger != nil {
		logger.LogUpstreamRequest(puterAPIURL, map[string]string{"provider": "puter"}, body)
	}

	reqCtx, cancel := util.WithDefaultTimeout(ctx, c.requestTimeout)
	defer cancel()
	resp, err := c.doChatRequest(reqCtx, body)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	result, err := consumePuterStream(resp.Body, onMessage)
	if err != nil {
		return err
	}
	if !result.SawMeaningfulEvent {
		return fmt.Errorf("puter API returned no usable stream events")
	}
	if onMessage != nil {
		event := map[string]interface{}{"finishReason": result.FinishReason()}
		if len(result.Usage) > 0 {
			event["usage"] = result.Usage
		}
		onMessage(upstream.SSEMessage{Type: "model.finish", Event: event})
	}
	return nil
}

func (c *Client) doChatRequest(ctx context.Context, body []byte) (*http.Response, error) {
	if err := waitForPuterRequestSlot(ctx, puterAPIURL, c.authToken); err != nil {
		return nil, fmt.Errorf("puter request pacing interrupted: %w", err)
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, puterAPIURL, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("failed to create puter request: %w", err)
	}
	c.applyHeaders(httpReq)

	resp, err := c.httpClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("failed to send puter request: %w", err)
	}
	if resp.StatusCode == http.StatusOK {
		return resp, nil
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 8192))
	return nil, fmt.Errorf("puter API error: status=%d, body=%s", resp.StatusCode, strings.TrimSpace(string(raw)))
}

func (c *Client) buildRequest(req upstream.UpstreamRequest, testMode bool) (*Request, error) {
	modelID := strings.TrimSpace(req.Model)
	if modelID == "" {
		modelID = defaultModelID
	}
	service, err := serviceForModel(modelID)
	if err != nil {
		return nil, err
	}

	tools := normalizeToolDefinitions(req.Tools)
	if req.NoTools {
		tools = nil
	}
	// 保真：系统条目逐字透传为独立 system 消息，不注入任何客户端未发送的内容。
	msgs := convertMessages(req.Messages, req.System)
	if service == "deepseek" {
		// puter 的 DeepSeekProvider 会在每个 tool 消息后注入 system 消息,
		// 多 tool_call 轮次会被打断配对;拆成单 tool_call 序列绕开该行为。
		msgs = splitMultiToolCalls(msgs)
	}
	return &Request{
		Interface: defaultIface,
		Service:   service,
		TestMode:  testMode,
		Method:    defaultMethod,
		Args: RequestArgs{
			Messages: msgs,
			Model:    modelID,
			Stream:   true,
			Tools:    tools,
		},
		AuthToken: c.authToken,
	}, nil
}

func (c *Client) applyHeaders(req *http.Request) {
	req.Header.Set("Accept", "application/x-ndjson, application/json")
	req.Header.Set("Content-Type", "text/plain;actually=json")
}

func (c *Client) applyUsageHeaders(req *http.Request) {
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.authToken)
}

func serviceForModel(modelID string) (string, error) {
	modelID = strings.ToLower(strings.TrimSpace(modelID))
	if !modelpolicy.IsLatestPuterModelID(modelID) {
		return "", fmt.Errorf("unsupported puter model %q", modelID)
	}
	switch {
	case strings.HasPrefix(modelID, "claude-"):
		return "claude", nil
	case strings.HasPrefix(modelID, "gpt-"):
		return "openai", nil
	case strings.HasPrefix(modelID, "gemini-"):
		return "google", nil
	case strings.HasPrefix(modelID, "grok-"):
		return "x-ai", nil
	case strings.HasPrefix(modelID, "deepseek-"):
		return "deepseek", nil
	case strings.HasPrefix(modelID, "mistral-"):
		return "mistral", nil
	default:
		return "", fmt.Errorf("puter model %q has no configured service", modelID)
	}
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
	if apiErr.Status > 0 {
		parts = append(parts, fmt.Sprintf("status=%d", apiErr.Status))
	}
	if msg := strings.TrimSpace(apiErr.Message); msg != "" {
		parts = append(parts, "message="+msg)
	}
	if len(parts) == 0 {
		return fmt.Errorf("puter API error: %s", strings.TrimSpace(raw))
	}
	return fmt.Errorf("puter API error: %s", strings.Join(parts, ", "))
}
