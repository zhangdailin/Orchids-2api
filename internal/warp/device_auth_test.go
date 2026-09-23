package warp

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func TestDeviceAuthenticatorStartAndExchange(t *testing.T) {
	var deviceCodeSeen string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		form, _ := url.ParseQuery(string(body))
		switch r.URL.Path {
		case "/device":
			if got := form.Get("client_id"); got != warpAgentCLIClientID {
				t.Errorf("client_id=%q want %q", got, warpAgentCLIClientID)
			}
			_, _ = w.Write([]byte(`{"device_code":"device-secret","user_code":"ABCD-1234","verification_uri":"https://app.warp.dev/device","verification_uri_complete":"https://app.warp.dev/device?user_code=ABCD-1234","expires_in":599,"interval":2}`))
		case "/token":
			deviceCodeSeen = form.Get("device_code")
			if got := form.Get("grant_type"); got != "urn:ietf:params:oauth:grant-type:device_code" {
				t.Errorf("grant_type=%q", got)
			}
			_, _ = w.Write([]byte(`{"access_token":"custom-token"}`))
		case "/firebase":
			if !strings.Contains(string(body), `"returnSecureToken":true`) || !strings.Contains(string(body), `"token":"custom-token"`) {
				t.Errorf("unexpected custom-token body %s", body)
			}
			_, _ = w.Write([]byte(`{"idToken":"short-lived-id-token","refreshToken":"refresh-secret"}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	authenticator := &DeviceAuthenticator{
		httpClient:     server.Client(),
		deviceURL:      server.URL + "/device",
		tokenURL:       server.URL + "/token",
		customTokenURL: server.URL + "/firebase",
	}
	details, err := authenticator.Start(context.Background())
	if err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	if details.DeviceCode != "device-secret" || details.UserCode != "ABCD-1234" {
		t.Fatalf("Start() = %#v", details)
	}
	refreshToken, err := authenticator.Exchange(context.Background(), details.DeviceCode)
	if err != nil {
		t.Fatalf("Exchange() error = %v", err)
	}
	if refreshToken != "refresh-secret" {
		t.Fatalf("refresh token=%q", refreshToken)
	}
	if deviceCodeSeen != "device-secret" {
		t.Fatalf("device code=%q", deviceCodeSeen)
	}
}

func TestDeviceAuthenticatorExchangePending(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":"authorization_pending"}`))
	}))
	defer server.Close()

	authenticator := &DeviceAuthenticator{httpClient: server.Client(), tokenURL: server.URL}
	_, err := authenticator.Exchange(context.Background(), "device-code")
	if err == nil || !IsDeviceAuthorizationPending(err) {
		t.Fatalf("Exchange() error=%v, want authorization pending", err)
	}
}
