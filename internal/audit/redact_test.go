package audit

import (
	"strings"
	"testing"
)

// TestSummarizeChange_MasksCredentialsKeepsShape pins the operation journal's
// contract: a reader learns WHICH fields changed without ever seeing a live
// credential.
func TestSummarizeChange_MasksCredentialsKeepsShape(t *testing.T) {
	body := []byte(`{"name":"grok-sso","client_cookie":"sso=super-secret","enabled":true,"weight":3,"oauth_refresh_token":"rt-123"}`)
	summary, redacted := SummarizeChange(body)

	for _, secret := range []string{"super-secret", "rt-123"} {
		if strings.Contains(summary, secret) {
			t.Fatalf("summary leaked %q: %s", secret, summary)
		}
	}
	for _, key := range []string{"client_cookie", "oauth_refresh_token"} {
		if !strings.Contains(summary, key) {
			t.Fatalf("summary lost the field name %q: %s", key, summary)
		}
	}
	if !strings.Contains(summary, "grok-sso") || !strings.Contains(summary, "weight") {
		t.Fatalf("summary lost the non-secret change: %s", summary)
	}
	if len(redacted) != 2 {
		t.Fatalf("redacted = %v, want the two secret paths", redacted)
	}
}

// TestSummarizeChange_NestedAndArraySecrets covers the real account payload
// shape, where credentials sit inside nested objects.
func TestSummarizeChange_NestedAndArraySecrets(t *testing.T) {
	body := []byte(`{"account":{"token":"tok-1","nested":{"api_key":"k-1"}},"keys":[{"refresh_token":"r-1"}]}`)
	summary, redacted := SummarizeChange(body)
	for _, secret := range []string{"tok-1", "k-1", "r-1"} {
		if strings.Contains(summary, secret) {
			t.Fatalf("summary leaked %q: %s", secret, summary)
		}
	}
	if len(redacted) != 3 {
		t.Fatalf("redacted = %v, want three secret paths", redacted)
	}
	// The masked marker is written through encoding/json, which escapes "<" as
	// \u003c; only the field names are asserted here.
	if !strings.Contains(summary, "account") || !strings.Contains(summary, "api_key") {
		t.Fatalf("nested field names not reported: %s", summary)
	}
	if strings.Contains(summary, `"api_key":"`) && !strings.Contains(summary, "u003credacted") {
		t.Fatalf("api_key was not masked: %s", summary)
	}
}

// TestSummarizeChange_NonJSONBodyNeverLeaksContent keeps a form or text body
// from being copied into the journal.
func TestSummarizeChange_NonJSONBodyNeverLeaksContent(t *testing.T) {
	summary, redacted := SummarizeChange([]byte("admin_pass=secret&x=1"))
	if strings.Contains(summary, "secret") {
		t.Fatalf("form body leaked: %s", summary)
	}
	if summary == "" || len(redacted) != 0 {
		t.Fatalf("summary = %q redacted = %v", summary, redacted)
	}
	if !strings.Contains(summary, "form") {
		t.Fatalf("summary should name the body kind: %q", summary)
	}
}

// TestSummarizeChange_EmptyBodyIsEmpty keeps noise out of the journal.
func TestSummarizeChange_EmptyBodyIsEmpty(t *testing.T) {
	summary, redacted := SummarizeChange([]byte("   "))
	if summary != "" || redacted != nil {
		t.Fatalf("summary = %q redacted = %v", summary, redacted)
	}
}

// TestEventKindsAreDeclared guards the three journals the log centre filters on.
func TestEventKindsAreDeclared(t *testing.T) {
	for _, kind := range []Kind{KindRequest, KindOperation, KindSystem} {
		if strings.TrimSpace(string(kind)) == "" {
			t.Fatal("an empty kind would be invisible to the log centre filter")
		}
	}
}

// TestSummarizeChange_MasksConfigSecrets is the reported leak: a config save with
// redis_password, proxy_pass and a password embedded in a URL reached the journal
// in clear text, because the redaction matched exact key names only.
func TestSummarizeChange_MasksConfigSecrets(t *testing.T) {
	body := []byte(`{
		"redis_password": "redis-secret-1",
		"proxy_pass": "proxy-secret-2",
		"proxy_url": "http://proxyuser:proxy-secret-3@proxy.internal:8080",
		"public_api_key": "public-secret-4",
		"admin_pass": "admin-secret-5",
		"upstream_token": "upstream-secret-6",
		"port": "3002",
		"debug_enabled": true
	}`)
	summary, redacted := SummarizeChange(body)

	for _, secret := range []string{
		"redis-secret-1", "proxy-secret-2", "proxy-secret-3",
		"public-secret-4", "admin-secret-5", "upstream-secret-6",
	} {
		if strings.Contains(summary, secret) {
			t.Fatalf("summary leaked %q: %s", secret, summary)
		}
	}
	// The non-secret settings still describe the change.
	if !strings.Contains(summary, "3002") || !strings.Contains(summary, "debug_enabled") {
		t.Fatalf("summary lost the non-secret settings: %s", summary)
	}
	if len(redacted) < 5 {
		t.Fatalf("redacted = %v, want every secret field named", redacted)
	}
	// The URL keeps its shape so the reader sees that a proxied URL was set.
	if !strings.Contains(summary, "proxy.internal") {
		t.Fatalf("summary lost the URL host: %s", summary)
	}
}

// TestSummarizeChange_MasksNestedURLPassword covers the same leak one level down.
func TestSummarizeChange_MasksNestedURLPassword(t *testing.T) {
	body := []byte(`{"databases":{"primary":{"dsn":"postgres://user:nested-secret@db:5432/app"}}}`)
	summary, redacted := SummarizeChange(body)
	if strings.Contains(summary, "nested-secret") {
		t.Fatalf("nested URL password leaked: %s", summary)
	}
	if len(redacted) == 0 {
		t.Fatal("the masked path was not reported")
	}
}