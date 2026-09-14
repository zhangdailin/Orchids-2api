package grok

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"orchids-api/internal/config"
	"orchids-api/internal/store"
)

var cliOAuthAccountLocks sync.Map // map[string]*sync.Mutex

// CLIOAuth handles the Build CLI OAuth token lifecycle: return the current
// access_token when unexpired, otherwise refresh via the refresh_token grant
// and persist the rotated pair back to Redis.

const (
	cliOAuthRefreshSkew  = 5 * time.Minute
	cliOAuthMaxBodyBytes = 1 << 20
)

// CLIOAuth refreshes Grok Build OAuth tokens.
type CLIOAuth struct {
	cfg        *config.Config
	httpClient *http.Client
	store      *store.Store
}

// NewCLIOAuth builds an OAuth helper for the CLI upstream.
func NewCLIOAuth(cfg *config.Config, httpClient *http.Client) *CLIOAuth {
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	return &CLIOAuth{cfg: cfg, httpClient: httpClient}
}

// SetAccountStore enables durable persistence of rotated OAuth tokens.
func (o *CLIOAuth) SetAccountStore(s *store.Store) {
	if o == nil {
		return
	}
	o.store = s
}

// AccessToken returns a valid access token for the account. It refreshes and
// persists when the stored token is missing or within skew of expiry.
func (o *CLIOAuth) AccessToken(ctx context.Context, acc *store.Account) (string, error) {
	if acc == nil {
		return "", fmt.Errorf("empty cli oauth account")
	}
	lock := cliOAuthLockForAccount(acc)
	lock.Lock()
	defer lock.Unlock()
	if o != nil && o.store != nil && acc.ID != 0 {
		if latest, err := o.store.GetAccount(ctx, acc.ID); err == nil && latest != nil {
			acc.OAuthAccessToken = latest.OAuthAccessToken
			acc.OAuthRefreshToken = latest.OAuthRefreshToken
			acc.OAuthExpiresAt = latest.OAuthExpiresAt
		}
	}
	accessToken := strings.TrimSpace(acc.OAuthAccessToken)
	refreshToken := strings.TrimSpace(acc.OAuthRefreshToken)
	expiresAt := acc.OAuthExpiresAt

	if accessToken != "" && (expiresAt.IsZero() || time.Until(expiresAt) > cliOAuthRefreshSkew) {
		return accessToken, nil
	}
	if refreshToken == "" {
		return "", &cliOAuthError{status: http.StatusUnauthorized, message: "grok cli oauth refresh token is missing"}
	}
	return o.refreshAndPersist(ctx, acc, refreshToken)
}

// ForceRefresh rotates an access token after the upstream explicitly rejects
// an otherwise unexpired token. Stateful Responses must retry the same account
// rather than switching to a different account and losing conversation state.
func (o *CLIOAuth) ForceRefresh(ctx context.Context, acc *store.Account) (string, error) {
	if acc == nil {
		return "", fmt.Errorf("empty cli oauth account")
	}
	lock := cliOAuthLockForAccount(acc)
	lock.Lock()
	defer lock.Unlock()
	if o != nil && o.store != nil && acc.ID != 0 {
		if latest, err := o.store.GetAccount(ctx, acc.ID); err == nil && latest != nil {
			acc.OAuthAccessToken = latest.OAuthAccessToken
			acc.OAuthRefreshToken = latest.OAuthRefreshToken
			acc.OAuthExpiresAt = latest.OAuthExpiresAt
		}
	}
	refreshToken := strings.TrimSpace(acc.OAuthRefreshToken)
	if refreshToken == "" {
		return "", &cliOAuthError{status: http.StatusUnauthorized, message: "grok cli oauth refresh token is missing"}
	}
	return o.refreshAndPersist(ctx, acc, refreshToken)
}

func cliOAuthLockForAccount(acc *store.Account) *sync.Mutex {
	key := "unknown"
	if acc != nil && acc.ID != 0 {
		key = fmt.Sprintf("account:%d", acc.ID)
	} else if acc != nil {
		sum := sha256.Sum256([]byte(strings.TrimSpace(acc.OAuthRefreshToken)))
		key = fmt.Sprintf("refresh:%x", sum[:])
	}
	value, _ := cliOAuthAccountLocks.LoadOrStore(key, &sync.Mutex{})
	return value.(*sync.Mutex)
}

func (o *CLIOAuth) refreshAndPersist(ctx context.Context, acc *store.Account, refreshToken string) (string, error) {
	accessToken, newRefresh, identityToken, expiresAt, err := o.refresh(ctx, refreshToken)
	if err != nil {
		return "", err
	}
	// Persist the rotated tokens back to the account (refresh tokens can rotate).
	acc.OAuthAccessToken = accessToken
	acc.OAuthExpiresAt = expiresAt
	if newRefresh != "" && newRefresh != refreshToken {
		acc.OAuthRefreshToken = newRefresh
	}
	ApplyCLIOAuthIdentity(acc)
	ApplyCLIOAuthIdentityToken(acc, identityToken)
	if o != nil && o.store != nil && acc.ID != 0 {
		if updateErr := o.store.UpdateAccount(ctx, acc); updateErr != nil {
			// Keep serving with the in-memory tokens, but log so operators can
			// detect a durable store write failure after rotation.
			slog.Warn("grok cli oauth: failed to persist rotated tokens", "account_id", acc.ID, "error", updateErr)
		}
	}
	return accessToken, nil
}

// refresh performs the OAuth refresh_token grant against auth.x.ai.
func (o *CLIOAuth) refresh(ctx context.Context, refreshToken string) (accessToken, newRefresh, identityToken string, expiresAt time.Time, err error) {
	form := url.Values{}
	form.Set("grant_type", "refresh_token")
	form.Set("client_id", o.clientID())
	form.Set("refresh_token", refreshToken)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, o.tokenURL(), strings.NewReader(form.Encode()))
	if err != nil {
		return "", "", "", time.Time{}, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")

	resp, err := o.httpClient.Do(req)
	if err != nil {
		return "", "", "", time.Time{}, err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, cliOAuthMaxBodyBytes))
	if err != nil {
		return "", "", "", time.Time{}, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		oauthErr := parseCLIOAuthErrorResponse(body, resp.StatusCode)
		return "", "", "", time.Time{}, oauthErr
	}

	var value struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		IDToken      string `json:"id_token"`
		ExpiresIn    int    `json:"expires_in"`
	}
	if err := json.Unmarshal(body, &value); err != nil {
		return "", "", "", time.Time{}, fmt.Errorf("grok cli oauth refresh parse: %w", err)
	}
	if strings.TrimSpace(value.AccessToken) == "" {
		return "", "", "", time.Time{}, &cliOAuthError{status: resp.StatusCode, message: "grok cli oauth response missing access_token"}
	}
	expiresIn := value.ExpiresIn
	if expiresIn <= 0 {
		expiresIn = 3600
	}
	recordCLIOAuthRefresh()
	return strings.TrimSpace(value.AccessToken), strings.TrimSpace(value.RefreshToken), strings.TrimSpace(value.IDToken), time.Now().UTC().Add(time.Duration(expiresIn) * time.Second), nil
}

func (o *CLIOAuth) clientID() string {
	if o != nil && o.cfg != nil {
		return o.cfg.GrokCLIOAuthClientIDOrDefault()
	}
	return "b1a00492-073a-47ea-816f-4c329264a828"
}

func (o *CLIOAuth) tokenURL() string {
	if o != nil && o.cfg != nil {
		return o.cfg.GrokCLIOAuthTokenURLOrDefault()
	}
	return "https://auth.x.ai/oauth2/token"
}

// parseCLIOAuthErrorResponse maps an OAuth error body to a status-coded error.
func parseCLIOAuthErrorResponse(body []byte, status int) error {
	var payload struct {
		Error            string `json:"error"`
		ErrorDescription string `json:"error_description"`
	}
	code := fmt.Sprintf("oauth_http_%d", status)
	_ = json.Unmarshal(body, &payload)
	if payload.Error != "" {
		code = payload.Error
	}
	// OAuth diagnostics can echo bearer tokens, cookies, or JWTs. Keep the
	// stable error code only; callers may log this error.
	message := ""
	// Permanent credential failures surface as 401 so the account is cooled down.
	switch code {
	case "refresh_denied", "expired_token", "access_denied", "invalid_grant", "unauthorized_client":
		return &cliOAuthError{status: http.StatusUnauthorized, message: "grok cli oauth refresh denied: " + code + " " + message}
	default:
		return &cliOAuthError{status: status, message: "grok cli oauth refresh failed (" + code + "): " + message}
	}
}

// IsCLIPermanentOAuthError reports whether an OAuth failure is permanent
// (credentials invalid) vs transient.
func IsCLIPermanentOAuthError(err error) bool {
	var oauthErr *cliOAuthError
	if errors.As(err, &oauthErr) {
		return oauthErr.status == http.StatusUnauthorized
	}
	return false
}
