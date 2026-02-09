package warp

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"strings"
	"sync"
	"time"
)

type session struct {
	mu             sync.Mutex
	jwt            string
	expiresAt      time.Time
	refreshToken   string
	loggedIn       bool
	lastLogin      time.Time
	lastUsed       time.Time // for cache eviction
	refreshing     bool      // prevents concurrent refresh attempts
	refreshDone    chan struct{}
	clientVersion  string
	osCategory     string
	osName         string
	osVersion      string
	experimentID   string
	experimentBuck string
	jar            http.CookieJar
}

type refreshResponse struct {
	AccessToken  string      `json:"access_token"`
	IDToken      string      `json:"idToken"`
	ExpiresIn    json.Number `json:"expires_in"`
	RefreshToken string      `json:"refresh_token"`
	ExpiresInAlt json.Number `json:"expiresIn"`
	RefreshAlt   string      `json:"refreshToken"`
}

var sessionCache sync.Map
var sessionCount int64 // approximate count for eviction
var sessionCountMu sync.Mutex

const maxSessionCacheSize = 10000

func sessionKey(accountID int64, refreshToken string) string {
	if accountID > 0 {
		return fmt.Sprintf("warp:%d", accountID)
	}
	if refreshToken == "" {
		return "warp:anon"
	}
	if len(refreshToken) > 16 {
		return "warp:tok:" + refreshToken[:16]
	}
	return "warp:tok:" + refreshToken
}

func getSession(accountID int64, refreshToken string) *session {
	// Simple parsing for format: email----device----token
	if strings.Contains(refreshToken, "----") {
		parts := strings.Split(refreshToken, "----")
		if len(parts) > 0 {
			refreshToken = strings.TrimSpace(parts[len(parts)-1])
		}
	}

	key := sessionKey(accountID, refreshToken)

	// Use LoadOrStore to atomically check-and-insert, preventing race conditions
	jar, _ := cookiejar.New(nil)
	newSess := &session{
		refreshToken:  refreshToken,
		clientVersion: clientVersion,
		osCategory:    osCategory,
		osName:        osName,
		osVersion:     osVersion,
		jar:           jar,
		lastUsed:      time.Now(),
	}
	val, loaded := sessionCache.LoadOrStore(key, newSess)
	sess := val.(*session)

	if loaded {
		// Existing session found — update refresh token if changed
		sess.mu.Lock()
		sess.lastUsed = time.Now()
		if refreshToken != "" && sess.refreshToken != refreshToken {
			// refresh_token 变更时更新会话，避免旧令牌导致认证异常
			sess.refreshToken = refreshToken
			sess.jwt = ""
			sess.expiresAt = time.Time{}
			sess.loggedIn = false
			sess.lastLogin = time.Time{}
		}
		sess.mu.Unlock()
	} else {
		// New session inserted — track count and evict if needed
		sessionCountMu.Lock()
		sessionCount++
		if sessionCount > maxSessionCacheSize {
			go evictStaleSessions()
		}
		sessionCountMu.Unlock()
	}
	return sess
}

// evictStaleSessions removes sessions not used in the last 2 hours.
func evictStaleSessions() {
	cutoff := time.Now().Add(-2 * time.Hour)
	evicted := 0
	sessionCache.Range(func(key, value interface{}) bool {
		sess := value.(*session)
		sess.mu.Lock()
		lastUsed := sess.lastUsed
		sess.mu.Unlock()
		if lastUsed.Before(cutoff) {
			sessionCache.Delete(key)
			evicted++
		}
		return true
	})
	if evicted > 0 {
		sessionCountMu.Lock()
		sessionCount -= int64(evicted)
		if sessionCount < 0 {
			sessionCount = 0
		}
		sessionCountMu.Unlock()
		slog.Info("Evicted stale warp sessions", "evicted", evicted)
	}
}

func (s *session) tokenValid() bool {
	if s.jwt == "" || s.expiresAt.IsZero() {
		return false
	}
	return time.Now().Add(20 * time.Minute).Before(s.expiresAt)
}

func (s *session) ensureToken(ctx context.Context, httpClient *http.Client, cid string) error {
	s.mu.Lock()
	if s.tokenValid() {
		s.mu.Unlock()
		return nil
	}
	// If another goroutine is already refreshing, wait for it
	if s.refreshing {
		ch := s.refreshDone
		s.mu.Unlock()
		if ch != nil {
			select {
			case <-ch:
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		// After waiting, check if token is now valid
		s.mu.Lock()
		valid := s.tokenValid()
		s.mu.Unlock()
		if valid {
			return nil
		}
		// If still invalid, fall through to try refresh ourselves
		return s.ensureToken(ctx, httpClient, cid)
	}
	// Mark that we are refreshing
	s.refreshing = true
	s.refreshDone = make(chan struct{})
	s.mu.Unlock()

	err := s.refreshTokenRequest(ctx, httpClient, cid)

	s.mu.Lock()
	s.refreshing = false
	close(s.refreshDone)
	s.refreshDone = nil
	s.mu.Unlock()

	return err
}

func isTransientNetworkError(err error) bool {
	if err == nil {
		return false
	}
	s := strings.ToLower(err.Error())
	return strings.HasSuffix(s, ": eof") || s == "eof" ||
		strings.Contains(s, "connection reset") ||
		strings.Contains(s, "connection refused") ||
		strings.Contains(s, "broken pipe") ||
		strings.Contains(s, "use of closed")
}

func (s *session) refreshTokenRequest(ctx context.Context, httpClient *http.Client, cid string) error {
	s.mu.Lock()
	refreshToken := strings.TrimSpace(s.refreshToken)
	s.mu.Unlock()

	var payload []byte
	if refreshToken != "" {
		payload = []byte("grant_type=refresh_token&refresh_token=" + url.QueryEscape(refreshToken))
	} else {
		decoded, err := base64.StdEncoding.DecodeString(refreshTokenB64)
		if err != nil {
			return fmt.Errorf("decode built-in refresh token: %w", err)
		}
		payload = decoded
	}

	const maxAuthRetries = 2
	var lastErr error
	for attempt := 0; attempt <= maxAuthRetries; attempt++ {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if attempt > 0 {
			slog.Info("Warp AI: Retrying refresh token request", "cid", cid, "attempt", attempt)
			time.Sleep(time.Duration(attempt) * time.Second)
		}
		lastErr = s.doRefreshTokenRequest(ctx, httpClient, cid, payload)
		if lastErr == nil {
			return nil
		}
		if !isTransientNetworkError(lastErr) {
			return lastErr
		}
		slog.Warn("Warp AI: Refresh request transient error, will retry", "cid", cid, "attempt", attempt, "error", lastErr)
	}
	return lastErr
}

func (s *session) doRefreshTokenRequest(ctx context.Context, httpClient *http.Client, cid string, payload []byte) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, refreshURL, bytes.NewReader(payload))
	if err != nil {
		return err
	}
	req.Header.Set("x-warp-client-version", clientVersion)
	req.Header.Set("x-warp-os-category", osCategory)
	req.Header.Set("x-warp-os-name", osName)
	req.Header.Set("x-warp-os-version", osVersion)
	req.Header.Set("content-type", "application/x-www-form-urlencoded")
	req.Header.Set("accept", "*/*")
	req.Header.Set("accept-encoding", "gzip")

	resp, err := httpClient.Do(req)
	if err != nil {
		slog.Warn("Warp AI: Refresh request failed", "cid", cid, "error", err)
		return err
	}
	defer resp.Body.Close()

	var reader io.ReadCloser = resp.Body
	if resp.Header.Get("Content-Encoding") == "gzip" {
		reader, err = gzip.NewReader(resp.Body)
		if err != nil {
			return err
		}
		defer reader.Close()
	}

	body, err := io.ReadAll(reader)
	if err != nil {
		slog.Warn("Warp refresh body read failed", "error", err)
		return err
	}

	if resp.StatusCode != http.StatusOK {
		retryAfter := parseRetryAfterHeader(resp.Header.Get("Retry-After"), time.Now())
		slog.Warn("Warp AI: Refresh failed", "cid", cid, "status", resp.StatusCode, "retry_after", retryAfter.String(), "body", string(body))
		return &HTTPStatusError{Operation: "refresh token", StatusCode: resp.StatusCode, RetryAfter: retryAfter}
	}

	var parsed refreshResponse
	if err := json.Unmarshal(body, &parsed); err != nil {
		return err
	}

	accessToken := parsed.AccessToken
	if accessToken == "" {
		accessToken = parsed.IDToken
	}
	if accessToken == "" {
		return fmt.Errorf("warp refresh token response missing access token")
	}

	var expiresIn int64
	if v, err := parsed.ExpiresIn.Int64(); err == nil && v > 0 {
		expiresIn = v
	}
	if expiresIn <= 0 {
		if v, err := parsed.ExpiresInAlt.Int64(); err == nil && v > 0 {
			expiresIn = v
		}
	}
	if expiresIn <= 0 {
		expiresIn = 3600
	}

	newRefresh := parsed.RefreshToken
	if newRefresh == "" {
		newRefresh = parsed.RefreshAlt
	}

	// 用 JWT 实际 exp 声明校验，防止 refresh 端点返回已过期的缓存 token
	actualExp := jwtExpiry(accessToken)
	expiresByResponse := time.Now().Add(time.Duration(expiresIn) * time.Second)

	effectiveExpiry := expiresByResponse
	if !actualExp.IsZero() {
		// 如果 JWT 实际过期时间早于 expires_in 推算值，以实际值为准
		if actualExp.Before(effectiveExpiry) {
			effectiveExpiry = actualExp
		}
		// 如果 JWT 已过期或即将在 1 分钟内过期，拒绝使用
		if time.Now().Add(1 * time.Minute).After(actualExp) {
			slog.Warn("Warp AI: Refresh returned already-expired JWT", "cid", cid, "exp", actualExp, "now", time.Now())
			return fmt.Errorf("refresh returned expired JWT (exp=%s)", actualExp.Format(time.RFC3339))
		}
	}

	s.mu.Lock()
	s.jwt = accessToken
	s.expiresAt = effectiveExpiry
	if newRefresh != "" {
		s.refreshToken = newRefresh
	}
	// 刷新令牌后需要重新登录，避免旧 cookie 失效
	s.loggedIn = false
	s.lastLogin = time.Time{}
	s.mu.Unlock()

	return nil
}

func (s *session) ensureLogin(ctx context.Context, httpClient *http.Client, cid string) error {
	s.mu.Lock()
	if s.loggedIn && time.Since(s.lastLogin) < 30*time.Minute {
		s.mu.Unlock()
		return nil
	}
	jwt := s.jwt
	if s.experimentID == "" {
		s.experimentID = newUUID()
	}
	if s.experimentBuck == "" {
		s.experimentBuck = newExperimentBucket()
	}
	experimentID := s.experimentID
	experimentBucket := s.experimentBuck
	s.mu.Unlock()

	if jwt == "" {
		return fmt.Errorf("missing jwt")
	}

	const maxAuthRetries = 2
	var lastErr error
	for attempt := 0; attempt <= maxAuthRetries; attempt++ {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if attempt > 0 {
			slog.Info("Warp AI: Retrying login request", "cid", cid, "attempt", attempt)
			time.Sleep(time.Duration(attempt) * time.Second)
		}
		lastErr = s.doLogin(ctx, httpClient, cid, jwt, experimentID, experimentBucket)
		if lastErr == nil {
			return nil
		}
		if !isTransientNetworkError(lastErr) {
			return lastErr
		}
		slog.Warn("Warp AI: Login request transient error, will retry", "cid", cid, "attempt", attempt, "error", lastErr)
	}
	return lastErr
}

func (s *session) doLogin(ctx context.Context, httpClient *http.Client, cid, jwt, experimentID, experimentBucket string) error {
	re, err := http.NewRequestWithContext(ctx, http.MethodPost, loginURL, nil)
	if err != nil {
		return err
	}
	re.Header.Set("x-warp-client-id", "warp-app")
	re.Header.Set("x-warp-client-version", clientVersion)
	re.Header.Set("x-warp-os-category", osCategory)
	re.Header.Set("x-warp-os-name", osName)
	re.Header.Set("x-warp-os-version", osVersion)
	re.Header.Set("authorization", "Bearer "+jwt)
	re.Header.Set("x-warp-experiment-id", experimentID)
	re.Header.Set("x-warp-experiment-bucket", experimentBucket)
	re.Header.Set("accept", "*/*")
	re.Header.Set("accept-encoding", "gzip")
	re.Header.Set("content-length", "0")

	resp, err := httpClient.Do(re)
	if err != nil {
		slog.Warn("Warp AI: Login request failed", "cid", cid, "error", err)
		return err
	}
	defer resp.Body.Close()

	var reader io.ReadCloser = resp.Body
	if resp.Header.Get("Content-Encoding") == "gzip" {
		reader, err = gzip.NewReader(resp.Body)
		if err != nil {
			return err
		}
		defer reader.Close()
	}

	body, err := io.ReadAll(reader)
	if err != nil {
		slog.Warn("Warp login body read failed", "error", err)
		return err
	}

	if resp.StatusCode != http.StatusNoContent {
		slog.Warn("Warp AI: Login failed", "cid", cid, "status", resp.StatusCode, "body", string(body))
		return fmt.Errorf("warp login failed: HTTP %d", resp.StatusCode)
	}

	s.mu.Lock()
	s.loggedIn = true
	s.lastLogin = time.Now()
	s.mu.Unlock()
	return nil
}

func newUUID() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return fmt.Sprintf("%d", time.Now().UnixNano())
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

func newExperimentBucket() string {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return fmt.Sprintf("%d", time.Now().UnixNano())
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func (s *session) currentJWT() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.jwt
}

func (s *session) currentRefreshToken() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.refreshToken
}

// InvalidateSession 清除指定账号的 session 缓存，使下次请求重新创建会话。
func InvalidateSession(accountID int64) {
	if accountID <= 0 {
		return
	}
	key := fmt.Sprintf("warp:%d", accountID)
	sessionCache.Delete(key)
}
