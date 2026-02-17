package grok

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func resetImagineSessionsForTest() {
	imagineSessionsMu.Lock()
	imagineSessions = map[string]imagineSession{}
	imagineSessionsMu.Unlock()
}

func TestImagineSessionLifecycle(t *testing.T) {
	resetImagineSessionsForTest()
	t.Cleanup(resetImagineSessionsForTest)

	id := createImagineSession("test prompt", "16:9", nil)
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
	session, ok := getImagineSession(taskID)
	if !ok {
		t.Fatal("expected imagine session")
	}
	if session.NSFW == nil || *session.NSFW != false {
		t.Fatalf("session.NSFW=%v want=false", session.NSFW)
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
