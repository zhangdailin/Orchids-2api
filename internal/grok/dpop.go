package grok

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/goccy/go-json"
)

const (
	consoleDPoPTokenURL = "https://console.x.ai/v1/dpop/token"
	dpopRefreshSkew     = 20 * time.Second
	maxDPoPLifetime     = time.Hour
)

type dpopJWK struct {
	Kty string `json:"kty"`
	Crv string `json:"crv"`
	X   string `json:"x"`
	Y   string `json:"y"`
}

type dpopSession struct {
	accessToken string
	privateKey  *ecdsa.PrivateKey
	publicJWK   dpopJWK
	expiresAt   time.Time
	clockSkew   time.Duration
}

type dpopSessionManager struct {
	mu       sync.Mutex
	sessions map[string]dpopSession
}

func newDPoPSessionManager() *dpopSessionManager {
	return &dpopSessionManager{sessions: make(map[string]dpopSession)}
}

func dpopCacheKey(token string) string {
	digest := sha256.Sum256([]byte(strings.TrimSpace(token)))
	return base64.RawURLEncoding.EncodeToString(digest[:])
}

// TokenFingerprint returns a short, non-reversible identifier for a credential so
// operators can tell whether two accounts share a token without printing it.
func TokenFingerprint(token string) string {
	token = strings.TrimSpace(token)
	if token == "" {
		return ""
	}
	digest := sha256.Sum256([]byte(token))
	return hex.EncodeToString(digest[:6])
}

func (m *dpopSessionManager) cached(key string) (dpopSession, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.sessions[key]
	if ok && s.expiresAt.After(time.Now().UTC().Add(dpopRefreshSkew)) {
		return s, true
	}
	delete(m.sessions, key)
	return dpopSession{}, false
}

func (m *dpopSessionManager) store(key string, s dpopSession) {
	m.mu.Lock()
	m.sessions[key] = s
	m.mu.Unlock()
}

func (m *dpopSessionManager) invalidate(key, accessToken string) {
	m.mu.Lock()
	if s, ok := m.sessions[key]; ok && (accessToken == "" || s.accessToken == accessToken) {
		delete(m.sessions, key)
	}
	m.mu.Unlock()
}

func publicDPoPJWK(key *ecdsa.PublicKey) dpopJWK {
	return dpopJWK{
		Kty: "EC", Crv: "P-256",
		X: base64.RawURLEncoding.EncodeToString(key.X.FillBytes(make([]byte, 32))),
		Y: base64.RawURLEncoding.EncodeToString(key.Y.FillBytes(make([]byte, 32))),
	}
}

func dpopJWKThumbprint(jwk dpopJWK) (string, error) {
	canonical := struct {
		Crv string `json:"crv"`
		Kty string `json:"kty"`
		X   string `json:"x"`
		Y   string `json:"y"`
	}{jwk.Crv, jwk.Kty, jwk.X, jwk.Y}
	data, err := json.Marshal(canonical)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(data)
	return base64.RawURLEncoding.EncodeToString(digest[:]), nil
}

func parseDPoPAccessToken(value string) (time.Time, string, error) {
	parts := strings.Split(value, ".")
	if len(parts) != 3 {
		return time.Time{}, "", errors.New("invalid DPoP access token")
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return time.Time{}, "", errors.New("invalid DPoP access token payload")
	}
	var claims struct {
		ExpiresAt int64 `json:"exp"`
		CNF       struct {
			JKT string `json:"jkt"`
		} `json:"cnf"`
	}
	if err := json.Unmarshal(payload, &claims); err != nil || claims.ExpiresAt <= 0 || strings.TrimSpace(claims.CNF.JKT) == "" {
		return time.Time{}, "", errors.New("invalid DPoP access token claims")
	}
	return time.Unix(claims.ExpiresAt, 0).UTC(), claims.CNF.JKT, nil
}

func dpopClockSkew(date string, before, after time.Time) time.Duration {
	server, err := http.ParseTime(strings.TrimSpace(date))
	if err != nil {
		return 0
	}
	if after.IsZero() || after.Before(before) {
		after = before
	}
	mid := before.Add(after.Sub(before) / 2)
	return server.UTC().Sub(mid.UTC()).Round(time.Second)
}

func dpopHTU(req *http.Request) string {
	path := req.URL.EscapedPath()
	if path == "" {
		path = "/"
	}
	return req.URL.Scheme + "://" + req.URL.Host + path
}

func signDPoPJWT(key *ecdsa.PrivateKey, jwk dpopJWK, claims map[string]interface{}) (string, error) {
	header, err := json.Marshal(map[string]interface{}{"alg": "ES256", "typ": "dpop+jwt", "jwk": jwk})
	if err != nil {
		return "", err
	}
	payload, err := json.Marshal(claims)
	if err != nil {
		return "", err
	}
	encoded := base64.RawURLEncoding.EncodeToString(header) + "." + base64.RawURLEncoding.EncodeToString(payload)
	digest := sha256.Sum256([]byte(encoded))
	r, s, err := ecdsa.Sign(rand.Reader, key, digest[:])
	if err != nil {
		return "", err
	}
	sig := append(r.FillBytes(make([]byte, 32)), s.FillBytes(make([]byte, 32))...)
	return encoded + "." + base64.RawURLEncoding.EncodeToString(sig), nil
}

func applyDPoPAuthorization(req *http.Request, session dpopSession) error {
	if req == nil || req.URL == nil || session.privateKey == nil || session.accessToken == "" {
		return errors.New("invalid DPoP request")
	}
	digest := sha256.Sum256([]byte(session.accessToken))
	proof, err := signDPoPJWT(session.privateKey, session.publicJWK, map[string]interface{}{
		"jti": randomUUID(), "htm": strings.ToUpper(req.Method), "htu": dpopHTU(req),
		"iat": time.Now().UTC().Add(session.clockSkew).Unix(),
		"ath": base64.RawURLEncoding.EncodeToString(digest[:]),
	})
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "DPoP "+session.accessToken)
	req.Header.Set("DPoP", proof)
	return nil
}

func (c *Client) fetchDPoPSession(ctx context.Context, token string) (dpopSession, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return dpopSession{}, err
	}
	jwk := publicDPoPJWK(&key.PublicKey)
	body, err := json.Marshal(map[string]interface{}{"jwk": jwk})
	if err != nil {
		return dpopSession{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, consoleDPoPTokenURL, bytes.NewReader(body))
	if err != nil {
		return dpopSession{}, err
	}
	req.Header = c.consoleHeaders(token)
	req.Header.Set("Content-Type", "application/json")
	before := time.Now().UTC()
	resp, err := c.httpClient.Do(req)
	after := time.Now().UTC()
	if err != nil {
		return dpopSession{}, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if err != nil {
		return dpopSession{}, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return dpopSession{}, fmt.Errorf("grok upstream status=%d body=%s", resp.StatusCode, raw)
	}
	var out struct {
		AccessToken string `json:"access_token"`
		TokenType   string `json:"token_type"`
		ExpiresIn   int    `json:"expires_in"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return dpopSession{}, fmt.Errorf("decode DPoP token: %w", err)
	}
	if out.AccessToken == "" || !strings.EqualFold(out.TokenType, "DPoP") || out.ExpiresIn <= 0 || time.Duration(out.ExpiresIn)*time.Second > maxDPoPLifetime {
		return dpopSession{}, errors.New("invalid DPoP token response")
	}
	thumbprint, err := dpopJWKThumbprint(jwk)
	if err != nil {
		return dpopSession{}, err
	}
	tokenExpiry, tokenThumbprint, err := parseDPoPAccessToken(out.AccessToken)
	if err != nil || tokenThumbprint != thumbprint {
		return dpopSession{}, errors.New("DPoP token key binding mismatch")
	}
	now := time.Now().UTC()
	expires := now.Add(time.Duration(out.ExpiresIn) * time.Second)
	if tokenExpiry.Before(expires) {
		expires = tokenExpiry
	}
	if !expires.After(now.Add(dpopRefreshSkew)) {
		return dpopSession{}, errors.New("DPoP token already expired")
	}
	return dpopSession{out.AccessToken, key, jwk, expires, dpopClockSkew(resp.Header.Get("Date"), before, after)}, nil
}

func (c *Client) dpopSession(ctx context.Context, token string) (dpopSession, string, error) {
	cacheKey := dpopCacheKey(token)
	if s, ok := c.dpop.cached(cacheKey); ok {
		return s, cacheKey, nil
	}
	s, err := c.fetchDPoPSession(ctx, token)
	if err == nil {
		c.dpop.store(cacheKey, s)
	}
	return s, cacheKey, err
}

func (c *Client) doConsoleDPoPRequest(ctx context.Context, token, method, endpoint string, body []byte) (*http.Response, error) {
	return c.doConsoleDPoPRequestWithHeaders(ctx, token, method, endpoint, body, nil)
}

// doConsoleDPoPRequestWithHeaders performs a Console request while preserving
// the DPoP retry rules used by Responses. Voice endpoints need to override the
// request Content-Type for multipart STT and the response Accept type for TTS.
func (c *Client) doConsoleDPoPRequestWithHeaders(ctx context.Context, token, method, endpoint string, body []byte, overrides http.Header) (*http.Response, error) {
	var payload struct {
		Model string `json:"model"`
	}
	_ = json.Unmarshal(body, &payload)
	if err := waitScopedRateLimit(ctx, ProviderConsole, token, payload.Model, c.cfg.GrokRequestsPerSecond(ProviderConsole)); err != nil {
		return nil, err
	}
	for attempt := 0; attempt < 2; attempt++ {
		session, cacheKey, err := c.dpopSession(ctx, token)
		if err != nil {
			return nil, err
		}
		req, err := http.NewRequestWithContext(ctx, method, endpoint, bytes.NewReader(body))
		if err != nil {
			return nil, err
		}
		req.Header = c.consoleHeaders(token)
		for key, values := range overrides {
			req.Header.Del(key)
			for _, value := range values {
				req.Header.Add(key, value)
			}
		}
		req.Header.Set("x-cluster", "https://us-east-1.api.x.ai")
		if err := applyDPoPAuthorization(req, session); err != nil {
			return nil, err
		}
		client := *c.httpClient
		client.Timeout = c.cfg.GrokRequestTimeout(ProviderConsole)
		resp, err := doUpstreamHTTP(req, client.Do, c.cfg.GrokStreamIdleTimeout())
		if err != nil {
			return nil, err
		}
		if resp.StatusCode == http.StatusOK {
			return resp, nil
		}
		raw, headerCopy := readBoundedResponse(resp)
		if resp.StatusCode == http.StatusTooManyRequests {
			noteScopedRateLimit(ctx, ProviderConsole, token, payload.Model, resp.StatusCode, resp.Header, raw)
		}
		// DPoP 401 (session expiry) and explicit DPoP 403 challenges invalidate the
		// DPoP session and retry at most once. Cloudflare/egress challenges and
		// ordinary 403s must NOT touch the DPoP session.
		dpopChallenge := resp.StatusCode == http.StatusUnauthorized ||
			(resp.StatusCode == http.StatusForbidden && IsDPoPProofRequiredBody(raw))
		if dpopChallenge && attempt == 0 {
			recordUpstreamChallenge("dpop")
			c.dpop.invalidate(cacheKey, session.accessToken)
			continue
		}

		kind := ClassifyUpstreamResponse(resp.StatusCode, resp.Header, raw)
		if kind == UpstreamErrorCloudflareChallenge {
			recordUpstreamChallenge("cloudflare")
		} else if kind == UpstreamErrorGenericForbidden {
			recordGenericForbidden()
		}
		return nil, newUpstreamError(resp.StatusCode, headerCopy, raw, "")
	}
	return nil, errors.New("DPoP retry exhausted")
}
