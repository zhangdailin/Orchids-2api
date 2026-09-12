package workbuddy

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/goccy/go-json"
)

// ErrAuthPending is returned while the browser authorization has not completed.
// The upstream reports it as business code 11217 ("login ing...") on HTTP 200.
var ErrAuthPending = errors.New("workbuddy authorization is still pending")

// authPollTimeout bounds one token poll request.
const authPollTimeout = 20 * time.Second

// StartAuthLogin asks the upstream for a login transaction. The returned URL is
// the official WorkBuddy login page; opening it and completing the Keycloak
// sign-in authorizes the transaction identified by State.
func (c *Client) StartAuthLogin(ctx context.Context, clientVersion string) (state, authURL string, err error) {
	if c == nil {
		return "", "", fmt.Errorf("workbuddy client is nil")
	}
	reqCtx, cancel := context.WithTimeout(ctx, authPollTimeout)
	defer cancel()

	endpoint := c.baseURL + "/v2/plugin/auth/state?platform=workbuddy-ai"
	req, err := http.NewRequestWithContext(reqCtx, http.MethodPost, endpoint, strings.NewReader("{}"))
	if err != nil {
		return "", "", fmt.Errorf("failed to create workbuddy auth request: %w", err)
	}
	applyHeaders(req, "", "", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return "", "", fmt.Errorf("failed to start workbuddy authorization: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	data, err := unwrapEnvelope(resp.StatusCode, raw)
	if err != nil {
		return "", "", err
	}

	var payload struct {
		State   string `json:"state"`
		AuthURL string `json:"authUrl"`
	}
	if err := json.Unmarshal(data, &payload); err != nil {
		return "", "", fmt.Errorf("failed to decode workbuddy auth response: %w", err)
	}
	state = strings.TrimSpace(payload.State)
	authURL = strings.TrimSpace(payload.AuthURL)
	if state == "" || authURL == "" {
		return "", "", fmt.Errorf("workbuddy auth response is missing state or authUrl")
	}
	if clientVersion != "" {
		if parsed, parseErr := url.Parse(authURL); parseErr == nil && parsed.Query().Get("version") == "" {
			query := parsed.Query()
			query.Set("version", clientVersion)
			parsed.RawQuery = query.Encode()
			authURL = parsed.String()
		}
	}
	// Only the official login page may ever be handed to a browser.
	if host := authHost(authURL); host != "" && !strings.EqualFold(host, hostOf(c.baseURL)) && !strings.EqualFold(host, "www.workbuddy.ai") {
		return "", "", fmt.Errorf("workbuddy auth response pointed at an unexpected host %q", host)
	}
	return state, authURL, nil
}

// PollAuthLogin exchanges one completed login transaction for credentials.
// ErrAuthPending means the browser step is not finished yet.
func (c *Client) PollAuthLogin(ctx context.Context, state string) (Credentials, error) {
	if c == nil {
		return Credentials{}, fmt.Errorf("workbuddy client is nil")
	}
	state = strings.TrimSpace(state)
	if state == "" {
		return Credentials{}, fmt.Errorf("workbuddy login state is empty")
	}
	reqCtx, cancel := context.WithTimeout(ctx, authPollTimeout)
	defer cancel()

	endpoint := c.baseURL + "/v2/plugin/auth/token?state=" + url.QueryEscape(state)
	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, endpoint, nil)
	if err != nil {
		return Credentials{}, fmt.Errorf("failed to create workbuddy token request: %w", err)
	}
	applyHeaders(req, "", "", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return Credentials{}, fmt.Errorf("failed to poll workbuddy authorization: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))

	if code := envelopeCode(raw); code == CodeLoginPending {
		return Credentials{}, ErrAuthPending
	}
	data, err := unwrapEnvelope(resp.StatusCode, raw)
	if err != nil {
		if errors.Is(err, ErrAuthPending) {
			return Credentials{}, ErrAuthPending
		}
		return Credentials{}, err
	}

	var payload struct {
		AccessToken  string `json:"accessToken"`
		RefreshToken string `json:"refreshToken"`
		ExpiresIn    int64  `json:"expiresIn"`
		TokenType    string `json:"tokenType"`
		Scope        string `json:"scope"`
	}
	if err := json.Unmarshal(data, &payload); err != nil {
		return Credentials{}, fmt.Errorf("failed to decode workbuddy token response: %w", err)
	}
	if strings.TrimSpace(payload.AccessToken) == "" {
		return Credentials{}, fmt.Errorf("workbuddy token response carried no accessToken")
	}

	creds := Credentials{
		AccessToken:  strings.TrimSpace(payload.AccessToken),
		RefreshToken: strings.TrimSpace(payload.RefreshToken),
	}
	if payload.ExpiresIn > 0 {
		creds.ExpiresAt = time.Now().Add(time.Duration(payload.ExpiresIn) * time.Second)
	}
	if claims := DecodeClaims(creds.AccessToken); claims.ExpiresAt > 0 {
		creds.ExpiresAt = time.Unix(claims.ExpiresAt, 0)
		creds.UID = claims.Sub
		creds.Email = claims.Email
	}
	return creds, nil
}

// FetchAccountIdentity reads the signed-in account profile for the given token.
// It is optional metadata; callers must tolerate its failure.
func (c *Client) FetchAccountIdentity(ctx context.Context, accessToken, state string) (uid, nickname, email string, err error) {
	if c == nil {
		return "", "", "", fmt.Errorf("workbuddy client is nil")
	}
	reqCtx, cancel := context.WithTimeout(ctx, authPollTimeout)
	defer cancel()

	endpoint := c.baseURL + "/v2/plugin/login/account"
	if state != "" {
		endpoint += "?state=" + url.QueryEscape(state)
	}
	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, endpoint, nil)
	if err != nil {
		return "", "", "", err
	}
	applyHeaders(req, accessToken, "", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return "", "", "", err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	data, err := unwrapEnvelope(resp.StatusCode, raw)
	if err != nil {
		return "", "", "", err
	}
	var payload struct {
		UID      string `json:"uid"`
		Nickname string `json:"nickname"`
		Email    string `json:"email"`
	}
	if err := json.Unmarshal(data, &payload); err != nil {
		return "", "", "", err
	}
	return strings.TrimSpace(payload.UID), strings.TrimSpace(payload.Nickname), strings.TrimSpace(payload.Email), nil
}

// envelopeCode returns the business code of a raw envelope body, or 0.
func envelopeCode(raw []byte) int {
	trimmed := strings.TrimSpace(string(raw))
	if !strings.HasPrefix(trimmed, "{") {
		return 0
	}
	var env struct {
		Code int `json:"code"`
	}
	if err := json.Unmarshal([]byte(trimmed), &env); err != nil {
		return 0
	}
	return env.Code
}

func authHost(raw string) string {
	parsed, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	return parsed.Hostname()
}

func hostOf(base string) string {
	parsed, err := url.Parse(base)
	if err != nil {
		return ""
	}
	return parsed.Hostname()
}

// SetBaseURLForTest points the client at a stub server. It exists so handler
// tests can exercise the login transaction without touching the network.
func (c *Client) SetBaseURLForTest(base string) {
	if c == nil {
		return
	}
	c.baseURL = strings.TrimSuffix(strings.TrimSpace(base), "/")
	if c.updater != nil {
		c.updater.baseURL = c.baseURL
	}
}
