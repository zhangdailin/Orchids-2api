package grok

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/goccy/go-json"
	"orchids-api/internal/store"
)

func TestAdminMediaImagesListSearchStatsAndExcludesInputs(t *testing.T) {
	oldBase := cacheBaseDir
	cacheBaseDir = t.TempDir()
	t.Cleanup(func() { cacheBaseDir = oldBase })
	dir := filepath.Join(cacheBaseDir, "image")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string]string{"sunset.png": "image", "portrait.jpg": "other", "input-secret.png": "input"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	h := &Handler{}
	rec := httptest.NewRecorder()
	h.HandleAdminMediaImages(rec, httptest.NewRequest(http.MethodGet, "/api/admin/v1/media/images?search=sunset&page=1&page_size=1", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("list status=%d body=%s", rec.Code, rec.Body.String())
	}
	var listed struct {
		Total int          `json:"total"`
		Items []cacheEntry `json:"items"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &listed); err != nil {
		t.Fatal(err)
	}
	if listed.Total != 1 || len(listed.Items) != 1 || listed.Items[0].Name != "sunset.png" {
		t.Fatalf("unexpected list: %#v", listed)
	}

	statsRec := httptest.NewRecorder()
	h.HandleAdminMediaImageStats(statsRec, httptest.NewRequest(http.MethodGet, "/api/admin/v1/media/images/stats", nil))
	var stats struct {
		Count int   `json:"count"`
		Size  int64 `json:"size_bytes"`
	}
	if err := json.Unmarshal(statsRec.Body.Bytes(), &stats); err != nil {
		t.Fatal(err)
	}
	if stats.Count != 2 || stats.Size != int64(len("image")+len("other")) {
		t.Fatalf("input media leaked into stats: %#v", stats)
	}

	contentRec := httptest.NewRecorder()
	h.HandleAdminMediaImageContent(contentRec, httptest.NewRequest(http.MethodGet, "/api/admin/v1/media/images/content/sunset.png", nil))
	if contentRec.Code != http.StatusOK || contentRec.Body.String() != "image" {
		t.Fatalf("content status=%d body=%q", contentRec.Code, contentRec.Body.String())
	}
	blockedRec := httptest.NewRecorder()
	h.HandleAdminMediaImageContent(blockedRec, httptest.NewRequest(http.MethodGet, "/api/admin/v1/media/images/content/input-secret.png", nil))
	if blockedRec.Code != http.StatusNotFound {
		t.Fatalf("input content status=%d", blockedRec.Code)
	}

	deleteRec := httptest.NewRecorder()
	h.HandleAdminMediaImagesDelete(deleteRec, httptest.NewRequest(http.MethodPost, "/api/admin/v1/media/images/delete", bytes.NewBufferString(`{"names":["sunset.png","../portrait.jpg","input-secret.png"]}`)))
	if deleteRec.Code != http.StatusOK {
		t.Fatalf("delete status=%d body=%s", deleteRec.Code, deleteRec.Body.String())
	}
	if _, err := os.Stat(filepath.Join(dir, "sunset.png")); !os.IsNotExist(err) {
		t.Fatalf("image was not deleted: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "input-secret.png")); err != nil {
		t.Fatalf("input protection lost: %v", err)
	}
}

func TestAdminMediaVideoContentAndTerminalDelete(t *testing.T) {
	h, s, mini := setupValidationHandler(t)
	defer func() { _ = s.Close(); mini.Close() }()
	oldBase := cacheBaseDir
	cacheBaseDir = t.TempDir()
	t.Cleanup(func() { cacheBaseDir = oldBase })
	videoDir := filepath.Join(cacheBaseDir, "video")
	if err := os.MkdirAll(videoDir, 0o755); err != nil {
		t.Fatal(err)
	}
	contentPath := filepath.Join(videoDir, "done.mp4")
	if err := os.WriteFile(contentPath, []byte("video-data"), 0o644); err != nil {
		t.Fatal(err)
	}
	job := &store.StoredVideoJob{ID: "video-done", OwnerHash: "owner", Model: "grok-video", Status: "completed", Progress: 100, ContentPath: contentPath, CreatedAt: time.Now().Unix()}
	if err := s.SaveStoredVideoJob(context.Background(), job, time.Hour); err != nil {
		t.Fatal(err)
	}

	listRec := httptest.NewRecorder()
	h.HandleAdminMediaVideos(listRec, httptest.NewRequest(http.MethodGet, "/api/admin/v1/media/videos", nil))
	if !bytes.Contains(listRec.Body.Bytes(), []byte(`/api/admin/v1/media/videos/content/video-done`)) {
		t.Fatalf("content URL missing: %s", listRec.Body.String())
	}
	contentRec := httptest.NewRecorder()
	h.HandleAdminMediaVideoContent(contentRec, httptest.NewRequest(http.MethodGet, "/api/admin/v1/media/videos/content/video-done?download=1", nil))
	if contentRec.Code != http.StatusOK || contentRec.Body.String() != "video-data" || !strings.HasPrefix(contentRec.Header().Get("Content-Disposition"), "attachment") {
		t.Fatalf("content response status=%d headers=%v body=%q", contentRec.Code, contentRec.Header(), contentRec.Body.String())
	}
	deleteRec := httptest.NewRecorder()
	h.HandleAdminMediaVideoDelete(deleteRec, httptest.NewRequest(http.MethodPost, "/api/admin/v1/media/videos/delete", bytes.NewBufferString(`{"id":"video-done"}`)))
	if deleteRec.Code != http.StatusOK {
		t.Fatalf("delete status=%d body=%s", deleteRec.Code, deleteRec.Body.String())
	}
	if _, err := s.GetStoredVideoJob(context.Background(), "video-done", "owner"); !errors.Is(err, store.ErrNoRows) {
		t.Fatalf("job remains: %v", err)
	}
	if _, err := os.Stat(contentPath); !os.IsNotExist(err) {
		t.Fatalf("content remains: %v", err)
	}
}

func TestAdminMediaVideoDeleteRejectsNonTerminal(t *testing.T) {
	h, s, mini := setupValidationHandler(t)
	defer func() { _ = s.Close(); mini.Close() }()
	if err := s.SaveStoredVideoJob(context.Background(), &store.StoredVideoJob{ID: "video-running", OwnerHash: "owner", Model: "grok-video", Status: "in_progress", CreatedAt: time.Now().Unix()}, time.Hour); err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	h.HandleAdminMediaVideoDelete(rec, httptest.NewRequest(http.MethodPost, "/api/admin/v1/media/videos/delete", bytes.NewBufferString(`{"id":"video-running"}`)))
	if rec.Code != http.StatusConflict {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
}
func TestAdminMediaVideosListSearchStatusPaginationAndStats(t *testing.T) {
	h, s, mini := setupValidationHandler(t)
	defer func() { _ = s.Close(); mini.Close() }()
	ctx := context.Background()
	now := time.Now()
	jobs := []*store.StoredVideoJob{
		{ID: "video-old", OwnerHash: "private-owner-a", Model: "grok-video", Prompt: "ocean", Status: "completed", Progress: 100, CreatedAt: now.Add(-time.Minute).Unix(), UpdatedAt: now.Add(-time.Minute)},
		{ID: "video-new", OwnerHash: "private-owner-b", Model: "grok-video", Prompt: "sunset city", Status: "failed", ErrorMessage: "boom", CreatedAt: now.Unix(), UpdatedAt: now},
	}
	for _, job := range jobs {
		if err := s.SaveStoredVideoJob(ctx, job, time.Hour); err != nil {
			t.Fatal(err)
		}
	}

	rec := httptest.NewRecorder()
	h.HandleAdminMediaVideos(rec, httptest.NewRequest(http.MethodGet, "/api/admin/v1/media/videos?status=failed&search=sunset&page=1&page_size=1", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("list status=%d body=%s", rec.Code, rec.Body.String())
	}
	var listed struct {
		Total int                      `json:"total"`
		Items []map[string]interface{} `json:"items"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &listed); err != nil {
		t.Fatal(err)
	}
	if listed.Total != 1 || len(listed.Items) != 1 || listed.Items[0]["id"] != "video-new" {
		t.Fatalf("unexpected list: %#v", listed)
	}
	if _, leaked := listed.Items[0]["owner_hash"]; leaked {
		t.Fatal("admin response must not expose owner hash")
	}

	statsRec := httptest.NewRecorder()
	h.HandleAdminMediaVideoStats(statsRec, httptest.NewRequest(http.MethodGet, "/api/admin/v1/media/videos/stats", nil))
	var stats struct {
		Total    int            `json:"total"`
		Statuses map[string]int `json:"statuses"`
	}
	if err := json.Unmarshal(statsRec.Body.Bytes(), &stats); err != nil {
		t.Fatal(err)
	}
	if stats.Total != 2 || stats.Statuses["completed"] != 1 || stats.Statuses["failed"] != 1 {
		t.Fatalf("unexpected stats: %#v", stats)
	}
}
