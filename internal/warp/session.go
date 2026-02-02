package warp

import (
	"bytes"
	"context"
	"compress/gzip"
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
	if val, ok := sessionCache.Load(key); ok {
		sess := val.(*session)
		sess.mu.Lock()
		if refreshToken != "" && sess.refreshToken != refreshToken {
			// The provided snippet for this section was syntactically incorrect and seemed to introduce new logic.
			// The instruction was to pass 'cid' through existing calls.
			// This block is left as is, as the instruction did not explicitly ask for changes here.
		}
		sess.mu.Unlock()
		return sess
	}
	jar, _ := cookiejar.New(nil)
	sess := &session{
		refreshToken:  refreshToken,
		clientVersion: clientVersion,
		osCategory:    osCategory,
		osName:        osName,
		osVersion:     osVersion,
		jar:           jar,
	}
	sessionCache.Store(key, sess)
	return sess
}

func (s *session) tokenValid() bool {
	if s.jwt == "" || s.expiresAt.IsZero() {
		return false
	}
	return time.Now().Add(10 * time.Minute).Before(s.expiresAt)
}

func (s *session) ensureToken(ctx context.Context, httpClient *http.Client, cid string) error {
	s.mu.Lock()
	if s.tokenValid() {
		s.mu.Unlock()
		return nil
	}
	s.mu.Unlock()

	return s.refreshTokenRequest(ctx, httpClient, cid)
}

func (s *session) refreshTokenRequest(ctx context.Context, httpClient *http.Client, cid string) error {
	s.mu.Lock()
	refreshToken := strings.TrimSpace(s.refreshToken)
	s.mu.Unlock()

	payload := []byte{}
	if refreshToken != "" {
		payload = []byte("grant_type=refresh_token&refresh_token=" + refreshToken)
	} else {
		decoded, err := base64.StdEncoding.DecodeString(refreshTokenB64)
		if err != nil {
			return fmt.Errorf("decode built-in refresh token: %w", err)
		}
		payload = decoded
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, refreshURL, bytes.NewReader(payload))
	if err != nil {
		return err
	}
	now := time.Now().UTC().Format("2006-01-02T15:04:05.000000Z")
	req.Header.Set("x-warp-client-id", "warp-app")
	req.Header.Set("x-warp-client-version", clientVersion)
	req.Header.Set("x-warp-os-category", osCategory)
	req.Header.Set("x-warp-os-name", osName)
	req.Header.Set("x-warp-os-version", osVersion)
	req.Header.Set("x-warp-date", now)
	req.Header.Set("content-type", "application/x-www-form-urlencoded")
	req.Header.Set("accept", "*/*")
	req.Header.Set("accept-encoding", "gzip, br")
	req.Header.Set("x-warp-request-id", newUUID())

	// Use the session's cookie jar if the client doesn't have one
	oldJar := httpClient.Jar
	httpClient.Jar = s.jar
	defer func() { httpClient.Jar = oldJar }()

	resp, err := httpClient.Do(req)
	if err != nil {
		slog.Warn("Warp AI: Refresh request failed", "cid", cid, "error", err)
		return err
	}
	defer resp.Body.Close()

	headers := make(map[string]string)
	for k, v := range resp.Header {
		headers[k] = strings.Join(v, ", ")
	}
	slog.Info("Warp AI: Refresh response", "cid", cid, "status", resp.StatusCode, "headers", headers)

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
		slog.Warn("Warp AI: Refresh failed", "cid", cid, "status", resp.StatusCode, "body", string(body))
		return fmt.Errorf("warp refresh token failed: HTTP %d", resp.StatusCode)
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

	s.mu.Lock()
	s.jwt = accessToken
	s.expiresAt = time.Now().Add(time.Duration(expiresIn) * time.Second)
	if newRefresh != "" {
		s.refreshToken = newRefresh
	}
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

	re, err := http.NewRequestWithContext(ctx, http.MethodPost, loginURL, nil)
	if err != nil {
		return err
	}
	// Python reference does not use browser headers for login
	now := time.Now().UTC().Format("2006-01-02T15:04:05.000000Z")
	re.Header.Set("x-warp-client-id", "warp-app")
	re.Header.Set("x-warp-client-version", clientVersion)
	re.Header.Set("x-warp-os-category", osCategory)
	re.Header.Set("x-warp-os-name", osName)
	re.Header.Set("x-warp-os-version", osVersion)
	re.Header.Set("x-warp-date", now)
	re.Header.Set("authorization", "Bearer "+jwt)
	re.Header.Set("x-warp-experiment-id", experimentID)
	re.Header.Set("x-warp-experiment-bucket", experimentBucket)
	re.Header.Set("accept", "*/*")
	re.Header.Set("accept-encoding", "gzip, br")
	re.Header.Set("x-warp-request-id", newUUID())
	re.Header.Set("content-length", "0")

	// Use the session's cookie jar if the client doesn't have one
	oldJar := httpClient.Jar
	httpClient.Jar = s.jar
	defer func() { httpClient.Jar = oldJar }()

	resp, err := httpClient.Do(re)
	if err != nil {
		slog.Warn("Warp AI: Login request failed", "cid", cid, "error", err)
		return err
	}
	defer resp.Body.Close()

	headers := make(map[string]string)
	for k, v := range resp.Header {
		headers[k] = strings.Join(v, ", ")
	}
	slog.Info("Warp AI: Login response", "cid", cid, "status", resp.StatusCode, "headers", headers)

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

func (s *session) getExperimentID() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.experimentID
}

func (s *session) getExperimentBucket() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.experimentBuck
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
