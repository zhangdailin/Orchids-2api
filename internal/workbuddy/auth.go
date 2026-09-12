package workbuddy

import (
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/goccy/go-json"

	"orchids-api/internal/store"
)

// AccountUpdater is the subset of the account store the client needs to persist
// rotated refresh tokens. It is satisfied by *store.Store.
type AccountUpdater interface {
	UpdateAccount(ctx context.Context, acc *store.Account) error
}

// Credentials holds the WorkBuddy account material needed to talk to the
// international backend. RefreshToken is the durable credential: the desktop
// client stores it beside the access token, and Keycloak rotates it on every
// refresh.
type Credentials struct {
	AccessToken  string
	RefreshToken string
	UID          string
	Email        string
	ExpiresAt    time.Time
}

// Token returns the stored access token when it is still comfortably valid.
func (c Credentials) Token(now time.Time) (string, bool) {
	if strings.TrimSpace(c.AccessToken) == "" {
		return "", false
	}
	if c.ExpiresAt.IsZero() {
		// An opaque token without a decodable expiry is used as-is; the
		// upstream will answer 12153 if it is stale.
		return c.AccessToken, true
	}
	if c.ExpiresAt.Sub(now) <= minRefreshLead {
		return c.AccessToken, false
	}
	return c.AccessToken, true
}

// ResolveCredentials extracts credentials from an account record. Accepted
// forms, in priority order:
//
//  1. a WorkBuddy auth JSON document ({auth:{accessToken,refreshToken},account:{uid}})
//  2. a raw access token (JWT) or refresh token
//  3. `key=value` pairs separated by newlines, commas or semicolons
//
// The long-lived refresh token is the preferred credential; the access token is
// accepted because it is what the desktop session exposes most visibly.
func ResolveCredentials(acc *store.Account) Credentials {
	if acc == nil {
		return Credentials{}
	}

	creds := Credentials{
		AccessToken:  normalizeToken(acc.WorkBuddyAccessToken),
		RefreshToken: normalizeToken(acc.WorkBuddyRefreshToken),
		UID:          strings.TrimSpace(acc.WorkBuddyUID),
		Email:        strings.TrimSpace(acc.Email),
		ExpiresAt:    acc.WorkBuddyExpiresAt,
	}

	for _, raw := range []string{acc.ClientCookie, acc.Token, acc.SessionCookie, acc.RefreshToken} {
		if strings.TrimSpace(raw) == "" {
			continue
		}
		parsed := parseCredentialBlob(raw)
		if creds.AccessToken == "" {
			creds.AccessToken = parsed.AccessToken
		}
		if creds.RefreshToken == "" {
			creds.RefreshToken = parsed.RefreshToken
		}
		if creds.UID == "" {
			creds.UID = parsed.UID
		}
		if creds.Email == "" {
			creds.Email = parsed.Email
		}
		if creds.ExpiresAt.IsZero() {
			creds.ExpiresAt = parsed.ExpiresAt
		}
	}

	// Fill what the token itself can prove.
	accessClaims := DecodeClaims(creds.AccessToken)
	if creds.UID == "" {
		creds.UID = accessClaims.Sub
	}
	if creds.Email == "" {
		creds.Email = accessClaims.Email
	}
	if creds.ExpiresAt.IsZero() && accessClaims.ExpiresAt > 0 {
		creds.ExpiresAt = time.Unix(accessClaims.ExpiresAt, 0)
	}
	return creds
}

// parseCredentialBlob understands one pasted credential value.
func parseCredentialBlob(raw string) Credentials {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return Credentials{}
	}

	if strings.HasPrefix(raw, "{") {
		if creds, ok := parseAuthDocument(raw); ok {
			return creds
		}
	}

	// key=value pairs (Cookie header style, desktop .info style, or an
	// explicit access_token=/refresh_token= pair).
	if strings.Contains(raw, "=") {
		var creds Credentials
		for _, part := range splitPairs(raw) {
			key, value, ok := strings.Cut(part, "=")
			if !ok {
				continue
			}
			value = normalizeToken(value)
			if value == "" {
				continue
			}
			switch strings.ToLower(strings.TrimSpace(key)) {
			case "accesstoken", "access_token":
				creds.AccessToken = value
			case "refreshtoken", "refresh_token":
				creds.RefreshToken = value
			case "uid", "sub", "userid", "user_id":
				creds.UID = value
			case "email":
				creds.Email = value
			}
		}
		if creds.AccessToken != "" || creds.RefreshToken != "" {
			return creds
		}
	}

	token := normalizeToken(raw)
	claims := DecodeClaims(token)
	if claims.Sub != "" || claims.ExpiresAt > 0 {
		// A decodable JWT is an access token.
		creds := Credentials{AccessToken: token, UID: claims.Sub, Email: claims.Email}
		if claims.ExpiresAt > 0 {
			creds.ExpiresAt = time.Unix(claims.ExpiresAt, 0)
		}
		return creds
	}
	// Otherwise treat it as the durable refresh token.
	return Credentials{RefreshToken: token}
}

func splitPairs(raw string) []string {
	replacer := strings.NewReplacer("\r\n", "\n", ";", "\n", ",", "\n")
	parts := make([]string, 0, 4)
	for _, line := range strings.Split(replacer.Replace(raw), "\n") {
		if trimmed := strings.TrimSpace(line); trimmed != "" {
			parts = append(parts, trimmed)
		}
	}
	return parts
}

// parseAuthDocument reads the WorkBuddy auth store shape:
//
//	{"account":{"uid":...,"nickname":...},"auth":{"accessToken":...,"refreshToken":...,"expiresAt":<ms|s>}}
func parseAuthDocument(raw string) (Credentials, bool) {
	var doc struct {
		AccessToken  string `json:"accessToken"`
		RefreshToken string `json:"refreshToken"`
		ExpiresAt    int64  `json:"expiresAt"`
		UID          string `json:"uid"`
		Auth         *struct {
			AccessToken  string `json:"accessToken"`
			RefreshToken string `json:"refreshToken"`
			ExpiresAt    int64  `json:"expiresAt"`
			Domain       string `json:"domain"`
		} `json:"auth"`
		Account *struct {
			UID      string `json:"uid"`
			Email    string `json:"email"`
			Nickname string `json:"nickname"`
		} `json:"account"`
	}
	if err := json.Unmarshal([]byte(raw), &doc); err != nil {
		return Credentials{}, false
	}

	creds := Credentials{
		AccessToken:  normalizeToken(doc.AccessToken),
		RefreshToken: normalizeToken(doc.RefreshToken),
		UID:          strings.TrimSpace(doc.UID),
	}
	expiresAt := doc.ExpiresAt
	if doc.Auth != nil {
		if creds.AccessToken == "" {
			creds.AccessToken = normalizeToken(doc.Auth.AccessToken)
		}
		if creds.RefreshToken == "" {
			creds.RefreshToken = normalizeToken(doc.Auth.RefreshToken)
		}
		if expiresAt == 0 {
			expiresAt = doc.Auth.ExpiresAt
		}
	}
	if doc.Account != nil {
		if creds.UID == "" {
			creds.UID = strings.TrimSpace(doc.Account.UID)
		}
		creds.Email = strings.TrimSpace(doc.Account.Email)
		if creds.Email == "" && strings.Contains(doc.Account.Nickname, "@") {
			creds.Email = strings.TrimSpace(doc.Account.Nickname)
		}
	}
	if creds.AccessToken == "" && creds.RefreshToken == "" {
		return Credentials{}, false
	}
	if expiresAt > 0 {
		// The desktop session file stores milliseconds; the bridge stores
		// seconds. Anything past year 3000 is milliseconds.
		if expiresAt > 32503680000 {
			expiresAt /= 1000
		}
		creds.ExpiresAt = time.Unix(expiresAt, 0)
	}
	return creds, true
}

func normalizeToken(value string) string {
	value = strings.Trim(strings.TrimSpace(value), `"'`)
	if value == "" {
		return ""
	}
	if idx := strings.Index(value, "="); idx > 0 {
		key := strings.ToLower(strings.TrimSpace(value[:idx]))
		if key == "bearer" || key == "authorization" {
			value = strings.TrimSpace(value[idx+1:])
		}
	}
	value = strings.TrimSpace(strings.TrimPrefix(value, "Bearer "))
	value = strings.TrimSpace(strings.TrimPrefix(value, "bearer "))
	return strings.Trim(value, `"'`)
}

// Claims is the subset of the Keycloak access-token claims the gateway uses.
type Claims struct {
	Sub       string `json:"sub"`
	Email     string `json:"email"`
	Issuer    string `json:"iss"`
	Scope     string `json:"scope"`
	ExpiresAt int64  `json:"exp"`
}

// DecodeClaims reads the JWT payload without verifying the signature; the
// upstream remains the authority on validity.
func DecodeClaims(token string) Claims {
	parts := strings.Split(strings.TrimSpace(token), ".")
	if len(parts) < 2 {
		return Claims{}
	}
	segment := parts[1]
	segment = strings.ReplaceAll(segment, "-", "+")
	segment = strings.ReplaceAll(segment, "_", "/")
	if pad := len(segment) % 4; pad != 0 {
		segment += strings.Repeat("=", 4-pad)
	}
	raw, err := base64.StdEncoding.DecodeString(segment)
	if err != nil {
		return Claims{}
	}
	var claims Claims
	if err := json.Unmarshal(raw, &claims); err != nil {
		return Claims{}
	}
	return claims
}

func applyHeaders(req *http.Request, accessToken, uid, accept string) {
	if accept != "" {
		req.Header.Set("Accept", accept)
	} else {
		req.Header.Set("Accept", "application/json, text/plain, */*")
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", originReferer)
	req.Header.Set("Referer", originReferer+"/")
	req.Header.Set("User-Agent", clientUA)
	req.Header.Set("X-Requested-With", "XMLHttpRequest")
	if accessToken != "" {
		req.Header.Set("Authorization", "Bearer "+accessToken)
	} else {
		req.Header.Set("X-No-Authorization", "1")
	}
	if uid != "" {
		req.Header.Set("X-User-Id", url.QueryEscape(uid))
	} else {
		req.Header.Set("X-No-User-Id", "1")
	}
	req.Header.Set("X-No-Enterprise-Id", "1")
	req.Header.Set("X-No-Department-Info", "1")
	req.Header.Set("X-Product", "SaaS")
}

// envelope mirrors the {code,msg,requestId,data} wrapper used by every /v2 and
// /v3 endpoint.
type envelope struct {
	Code int             `json:"code"`
	Msg  string          `json:"msg"`
	Data json.RawMessage `json:"data"`
}

var errorHints = map[int]string{
	CodeLoginPending:  "workbuddy login has not completed; finish the browser authorization",
	CodeModelThrottle: "this model hit the per-model frequency limit; wait for the window reset or use another model",
	CodeSystemFirst:   "workbuddy requires messages[0] to be a system message",
	CodeSessionDead:   "workbuddy session is dead (Offline user session not found); re-login the account",
}

// apiError renders an upstream failure in the form the shared error classifier
// understands (`status=`, `code=`), so account health and retry decisions stay
// accurate.
func apiError(status int, raw []byte) error {
	body := strings.TrimSpace(string(raw))
	code := 0
	msg := ""
	trimmed := strings.TrimSpace(body)
	if strings.HasPrefix(trimmed, "{") {
		var env envelope
		if err := json.Unmarshal([]byte(trimmed), &env); err == nil {
			code = env.Code
			msg = strings.TrimSpace(env.Msg)
		}
	}
	if code != 0 && msg == "" {
		msg = body
	}
	parts := []string{fmt.Sprintf("status=%d", status)}
	if code != 0 {
		parts = append(parts, fmt.Sprintf("code=%d", code))
	}
	if msg != "" {
		parts = append(parts, "message="+truncate(msg, 300))
	} else if body != "" {
		parts = append(parts, "message="+truncate(body, 300))
	}
	err := fmt.Errorf("workbuddy API error: %s", strings.Join(parts, ", "))
	if code != 0 {
		if hint := errorHints[code]; hint != "" {
			return fmt.Errorf("%w (%s)", err, hint)
		}
	}
	return err
}

// unwrapEnvelope validates the business envelope and returns data.
func unwrapEnvelope(status int, raw []byte) (json.RawMessage, error) {
	if status >= http.StatusBadRequest {
		return nil, apiError(status, raw)
	}
	var env envelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, fmt.Errorf("workbuddy response is not JSON: %s", truncate(string(raw), 200))
	}
	if env.Code != 0 {
		if env.Code == CodeLoginPending {
			// Login polling is expected to observe this while the browser step
			// is still running, so it must be distinguishable from a failure.
			return nil, fmt.Errorf("%w: %s", ErrAuthPending, truncate(env.Msg, 120))
		}
		return nil, apiError(status, raw)
	}
	return env.Data, nil
}

func truncate(value string, limit int) string {
	value = strings.TrimSpace(value)
	if len(value) <= limit {
		return value
	}
	return value[:limit] + "..."
}

// tokenUpdater owns access-token renewal for one account. Keycloak rotates the
// refresh token on every refresh, so the rotated pair is persisted back to the
// account record.
type tokenUpdater struct {
	baseURL      string
	httpClient   *http.Client
	accountStore AccountUpdater
	account      *store.Account

	mu    sync.Mutex
	token string
	until time.Time
}

func newTokenUpdater(baseURL string, httpClient *http.Client, accountStore AccountUpdater, acc *store.Account) *tokenUpdater {
	return &tokenUpdater{
		baseURL:      strings.TrimSuffix(baseURL, "/"),
		httpClient:   httpClient,
		accountStore: accountStore,
		account:      acc,
	}
}

// Token returns a valid access token, refreshing only when needed.
func (t *tokenUpdater) Token(ctx context.Context, creds Credentials) (string, error) {
	now := time.Now()

	t.mu.Lock()
	defer t.mu.Unlock()

	if t.token != "" && t.until.Sub(now) > minRefreshLead {
		return t.token, nil
	}

	if token, ok := creds.Token(now); ok {
		t.token = token
		t.until = creds.ExpiresAt
		return token, nil
	}

	if strings.TrimSpace(creds.RefreshToken) == "" {
		if strings.TrimSpace(creds.AccessToken) != "" {
			// Only an access token is available and it is close to expiry.
			// Try it anyway rather than failing the request outright.
			t.token = creds.AccessToken
			return creds.AccessToken, nil
		}
		return "", fmt.Errorf("workbuddy account is missing credentials: paste the refresh token from the WorkBuddy desktop session")
	}

	refreshed, err := t.refresh(ctx, creds)
	if err != nil {
		if strings.TrimSpace(creds.AccessToken) != "" {
			t.token = creds.AccessToken
			return creds.AccessToken, nil
		}
		return "", err
	}
	t.token = refreshed.AccessToken
	t.until = refreshed.ExpiresAt
	t.persist(refreshed, creds)
	return refreshed.AccessToken, nil
}

// RefreshNow forces one refresh cycle, used by account verification.
func (t *tokenUpdater) RefreshNow(ctx context.Context, creds Credentials) (Credentials, error) {
	if strings.TrimSpace(creds.RefreshToken) == "" {
		return creds, fmt.Errorf("workbuddy account is missing a refresh token")
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	refreshed, err := t.refresh(ctx, creds)
	if err != nil {
		return creds, err
	}
	t.token = refreshed.AccessToken
	t.until = refreshed.ExpiresAt
	t.persist(refreshed, creds)
	return refreshed, nil
}

func (t *tokenUpdater) refresh(ctx context.Context, creds Credentials) (Credentials, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, t.baseURL+"/v2/plugin/auth/token/refresh", nil)
	if err != nil {
		return creds, err
	}
	applyHeaders(req, creds.AccessToken, creds.UID, "application/json")
	req.Header.Set("X-Refresh-Token", creds.RefreshToken)
	req.Header.Set("X-Auth-Refresh-Source", "plugin")

	resp, err := t.httpClient.Do(req)
	if err != nil {
		return creds, fmt.Errorf("failed to refresh workbuddy token: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	data, err := unwrapEnvelope(resp.StatusCode, raw)
	if err != nil {
		return creds, fmt.Errorf("failed to refresh workbuddy token: %w", err)
	}

	var payload struct {
		AccessToken  string `json:"accessToken"`
		RefreshToken string `json:"refreshToken"`
		ExpiresIn    int64  `json:"expiresIn"`
		Domain       string `json:"domain"`
	}
	if err := json.Unmarshal(data, &payload); err != nil {
		return creds, fmt.Errorf("failed to decode workbuddy refresh response: %w", err)
	}
	if strings.TrimSpace(payload.AccessToken) == "" {
		return creds, fmt.Errorf("workbuddy token refresh returned no accessToken; re-login required")
	}

	out := creds
	out.AccessToken = strings.TrimSpace(payload.AccessToken)
	if token := strings.TrimSpace(payload.RefreshToken); token != "" {
		// Keycloak rotates the refresh token; keeping the old one breaks the
		// next refresh.
		out.RefreshToken = token
	}
	if payload.ExpiresIn > 0 {
		out.ExpiresAt = time.Now().Add(time.Duration(payload.ExpiresIn) * time.Second)
	} else {
		out.ExpiresAt = time.Time{}
	}
	if claims := DecodeClaims(out.AccessToken); claims.ExpiresAt > 0 {
		out.ExpiresAt = time.Unix(claims.ExpiresAt, 0)
		if out.UID == "" {
			out.UID = claims.Sub
		}
		if out.Email == "" {
			out.Email = claims.Email
		}
	}
	return out, nil
}

func (t *tokenUpdater) persist(creds, previous Credentials) {
	if t.accountStore == nil || t.account == nil || t.account.ID == 0 {
		return
	}
	if strings.TrimSpace(creds.AccessToken) == previous.AccessToken &&
		strings.TrimSpace(creds.RefreshToken) == previous.RefreshToken {
		return
	}
	acc := *t.account
	acc.WorkBuddyAccessToken = creds.AccessToken
	if strings.TrimSpace(creds.RefreshToken) != "" {
		acc.WorkBuddyRefreshToken = creds.RefreshToken
		acc.ClientCookie = creds.RefreshToken
	}
	if !creds.ExpiresAt.IsZero() {
		acc.WorkBuddyExpiresAt = creds.ExpiresAt
	}
	if creds.UID != "" {
		acc.WorkBuddyUID = creds.UID
	}
	if creds.Email != "" && strings.TrimSpace(acc.Email) == "" {
		acc.Email = creds.Email
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := t.accountStore.UpdateAccount(ctx, &acc); err != nil {
		return
	}
	// Keep the in-memory snapshot coherent so the next request on this client
	// does not replay the pre-rotation refresh token.
	t.account.WorkBuddyAccessToken = acc.WorkBuddyAccessToken
	t.account.WorkBuddyRefreshToken = acc.WorkBuddyRefreshToken
	t.account.WorkBuddyExpiresAt = acc.WorkBuddyExpiresAt
	if acc.ClientCookie != "" {
		t.account.ClientCookie = acc.ClientCookie
	}
}

// WorkBuddyModel is one entry of the /v3/config catalog.
type WorkBuddyModel struct {
	ID              string `json:"id"`
	Name            string `json:"name"`
	MaxInputTokens  int64  `json:"maxInputTokens"`
	MaxOutputTokens int64  `json:"maxOutputTokens"`
	SupportsTools   bool   `json:"supportsToolCall"`
	SupportsReason  bool   `json:"supportsReasoning"`
	OnlyReasoning   bool   `json:"onlyReasoning"`
	Disabled        bool   `json:"disabled"`
	Reasoning       struct {
		Effort           string   `json:"effort"`
		SupportedEfforts []string `json:"supportedEfforts"`
	} `json:"reasoning"`
}

type configResponse struct {
	Models []WorkBuddyModel `json:"models"`
	Agents []struct {
		Name   string   `json:"name"`
		Models []string `json:"models"`
	} `json:"agents"`
}

// FetchModels reads the account-scoped model catalog. The effective set is the
// `cli` agent whitelist intersected with the model records.
func (c *Client) FetchModels(ctx context.Context) ([]WorkBuddyModel, error) {
	if c == nil {
		return nil, fmt.Errorf("workbuddy client is nil")
	}
	accessToken, err := c.ensureAccessToken(ctx)
	if err != nil {
		return nil, err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/v3/config", nil)
	if err != nil {
		return nil, err
	}
	applyHeaders(req, accessToken, c.creds.UID, "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to fetch workbuddy config: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	data, err := unwrapEnvelope(resp.StatusCode, raw)
	if err != nil {
		return nil, err
	}

	var cfg configResponse
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("failed to decode workbuddy config: %w", err)
	}

	allow := make(map[string]struct{}, len(cfg.Models))
	for _, agent := range cfg.Agents {
		if !strings.EqualFold(strings.TrimSpace(agent.Name), "cli") {
			continue
		}
		for _, id := range agent.Models {
			allow[strings.TrimSpace(id)] = struct{}{}
		}
	}

	out := make([]WorkBuddyModel, 0, len(cfg.Models))
	for _, model := range cfg.Models {
		id := strings.TrimSpace(model.ID)
		if id == "" || model.Disabled {
			continue
		}
		if len(allow) > 0 {
			if _, ok := allow[id]; !ok {
				continue
			}
		}
		model.ID = id
		if strings.TrimSpace(model.Name) == "" {
			model.Name = id
		}
		out = append(out, model)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("workbuddy config returned no cli models")
	}
	return out, nil
}
