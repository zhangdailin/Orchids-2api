package puter

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
)

type authRoundTripper func(*http.Request) (*http.Response, error)

func (fn authRoundTripper) RoundTrip(r *http.Request) (*http.Response, error) { return fn(r) }

func TestFetchUserVerifiesPuterIdentity(t *testing.T) {
	for _, tc := range []struct {
		status int
		body   string
		ok     bool
	}{
		{200, `{"uuid":"id","username":"user","app_name":"app-id"}`, true},
		{200, `{}`, false}, {200, `invalid`, false}, {401, `secret`, false}, {302, `secret`, false},
	} {
		calls := 0
		client := &Client{authToken: "secret", httpClient: &http.Client{Transport: authRoundTripper(func(r *http.Request) (*http.Response, error) {
			calls++
			if r.URL.String() != "https://api.puter.com/whoami" || r.Header.Get("Authorization") != "Bearer secret" {
				t.Fatal("unexpected identity request")
			}
			return &http.Response{StatusCode: tc.status, Header: http.Header{"Location": []string{"https://other.example"}}, Body: io.NopCloser(strings.NewReader(tc.body)), Request: r}, nil
		})}}
		user, err := client.FetchUser(context.Background())
		if (err == nil) != tc.ok || calls != 1 {
			t.Fatalf("status=%d err=%v calls=%d", tc.status, err, calls)
		}
		if err != nil && strings.Contains(err.Error(), "secret") {
			t.Fatal("upstream secret leaked")
		}
		if tc.ok && (user.UUID != "id" || user.Username != "user") {
			t.Fatal("wrong identity")
		}
	}
}
