package orchids

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"orchids-api/internal/clerk"
	"orchids-api/internal/config"
	"orchids-api/internal/debug"
	"orchids-api/internal/perf"
	"orchids-api/internal/prompt"
	"orchids-api/internal/store"
	"orchids-api/internal/upstream"
)

const upstreamURL = "https://orchids-server.calmstone-6964e08a.westeurope.azurecontainerapps.io/agent/coding-agent"

const (
	defaultTokenTTL = 5 * time.Minute
	tokenExpirySkew = 30 * time.Second
)

type Client struct {
	config         *config.Config
	account        *store.Account
	httpClient     *http.Client
	fsCache        *perf.TTLCache
	wsPool         *upstream.WSPool
	wsWriteMu      sync.Mutex // Protects concurrent writes to WebSocket
	fsIndex        map[string][]string
	fsFileList     []string
	fsIndexMu      sync.RWMutex
	fsIndexRefresh atomic.Bool
	fsIndexPending atomic.Bool
	fsExecutor     func(op map[string]interface{}, workdir string) (bool, interface{}, string)
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

func newHTTPClient(cfg *config.Config) *http.Client {
	transport := &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		MaxIdleConns:          100,
		MaxIdleConnsPerHost:   100,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
		ResponseHeaderTimeout: 30 * time.Second,
	}

	if cfg != nil && cfg.ProxyHTTP != "" {
		if u, err := url.Parse(cfg.ProxyHTTP); err == nil {
			if cfg.ProxyUser != "" && cfg.ProxyPass != "" {
				u.User = url.UserPassword(cfg.ProxyUser, cfg.ProxyPass)
			}
			transport.Proxy = http.ProxyURL(u)
		}
	}

	return &http.Client{
		Transport: transport,
	}
}

func New(cfg *config.Config) *Client {
	c := &Client{
		config:     cfg,
		httpClient: newHTTPClient(cfg),
		fsCache:    perf.NewTTLCache(60 * time.Second),
	}
	c.wsPool = upstream.NewWSPool(c.createWSConnection, 5, 20)

	go c.RefreshFSIndex()
	return c
}

func (c *Client) SetFSExecutor(fn func(op map[string]interface{}, workdir string) (bool, interface{}, string)) {
	c.fsExecutor = fn
}

func NewFromAccount(acc *store.Account, base *config.Config) *Client {
	cfg := &config.Config{
		SessionID:         acc.SessionID,
		ClientCookie:      acc.ClientCookie,
		SessionCookie:     acc.SessionCookie,
		ClientUat:         acc.ClientUat,
		ProjectID:         acc.ProjectID,
		UserID:            acc.UserID,
		AgentMode:         acc.AgentMode,
		Email:             acc.Email,
		UpstreamMode:      "",
		UpstreamURL:       "",
		UpstreamToken:     "",
		OrchidsAPIBaseURL: "",
		OrchidsWSURL:      "",
		OrchidsAPIVersion: "",
	}
	if base != nil {
		cfg.UpstreamMode = base.UpstreamMode
		cfg.UpstreamURL = base.UpstreamURL
		cfg.UpstreamToken = base.UpstreamToken
		cfg.OrchidsAPIBaseURL = base.OrchidsAPIBaseURL
		cfg.OrchidsWSURL = base.OrchidsWSURL
		cfg.OrchidsAPIVersion = base.OrchidsAPIVersion

		cfg.OrchidsRunAllowlist = base.OrchidsRunAllowlist
		cfg.OrchidsFSIgnore = base.OrchidsFSIgnore // Critical for performance
		cfg.AutoRefreshToken = base.AutoRefreshToken
		cfg.DebugEnabled = base.DebugEnabled
		cfg.DebugLogSSE = base.DebugLogSSE
		cfg.MaxRetries = base.MaxRetries
		cfg.RetryDelay = base.RetryDelay
		cfg.RequestTimeout = base.RequestTimeout

		// Copy Proxy Config
		cfg.ProxyHTTP = base.ProxyHTTP
		cfg.ProxyHTTPS = base.ProxyHTTPS
		cfg.ProxyUser = base.ProxyUser
		cfg.ProxyPass = base.ProxyPass
		cfg.ProxyBypass = base.ProxyBypass
	}

	c := &Client{
		config:     cfg,
		account:    acc,
		httpClient: newHTTPClient(cfg),
		fsCache:    perf.NewTTLCache(60 * time.Second),
	}
	c.wsPool = upstream.NewWSPool(c.createWSConnection, 5, 20)
	go c.RefreshFSIndex()
	return c
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
	sid := strings.TrimSpace(c.config.SessionID)
	if sid == "" {
		return "", errors.New("missing orchids session id")
	}

	url := fmt.Sprintf("https://clerk.orchids.app/v1/client/sessions/%s/tokens?__clerk_api_version=2025-11-10&_clerk_js_version=5.117.0", sid)

	ctx, cancel := withDefaultTimeout(context.Background(), c.requestTimeout())
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, "POST", url, strings.NewReader("organization_id="))
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
		body, err := io.ReadAll(io.LimitReader(resp.Body, 1024*1024))
		if err != nil {
			return "", fmt.Errorf("token request failed with status %d (failed to read body: %v)", resp.StatusCode, err)
		}
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

func (c *Client) SendRequest(ctx context.Context, prompt string, chatHistory []interface{}, model string, onMessage func(upstream.SSEMessage), logger *debug.Logger) error {
	req := upstream.UpstreamRequest{
		Prompt:      prompt,
		ChatHistory: chatHistory,
		Model:       model,
		Messages:    nil, // SendRequest is legacy, use SendRequestWithPayload for full objects
	}
	return c.SendRequestWithPayload(ctx, req, onMessage, logger)
}

func (c *Client) SendRequestWithPayload(ctx context.Context, req upstream.UpstreamRequest, onMessage func(upstream.SSEMessage), logger *debug.Logger) error {
	mode := strings.ToLower(strings.TrimSpace(c.config.UpstreamMode))
	if c.config.DebugEnabled {
		slog.Debug("Sending upstream request", "mode", mode, "url", c.upstreamURL())
	}
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

func (c *Client) sendRequestSSE(ctx context.Context, req upstream.UpstreamRequest, onMessage func(upstream.SSEMessage), logger *debug.Logger) error {
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

	buf := perf.AcquireByteBuffer()
	defer perf.ReleaseByteBuffer(buf)

	if err := json.NewEncoder(buf).Encode(payload); err != nil {
		return err
	}

	url := c.upstreamURL()

	// 使用 Circuit Breaker 保护上游调用
	breaker := upstream.GetAccountBreaker(c.config.Email)
	start := time.Now()

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
		if c.config.DebugEnabled {
			slog.Info("[Performance] Upstream Request Failed", "duration", time.Since(start), "error", err)
		}
		return err
	}
	if c.config.DebugEnabled {
		slog.Info("[Performance] Upstream Request Headers Received", "duration", time.Since(start))
	}
	resp := result.(*http.Response)
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, err := io.ReadAll(io.LimitReader(resp.Body, 10*1024*1024))
		if err != nil {
			return fmt.Errorf("upstream request failed with status %d (failed to read error body: %v)", resp.StatusCode, err)
		}
		return fmt.Errorf("upstream request failed with status %d: %s", resp.StatusCode, string(body))
	}

	limitedBody := io.LimitReader(resp.Body, 100*1024*1024)

	reader := perf.AcquireBufioReader(limitedBody)
	defer perf.ReleaseBufioReader(reader)

	buffer := perf.AcquireStringBuilder()
	defer perf.ReleaseStringBuilder(buffer)

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

					// Allow informative events for real-time feedback
					if msgType != "model" &&
						!strings.HasPrefix(msgType, "coding_agent.") &&
						msgType != "fs_operation" &&
						msgType != "init" {
						continue
					}

					sseMsg := upstream.SSEMessage{
						Type: msgType,
						Raw:  msg,
					}

					if event, ok := msg["event"].(map[string]interface{}); ok {
						sseMsg.Event = event
					} else {
						// For flat events like fs_operation, the event itself is the data map
						sseMsg.Event = msg
					}

					onMessage(sseMsg)
				}
			}
		}
	}
	return nil
}

func (c *Client) GetProjectSummary() string {
	c.fsIndexMu.RLock()
	defer c.fsIndexMu.RUnlock()

	if len(c.fsIndex) == 0 {
		return "Project structure currently unknown (indexing in progress)."
	}

	rootFiles, ok := c.fsIndex["."]
	projectType := "Generic"
	if ok {
		hasFile := func(name string) bool {
			for _, f := range rootFiles {
				if f == name {
					return true
				}
			}
			return false
		}
		if hasFile("go.mod") {
			projectType = "Go"
		} else if hasFile("package.json") {
			projectType = "JavaScript/TypeScript"
		} else if hasFile("requirements.txt") || hasFile("setup.py") || hasFile("pyproject.toml") {
			projectType = "Python"
		} else if hasFile("Cargo.toml") {
			projectType = "Rust"
		} else if hasFile("pom.xml") || hasFile("build.gradle") {
			projectType = "Java/Kotlin"
		}
	}

	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("This is a %s project. Directory structure:\n", projectType))

	// 限制深度和项数以节省 token
	count := 0
	keys := make([]string, 0, len(c.fsIndex))
	for k := range c.fsIndex {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	for _, rel := range keys {
		if count > 20 { // 仅显示前20个核心目录
			break
		}
		depth := strings.Count(rel, "/")
		if depth > 1 { // 仅显示两层深度
			continue
		}

		indent := strings.Repeat("  ", depth)
		sb.WriteString(fmt.Sprintf("%s- %s/\n", indent, rel))

		files := c.fsIndex[rel]
		for i, f := range files {
			if i > 5 { // 核心目录仅显示前5个文件
				sb.WriteString(fmt.Sprintf("%s  - ... (%d more files)\n", indent, len(files)-5))
				break
			}
			sb.WriteString(fmt.Sprintf("%s  - %s\n", indent, f))
		}
		count++
	}

	return sb.String()
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

	ctx, cancel := withDefaultTimeout(ctx, c.requestTimeout())
	defer cancel()

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

		if resp.StatusCode == http.StatusOK {
			var parsed UpstreamModelsResponse
			if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil {
				resp.Body.Close()
				// Try parsing as just []UpstreamModel
				return nil, err
			}
			resp.Body.Close()
			return parsed.Data, nil
		}

		// Read body for error details
		body, rErr := io.ReadAll(io.LimitReader(resp.Body, 1024*1024))
		resp.Body.Close()

		errMsg := string(body)
		if rErr != nil {
			errMsg = fmt.Sprintf("%s (read error: %v)", errMsg, rErr)
		}
		lastErr = fmt.Errorf("upstream models request failed to %s: %s", p, errMsg)
	}

	return nil, lastErr
}

func (c *Client) upstreamURL() string {
	if c != nil && c.config != nil && c.config.UpstreamURL != "" {
		return c.config.UpstreamURL
	}
	return upstreamURL
}

func (c *Client) requestTimeout() time.Duration {
	if c != nil && c.config != nil && c.config.RequestTimeout > 0 {
		return time.Duration(c.config.RequestTimeout) * time.Second
	}
	return 30 * time.Second
}

func withDefaultTimeout(ctx context.Context, timeout time.Duration) (context.Context, context.CancelFunc) {
	if timeout <= 0 {
		return context.WithCancel(ctx)
	}
	if _, ok := ctx.Deadline(); ok {
		return context.WithCancel(ctx)
	}
	return context.WithTimeout(ctx, timeout)
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
		if current, ok := tokenCache.items[sessionID]; ok && current.token == entry.token && current.expiresAt.Equal(entry.expiresAt) {
			delete(tokenCache.items, sessionID)
		}
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
	firstDot := strings.IndexByte(token, '.')
	if firstDot < 0 {
		return time.Time{}
	}
	rest := token[firstDot+1:]
	secondDot := strings.IndexByte(rest, '.')
	if secondDot < 0 {
		return time.Time{}
	}

	payload, err := base64.RawURLEncoding.DecodeString(rest[:secondDot])
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
		exp, _ = v.Int64() // Error ignored as we return 0 on failure anyway
	}

	if exp == 0 {
		return time.Time{}
	}

	return time.Unix(exp, 0).Add(-tokenExpirySkew)
}
