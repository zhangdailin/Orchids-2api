package grok

import (
	"errors"
	"net/http"
	"testing"
)

func TestIsDefinitiveAccountBlockBody(t *testing.T) {
	cases := []struct {
		body string
		want bool
	}{
		{`{"error":{"code":"blocked-user","message":"user is blocked"}}`, true},
		{`{"code":"blocked-user"}`, true},
		{`{"error":"user is blocked"}`, true},
		{`user is blocked`, true},
		{`{"error":{"code":"content_policy_violation"}}`, false},
		{`{"error":"rate limit"}`, false},
		{`{"code":"resource-exhausted"}`, false},
	}
	for _, c := range cases {
		if got := IsDefinitiveAccountBlockBody([]byte(c.body)); got != c.want {
			t.Errorf("body %q: got %v want %v", c.body, got, c.want)
		}
	}
}

func TestIsCloudflareChallengeBody(t *testing.T) {
	cases := []struct {
		body string
		want bool
	}{
		{`cf-mitigated: challenge`, true},
		{`Just a moment... verifying you are human`, true},
		{`Enable JavaScript and cookies to continue`, true},
		{`__cf_chl_opt_tKJ`, true},
		{`{"error":"blocked-user"}`, false},
		{`{"error":"content policy violation"}`, false},
	}
	for _, c := range cases {
		if got := IsCloudflareChallengeBody([]byte(c.body)); got != c.want {
			t.Errorf("body %q: got %v want %v", c.body, got, c.want)
		}
	}
}

func TestIsEgressChallengeError(t *testing.T) {
	if !isEgressChallengeError(errGrokBlockedBody()) {
		t.Fatal("Cloudflare body should be flagged as egress challenge")
	}
	if isEgressChallengeError(errNil()) {
		t.Fatal("nil error must not be flagged")
	}
}

func errGrokBlockedBody() error {
	return &grokUpstreamError{status: 403, body: "cf-mitigated: challenge"}
}

func errNil() error {
	return nil
}

func TestClassifyUpstreamResponse(t *testing.T) {
	cases := []struct {
		name   string
		status int
		header map[string]string
		body   string
		want   UpstreamErrorKind
	}{
		{name: "429", status: 429, body: "too many requests", want: UpstreamErrorRateLimited},
		{name: "blocked-user", status: 403, body: `{"code":"blocked-user"}`, want: UpstreamErrorAccountBlock},
		{name: "plain 403", status: 403, body: "forbidden", want: UpstreamErrorGenericForbidden},
		{name: "cf body 403", status: 403, body: "<html>Just a moment...</html>", want: UpstreamErrorCloudflareChallenge},
		{name: "cf header 403", status: 403, header: map[string]string{"CF-Mitigated": "challenge"}, body: "", want: UpstreamErrorCloudflareChallenge},
		{name: "plain 200", status: 200, body: "", want: UpstreamErrorUnknown},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			header := make(map[string][]string, len(c.header))
			for k, v := range c.header {
				header[k] = []string{v}
			}
			if got := ClassifyUpstreamResponse(c.status, http.Header(header), []byte(c.body)); got != c.want {
				t.Fatalf("ClassifyUpstreamResponse(%d,%q)=%v want=%v", c.status, c.body, got, c.want)
			}
		})
	}
}

func TestClassifyUpstreamError_LegacyText(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want UpstreamErrorKind
	}{
		{name: "plain 403", err: &grokUpstreamError{status: 403, body: "forbidden"}, want: UpstreamErrorGenericForbidden},
		{name: "blocked-user", err: &grokUpstreamError{status: 403, body: `{"code":"blocked-user"}`}, want: UpstreamErrorAccountBlock},
		{name: "cf header", err: &grokUpstreamError{status: 403, header: http.Header{"CF-Mitigated": {"challenge"}}}, want: UpstreamErrorCloudflareChallenge},
		{name: "nil", err: nil, want: UpstreamErrorUnknown},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := ClassifyUpstreamError(c.err); got != c.want {
				t.Fatalf("ClassifyUpstreamError()=%v want=%v", got, c.want)
			}
		})
	}
}

func TestClassifyUpstreamError_PlainText(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want UpstreamErrorKind
	}{
		{name: "legacy 403", err: errors.New("grok upstream status=403 body=forbidden"), want: UpstreamErrorGenericForbidden},
		{name: "legacy blocked", err: errors.New("grok upstream status=403 body={\"code\":\"blocked-user\"}"), want: UpstreamErrorAccountBlock},
		{name: "legacy 429", err: errors.New("grok upstream status=429 body=rate limit exceeded"), want: UpstreamErrorRateLimited},
		{name: "legacy cloudflare", err: errors.New("grok upstream status=403 body=<html>Just a moment...</html>"), want: UpstreamErrorCloudflareChallenge},
		{name: "legacy no status", err: errors.New("forbidden"), want: UpstreamErrorUnknown},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := ClassifyUpstreamError(c.err); got != c.want {
				t.Fatalf("ClassifyUpstreamError()=%v want=%v", got, c.want)
			}
		})
	}
}
