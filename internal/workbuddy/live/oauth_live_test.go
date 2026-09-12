package live

import (
	"context"
	"errors"
	"net/url"
	"testing"
	"time"

	"orchids-api/internal/workbuddy"
)

// TestLive_StartAuthLogin exercises the real authorization bootstrap: it must
// return the official login page for a fresh state and report the transaction as
// pending until a browser completes it (which this test never does). The
// bootstrap endpoint is unauthenticated and creates only a short-lived state, so
// unlike the catalog/chat checks it needs no credential — only outbound network.
func TestLive_StartAuthLogin(t *testing.T) {
	if testing.Short() {
		t.Skip("live check skipped in short mode")
	}
	client := workbuddy.NewFromAccount(nil, nil)
	defer client.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	state, authURL, err := client.StartAuthLogin(ctx, "5.5.2")
	if err != nil {
		t.Fatalf("StartAuthLogin() error = %v", err)
	}
	if state == "" {
		t.Fatal("StartAuthLogin() returned an empty state")
	}
	parsed, err := url.Parse(authURL)
	if err != nil {
		t.Fatalf("login URL is not parseable: %v", err)
	}
	if parsed.Host != "www.workbuddy.ai" || parsed.Path != "/login" {
		t.Fatalf("login URL = %q, want the official workbuddy.ai login page", authURL)
	}
	if parsed.Query().Get("state") != state || parsed.Query().Get("platform") != "workbuddy-ai" {
		t.Fatalf("login URL query = %q", parsed.RawQuery)
	}
	if parsed.Query().Get("version") != "5.5.2" {
		t.Fatalf("login URL missing version: %q", authURL)
	}
	t.Logf("state=%s url=%s", state, authURL)

	creds, err := client.PollAuthLogin(ctx, state)
	if err == nil {
		t.Fatalf("PollAuthLogin() = %+v, want a pending result", creds)
	}
	if !errors.Is(err, workbuddy.ErrAuthPending) {
		t.Fatalf("PollAuthLogin() error = %v, want ErrAuthPending", err)
	}
}
