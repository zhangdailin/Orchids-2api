package warp

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/goccy/go-json"

	"orchids-api/internal/util"
)

type session struct {
	mu             sync.Mutex
	refreshToken   string
	jwt            string
	expiresAt      time.Time
	deviceID       string
	requestID      string
	loggedIn       bool
	lastLogin      time.Time
	experimentID   string
	experimentBuck string
	jar            http.CookieJar
	lastUsed       time.Time
	refreshing     bool
	refreshDone    chan struct{}
}

type refreshResponse struct {
	AccessToken     string      `json:"access_token"`
	IDToken         string      `json:"id_token"`
	IDTokenAlt      string      `json:"idToken"`
	RefreshToken    string      `json:"refresh_token"`
	RefreshTokenAlt string      `json:"refreshToken"`
	ExpiresIn       interface{} `json:"expires_in"`
	ExpiresInAlt    interface{} `json:"expiresIn"`
}

var sessionCache sync.Map

func sessionKey(accountID int64, refreshToken string) string {
	if accountID > 0 {
		return fmt.Sprintf("warp:%d", accountID)
	}
	refreshToken = strings.TrimSpace(refreshToken)
	if refreshToken == "" {
		return "warp:anon"
	}
	sum := sha256.Sum256([]byte(refreshToken))
	return "warp:" + hex.EncodeToString(sum[:8])
}

func getSession(accountID int64, refreshToken, deviceID, requestID string) *session {
	refreshToken = normalizeRefreshToken(refreshToken)
	deviceID, requestID = ensureSessionIDs(deviceID, requestID)
	key := sessionKey(accountID, refreshToken)

	if cached, ok := sessionCache.Load(key); ok {
		sess := cached.(*session)
		sess.mu.Lock()
		sess.lastUsed = time.Now()
		if sess.jar == nil {
			sess.jar = mustNewCookieJar()
		}
		if refreshToken != "" && sess.refreshToken != refreshToken {
			sess.refreshToken = refreshToken
			sess.jwt = ""
			sess.expiresAt = time.Time{}
			sess.loggedIn = false
			sess.lastLogin = time.Time{}
		}
		if sess.deviceID == "" {
			sess.deviceID = deviceID
		}
		if sess.requestID == "" {
			sess.requestID = requestID
		}
		sess.mu.Unlock()
		return sess
	}

	sess := &session{
		refreshToken: refreshToken,
		deviceID:     deviceID,
		requestID:    requestID,
		jar:          mustNewCookieJar(),
		lastUsed:     time.Now(),
	}
	actual, _ := sessionCache.LoadOrStore(key, sess)
	return actual.(*session)
}

func mustNewCookieJar() http.CookieJar {
	jar, err := cookiejar.New(nil)
	if err != nil {
		return nil
	}
	return jar
}



func normalizeRefreshToken(refreshToken string) string {
	refreshToken = strings.TrimSpace(strings.Trim(refreshToken, "\"'"))
	if refreshToken == "" {
		return ""
	}

	if token := extractRefreshTokenFromJSON(refreshToken); token != "" {
		refreshToken = token
	}
	if token := extractRefreshTokenFromPairs(refreshToken); token != "" {
		refreshToken = token
	}
	if strings.Contains(refreshToken, "----") {
		parts := strings.Split(refreshToken, "----")
		refreshToken = strings.TrimSpace(parts[len(parts)-1])
		if token := extractRefreshTokenFromPairs(refreshToken); token != "" {
			refreshToken = token
		}
	}

	return strings.TrimSpace(strings.Trim(refreshToken, "\"'"))
}

func extractRefreshTokenFromJSON(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	first := raw[0]
	if first != '{' && first != '[' {
		return ""
	}

	var decoded interface{}
	if err := json.Unmarshal([]byte(raw), &decoded); err != nil {
		return ""
	}
	return findRefreshTokenValue(decoded)
}

func findRefreshTokenValue(value interface{}) string {
	switch v := value.(type) {
	case map[string]interface{}:
		for key, item := range v {
			switch strings.ToLower(strings.TrimSpace(key)) {
			case "refresh_token", "refreshtoken":
				if token := normalizeRefreshTokenValue(item); token != "" {
					return token
				}
			}
			if token := findRefreshTokenValue(item); token != "" {
				return token
			}
		}
	case []interface{}:
		for _, item := range v {
			if token := findRefreshTokenValue(item); token != "" {
				return token
			}
		}
	}
	return ""
}

func normalizeRefreshTokenValue(value interface{}) string {
	switch v := value.(type) {
	case string:
		return strings.TrimSpace(strings.Trim(v, "\"'"))
	case json.Number:
		return strings.TrimSpace(v.String())
	default:
		return strings.TrimSpace(strings.Trim(fmt.Sprintf("%v", v), "\"'"))
	}
}

func extractRefreshTokenFromPairs(raw string) string {
	candidates := []string{strings.TrimSpace(raw)}
	if idx := strings.Index(raw, "?"); idx >= 0 && idx < len(raw)-1 {
		candidates = append(candidates, strings.TrimSpace(raw[idx+1:]))
	}

	replacer := strings.NewReplacer(";", "&", "\n", "&", "\r", "&")
	for _, candidate := range candidates {
		candidate = strings.TrimSpace(candidate)
		if candidate == "" {
			continue
		}
		normalized := replacer.Replace(candidate)
		values := make(url.Values)
		for _, segment := range strings.Split(normalized, "&") {
			segment = strings.TrimSpace(segment)
			if segment == "" {
				continue
			}
			key, value, ok := strings.Cut(segment, "=")
			if !ok {
				key, value, ok = strings.Cut(segment, ":")
			}
			if !ok {
				continue
			}
			key = strings.TrimSpace(key)
			value = strings.TrimSpace(value)
			decoded, err := url.QueryUnescape(value)
			if err == nil {
				value = decoded
			}
			values.Add(key, value)
		}
		for _, key := range []string{"refresh_token", "refreshToken"} {
			if token := strings.TrimSpace(values.Get(key)); token != "" {
				return strings.TrimSpace(strings.Trim(token, "\"'"))
			}
		}
	}
	return ""
}

func ensureSessionIDs(deviceID, requestID string) (string, string) {
	deviceID = strings.TrimSpace(deviceID)
	requestID = strings.TrimSpace(requestID)
	if deviceID == "" {
		deviceID = newSessionUUID()
	}
	if requestID == "" {
		requestID = newSessionUUID()
	}
	return deviceID, requestID
}

func newSessionUUID() string {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		sum := sha256.Sum256([]byte(fmt.Sprintf("%d", time.Now().UnixNano())))
		copy(buf, sum[:16])
	}
	buf[6] = (buf[6] & 0x0f) | 0x40
	buf[8] = (buf[8] & 0x3f) | 0x80

	encoded := hex.EncodeToString(buf)
	return fmt.Sprintf("%s-%s-%s-%s-%s",
		encoded[0:8],
		encoded[8:12],
		encoded[12:16],
		encoded[16:20],
		encoded[20:32],
	)
}

func (s *session) tokenValid() bool {
	if s == nil || s.jwt == "" || s.expiresAt.IsZero() {
		return false
	}
	return time.Now().Add(5 * time.Minute).Before(s.expiresAt)
}

func (s *session) seedJWT(jwt string) {
	jwt = strings.TrimSpace(jwt)
	if jwt == "" {
		return
	}
	exp := util.JWTExpiry(jwt, 0)
	if exp.IsZero() || time.Now().Add(5*time.Minute).After(exp) {
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.tokenValid() {
		return
	}
	s.jwt = jwt
	s.expiresAt = exp
}

func (s *session) ensureToken(ctx context.Context, httpClient *http.Client) error {
	s.mu.Lock()
	if s.tokenValid() {
		s.lastUsed = time.Now()
		s.mu.Unlock()
		return nil
	}
	if s.refreshing {
		wait := s.refreshDone
		s.mu.Unlock()
		if wait != nil {
			select {
			case <-wait:
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		s.mu.Lock()
		defer s.mu.Unlock()
		if s.tokenValid() {
			return nil
		}
		return fmt.Errorf("warp refresh did not produce a valid token")
	}

	s.refreshing = true
	s.refreshDone = make(chan struct{})
	s.mu.Unlock()

	err := s.refresh(ctx, httpClient)

	s.mu.Lock()
	s.refreshing = false
	close(s.refreshDone)
	s.refreshDone = nil
	s.mu.Unlock()

	return err
}

func (s *session) clearToken() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.jwt = ""
	s.expiresAt = time.Time{}
	s.loggedIn = false
	s.lastLogin = time.Time{}
}

func (s *session) refresh(ctx context.Context, httpClient *http.Client) error {
	s.mu.Lock()
	refreshToken := normalizeRefreshToken(s.refreshToken)
	s.mu.Unlock()

	if refreshToken == "" {
		return fmt.Errorf("warp refresh token is empty")
	}

	form := url.Values{
		"grant_type":    {"refresh_token"},
		"refresh_token": {refreshToken},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, warpFirebaseURL, bytes.NewBufferString(form.Encode()))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")

	resp, err := httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	body, err := readLimitedBody(resp, 1<<20)
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusOK {
		return &HTTPStatusError{
			Operation:  "refresh token",
			StatusCode: resp.StatusCode,
			RetryAfter: parseRetryAfterHeader(resp.Header.Get("Retry-After"), time.Now()),
		}
	}

	var parsed refreshResponse
	if err := json.Unmarshal(body, &parsed); err != nil {
		return fmt.Errorf("decode warp refresh response: %w", err)
	}

	jwt := strings.TrimSpace(parsed.IDToken)
	if jwt == "" {
		jwt = strings.TrimSpace(parsed.IDTokenAlt)
	}
	if jwt == "" {
		jwt = strings.TrimSpace(parsed.AccessToken)
	}
	if jwt == "" {
		return fmt.Errorf("warp refresh response missing id_token")
	}

	expiry := util.JWTExpiry(jwt, 0)
	if expiry.IsZero() {
		if seconds := parseExpiresIn(parsed.ExpiresIn); seconds > 0 {
			expiry = time.Now().Add(time.Duration(seconds) * time.Second)
		} else if seconds := parseExpiresIn(parsed.ExpiresInAlt); seconds > 0 {
			expiry = time.Now().Add(time.Duration(seconds) * time.Second)
		}
	}
	if expiry.IsZero() {
		expiry = time.Now().Add(55 * time.Minute)
	}

	refresh := strings.TrimSpace(parsed.RefreshToken)
	if refresh == "" {
		refresh = strings.TrimSpace(parsed.RefreshTokenAlt)
	}

	s.mu.Lock()
	s.jwt = jwt
	s.expiresAt = expiry
	if refresh != "" {
		s.refreshToken = normalizeRefreshToken(refresh)
	} else {
		s.refreshToken = normalizeRefreshToken(s.refreshToken)
	}
	s.loggedIn = false
	s.lastLogin = time.Time{}
	s.lastUsed = time.Now()
	s.mu.Unlock()

	return nil
}

func (s *session) ensureLogin(ctx context.Context, httpClient *http.Client) error {
	if s == nil {
		return fmt.Errorf("warp session is nil")
	}
	if httpClient == nil {
		httpClient = http.DefaultClient
	}

	s.mu.Lock()
	if s.loggedIn && time.Since(s.lastLogin) < 30*time.Minute {
		s.lastUsed = time.Now()
		s.mu.Unlock()
		return nil
	}
	jwt := strings.TrimSpace(s.jwt)
	if jwt == "" {
		s.mu.Unlock()
		return fmt.Errorf("warp jwt missing")
	}
	if strings.TrimSpace(s.experimentID) == "" {
		s.experimentID = newSessionUUID()
	}
	if strings.TrimSpace(s.experimentBuck) == "" {
		s.experimentBuck = newExperimentBucket()
	}
	experimentID := s.experimentID
	experimentBuck := s.experimentBuck
	s.mu.Unlock()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, warpLegacyLoginURL, nil)
	if err != nil {
		return err
	}
	req.Header.Set("X-Warp-Client-ID", clientID)
	req.Header.Set("X-Warp-Client-Version", clientVersion)
	req.Header.Set("X-Warp-OS-Category", clientOSCategory)
	req.Header.Set("X-Warp-OS-Name", clientOSName)
	req.Header.Set("X-Warp-OS-Version", clientOSVersion)
	req.Header.Set("Authorization", "Bearer "+jwt)
	req.Header.Set("X-Warp-Experiment-Id", experimentID)
	req.Header.Set("X-Warp-Experiment-Bucket", experimentBuck)
	req.Header.Set("Accept", "*/*")
	req.Header.Set("Accept-Encoding", "gzip")
	req.Header.Set("Content-Length", "0")

	resp, err := httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if _, err := readLimitedBody(resp, 1<<20); err != nil {
		return err
	}
	if resp.StatusCode != http.StatusNoContent {
		return &HTTPStatusError{
			Operation:  "login",
			StatusCode: resp.StatusCode,
			RetryAfter: parseRetryAfterHeader(resp.Header.Get("Retry-After"), time.Now()),
		}
	}

	s.mu.Lock()
	s.loggedIn = true
	s.lastLogin = time.Now()
	s.lastUsed = time.Now()
	s.mu.Unlock()
	return nil
}

func newExperimentBucket() string {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		sum := sha256.Sum256([]byte(fmt.Sprintf("%d", time.Now().UnixNano())))
		return hex.EncodeToString(sum[:])
	}
	sum := sha256.Sum256(buf)
	return hex.EncodeToString(sum[:])
}

func readLimitedBody(resp *http.Response, limit int64) ([]byte, error) {
	if resp == nil || resp.Body == nil {
		return nil, nil
	}
	var reader io.Reader = resp.Body
	if strings.EqualFold(resp.Header.Get("Content-Encoding"), "gzip") {
		gr, err := gzip.NewReader(resp.Body)
		if err != nil {
			return nil, err
		}
		defer gr.Close()
		reader = gr
	}
	return io.ReadAll(io.LimitReader(reader, limit))
}

func parseExpiresIn(raw interface{}) int64 {
	switch v := raw.(type) {
	case string:
		n, _ := strconv.ParseInt(strings.TrimSpace(v), 10, 64)
		return n
	case float64:
		return int64(v)
	case json.Number:
		n, _ := v.Int64()
		return n
	default:
		return 0
	}
}

func (s *session) currentJWT() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return strings.TrimSpace(s.jwt)
}

func (s *session) currentRefreshToken() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return strings.TrimSpace(s.refreshToken)
}

func (s *session) currentDeviceID() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return strings.TrimSpace(s.deviceID)
}

func (s *session) currentRequestID() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return strings.TrimSpace(s.requestID)
}

func (s *session) beginRequest() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if strings.TrimSpace(s.requestID) == "" {
		s.requestID = newSessionUUID()
	}
	s.lastUsed = time.Now()
	return s.requestID
}

func InvalidateSession(accountID int64) {
	if accountID <= 0 {
		return
	}
	sessionCache.Delete(sessionKey(accountID, ""))
}
