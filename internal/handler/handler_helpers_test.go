package handler

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestResolveWorkdir_NoSessionFallbackWithoutExplicitConversation(t *testing.T) {
	h := &Handler{
		sessionWorkdirs:   map[string]string{"k1": "/stale/workdir"},
		sessionConvIDs:    map[string]string{},
		sessionLastAccess: map[string]time.Time{},
	}
	r := httptest.NewRequest(http.MethodPost, "http://example.com/warp/v1/messages", nil)
	req := ClaudeRequest{}

	got, prev, changed := h.resolveWorkdir(r, req, "k1")
	if got != "" {
		t.Fatalf("expected empty workdir, got %q", got)
	}
	if prev != "/stale/workdir" {
		t.Fatalf("expected prev workdir retained, got %q", prev)
	}
	if changed {
		t.Fatalf("expected changed=false when no new workdir")
	}
}
