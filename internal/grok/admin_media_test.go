package grok

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
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
