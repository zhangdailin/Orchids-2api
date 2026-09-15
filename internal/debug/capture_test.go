package debug

import (
	"context"
	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"strings"
	"testing"
	"time"
)

func TestCaptureRoundTripAndRetention(t *testing.T) {
	server := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: server.Addr()})
	defer client.Close()
	store := NewDiagnosticStore(client, "test:")
	ctx, capture := WithCapture(context.Background(), "request/../1")
	logger := NewForContext(ctx, false, false)
	logger.LogIncomingRequest(map[string]interface{}{"model": "test", "api_key": "sensitive-key", "max_tokens": 100})
	logger.LogUpstreamRequest("https://upstream.test", map[string]string{"Authorization": "Bearer a-secret"}, []byte(`{"messages":[{"content":"hello"}]}`))
	raw := `data: {"text":"hello","access_token":"super-secret"}` + "\n"
	capture.Append("4_upstream_sse.jsonl", raw)
	logger.Close()
	bundle := capture.Bundle()
	if err := store.Save(ctx, bundle); err != nil {
		t.Fatal(err)
	}
	loaded, err := store.Get(ctx, bundle.RequestID)
	if err != nil || loaded == nil {
		t.Fatalf("bundle=%v err=%v", loaded, err)
	}
	joined := ""
	for _, section := range loaded.Sections {
		joined += section.Payload
	}
	if strings.Contains(joined, "super-secret") || strings.Contains(joined, "sensitive-key") || strings.Contains(joined, "a-secret") {
		t.Fatal("credential leaked")
	}
	if !strings.Contains(joined, "hello") || !strings.Contains(joined, "max_tokens") {
		t.Fatal("useful diagnostics lost")
	}
	indexes, err := store.Indexes(ctx, []string{bundle.RequestID, "missing"})
	if err != nil || len(indexes) != 1 {
		t.Fatalf("indexes=%v err=%v", indexes, err)
	}
	server.FastForward(DiagnosticRetention + time.Second)
	loaded, err = store.Get(ctx, bundle.RequestID)
	if err != nil || loaded != nil {
		t.Fatal("expired bundle is still visible")
	}
	indexes, err = store.Indexes(ctx, []string{bundle.RequestID})
	if err != nil || len(indexes) != 0 {
		t.Fatal("expired index is still visible")
	}
}
func TestCaptureBoundsAndRawJSON(t *testing.T) {
	ctx, c := WithCapture(context.Background(), "bounded")
	logger := NewForContext(ctx, false, false)
	logger.LogUpstreamRequest("https://example.test", nil, []byte(`{"messages":["human-readable"]}`))
	c.Append("4_upstream_sse.jsonl", strings.Repeat("x", maxCaptureBytes+100))
	b := c.Bundle()
	if !b.Truncated {
		t.Fatal("missing truncation marker")
	}
	for _, s := range b.Sections {
		if s.Bytes > maxCaptureBytes {
			t.Fatal("unbounded section")
		}
		if s.Name == "upstream_001_request.json" && !strings.Contains(s.Payload, "human-readable") {
			t.Fatal("request body encoded as base64")
		}
	}
}

func TestCaptureRedactsTruncatedCredentialAndPrefixedKeys(t *testing.T) {
	for _, raw := range []string{`{"oauth_access_token":"incomplete-secret`, `{"client_cookie":"sso-secret"}`, `{"url":"http://user:pwd@host"}`} {
		clean := sanitizeCapture(raw)
		for _, secret := range []string{"incomplete-secret", "sso-secret", ":pwd@"} {
			if strings.Contains(clean, secret) {
				t.Fatalf("leaked %q", secret)
			}
		}
	}
}
