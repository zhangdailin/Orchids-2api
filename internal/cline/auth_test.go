package cline

import (
	"strings"
	"testing"
	"time"

	"orchids-api/internal/store"
)

// TestCredentialsBearerPinsTheWirePrefix pins the request credential. The
// upstream expects the literal `workos:` prefix; a client that sent the bare
// token would be rejected on every request.
func TestCredentialsBearerPinsTheWirePrefix(t *testing.T) {
	creds := Credentials{AccessToken: "abc"}
	if got := creds.Bearer(); got != "workos:abc" {
		t.Fatalf("Bearer() = %q, want workos:abc", got)
	}
	if !creds.HasCredential() {
		t.Error("HasCredential() = false, want true")
	}
}

// TestCredentialsAccessValidHonoursTheLead pins the renewal margin: a token is
// renewed before it actually expires, because one that expires mid-stream is
// rejected upstream.
func TestCredentialsAccessValidHonoursTheLead(t *testing.T) {
	now := time.Now()
	creds := Credentials{AccessToken: "abc", ExpiresAt: now.Add(RefreshLead + time.Minute)}
	if !creds.AccessValid(now) {
		t.Error("AccessValid() = false, want true while the lead is covered")
	}
	creds.ExpiresAt = now.Add(RefreshLead - time.Minute)
	if creds.AccessValid(now) {
		t.Error("AccessValid() = true, want false inside the lead")
	}
	if (Credentials{}).AccessValid(now) {
		t.Error("AccessValid() = true for an empty credential")
	}
}

// TestResolveCredentialsReadsTheChannelFields proves the channel reads only its
// own slots: a value in a generic slot is not this channel's credential.
func TestResolveCredentialsReadsTheChannelFields(t *testing.T) {
	expires := time.Now().Add(time.Hour)
	acc := &store.Account{
		ClineAccessToken:  " access ",
		ClineRefreshToken: " refresh ",
		ClineExpiresAt:    expires,
		ClineEmail:        " operator@example.com ",
		RefreshToken:      "generic",
	}
	creds := ResolveCredentials(acc)
	if creds.AccessToken != "access" || creds.RefreshToken != "refresh" {
		t.Fatalf("credentials = %+v, want the trimmed pair", creds)
	}
	if creds.Email != "operator@example.com" {
		t.Errorf("email = %q, want operator@example.com", creds.Email)
	}
	if !creds.ExpiresAt.Equal(expires) {
		t.Errorf("expires at = %v, want %v", creds.ExpiresAt, expires)
	}
	if ResolveCredentials(nil).HasCredential() {
		t.Error("a nil account must resolve to no credential")
	}
}

// TestParseExpiryAcceptsEveryUpstreamSpelling covers the three shapes the two
// endpoints use, and proves a value too small to be an epoch is ignored instead
// of rendering as 1970 (which would make the token look permanently expired).
func TestParseExpiryAcceptsEveryUpstreamSpelling(t *testing.T) {
	const want = int64(4102444800000)
	for _, value := range []interface{}{
		float64(want),
		int64(want),
		"4102444800000",
	} {
		parsed := ParseExpiry(value)
		if parsed.IsZero() {
			t.Fatalf("ParseExpiry(%#v) = zero, want a timestamp", value)
		}
		if parsed.UnixMilli() != want {
			t.Errorf("ParseExpiry(%#v) = %v, want %v", value, parsed.UnixMilli(), want)
		}
	}
	if got := ParseExpiry("12345"); !got.IsZero() {
		t.Errorf("ParseExpiry(12345) = %v, want zero", got)
	}
	if got := ParseExpiry(nil); !got.IsZero() {
		t.Errorf("ParseExpiry(nil) = %v, want zero", got)
	}
}

// TestParseInferenceCapDurationReadsTheStatedWait pins the one number the
// scheduler needs: the upstream writes the recovery window in prose, and a known
// window turns a blind retry into a cooldown.
func TestParseInferenceCapDurationReadsTheStatedWait(t *testing.T) {
	cases := []struct {
		body string
		want time.Duration
	}{
		{`{"error":"INFERENCE_CAP_ERROR","message":"Try again in 17h 59m"}`, 17*time.Hour + 59*time.Minute},
		{`{"error":"INFERENCE_CAP_ERROR","message":"Try again in 2h"}`, 2 * time.Hour},
		{`{"error":"INFERENCE_CAP_ERROR","message":"Try again in 59m"}`, 59 * time.Minute},
		{`{"error":"INFERENCE_CAP_ERROR","message":"Try again in 30s"}`, 30 * time.Second},
		{`{"error":"INFERENCE_CAP_ERROR","message":"Try again in 1d 2h"}`, 26 * time.Hour},
		{`{"error":"SOMETHING_ELSE"}`, 0},
	}
	for _, tc := range cases {
		if got := ParseInferenceCapDuration(tc.body); got != tc.want {
			t.Errorf("ParseInferenceCapDuration(%s) = %v, want %v", tc.body, got, tc.want)
		}
	}
}

// TestInferenceCapErrorNamesTheWait proves the classified error carries the
// window, so the operator-facing text says when to come back.
func TestInferenceCapErrorNamesTheWait(t *testing.T) {
	err := inferenceCapError(`{"error":"INFERENCE_CAP_ERROR","message":"Try again in 17h 59m"}`)
	if err.Wait != 17*time.Hour+59*time.Minute {
		t.Fatalf("wait = %v, want 17h59m", err.Wait)
	}
	if !strings.Contains(err.Error(), "cline inference cap reached") {
		t.Fatalf("error text = %q, want the classified phrase", err.Error())
	}
	if !strings.Contains(err.Error(), "17h 59m") {
		t.Errorf("error text = %q, want it to name the window", err.Error())
	}
}

// TestAllowedLoginHostRefusesForeignHosts is the one check standing between a
// login and a third party: the operator's password is typed on the page this
// decides about.
func TestAllowedLoginHostRefusesForeignHosts(t *testing.T) {
	allowed := []string{
		"api.workos.com", "workos.com", "dashboard.workos.com",
		// WorkOS serves the device page on the customer's AuthKit domain, which
		// is what the production tenant actually answers with.
		"authkit.cline.bot", "api.cline.bot",
		"localhost", "127.0.0.1", "::1",
	}
	for _, host := range allowed {
		if !allowedLoginHost(host, "") {
			t.Errorf("allowedLoginHost(%q) = false, want true", host)
		}
	}
	for _, host := range []string{"", "evil.example.com", "api.workos.com.evil.example.com", "workos.com.co"} {
		if allowedLoginHost(host, "") {
			t.Errorf("allowedLoginHost(%q) = true, want false", host)
		}
	}
	// A deployment that points the flow at its own host must still work.
	if !allowedLoginHost("workos.internal", "workos.internal") {
		t.Error("the configured authorization host must be honoured")
	}
}

// TestNewTaskIDCarriesTheUpstreamPrefix pins the correlation header's shape: the
// upstream reads it as the session identity.
func TestNewTaskIDCarriesTheUpstreamPrefix(t *testing.T) {
	id := newTaskID(time.UnixMilli(1700000000000))
	if id != "sess_1700000000000" {
		t.Fatalf("newTaskID() = %q, want sess_1700000000000", id)
	}
}
