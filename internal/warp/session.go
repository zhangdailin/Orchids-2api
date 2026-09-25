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
	email          string
	expiresAt      time.Time
	deviceID       string
	requestID      string
	loggedIn       bool
	lastLogin      time.Time
	experimentID   string
	experimentBuck string
	jar            http.CookieJar
	refreshing     bool
	refreshDone    chan struct{}
	refreshErr     error
	refreshErrAt   time.Time
	loggingIn      bool
	loginDone      chan struct{}
	loginErr       error
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

const refreshFailureCoalesceWindow = 250 * time.Millisecond

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
	refreshToken = strings.TrimSpace(strings.Trim(refreshToken, "\"'"))
	deviceID, requestID = ensureSessionIDs(deviceID, requestID)
	key := sessionKey(accountID, refreshToken)

	if cached, ok := sessionCache.Load(key); ok {
		sess := cached.(*session)
		sess.mu.Lock()
		if sess.jar == nil {
			sess.jar = mustNewCookieJar()
		}
		if refreshToken != "" && sess.refreshToken != refreshToken {
			sess.refreshToken = refreshToken
			sess.jwt = ""
			sess.email = ""
			sess.expiresAt = time.Time{}
			sess.loggedIn = false
			sess.lastLogin = time.Time{}
			sess.refreshErr = nil
			sess.refreshErrAt = time.Time{}
			// A refresh-token replacement denotes a new Firebase/Warp identity.
			// Cookies and experiment/request identity from the previous login must
			// not cross that boundary.
			sess.jar = mustNewCookieJar()
			sess.deviceID = deviceID
			sess.requestID = requestID
			sess.experimentID = ""
			sess.experimentBuck = ""
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

func (s *session) ensureToken(ctx context.Context, httpClient *http.Client) error {
	s.mu.Lock()
	if s.tokenValid() {
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
		if s.refreshErr != nil {
			return s.refreshErr
		}
		return fmt.Errorf("warp refresh did not produce a valid token")
	}
	if s.refreshErr != nil && !s.refreshErrAt.IsZero() && time.Since(s.refreshErrAt) < refreshFailureCoalesceWindow {
		err := s.refreshErr
		s.mu.Unlock()
		return err
	}

	s.refreshing = true
	s.refreshDone = make(chan struct{})
	s.refreshErr = nil
	s.refreshErrAt = time.Time{}
	s.mu.Unlock()

	err := s.refresh(ctx, httpClient, "")

	s.mu.Lock()
	s.refreshErr = err
	if err != nil && ctx.Err() == nil {
		s.refreshErrAt = time.Now()
	} else {
		s.refreshErrAt = time.Time{}
	}
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
	s.email = ""
	s.expiresAt = time.Time{}
	s.loggedIn = false
	s.lastLogin = time.Time{}
}

func (s *session) refresh(ctx context.Context, httpClient *http.Client, firebaseTokenURL string) error {
	s.mu.Lock()
	refreshToken := strings.TrimSpace(strings.Trim(s.refreshToken, "\"'"))
	s.mu.Unlock()

	if refreshToken == "" {
		return fmt.Errorf("warp refresh token is empty")
	}
	if strings.TrimSpace(firebaseTokenURL) == "" {
		var err error
		firebaseTokenURL, err = warpFirebaseTokenURL()
		if err != nil {
			return err
		}
	}

	form := url.Values{
		"grant_type":    {"refresh_token"},
		"refresh_token": {refreshToken},
	}

	body, err := postWarpTokenForm(ctx, httpClient, firebaseTokenURL, form)
	if err != nil {
		return err
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
	s.email = util.JWTEmail(jwt)
	s.expiresAt = expiry
	s.registerExperimentHeadersLocked(jwt)
	if refresh != "" {
		s.refreshToken = strings.TrimSpace(strings.Trim(refresh, "\"'"))
	} else {
		s.refreshToken = strings.TrimSpace(strings.Trim(s.refreshToken, "\"'"))
	}
	s.loggedIn = false
	s.lastLogin = time.Time{}
	s.mu.Unlock()

	return nil
}

func postWarpTokenForm(ctx context.Context, httpClient *http.Client, endpoint string, form url.Values) ([]byte, error) {
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewBufferString(form.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")

	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	body, err := readLimitedBody(resp, 1<<20)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, &HTTPStatusError{
			Operation:       "refresh token",
			StatusCode:      resp.StatusCode,
			ErrorCode:       resp.Header.Get("X-Warp-Error-Code"),
			RetryAfterDelay: parseRetryAfterHeader(resp.Header.Get("Retry-After"), time.Now()),
		}
	}
	return body, nil
}

func (s *session) ensureLogin(ctx context.Context, httpClient *http.Client) (err error) {
	if s == nil {
		return fmt.Errorf("warp session is nil")
	}
	if httpClient == nil {
		httpClient = http.DefaultClient
	}

	s.mu.Lock()
	if s.loggedIn && !s.lastLogin.IsZero() && time.Since(s.lastLogin) < 5*time.Minute {
		s.mu.Unlock()
		return nil
	}
	if s.loggingIn {
		wait := s.loginDone
		s.mu.Unlock()
		select {
		case <-wait:
		case <-ctx.Done():
			return ctx.Err()
		}
		s.mu.Lock()
		defer s.mu.Unlock()
		if s.loggedIn && !s.lastLogin.IsZero() && time.Since(s.lastLogin) < 5*time.Minute {
			return nil
		}
		if s.loginErr != nil {
			return s.loginErr
		}
		return fmt.Errorf("warp login did not complete")
	}
	jwt := strings.TrimSpace(s.jwt)
	if jwt == "" {
		s.mu.Unlock()
		return fmt.Errorf("warp jwt missing")
	}
	s.loggingIn = true
	s.loginDone = make(chan struct{})
	s.loginErr = nil
	if strings.TrimSpace(s.experimentID) == "" {
		s.experimentID = newSessionUUID()
	}
	if strings.TrimSpace(s.experimentBuck) == "" {
		s.experimentBuck = newExperimentBucket()
	}
	experimentID := s.experimentID
	experimentBuck := s.experimentBuck
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		if err == nil && strings.TrimSpace(s.jwt) == jwt {
			s.loggedIn = true
			s.lastLogin = time.Now()
		} else if err == nil {
			err = fmt.Errorf("warp credentials changed during login")
		}
		s.loginErr = err
		s.loggingIn = false
		close(s.loginDone)
		s.loginDone = nil
		s.mu.Unlock()
	}()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, warpLoginURL, nil)
	if err != nil {
		return err
	}
	applyWarpClientHeaders(req)
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
			Operation:       "login",
			StatusCode:      resp.StatusCode,
			ErrorCode:       resp.Header.Get("X-Warp-Error-Code"),
			RetryAfterDelay: parseRetryAfterHeader(resp.Header.Get("Retry-After"), time.Now()),
		}
	}

	return nil
}

func (s *session) experimentHeaders() (string, string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if strings.TrimSpace(s.experimentID) == "" {
		s.experimentID = newSessionUUID()
	}
	if strings.TrimSpace(s.experimentBuck) == "" {
		s.experimentBuck = newExperimentBucket()
	}
	if s.jwt != "" {
		s.registerExperimentHeadersLocked(s.jwt)
	}
	return s.experimentID, s.experimentBuck
}

func (s *session) registerExperimentHeadersLocked(jwt string) {
	if strings.TrimSpace(s.experimentID) == "" {
		s.experimentID = newSessionUUID()
	}
	if strings.TrimSpace(s.experimentBuck) == "" {
		s.experimentBuck = newExperimentBucket()
	}
	registerJWTExperimentHeaders(jwt, s.experimentID, s.experimentBuck)
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

func (s *session) currentEmail() string {
	if s == nil {
		return ""
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.email
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

func (s *session) beginRequest() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if strings.TrimSpace(s.requestID) == "" {
		s.requestID = newSessionUUID()
	}
}

func InvalidateSession(accountID int64) {
	if accountID <= 0 {
		return
	}
	sessionCache.Delete(sessionKey(accountID, ""))
}
