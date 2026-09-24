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
