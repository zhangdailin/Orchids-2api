package warp

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/goccy/go-json"

	"orchids-api/internal/config"
)

// warpAgentCLIClientID is the public client identifier used by Warp's open
// source Agent CLI device-authorization implementation.
const warpAgentCLIClientID = "warp-agent-cli"

var (
	warpDeviceAuthorizationURL = warpAPIBaseURL + "/api/v1/oauth/device/auth"
	warpDeviceTokenURL         = warpAPIBaseURL + "/api/v1/oauth/token"
)

// DeviceAuthorization is a short-lived Warp device login. DeviceCode is
// intentionally excluded from JSON and must never be sent to the browser.
type DeviceAuthorization struct {
	DeviceCode              string `json:"-"`
	UserCode                string `json:"user_code"`
	VerificationURI         string `json:"verification_uri"`
	VerificationURIComplete string `json:"verification_uri_complete,omitempty"`
	ExpiresIn               int    `json:"expires_in"`
	Interval                int    `json:"interval"`
}

// DeviceAuthorizationPendingError indicates that the account owner has not
// yet finished the official Warp browser approval.
type DeviceAuthorizationPendingError struct{}

func (DeviceAuthorizationPendingError) Error() string { return "warp device authorization pending" }

func IsDeviceAuthorizationPending(err error) bool {
	_, ok := err.(DeviceAuthorizationPendingError)
	return ok
}

// DeviceAuthenticator implements the same device authorization exchange as
// Warp's official Agent CLI. It never reads local Warp files or accepts a
// password.
type DeviceAuthenticator struct {
	httpClient         *http.Client
	deviceURL          string
	tokenURL           string
	customTokenURL     string
	configurationError error
}

func NewDeviceAuthenticator(cfg *config.Config) *DeviceAuthenticator {
	customTokenURL, configurationError := warpFirebaseCustomTokenURL()
	return &DeviceAuthenticator{
		httpClient:         newHTTPClient(20*time.Second, cfg),
		deviceURL:          warpDeviceAuthorizationURL,
		tokenURL:           warpDeviceTokenURL,
		customTokenURL:     customTokenURL,
		configurationError: configurationError,
	}
}

func (a *DeviceAuthenticator) Start(ctx context.Context) (*DeviceAuthorization, error) {
	if a != nil && a.configurationError != nil {
		return nil, a.configurationError
	}
	var upstreamResponse struct {
		DeviceCode              string `json:"device_code"`
		UserCode                string `json:"user_code"`
		VerificationURI         string `json:"verification_uri"`
		VerificationURIComplete string `json:"verification_uri_complete"`
		ExpiresIn               int    `json:"expires_in"`
		Interval                int    `json:"interval"`
	}
	if err := a.postForm(ctx, a.deviceURL, url.Values{"client_id": {warpAgentCLIClientID}}, &upstreamResponse); err != nil {
		return nil, fmt.Errorf("request warp device code: %w", err)
	}
	response := DeviceAuthorization{
		DeviceCode:              upstreamResponse.DeviceCode,
		UserCode:                upstreamResponse.UserCode,
		VerificationURI:         upstreamResponse.VerificationURI,
		VerificationURIComplete: upstreamResponse.VerificationURIComplete,
		ExpiresIn:               upstreamResponse.ExpiresIn,
		Interval:                upstreamResponse.Interval,
	}
	if strings.TrimSpace(response.DeviceCode) == "" || strings.TrimSpace(response.UserCode) == "" || strings.TrimSpace(response.VerificationURI) == "" {
		return nil, fmt.Errorf("warp device code response is incomplete")
	}
	if response.Interval < 1 {
		response.Interval = 2
	}
	if response.ExpiresIn < 1 {
		response.ExpiresIn = 600
	}
	return &response, nil
}

// Exchange polls the official Warp OAuth endpoint. On success it returns only
// the Firebase refresh token, which is the sole credential persisted by this
// project.
func (a *DeviceAuthenticator) Exchange(ctx context.Context, deviceCode string) (string, error) {
	if a != nil && a.configurationError != nil {
		return "", a.configurationError
	}
	deviceCode = strings.TrimSpace(deviceCode)
	if deviceCode == "" {
		return "", fmt.Errorf("missing warp device code")
	}

	var tokenResponse struct {
		AccessToken string `json:"access_token"`
	}
	form := url.Values{
		"client_id":   {warpAgentCLIClientID},
		"device_code": {deviceCode},
		"grant_type":  {"urn:ietf:params:oauth:grant-type:device_code"},
	}
	if err := a.postForm(ctx, a.tokenURL, form, &tokenResponse); err != nil {
		return "", err
	}
	if strings.TrimSpace(tokenResponse.AccessToken) == "" {
		return "", fmt.Errorf("warp device token response is missing access_token")
	}

	var firebaseResponse struct {
		RefreshToken string `json:"refreshToken"`
	}
	customBody, err := json.Marshal(map[string]interface{}{
		"returnSecureToken": true,
		"token":             tokenResponse.AccessToken,
	})
	if err != nil {
		return "", fmt.Errorf("encode warp custom-token exchange: %w", err)
	}
	if err := a.postJSON(ctx, a.customTokenURL, customBody, &firebaseResponse); err != nil {
		return "", fmt.Errorf("exchange warp device token: %w", err)
	}
	refreshToken := strings.TrimSpace(firebaseResponse.RefreshToken)
	if refreshToken == "" {
		return "", fmt.Errorf("warp custom-token exchange is missing refreshToken")
	}
	return refreshToken, nil
}

func (a *DeviceAuthenticator) postForm(ctx context.Context, endpoint string, form url.Values, target interface{}) error {
	return a.doJSON(ctx, http.MethodPost, endpoint, "application/x-www-form-urlencoded", []byte(form.Encode()), target)
}

func (a *DeviceAuthenticator) postJSON(ctx context.Context, endpoint string, body []byte, target interface{}) error {
	return a.doJSON(ctx, http.MethodPost, endpoint, "application/json", body, target)
}

func (a *DeviceAuthenticator) doJSON(ctx context.Context, method, endpoint, contentType string, body []byte, target interface{}) error {
	if a == nil || a.httpClient == nil {
		return fmt.Errorf("warp device authenticator is not configured")
	}
	req, err := http.NewRequestWithContext(ctx, method, endpoint, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", contentType)
	resp, err := a.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		var upstreamError struct {
			Error string `json:"error"`
		}
		_ = json.Unmarshal(raw, &upstreamError)
		if strings.EqualFold(strings.TrimSpace(upstreamError.Error), "authorization_pending") {
			return DeviceAuthorizationPendingError{}
		}
		return fmt.Errorf("warp device authorization returned HTTP %d", resp.StatusCode)
	}
	if err := json.Unmarshal(raw, target); err != nil {
		return fmt.Errorf("decode warp device authorization response: %w", err)
	}
	return nil
}
