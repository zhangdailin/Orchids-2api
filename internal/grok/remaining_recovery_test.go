package grok

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/goccy/go-json"
	"orchids-api/internal/store"
)

func TestRemainingAttemptDiagnosticsRedactAndBound(t *testing.T) {
	log := &parityAuditLog{}
	h := &Handler{auditLogger: log}
	acc := &store.Account{ID: 7, Token: "synthetic-account-secret", OAuthAccessToken: "synthetic-oauth-secret"}
	body, _ := json.Marshal(map[string]interface{}{"error": map[string]interface{}{"code": "invalid_encrypted_content", "message": "retry Bearer private-bearer password=private-password synthetic-account-secret synthetic-oauth-secret " + strings.Repeat("x", 2000)}, "output": "private-user-output", "encrypted_content": "private-cipher"})
	header := http.Header{"Set-Cookie": []string{"private-cookie"}, "Authorization": []string{"Bearer private-auth"}, "X-Request-Id": []string{"request-123"}}
	err := fmt.Errorf("wrapped: %w", newUpstreamError(400, header, body, ""))
	h.auditAttempt(context.Background(), acc, ProviderBuild, 2, time.Now(), err, "reasoning_replay_recovery")
	if len(log.events) != 1 {
		t.Fatal(log.events)
	}
	event := log.events[0]
	encoded, _ := json.Marshal(event)
	text := string(encoded)
	for _, secret := range []string{"private-bearer", "private-password", "synthetic-account-secret", "synthetic-oauth-secret", "private-cookie", "private-auth", "private-user-output", "private-cipher"} {
		if strings.Contains(text, secret) {
			t.Fatalf("diagnostic leaked %s", secret)
		}
	}
	if !strings.Contains(text, "invalid_encrypted_content") || !strings.Contains(text, "request-123") || event.Metadata["http_status"] != 400 || len(text) > 5000 {
		t.Fatalf("diagnostic missing useful bounded fields: %s", text)
	}
}
