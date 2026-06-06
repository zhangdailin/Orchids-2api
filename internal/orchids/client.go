package orchids

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/goccy/go-json"

	"orchids-api/internal/clerk"
	"orchids-api/internal/config"
	"orchids-api/internal/debug"
	"orchids-api/internal/logutil"
	"orchids-api/internal/store"
	"orchids-api/internal/upstream"
	"orchids-api/internal/util"
)

type orchidsFastEnvelope struct {
	Type string `json:"type"`
}

type orchidsFastModelMessage struct {
	Type  string          `json:"type"`
	Event json.RawMessage `json:"event"`
}

type orchidsFastErrorMessage struct {
	Type    string `json:"type"`
	Message string `json:"message"`
	Code    string `json:"code"`
	Data    struct {
		Message string `json:"message"`
		Code    string `json:"code"`
	} `json:"data"`
}

const defaultUpstreamBaseURL = "https://orchids-v2-alpha-108292236521.europe-west1.run.app"
const upstreamURL = defaultUpstreamBaseURL + "/agent/coding-agent"

const (
	defaultTokenTTL = 5 * time.Minute
	tokenExpirySkew = 30 * time.Second
)

type Client struct {
	config     *config.Config
	account    *store.Account
	httpClient *http.Client
}

type TokenResponse struct {
	JWT string `json:"jwt"`
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

var noActiveSessionLogState = struct {
	mu   sync.Mutex
	last map[string]time.Time
}{
	last: map[string]time.Time{},
}

const noActiveSessionLogInterval = 5 * time.Minute

var orchidsFetchClerkInfoWithSession = clerk.FetchAccountInfoWithSessionContextProxy
var orchidsFetchClerkInfoWithProjectAndSession = clerk.FetchAccountInfoWithProjectAndSessionContextProxy

func traceIDForLog(req upstream.UpstreamRequest) string {
	traceID := strings.TrimSpace(req.TraceID)
	if traceID == "" {
		return "unknown"
	}
	return traceID
}

func attemptForLog(req upstream.UpstreamRequest) int {
	if req.Attempt <= 0 {
		return 1
	}
	return req.Attempt
}

func shouldLogNoActiveSession(key string) bool {
	if strings.TrimSpace(key) == "" {
		key = "default"
	}
	now := time.Now()
	noActiveSessionLogState.mu.Lock()
	defer noActiveSessionLogState.mu.Unlock()
	if t, ok := noActiveSessionLogState.last[key]; ok && now.Sub(t) < noActiveSessionLogInterval {
		return false
	}
	noActiveSessionLogState.last[key] = now
	return true
}

func newHTTPClient(cfg *config.Config) *http.Client {
	var proxyFunc func(*http.Request) (*url.URL, error)
	proxyKey := "direct"

	if cfg != nil {
		proxyFunc = util.ProxyFuncFromConfig(cfg)
		proxyKey = util.GenerateProxyKeyFromConfig(cfg)
	} else {
		proxyFunc = http.ProxyFromEnvironment
	}

	// Use shared http.Client to preserve connection pool TCP/TLS cache.
	return util.GetSharedHTTPClient(proxyKey, 30*time.Second, proxyFunc)
}

func New(cfg *config.Config) *Client {
	c := &Client{
		config:     cfg,
		httpClient: newHTTPClient(cfg),
	}
	return c
}

func (c *Client) OwnsFinalSSELifecycle() bool {
	return true
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
		cfg.SuppressThinking = base.SuppressThinking
		cfg.OrchidsMaxToolResults = base.OrchidsMaxToolResults
		cfg.OrchidsMaxHistoryMessages = base.OrchidsMaxHistoryMessages

		// Copy Proxy Config
		cfg.ProxyURL = base.ProxyURL
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
	}
	return c
}

func (c *Client) Close() {
	if c == nil {
		return
	}
}

func (c *Client) wsConnectionKey() string {
	parts := make([]string, 0, 4)

	if c.account != nil && c.account.ID != 0 {
		parts = append(parts, fmt.Sprintf("account:%d", c.account.ID))
	}

	if c.config != nil {
		if sessionID := strings.TrimSpace(c.config.SessionID); sessionID != "" {
			parts = append(parts, "session:"+sessionID)
		} else if email := strings.ToLower(strings.TrimSpace(c.config.Email)); email != "" {
			parts = append(parts, "email:"+email)
		}

		if wsURL := strings.TrimSpace(c.config.OrchidsWSURL); wsURL != "" {
			parts = append(parts, "ws:"+wsURL)
		}

		if proxyKey := util.GenerateProxyKeyFromConfig(c.config); proxyKey != "" {
			parts = append(parts, "proxy:"+proxyKey)
		}
	}

	if len(parts) == 0 {
		return "orchids:default"
	}
	return strings.Join(parts, "|")
}

func (c *Client) GetToken() (string, error) {
	if c == nil || c.config == nil {
		return "", errors.New("missing config")
	}
	if c.config.UpstreamToken != "" {
		return c.config.UpstreamToken, nil
	}

	var accountClerkInfoAttempted bool
	var accountClerkInfoErr error

	// Orchids OAuth: if we have a legacy __client cookie, prefer fetching
	// Clerk /v1/client and using sessions[0].last_active_token.jwt.
	// That token is typically short-lived, so we cache by sessionID using its exp claim.
	if c.account != nil {
		c.syncConfigFromStoredAccount()
		if strings.TrimSpace(c.account.ClientCookie) == "" && strings.TrimSpace(c.account.SessionCookie) != "" {
			_ = c.bootstrapClientCookieFromSession()
			c.syncConfigFromStoredAccount()
		}
		if cached, ok := getCachedToken(strings.TrimSpace(c.account.SessionID)); ok {
			return cached, nil
		}
		if tok := strings.TrimSpace(c.account.Token); tok != "" && tokenStillUsable(tok) {
			if sid := strings.TrimSpace(c.account.SessionID); sid != "" {
				setCachedToken(sid, tok)
			}
			return tok, nil
		}

		if strings.TrimSpace(c.account.ClientCookie) != "" {
			accountClerkInfoAttempted = true
			info, err := orchidsFetchClerkInfoWithSession(c.account.ClientCookie, c.account.SessionCookie, c.config.ClientUat, c.config.SessionID, orchidsProxyFunc(c.config))
			if err == nil && info != nil {
				// Update runtime config (used by some upstream payload fields)
				c.applyAccountInfo(info)
				// Persist rotated __client and identity fields back to store account snapshot
				c.persistAccountInfo(info)
				if jwt := strings.TrimSpace(info.JWT); jwt != "" {
					if sid := strings.TrimSpace(info.SessionID); sid != "" {
						setCachedToken(sid, jwt)
					}
					slog.Debug("Orchids token source", "source", "clerk_client_last_active_token", "session_id", info.SessionID, "has_session_cookie", strings.TrimSpace(c.account.SessionCookie) != "")
					return jwt, nil
				}
				if strings.TrimSpace(info.SessionID) != "" {
					// Ensure config has the latest session id/cookies then fetch a bearer token
					// via the official Clerk tokens endpoint.
					c.config.SessionID = strings.TrimSpace(info.SessionID)
					bearer, tokErr := c.fetchToken()
					if tokErr == nil && strings.TrimSpace(bearer) != "" {
						setCachedToken(info.SessionID, bearer)
						slog.Debug("Orchids token source", "source", "clerk_session_tokens_endpoint", "session_id", info.SessionID, "has_session_cookie", strings.TrimSpace(c.account.SessionCookie) != "")
						return bearer, nil
					}
					if tokErr != nil {
						slog.Warn("Orchids token fetch: tokens endpoint failed", "session_id", info.SessionID, "error", tokErr)
						return "", tokErr
					}
				}
				return "", fmt.Errorf("orchids clerk info missing usable jwt/session")
			} else if err != nil {
				accountClerkInfoErr = err
				lower := strings.ToLower(err.Error())
				if strings.Contains(lower, "no active sessions found") {
					logKey := "clerk_info"
					if c.account != nil {
						logKey = fmt.Sprintf("clerk_info:acct:%d", c.account.ID)
					}
					if shouldLogNoActiveSession(logKey) {
						slog.Debug("Orchids token fetch: clerk info failed (no active sessions)", "error", err)
					}
				} else {
					slog.Warn("Orchids token fetch: clerk info failed", "error", err)
				}
				return "", err
			}
			return "", fmt.Errorf("orchids clerk info missing response")
		}

		// Per-account JWT: allow using a pasted bearer token directly.
		if tok := strings.TrimSpace(c.account.Token); tok != "" {
			return tok, nil
		}

		c.syncConfigFromStoredAccount()
	}

	if c.config.AutoRefreshToken {
		if accountClerkInfoAttempted {
			if strings.TrimSpace(c.config.SessionID) != "" {
				return c.fetchToken()
			}
			if accountClerkInfoErr != nil {
				return "", accountClerkInfoErr
			}
		}
		return c.forceRefreshToken()
	}

	if cached, ok := getCachedToken(c.config.SessionID); ok {
		return cached, nil
	}

	if accountClerkInfoErr != nil && strings.TrimSpace(c.config.SessionID) == "" {
		return "", accountClerkInfoErr
	}

	return c.fetchToken()
}

func (c *Client) forceRefreshToken() (string, error) {
	if c.config == nil {
		return "", fmt.Errorf("missing config")
	}

	if strings.TrimSpace(c.config.ClientCookie) != "" {
		info, err := orchidsFetchClerkInfoWithProjectAndSession(c.config.ClientCookie, c.config.SessionCookie, c.config.ClientUat, c.config.SessionID, c.config.ProjectID, orchidsProxyFunc(c.config))
		if err == nil && info.JWT != "" {
			c.applyAccountInfo(info)
			c.persistAccountInfo(info)
			setCachedToken(c.config.SessionID, info.JWT)
			return info.JWT, nil
		}
		if err != nil {
			lower := strings.ToLower(err.Error())
			if strings.Contains(lower, "no active sessions found") {
				logKey := "default"
				accountID := int64(0)
				email := strings.TrimSpace(c.config.Email)
				hasSessionID := strings.TrimSpace(c.config.SessionID) != ""
				if c.account != nil {
					accountID = c.account.ID
					if strings.TrimSpace(c.account.Email) != "" {
						email = strings.TrimSpace(c.account.Email)
					}
					hasSessionID = strings.TrimSpace(c.account.SessionID) != ""
					logKey = fmt.Sprintf("acct:%d", accountID)
				}
				if shouldLogNoActiveSession(logKey) {
					slog.Warn("Clerk token refresh failed without active session", "account_id", accountID, "email", email, "has_session_id", hasSessionID, "error", err)
				}
			} else {
				slog.Warn("Clerk token refresh failed", "error", err)
			}
			return "", err
		} else {
			return "", fmt.Errorf("clerk token refresh returned empty jwt")
		}
	}

	return c.fetchToken()
}

func (c *Client) fetchToken() (string, error) {
	if c == nil || c.config == nil {
		return "", errors.New("missing config")
	}
	sid := strings.TrimSpace(c.config.SessionID)
	if sid == "" {
		return "", errors.New("missing orchids session id")
	}

	url := fmt.Sprintf("%s/v1/client/sessions/%s/tokens?__clerk_api_version=%s&_clerk_js_version=%s",
		clerk.ClerkBaseURL, sid, clerk.ClerkAPIVersion, clerk.ClerkJSVersion)

	ctx, cancel := util.WithDefaultTimeout(context.Background(), c.requestTimeout())
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, "POST", url, strings.NewReader("organization_id="))
	if err != nil {
		return "", err
	}

	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if cookieHeader := orchidsClerkCookieHeader(c.config.ClientCookie, c.config.SessionCookie, c.config.ClientUat, c.config.SessionID); cookieHeader != "" {
		req.Header.Set("Cookie", cookieHeader)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, err := io.ReadAll(resp.Body)
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
	if strings.TrimSpace(info.ClientCookie) != "" {
		c.config.ClientCookie = info.ClientCookie
	}
}

func (c *Client) syncConfigFromStoredAccount() {
	if c == nil || c.config == nil || c.account == nil {
		return
	}
	if strings.TrimSpace(c.config.SessionID) == "" && strings.TrimSpace(c.account.SessionID) != "" {
		c.config.SessionID = strings.TrimSpace(c.account.SessionID)
	}
	if strings.TrimSpace(c.config.ClientCookie) == "" && strings.TrimSpace(c.account.ClientCookie) != "" {
		c.config.ClientCookie = strings.TrimSpace(c.account.ClientCookie)
	}
	if strings.TrimSpace(c.config.SessionCookie) == "" && strings.TrimSpace(c.account.SessionCookie) != "" {
		c.config.SessionCookie = strings.TrimSpace(c.account.SessionCookie)
	}
	if strings.TrimSpace(c.config.ClientUat) == "" && strings.TrimSpace(c.account.ClientUat) != "" {
		c.config.ClientUat = strings.TrimSpace(c.account.ClientUat)
	}
	if strings.TrimSpace(c.config.ProjectID) == "" && strings.TrimSpace(c.account.ProjectID) != "" {
		c.config.ProjectID = strings.TrimSpace(c.account.ProjectID)
	}
	if strings.TrimSpace(c.config.UserID) == "" && strings.TrimSpace(c.account.UserID) != "" {
		c.config.UserID = strings.TrimSpace(c.account.UserID)
	}
	if strings.TrimSpace(c.config.Email) == "" && strings.TrimSpace(c.account.Email) != "" {
		c.config.Email = strings.TrimSpace(c.account.Email)
	}
}

// persistAccountInfo 将刷新后的账号信息同步回 store，防止重启后丢失。
func (c *Client) persistAccountInfo(info *clerk.AccountInfo) {
	if c.account == nil || info == nil {
		return
	}
	if strings.TrimSpace(info.SessionID) != "" {
		c.account.SessionID = info.SessionID
	}
	if strings.TrimSpace(info.ClientUat) != "" {
		c.account.ClientUat = info.ClientUat
	}
	if strings.TrimSpace(info.ProjectID) != "" {
		c.account.ProjectID = info.ProjectID
	}
	if strings.TrimSpace(info.UserID) != "" {
		c.account.UserID = info.UserID
	}
	if strings.TrimSpace(info.Email) != "" {
		c.account.Email = info.Email
	}
	if strings.TrimSpace(info.ClientCookie) != "" {
		c.account.ClientCookie = info.ClientCookie
	}
	if strings.TrimSpace(info.JWT) != "" {
		c.account.Token = info.JWT
	}
}

// (method SyncAccountState removed to use store.Account.SyncState logic)

func (c *Client) SendRequestWithPayload(ctx context.Context, req upstream.UpstreamRequest, onMessage func(upstream.SSEMessage), logger *debug.Logger) error {
	if c == nil {
		return errors.New("orchids client is nil")
	}
	cfg := c.config
	timeout := 120
	if cfg != nil {
		if cfg.RequestTimeout > 0 {
			timeout = cfg.RequestTimeout
		}
	}
	transport := c.resolveTransport()
	if logutil.VerboseDiagnosticsEnabled() {
		slog.Debug("Sending upstream request", "trace_id", traceIDForLog(req), "attempt", attemptForLog(req), "transport", transport, "url", c.upstreamURL(), "timeout", timeout)
	}
	if logutil.VerboseDiagnosticsEnabled() {
		slog.Debug("Orchids transport dispatch", "trace_id", traceIDForLog(req), "attempt", attemptForLog(req), "transport", transport, "chat_session_id", req.ChatSessionID, "model", req.Model)
	}
	err := c.dispatchTransport(ctx, transport, req, onMessage, logger)
	if err == nil {
		return nil
	}
	return err
}

type UpstreamModel struct {
	ID      string `json:"id"`
	Created int    `json:"created"`
	Object  string `json:"object"`
	OwnedBy string `json:"owned_by"`
}

type upstreamModelsEnvelope struct {
	Object string          `json:"object"`
	Data   []UpstreamModel `json:"data"`
	Models []UpstreamModel `json:"models"`
}

func decodeUpstreamModelsResponse(body []byte) ([]UpstreamModel, error) {
	var envelope upstreamModelsEnvelope
	envelopeErr := json.Unmarshal(body, &envelope)
	if envelopeErr == nil {
		if envelope.Data != nil {
			return envelope.Data, nil
		}
		if envelope.Models != nil {
			return envelope.Models, nil
		}
	}

	var direct []UpstreamModel
	directErr := json.Unmarshal(body, &direct)
	if directErr == nil {
		return direct, nil
	}

	if envelopeErr != nil {
		return nil, envelopeErr
	}
	return nil, directErr
}

func (c *Client) FetchUpstreamModels(ctx context.Context) ([]UpstreamModel, error) {
	token, err := c.GetToken()
	if err != nil {
		return nil, fmt.Errorf("failed to get token: %w", err)
	}

	ctx, cancel := util.WithDefaultTimeout(ctx, c.requestTimeout())
	defer cancel()

	// Replace /agent/coding-agent with /v1/models if needed, or just append /v1/models if base is different
	baseURL := defaultUpstreamBaseURL
	if c.config != nil {
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
			body, readErr := io.ReadAll(resp.Body)
			resp.Body.Close()
			if readErr != nil {
				lastErr = readErr
				continue
			}
			models, decodeErr := decodeUpstreamModelsResponse(body)
			if decodeErr != nil {
				lastErr = fmt.Errorf("upstream models decode failed for %s: %w", p, decodeErr)
				continue
			}
			return models, nil
		}

		// Read body for error details
		body, rErr := io.ReadAll(resp.Body)
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

// InvalidateCachedToken 清除指定 sessionID 的 token 缓存，
// 用于账号 401 冷却恢复后强制重新获取 token。
func InvalidateCachedToken(sessionID string) {
	if sessionID == "" {
		return
	}
	tokenCache.mu.Lock()
	delete(tokenCache.items, sessionID)
	tokenCache.mu.Unlock()
}

func tokenExpiry(token string) time.Time {
	return util.JWTExpiry(token, tokenExpirySkew)
}

func tokenStillUsable(token string) bool {
	token = strings.TrimSpace(token)
	if token == "" {
		return false
	}
	expiresAt := tokenExpiry(token)
	return expiresAt.IsZero() || time.Now().Before(expiresAt)
}
