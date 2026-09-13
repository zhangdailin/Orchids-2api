package grok

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
)

func TestBuildVideoUploadIsOneTime(t *testing.T) {
	previousCacheDir := cacheBaseDir
	cacheBaseDir = t.TempDir()
	t.Cleanup(func() { cacheBaseDir = previousCacheDir })

	job := &videoJob{ID: "video_test", PublicBaseURL: "https://media.example", CreatedAt: time.Now().Unix(), Status: "in_progress"}
	uploadURL, err := (&Handler{}).registerBuildVideoUpload(job)
	if err != nil {
		t.Fatalf("registerBuildVideoUpload() error = %v", err)
	}
	token := strings.TrimPrefix(uploadURL, "https://media.example/media/uploads/")
	h := &Handler{}

	req := httptest.NewRequest(http.MethodPut, "/media/uploads/"+token, bytes.NewReader([]byte("video-data")))
	req.Header.Set("Content-Type", "video/mp4")
	rec := httptest.NewRecorder()
	h.HandleVideoUpload(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("first upload status=%d body=%s", rec.Code, rec.Body.String())
	}
	if job.Status != "completed" || job.ContentPath == "" {
		t.Fatalf("job was not completed: %#v", job)
	}
	if _, err := os.Stat(job.ContentPath); err != nil {
		t.Fatalf("uploaded content not persisted: %v", err)
	}

	retry := httptest.NewRequest(http.MethodPut, "/media/uploads/"+token, bytes.NewReader([]byte("again")))
	retry.Header.Set("Content-Type", "video/mp4")
	retryRec := httptest.NewRecorder()
	h.HandleVideoUpload(retryRec, retry)
	if retryRec.Code != http.StatusNotFound {
		t.Fatalf("reused upload status=%d want=%d", retryRec.Code, http.StatusNotFound)
	}
}

func TestBuildVideoUploadTokenIsSharedThroughRedis(t *testing.T) {
	h1, s, mini := setupValidationHandler(t)
	defer func() {
		_ = s.Close()
		mini.Close()
	}()
	job := &videoJob{
		ID: "video_shared", OwnerHash: "owner_shared", PublicBaseURL: "https://media.example",
		CreatedAt: time.Now().Unix(), Status: "in_progress", Model: "build/grok-imagine-video-1.5",
	}
	h1.persistVideoJob(t.Context(), job)
	uploadURL, err := h1.registerBuildVideoUpload(job)
	if err != nil {
		t.Fatalf("registerBuildVideoUpload() error = %v", err)
	}
	token := strings.TrimPrefix(uploadURL, "https://media.example/media/uploads/")
	h2 := &Handler{lb: h1.lb}
	resolved, ok := h2.consumeBuildVideoUpload(t.Context(), token)
	if !ok || resolved == nil || resolved.ID != job.ID {
		t.Fatalf("shared token did not resolve: ok=%v job=%#v", ok, resolved)
	}
	if _, ok := h1.consumeBuildVideoUpload(t.Context(), token); ok {
		t.Fatal("shared token was reusable")
	}
}
