package grok

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"github.com/goccy/go-json"
	"io"
	"net/http"
	"net/http/httptest"
	"orchids-api/internal/config"
	"orchids-api/internal/store"
	"os"
	"path/filepath"
	"strings"
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

	id := createImagineSession("test prompt", "16:9", "", nil)
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
		t.Fatalf("model=%q want=grok-imagine-image-lite", got)
	}
	session, ok := getImagineSession(taskID)
	if !ok {
		t.Fatal("expected imagine session")
	}
	if session.NSFW == nil || *session.NSFW != false {
		t.Fatalf("session.NSFW=%v want=false", session.NSFW)
	}
	if session.Model != "grok-imagine-image-lite" {
		t.Fatalf("session.Model=%q want=grok-imagine-image-lite", session.Model)
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

func TestImagineImageB64FromURL_LocalCachedFile(t *testing.T) {
	oldBase := cacheBaseDir
	cacheBaseDir = t.TempDir()
	t.Cleanup(func() { cacheBaseDir = oldBase })

	imageDir := filepath.Join(cacheBaseDir, "image")
	if err := os.MkdirAll(imageDir, 0o755); err != nil {
		t.Fatalf("mkdir image dir: %v", err)
	}
	raw := []byte("fake-image-bytes")
	if err := os.WriteFile(filepath.Join(imageDir, "sample.jpg"), raw, 0o644); err != nil {
		t.Fatalf("write sample image: %v", err)
	}

	got := imagineImageB64FromURL("/v1/files/image/sample.jpg")
	want := base64.StdEncoding.EncodeToString(raw)
	if got != want {
		t.Fatalf("b64 mismatch: got=%q want=%q", got, want)
	}
}

func TestGenerateAppChatImagineBatch_ReturnsLocalCachedURL(t *testing.T) {
	oldBase := cacheBaseDir
	cacheBaseDir = t.TempDir()
	t.Cleanup(func() { cacheBaseDir = oldBase })

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != defaultChatPath {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"result":{"response":{"modelResponse":{"imageGenerationResponse":{"progress":100,"imageUrl":"https://assets.grok.com/users/u/generated/a/image.png"}}}}}` + "\n"))
	}))
	defer upstream.Close()

	h := NewHandler(&config.Config{GrokAPIBaseURL: upstream.URL}, nil)
	h.client.assetClient = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"image/png"}},
			Body:       io.NopCloser(bytes.NewReader(testPNGBytes(t))),
			Request:    req,
		}, nil
	})}
	sess := &chatAccountSession{
		acc:   &store.Account{ID: 1, Subscription: "basic"},
		token: "basic-token",
	}
	spec, ok := ResolveModel("grok-imagine-image-lite")
	if !ok {
		t.Fatal("missing grok-imagine-image-lite spec")
	}

	images, _, err := h.generateAppChatImagineBatch(context.Background(), sess, spec, "apple", "2:3", "grok-imagine-image-lite", 1, nil)
	if err != nil {
		t.Fatalf("generateAppChatImagineBatch error: %v", err)
	}
	if len(images) != 1 {
		t.Fatalf("images=%d want=1", len(images))
	}
	if !isLocalImagineImageURL(images[0].URL) {
		t.Fatalf("url=%q want local cached file url", images[0].URL)
	}
	if strings.Contains(images[0].URL, "assets.grok.com") {
		t.Fatalf("url=%q should not expose grok asset url", images[0].URL)
	}
	_, fileName, ok := parseFilesPath(images[0].URL)
	if !ok {
		t.Fatalf("parse local file url failed: %q", images[0].URL)
	}
	if _, err := os.Stat(filepath.Join(cacheBaseDir, "image", fileName)); err != nil {
		t.Fatalf("cached image missing: %v", err)
	}
}
