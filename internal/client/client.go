package client

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"strings"
	"sync"
	"time"

	"orchids-api/internal/clerk"
	"orchids-api/internal/config"
	"orchids-api/internal/debug"
	"orchids-api/internal/prompt"
	"orchids-api/internal/store"
)

const upstreamURL = "https://orchids-server.calmstone-6964e08a.westeurope.azurecontainerapps.io/agent/coding-agent"

const (
	defaultTokenTTL = 5 * time.Minute
	tokenExpirySkew = 30 * time.Second
)

type Client struct {
	config     *config.Config
	account    *store.Account
	httpClient *http.Client
}

type UpstreamRequest struct {
	Prompt      string
	ChatHistory []interface{}
	Model       string
	Messages    []prompt.Message
	System      []prompt.SystemItem
	Tools       []interface{}
	NoTools     bool
	NoThinking  bool
}

type TokenResponse struct {
	JWT string `json:"jwt"`
}

type AgentRequest struct {
	Prompt        string              `json:"prompt"`
	ChatHistory   []interface{}       `json:"chatHistory"`
	ProjectID     string              `json:"projectId"`
	CurrentPage   interface{}         `json:"currentPage"`
	AgentMode     string              `json:"agentMode"`
	Mode          string              `json:"mode"`
	GitRepoUrl    string              `json:"gitRepoUrl"`
	Email         string              `json:"email"`
	ChatSessionID int                 `json:"chatSessionId"`
	UserID        string              `json:"userId"`
	APIVersion    int                 `json:"apiVersion"`
	Model         string              `json:"model,omitempty"`
	Messages      []prompt.Message    `json:"messages,omitempty"`
	System        []prompt.SystemItem `json:"system,omitempty"`
	Tools         []interface{}       `json:"tools,omitempty"`
}

type SSEMessage struct {
	Type  string                 `json:"type"`
	Event map[string]interface{} `json:"event,omitempty"`
	Raw   map[string]interface{} `json:"-"`
}

type cachedToken struct {
	token     string
	expiresAt time.Time
}

var tokenCache = struct {
	mu    sync.RWMutex
	items map[string]cachedToken
}{
	items: map[string]cachedToken{},
}

var defaultHTTPClient = newHTTPClient()

var bufferPool = sync.Pool{
	New: func() interface{} {
		sb := new(strings.Builder)
		sb.Grow(4096)
		return sb
	},
}

var bufioReaderPool = sync.Pool{
	New: func() interface{} {
		return bufio.NewReaderSize(nil, 32768)
	},
}

var byteBufferPool = sync.Pool{
	New: func() interface{} {
		return bytes.NewBuffer(make([]byte, 0, 4096))
	},
}

func newHTTPClient() *http.Client {
	return &http.Client{
		Transport: &http.Transport{
			Proxy:                 http.ProxyFromEnvironment,
			MaxIdleConns:          100,
			MaxIdleConnsPerHost:   100,
			IdleConnTimeout:       90 * time.Second,
			TLSHandshakeTimeout:   10 * time.Second,
			ExpectContinueTimeout: 1 * time.Second,
			ResponseHeaderTimeout: 30 * time.Second,
		},
	}
}

func New(cfg *config.Config) *Client {
	return &Client{
		config:     cfg,
		httpClient: defaultHTTPClient,
	}
}

func NewFromAccount(acc *store.Account, base *config.Config) *Client {
	cfg := &config.Config{
		SessionID:           acc.SessionID,
		ClientCookie:        acc.ClientCookie,
		SessionCookie:       acc.SessionCookie,
		ClientUat:           acc.ClientUat,
		ProjectID:           acc.ProjectID,
		UserID:              acc.UserID,
		AgentMode:           acc.AgentMode,
		Email:               acc.Email,
		UpstreamMode:        "",
		UpstreamURL:         "",
		UpstreamToken:       "",
		OrchidsAPIBaseURL:   "",
		OrchidsWSURL:        "",
		OrchidsAPIVersion:   "",
		OrchidsImpl:         "",
		OrchidsLocalWorkdir: "",
		AutoRefreshToken:    false,
	}
	if base != nil {
		cfg.UpstreamMode = base.UpstreamMode
		cfg.UpstreamURL = base.UpstreamURL
		cfg.UpstreamToken = base.UpstreamToken
		cfg.OrchidsAPIBaseURL = base.OrchidsAPIBaseURL
		cfg.OrchidsWSURL = base.OrchidsWSURL
		cfg.OrchidsAPIVersion = base.OrchidsAPIVersion
		cfg.OrchidsImpl = base.OrchidsImpl
		cfg.OrchidsLocalWorkdir = base.OrchidsLocalWorkdir
		cfg.OrchidsAllowRunCommand = base.OrchidsAllowRunCommand
		cfg.OrchidsRunAllowlist = base.OrchidsRunAllowlist
		cfg.AutoRefreshToken = base.AutoRefreshToken
	}
	return &Client{
		config:     cfg,
		account:    acc,
		httpClient: defaultHTTPClient,
	}
}

func (c *Client) GetToken() (string, error) {
	if c.config != nil && c.config.UpstreamToken != "" {
		return c.config.UpstreamToken, nil
	}

	if c.config != nil && c.config.AutoRefreshToken {
		return c.forceRefreshToken()
	}

	if cached, ok := getCachedToken(c.config.SessionID); ok {
		return cached, nil
	}

	return c.fetchToken()
}

func (c *Client) forceRefreshToken() (string, error) {
	if c.config == nil {
		return "", fmt.Errorf("missing config")
	}

	if strings.TrimSpace(c.config.ClientCookie) != "" {
		info, err := clerk.FetchAccountInfo(c.config.ClientCookie)
		if err == nil && info.JWT != "" {
			c.applyAccountInfo(info)
			setCachedToken(c.config.SessionID, info.JWT)
			return info.JWT, nil
		}
	}

	return c.fetchToken()
}

func (c *Client) fetchToken() (string, error) {
	if c.config == nil {
		return "", fmt.Errorf("missing config")
	}

	url := fmt.Sprintf("https://clerk.orchids.app/v1/client/sessions/%s/tokens?__clerk_api_version=2025-11-10&_clerk_js_version=5.117.0", c.config.SessionID)

	req, err := http.NewRequest("POST", url, strings.NewReader("organization_id="))
	if err != nil {
		return "", err
	}

	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Cookie", c.config.GetCookies())

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("token request failed with status %d: %s", resp.StatusCode, string(body))
	}

	var tokenResp TokenResponse
	if err := json.NewDecoder(resp.Body).Decode(&tokenResp); err != nil {
		return "", err
	}

	setCachedToken(c.config.SessionID, tokenResp.JWT)
	return tokenResp.JWT, nil
}

func (c *Client) applyAccountInfo(info *clerk.AccountInfo) {
	if c.config == nil || info == nil {
		return
	}
	if strings.TrimSpace(info.SessionID) != "" {
		c.config.SessionID = info.SessionID
	}
	if strings.TrimSpace(info.ClientUat) != "" {
		c.config.ClientUat = info.ClientUat
	}
	if strings.TrimSpace(info.ProjectID) != "" {
		c.config.ProjectID = info.ProjectID
	}
	if strings.TrimSpace(info.UserID) != "" {
		c.config.UserID = info.UserID
	}
	if strings.TrimSpace(info.Email) != "" {
		c.config.Email = info.Email
	}
}

func (c *Client) SendRequest(ctx context.Context, prompt string, chatHistory []interface{}, model string, onMessage func(SSEMessage), logger *debug.Logger) error {
	req := UpstreamRequest{
		Prompt:      prompt,
		ChatHistory: chatHistory,
		Model:       model,
		Messages:    nil, // SendRequest is legacy, use SendRequestWithPayload for full objects
	}
	return c.SendRequestWithPayload(ctx, req, onMessage, logger)
}

func (c *Client) SendRequestWithPayload(ctx context.Context, req UpstreamRequest, onMessage func(SSEMessage), logger *debug.Logger) error {
	mode := strings.ToLower(strings.TrimSpace(c.config.UpstreamMode))
	if mode == "ws" || mode == "websocket" {
		err := c.sendRequestWSAIClient(ctx, req, onMessage, logger)
		if err != nil {
			if isWSFallback(err) && ctx.Err() == nil {
				if logger != nil {
					logger.LogUpstreamSSE("ws_fallback", err.Error())
				}
				return c.sendRequestSSE(ctx, req, onMessage, logger)
			}
			return err
		}
		return nil
	}
	return c.sendRequestSSE(ctx, req, onMessage, logger)
}

func (c *Client) sendRequestSSE(ctx context.Context, req UpstreamRequest, onMessage func(SSEMessage), logger *debug.Logger) error {
	timeout := 120 * time.Second
	if c.config != nil && c.config.RequestTimeout > 0 {
		timeout = time.Duration(c.config.RequestTimeout) * time.Second
	}

	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	token, err := c.GetToken()
	if err != nil {
		return fmt.Errorf("failed to get token: %w", err)
	}

	payload := AgentRequest{
		Prompt:        req.Prompt,
		ChatHistory:   req.ChatHistory,
		ProjectID:     c.config.ProjectID,
		CurrentPage:   map[string]interface{}{},
		AgentMode:     c.config.AgentMode,
		Mode:          "agent",
		GitRepoUrl:    "",
		Email:         c.config.Email,
		ChatSessionID: rand.IntN(90000000) + 10000000,
		UserID:        c.config.UserID,
		APIVersion:    2,
		Model:         req.Model,
		Messages:      req.Messages,
		System:        req.System,
		Tools:         req.Tools,
	}

	buf := byteBufferPool.Get().(*bytes.Buffer)
	buf.Reset()
	defer byteBufferPool.Put(buf)

	if err := json.NewEncoder(buf).Encode(payload); err != nil {
		return err
	}

	url := c.upstreamURL()

	// 使用 Circuit Breaker 保护上游调用
	breaker := GetAccountBreaker(c.config.Email)

	result, err := breaker.Execute(func() (interface{}, error) {
		httpReq, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(buf.Bytes()))
		if err != nil {
			return nil, err
		}

		httpReq.Header.Set("Accept", "text/event-stream")
		httpReq.Header.Set("Authorization", "Bearer "+token)
		httpReq.Header.Set("Content-Type", "application/json")
		httpReq.Header.Set("X-Orchids-Api-Version", "2")

		// 记录上游请求
		if logger != nil {
			headers := map[string]string{
				"Accept":                "text/event-stream",
				"Authorization":         "Bearer [REDACTED]",
				"Content-Type":          "application/json",
				"X-Orchids-Api-Version": "2",
			}
			logger.LogUpstreamRequest(url, headers, payload)
		}

		return c.httpClient.Do(httpReq)
	})

	if err != nil {
		return err
	}
	resp := result.(*http.Response)
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 10*1024*1024))
		return fmt.Errorf("upstream request failed with status %d: %s", resp.StatusCode, string(body))
	}

	limitedBody := io.LimitReader(resp.Body, 100*1024*1024)

	reader := bufioReaderPool.Get().(*bufio.Reader)
	reader.Reset(limitedBody)
	defer bufioReaderPool.Put(reader)

	buffer := bufferPool.Get().(*strings.Builder)
	buffer.Reset()
	defer bufferPool.Put(buffer)

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		line, err := reader.ReadString('\n')
		if err != nil {
			if err == io.EOF {
				break
			}
			return err
		}

		buffer.WriteString(line)

		if line == "\n" {
			eventData := buffer.String()
			buffer.Reset()

			lines := strings.Split(eventData, "\n")
			for _, l := range lines {
				if strings.HasPrefix(l, "data: ") {
					rawData := strings.TrimPrefix(l, "data: ")

					var msg map[string]interface{}
					if err := json.Unmarshal([]byte(rawData), &msg); err != nil {
						continue
					}

					msgType, _ := msg["type"].(string)

					// 记录上游 SSE
					if logger != nil {
						logger.LogUpstreamSSE(msgType, rawData)
					}

					// 只处理 "model" 类型的事件，以及 coding_agent.Edit 事件
					if msgType != "model" && !strings.HasPrefix(msgType, "coding_agent.") {
						continue
					}

					sseMsg := SSEMessage{
						Type: msgType,
						Raw:  msg,
					}

					if event, ok := msg["event"].(map[string]interface{}); ok {
						sseMsg.Event = event
					}

					onMessage(sseMsg)
				}
			}
		}
	}

	return nil
}

type UpstreamModel struct {
	ID      string `json:"id"`
	Created int    `json:"created"`
	Object  string `json:"object"`
	OwnedBy string `json:"owned_by"`
}

type UpstreamModelsResponse struct {
	Object string          `json:"object"`
	Data   []UpstreamModel `json:"data"`
}

func (c *Client) FetchUpstreamModels(ctx context.Context) ([]UpstreamModel, error) {
	token, err := c.GetToken()
	if err != nil {
		return nil, fmt.Errorf("failed to get token: %w", err)
	}

	// Replace /agent/coding-agent with /v1/models if needed, or just append /v1/models if base is different
	// The upstreamURL in client.go is "https://orchids-server.../agent/coding-agent"
	baseURL := "https://orchids-server.calmstone-6964e08a.westeurope.azurecontainerapps.io"
	if c.config != nil && c.config.OrchidsAPIBaseURL != "" {
		baseURL = c.config.OrchidsAPIBaseURL
	}
	baseURL = strings.TrimSuffix(baseURL, "/")

	paths := []string{"/v1/models", "/api/models", "/api/v1/models", "/models"}
	var lastErr error

	for _, p := range paths {
		reqURL := baseURL + p
		req, err := http.NewRequestWithContext(ctx, "GET", reqURL, nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Content-Type", "application/json")

		resp, err := c.httpClient.Do(req)
		if err != nil {
			lastErr = err
			continue
		}
		defer resp.Body.Close()

		if resp.StatusCode == http.StatusOK {
			var parsed UpstreamModelsResponse
			if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil {
				// Try parsing as just []UpstreamModel
				return nil, err
			}
			return parsed.Data, nil
		}

		// Read body for error details
		body, _ := io.ReadAll(resp.Body)
		lastErr = fmt.Errorf("upstream models request failed to %s: %s", p, string(body))
	}

	return nil, lastErr
}

func getUpstreamURL() string {
	return upstreamURL
}

func (c *Client) upstreamURL() string {
	if c != nil && c.config != nil && c.config.UpstreamURL != "" {
		return c.config.UpstreamURL
	}
	return upstreamURL
}

func isWSFallback(err error) bool {
	var fallback wsFallbackError
	return errors.As(err, &fallback)
}

func getCachedToken(sessionID string) (string, bool) {
	if sessionID == "" {
		return "", false
	}

	tokenCache.mu.RLock()
	entry, ok := tokenCache.items[sessionID]
	tokenCache.mu.RUnlock()
	if !ok {
		return "", false
	}

	if time.Now().After(entry.expiresAt) {
		tokenCache.mu.Lock()
		delete(tokenCache.items, sessionID)
		tokenCache.mu.Unlock()
		return "", false
	}

	return entry.token, true
}

func setCachedToken(sessionID, token string) {
	if sessionID == "" || token == "" {
		return
	}

	expiresAt := tokenExpiry(token)
	if expiresAt.IsZero() {
		expiresAt = time.Now().Add(defaultTokenTTL)
	}

	tokenCache.mu.Lock()
	tokenCache.items[sessionID] = cachedToken{
		token:     token,
		expiresAt: expiresAt,
	}
	tokenCache.mu.Unlock()
}

func tokenExpiry(token string) time.Time {
	parts := strings.Split(token, ".")
	if len(parts) < 2 {
		return time.Time{}
	}

	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return time.Time{}
	}

	var claims map[string]interface{}
	if err := json.Unmarshal(payload, &claims); err != nil {
		return time.Time{}
	}

	expValue, ok := claims["exp"]
	if !ok {
		return time.Time{}
	}

	var exp int64
	switch v := expValue.(type) {
	case float64:
		exp = int64(v)
	case json.Number:
		exp, _ = v.Int64()
	}

	if exp == 0 {
		return time.Time{}
	}

	return time.Unix(exp, 0).Add(-tokenExpirySkew)
}
