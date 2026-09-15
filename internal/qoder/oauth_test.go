package qoder

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"orchids-api/internal/config"
	"orchids-api/internal/store"
)

// newLoginClient builds an account-less client pointed at stub endpoints. It is
// the shape the admin login flow uses.
func newLoginClient(t *testing.T, oauth, openAPI, inference string) *Client {
	t.Helper()
	client := NewFromAccount(nil, nil)
	setTestEndpoints(client, oauth, openAPI, inference)
	return client
}

// TestStartLoginBuildsOfficialURL pins the authorization URL: the official host,
// the S256 challenge, the nonce, the machine id, the client id, and no redirect
// URI or scope that a device flow does not use.
func TestStartLoginBuildsOfficialURL(t *testing.T) {
	t.Parallel()

	page := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodHead {
			t.Errorf("probe method = %s, want HEAD", r.Method)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer page.Close()

	client := newLoginClient(t, page.URL, page.URL, page.URL)
	setTestEntropy(client, strings.NewReader(strings.Repeat("\x00", 512)))

	tx, err := client.StartLogin(context.Background())
	if err != nil {
		t.Fatalf("StartLogin() error = %v", err)
	}
	if !strings.HasPrefix(tx.VerifyURL, page.URL+"/device/selectAccounts?") {
		t.Fatalf("VerifyURL = %q, want the official authorization path", tx.VerifyURL)
	}
	for _, want := range []string{"challenge=", "challenge_method=S256", "nonce=", "machine_id=", "client_id=" + DefaultClientID} {
		if !strings.Contains(tx.VerifyURL, want) {
			t.Errorf("VerifyURL = %q, want it to contain %q", tx.VerifyURL, want)
		}
	}
	for _, unwanted := range []string{"redirect_uri", "scope=", "response_type"} {
		if strings.Contains(tx.VerifyURL, unwanted) {
			t.Errorf("VerifyURL = %q, want it not to contain %q", tx.VerifyURL, unwanted)
		}
	}
	if len(tx.MachineID) != 36 {
		t.Errorf("MachineID = %q, want a 36-character UUID", tx.MachineID)
	}
	if len(tx.Verifier) < 43 || len(tx.Verifier) > 128 {
		t.Errorf("Verifier length = %d, want 43..128", len(tx.Verifier))
	}
	if !tx.ExpiresAt.After(time.Now()) {
		t.Error("ExpiresAt is not in the future")
	}
}

// TestAuthorizationHostAllowlist pins the login redirect boundary: only the
// Qoder hosts, the deployment's configured endpoint and loopback may be handed
// to a browser. A third-party host here would be a credential-phishing path, so
// the allowlist is asserted directly.
func TestAuthorizationHostAllowlist(t *testing.T) {
	t.Parallel()

	client := NewFromAccount(nil, nil)
	for _, host := range []string{"qoder.com", "www.qoder.com", "openapi.qoder.sh", "localhost", "127.0.0.1", "::1", "qoder.com"} {
		if !client.allowedLoginHost(host) {
			t.Errorf("allowedLoginHost(%q) = false, want true", host)
		}
	}
	for _, host := range []string{"evil.example.com", "qoder.com.evil.example", "", "openapi.qoder.sh.evil"} {
		if client.allowedLoginHost(host) {
			t.Errorf("allowedLoginHost(%q) = true, want false", host)
		}
	}
}

// TestConfiguredOAuthHostIsAllowedButForeignHostsAreNot proves the deployment's
// own endpoint is honoured while an unrelated host stays refused: the override
// is a configuration seam, not a wildcard.
func TestConfiguredOAuthHostIsAllowedButForeignHostsAreNot(t *testing.T) {
	t.Parallel()

	client := NewFromAccount(nil, &config.Config{QoderOAuthBaseURL: "https://auth.example.cn"})
	if !client.allowedLoginHost("auth.example.cn") {
		t.Fatal("the configured OAuth host was refused")
	}
	if client.allowedLoginHost("attacker.example") {
		t.Fatal("an unrelated host was accepted because an override exists")
	}
	// A suffix match is not a match.
	if client.allowedLoginHost("auth.example.cn.evil") {
		t.Fatal("a look-alike host was accepted")
	}
}

// TestStartLoginClassifiesUnreachable proves a blocked egress path is reported
// as unreachable rather than as a rejected transaction, because the two need
// different operator action.
func TestStartLoginClassifiesUnreachable(t *testing.T) {
	t.Parallel()

	dead := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	base := dead.URL
	dead.Close()

	client := newLoginClient(t, base, base, base)
	setTestEntropy(client, strings.NewReader(strings.Repeat("\x02", 512)))

	_, err := client.StartLogin(context.Background())
	if !errors.Is(err, ErrAuthUnavailable) {
		t.Fatalf("error = %v, want ErrAuthUnavailable", err)
	}
}

// TestPollLoginTreats404AsPending pins the pending signal: the device token
// endpoint answers 404 until the browser step completes, and treating that as a
// hard failure would abort every login that takes more than one poll.
func TestPollLoginTreats404AsPending(t *testing.T) {
	t.Parallel()

	var calls int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.URL.Path != "/api/v1/deviceToken/poll" {
			t.Errorf("path = %q, want /api/v1/deviceToken/poll", r.URL.Path)
		}
		if got := r.Header.Get("Accept"); got != "application/json" {
			t.Errorf("Accept = %q, want application/json", got)
		}
		if calls < 2 {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"token":"access-1","refresh_token":"refresh-1","expires_in":3600,"refresh_token_expires_in":864000,"user_id":"uid-1","user_name":"tester"}`))
	}))
	defer server.Close()

	client := newLoginClient(t, server.URL, server.URL, server.URL)
	tx := &LoginTransaction{Nonce: "n", Verifier: "v", MachineID: "m", ExpiresAt: time.Now().Add(time.Minute)}

	if _, err := client.PollLogin(context.Background(), tx); !errors.Is(err, ErrAuthPending) {
		t.Fatalf("first poll error = %v, want ErrAuthPending", err)
	}
	creds, err := client.PollLogin(context.Background(), tx)
	if err != nil {
		t.Fatalf("second poll error = %v", err)
	}
	if creds.AccessToken != "access-1" || creds.RefreshToken != "refresh-1" {
		t.Fatalf("credentials = %+v, want the returned token pair", creds)
	}
	if creds.UID != "uid-1" || creds.Name != "tester" {
		t.Fatalf("identity = %q/%q, want uid-1/tester", creds.UID, creds.Name)
	}
	if creds.AccessExpiresAt.IsZero() || creds.RefreshExpiresAt.IsZero() {
		t.Fatalf("expiries = %v/%v, want both resolved from expires_in", creds.AccessExpiresAt, creds.RefreshExpiresAt)
	}
}

// TestPollLoginRejectsOtherStatuses proves a non-200, non-404 answer is a
// rejection rather than a silent retry loop.
func TestPollLoginRejectsOtherStatuses(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"message":"bad nonce"}`))
	}))
	defer server.Close()

	client := newLoginClient(t, server.URL, server.URL, server.URL)
	tx := &LoginTransaction{Nonce: "n", Verifier: "v"}
	_, err := client.PollLogin(context.Background(), tx)
	if !errors.Is(err, ErrAuthRejected) {
		t.Fatalf("error = %v, want ErrAuthRejected", err)
	}
}

// TestPollLoginRejectsTokenlessSuccess proves a 200 without a token is not
// mistaken for a successful login.
func TestPollLoginRejectsTokenlessSuccess(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"user_id":"uid-1"}`))
	}))
	defer server.Close()

	client := newLoginClient(t, server.URL, server.URL, server.URL)
	_, err := client.PollLogin(context.Background(), &LoginTransaction{Nonce: "n", Verifier: "v"})
	if !errors.Is(err, ErrAuthRejected) {
		t.Fatalf("error = %v, want ErrAuthRejected", err)
	}
}

// TestRefreshAcceptsBothTokenSpellings pins the field-name difference between
// the poll and refresh endpoints, and the refresh-token rotation.
func TestRefreshAcceptsBothTokenSpellings(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/api/v1/deviceToken/refresh" {
			t.Errorf("request = %s %s, want POST /api/v1/deviceToken/refresh", r.Method, r.URL.Path)
		}
		body, _ := io.ReadAll(r.Body)
		if !strings.Contains(string(body), `"refresh_token":"refresh-1"`) {
			t.Errorf("body = %s, want the refresh token", body)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"device_token":"access-2","refresh_token":"refresh-2","expires_in":1800}`))
	}))
	defer server.Close()

	client := newLoginClient(t, server.URL, server.URL, server.URL)
	creds, err := client.Refresh(context.Background(), "refresh-1")
	if err != nil {
		t.Fatalf("Refresh() error = %v", err)
	}
	if creds.AccessToken != "access-2" {
		t.Fatalf("AccessToken = %q, want access-2 (from device_token)", creds.AccessToken)
	}
	if creds.RefreshToken != "refresh-2" {
		t.Fatalf("RefreshToken = %q, want refresh-2 (rotation)", creds.RefreshToken)
	}
}

// TestRefreshClassifiesReLoginRequired proves a refused refresh grant is
// reported as requiring a new login rather than as a transient failure the
// scheduler would retry forever.
func TestRefreshClassifiesReLoginRequired(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"message":"refresh token expired"}`))
	}))
	defer server.Close()

	client := newLoginClient(t, server.URL, server.URL, server.URL)
	_, err := client.Refresh(context.Background(), "refresh-1")
	if !errors.Is(err, ErrReLoginRequired) {
		t.Fatalf("error = %v, want ErrReLoginRequired", err)
	}
}

// TestRefreshWithoutTokenIsMissingCredential pins the refusal to call the
// endpoint with nothing to present.
func TestRefreshWithoutTokenIsMissingCredential(t *testing.T) {
	t.Parallel()

	client := newLoginClient(t, "http://127.0.0.1:1", "http://127.0.0.1:1", "http://127.0.0.1:1")
	if _, err := client.Refresh(context.Background(), "  "); !errors.Is(err, ErrCredentialMissing) {
		t.Fatalf("error = %v, want ErrCredentialMissing", err)
	}
}

// TestFetchProfileToleratesFailure proves the enrichment is best effort.
func TestFetchProfileToleratesFailure(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer access-1" {
			t.Errorf("Authorization = %q, want Bearer access-1", got)
		}
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()

	client := newLoginClient(t, server.URL, server.URL, server.URL)
	if _, err := client.FetchProfile(context.Background(), "access-1"); err == nil {
		t.Fatal("FetchProfile() error = nil for a 500 response")
	}
}

// TestResolveCredentialsPrefersDedicatedFields pins the field mapping, including
// the refusal to treat a generic slot holding an unrelated value as a Qoder
// credential.
func TestResolveCredentialsPrefersDedicatedFields(t *testing.T) {
	t.Parallel()

	acc := &store.Account{
		AccountType:       "qoder",
		QoderAccessToken:  "access",
		QoderRefreshToken: "refresh",
		QoderUserID:       "uid",
		QoderExpiresAt:    time.Unix(1700000000, 0),
		Token:             "some-other-channel-token",
	}
	creds := ResolveCredentials(acc)
	if creds.AccessToken != "access" || creds.RefreshToken != "refresh" || creds.UID != "uid" {
		t.Fatalf("credentials = %+v, want the dedicated fields", creds)
	}

	// With the dedicated fields empty, an unrelated opaque value must not be
	// adopted.
	other := &store.Account{AccountType: "qoder", Token: "opaque-value"}
	if got := ResolveCredentials(other); got.HasCredential() {
		t.Fatalf("credentials = %+v, want nothing resolved from an opaque generic slot", got)
	}
}

// TestParseCredentialDocumentRoundTrips proves an imported CLI credential is
// understood, and that a document without a token is refused.
func TestParseCredentialDocumentRoundTrips(t *testing.T) {
	t.Parallel()

	raw := `{"uid":"u1","name":"n1","email":"e@example.com","organization_id":"org1","organization_tags":["a","b"],"security_oauth_token":"sot","refresh_token":"rt","expire_time":1700000000,"refresh_token_expire_time":1700086400}`
	creds, ok := ParseCredentialDocument(raw)
	if !ok {
		t.Fatal("ParseCredentialDocument() = false, want true")
	}
	if creds.AccessToken != "sot" || creds.RefreshToken != "rt" || creds.UID != "u1" || creds.OrgID != "org1" {
		t.Fatalf("credentials = %+v", creds)
	}
	if len(creds.OrgTags) != 2 {
		t.Fatalf("OrgTags = %v, want two entries", creds.OrgTags)
	}
	if creds.AccessExpiresAt.Unix() != 1700000000 {
		t.Fatalf("AccessExpiresAt = %v, want 1700000000", creds.AccessExpiresAt)
	}

	if _, ok := ParseCredentialDocument(`{"uid":"u1"}`); ok {
		t.Fatal("ParseCredentialDocument() = true for a document without a token")
	}
	if _, ok := ParseCredentialDocument("not json"); ok {
		t.Fatal("ParseCredentialDocument() = true for a non-JSON value")
	}
}

// TestUnixSecondsNormalizesMilliseconds proves a millisecond timestamp does not
// land the expiry tens of thousands of years in the future, which would disable
// refresh silently.
func TestUnixSecondsNormalizesMilliseconds(t *testing.T) {
	t.Parallel()

	if got := unixSeconds(1700000000000).Unix(); got != 1700000000 {
		t.Fatalf("unixSeconds(millis) = %d, want 1700000000", got)
	}
	if got := unixSeconds(1700000000).Unix(); got != 1700000000 {
		t.Fatalf("unixSeconds(seconds) = %d, want 1700000000", got)
	}
}

// TestParseExpiryAcceptsBothForms proves the relative and absolute spellings are
// both understood; reading a lifetime as an absolute instant would corrupt the
// freshness decision.
func TestParseExpiryAcceptsBothForms(t *testing.T) {
	t.Parallel()

	now := time.Unix(1700000000, 0)
	if got := parseExpiry("", 3600, now); !got.Equal(now.Add(time.Hour)) {
		t.Fatalf("parseExpiry(relative) = %v, want %v", got, now.Add(time.Hour))
	}
	if got := parseExpiry("2023-11-14T22:13:20Z", 0, now); got.Unix() != 1700000000 {
		t.Fatalf("parseExpiry(absolute) = %v, want 1700000000", got.Unix())
	}
	if got := parseExpiry("", 0, now); !got.IsZero() {
		t.Fatalf("parseExpiry(absent) = %v, want zero", got)
	}
}

// TestProbeReachabilityDetectsBlockedEgress covers the startup diagnostic.
func TestProbeReachabilityDetectsBlockedEgress(t *testing.T) {
	t.Parallel()

	dead := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	base := dead.URL
	dead.Close()

	client := newLoginClient(t, base, base, base)
	if err := client.ProbeReachability(context.Background()); !errors.Is(err, ErrAuthUnavailable) {
		t.Fatalf("error = %v, want ErrAuthUnavailable", err)
	}

	live := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer live.Close()
	client = newLoginClient(t, live.URL, live.URL, live.URL)
	if err := client.ProbeReachability(context.Background()); err != nil {
		t.Fatalf("ProbeReachability() error = %v, want success", err)
	}
}

// TestEnsureAccessTokenSkipsRefreshWhileValid proves a fresh token is reused
// instead of burning a rotation on every request.
func TestEnsureAccessTokenSkipsRefreshWhileValid(t *testing.T) {
	t.Parallel()

	var refreshes int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "deviceToken/refresh") {
			refreshes++
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"token":"access-new","refresh_token":"refresh-new"}`))
	}))
	defer server.Close()

	acc := &store.Account{
		AccountType:       "qoder",
		QoderAccessToken:  "access-fresh",
		QoderRefreshToken: "refresh-1",
		QoderExpiresAt:    time.Now().Add(6 * time.Hour),
	}
	client := NewFromAccount(acc, nil)
	setTestEndpoints(client, server.URL, server.URL, server.URL)

	creds, err := client.ensureAccessToken(context.Background())
	if err != nil {
		t.Fatalf("ensureAccessToken() error = %v", err)
	}
	if creds.AccessToken != "access-fresh" {
		t.Fatalf("AccessToken = %q, want the stored token", creds.AccessToken)
	}
	if refreshes != 0 {
		t.Fatalf("refresh calls = %d, want 0 for a valid token", refreshes)
	}
}
