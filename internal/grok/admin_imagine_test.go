package grok

import (
	"bytes"
	"errors"
	"github.com/goccy/go-json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func resetImagineSessionsForTest() {
	imagineSessionsMu.Lock()
	imagineSessions = map[string]imagineSession{}
	imagineSessionsMu.Unlock()
}

func TestImagineErrorRetryDelay_UsesLongDelayForRateLimit(t *testing.T) {
	if got := imagineErrorRetryDelay(errors.New("grok upstream status=429 body=too many requests")); got != time.Minute {
		t.Fatalf("delay=%v want 1m", got)
	}
	if got := imagineErrorRetryDelay(errors.New("no image generated")); got != time.Minute {
		t.Fatalf("delay=%v want 1m", got)
	}
}

func TestImagineSessionLifecycle(t *testing.T) {
	resetImagineSessionsForTest()
	t.Cleanup(resetImagineSessionsForTest)

	id := createImagineSession("test prompt", "16:9", "", "", nil)
	if id == "" {
		t.Fatal("expected task id")
	}

	session, ok := getImagineSession(id)
	if !ok {
		t.Fatal("expected session to exist")
	}
	if session.Prompt != "test prompt" {
		t.Fatalf("unexpected prompt: %q", session.Prompt)
	}
	if session.AspectRatio != "16:9" {
		t.Fatalf("unexpected aspect ratio: %q", session.AspectRatio)
	}
	if session.Model != "grok-imagine-image-lite" {
		t.Fatalf("unexpected model: %q", session.Model)
	}
	if session.Route != "ws" {
		t.Fatalf("unexpected route: %q", session.Route)
	}

	removed := deleteImagineSessions([]string{id})
	if removed != 1 {
		t.Fatalf("removed=%d want=1", removed)
	}
	if _, ok := getImagineSession(id); ok {
		t.Fatal("session should be removed")
	}
}

func TestHandleAdminImagineStartStop(t *testing.T) {
	resetImagineSessionsForTest()
	t.Cleanup(resetImagineSessionsForTest)

	h := &Handler{}

	startBody := map[string]interface{}{
		"prompt":       "a cat on mars",
		"aspect_ratio": "1024x576",
		"route":        "ws",
		"model":        "grok-imagine-image-lite",
		"nsfw":         false,
	}
	raw, _ := json.Marshal(startBody)
	startReq := httptest.NewRequest(http.MethodPost, "/api/v1/admin/imagine/start", bytes.NewReader(raw))
	startRec := httptest.NewRecorder()
	h.HandleAdminImagineStart(startRec, startReq)
	if startRec.Code != http.StatusOK {
		t.Fatalf("start status=%d want=200", startRec.Code)
	}

	var startResp map[string]interface{}
	if err := json.Unmarshal(startRec.Body.Bytes(), &startResp); err != nil {
		t.Fatalf("decode start response: %v", err)
	}
	taskID, _ := startResp["task_id"].(string)
	if taskID == "" {
		t.Fatal("expected non-empty task_id")
	}
	if got, _ := startResp["aspect_ratio"].(string); got != "16:9" {
		t.Fatalf("aspect_ratio=%q want=16:9", got)
	}
	if got, _ := startResp["model"].(string); got != "grok-imagine-image-lite" {
		t.Fatalf("model=%q want grok-imagine-image-lite", got)
	}
	if got, _ := startResp["route"].(string); got != "ws" {
		t.Fatalf("route=%q want=ws", got)
	}
	session, ok := getImagineSession(taskID)
	if !ok {
		t.Fatal("expected imagine session")
	}
	if session.NSFW == nil || *session.NSFW != false {
		t.Fatalf("session.NSFW=%v want=false", session.NSFW)
	}
	if session.Model != "grok-imagine-image-lite" {
		t.Fatalf("session.Model=%q want grok-imagine-image-lite", session.Model)
	}
	if session.Route != "ws" {
		t.Fatalf("session.Route=%q want=ws", session.Route)
	}

	stopBody := map[string]interface{}{
		"task_ids": []string{taskID},
	}
	stopRaw, _ := json.Marshal(stopBody)
	stopReq := httptest.NewRequest(http.MethodPost, "/api/v1/admin/imagine/stop", bytes.NewReader(stopRaw))
	stopRec := httptest.NewRecorder()
	h.HandleAdminImagineStop(stopRec, stopReq)
	if stopRec.Code != http.StatusOK {
		t.Fatalf("stop status=%d want=200", stopRec.Code)
	}
}

func TestNormalizeImagineModel_DefaultsToLite(t *testing.T) {
	tests := []struct {
		in   string
		want string
	}{
		{"", "grok-imagine-image-lite"},
		{"speed", "grok-imagine-image-lite"},
		{"fast", "grok-imagine-image-lite"},
		{"quality", "grok-imagine-image-pro"},
		{"pro", "grok-imagine-image-pro"},
		{"grok-imagine-image", "grok-imagine-image"},
		{"grok-imagine-image-pro", "grok-imagine-image-pro"},
		{"grok-4.20-auto", "grok-imagine-image-lite"},
	}
	for _, tt := range tests {
		if got := normalizeImagineModel(tt.in); got != tt.want {
			t.Fatalf("normalizeImagineModel(%q)=%q want %q", tt.in, got, tt.want)
		}
	}
}

func TestNormalizeImagineRoute(t *testing.T) {
	tests := []struct {
		in   string
		want string
	}{
		{"", "ws"},
		{"ws", "ws"},
		{"basic", "ws"},
		{"chat", "ws"},
	}
	for _, tt := range tests {
		if got := normalizeImagineRoute(tt.in); got != tt.want {
			t.Fatalf("normalizeImagineRoute(%q)=%q want %q", tt.in, got, tt.want)
		}
	}
}
