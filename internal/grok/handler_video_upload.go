package grok

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/goccy/go-json"
)

var videoUploadTokenPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)

type videoUploadTarget struct {
	job       *videoJob
	expiresAt time.Time
}

var buildVideoUploads = struct {
	sync.Mutex
	items map[string]videoUploadTarget
}{items: make(map[string]videoUploadTarget)}

type persistedVideoUploadTarget struct {
	JobID     string `json:"job_id"`
	OwnerHash string `json:"owner_hash"`
}

func (h *Handler) registerBuildVideoUpload(job *videoJob) (string, error) {
	if job == nil {
		return "", fmt.Errorf("video job is unavailable")
	}
	base := strings.TrimRight(strings.TrimSpace(job.PublicBaseURL), "/")
	if !strings.HasPrefix(strings.ToLower(base), "https://") {
		return "", fmt.Errorf("Build video fallback requires a public HTTPS base URL")
	}
	token := randomHex(32)
	digest := sha256.Sum256([]byte(token))
	key := hex.EncodeToString(digest[:])
	if h != nil && h.lb != nil && h.lb.Store != nil && h.lb.Store.RedisClient() != nil {
		raw, err := json.Marshal(persistedVideoUploadTarget{JobID: job.ID, OwnerHash: firstNonEmpty(job.OwnerHash, "anonymous")})
		if err != nil {
			return "", err
		}
		redisKey := h.lb.Store.RedisPrefix() + "video_upload:" + key
		if err := h.lb.Store.RedisClient().Set(context.Background(), redisKey, raw, videoJobTTL).Err(); err != nil {
			return "", fmt.Errorf("persist Build video upload token: %w", err)
		}
		return base + "/media/uploads/" + token, nil
	}
	buildVideoUploads.Lock()
	now := time.Now()
	for existing, target := range buildVideoUploads.items {
		if now.After(target.expiresAt) {
			delete(buildVideoUploads.items, existing)
		}
	}
	buildVideoUploads.items[key] = videoUploadTarget{job: job, expiresAt: now.Add(videoJobTTL)}
	buildVideoUploads.Unlock()
	return base + "/media/uploads/" + token, nil
}

func (h *Handler) consumeBuildVideoUpload(ctx context.Context, token string) (*videoJob, bool) {
	digest := sha256.Sum256([]byte(token))
	key := hex.EncodeToString(digest[:])
	if h != nil && h.lb != nil && h.lb.Store != nil && h.lb.Store.RedisClient() != nil {
		redisKey := h.lb.Store.RedisPrefix() + "video_upload:" + key
		raw, err := h.lb.Store.RedisClient().GetDel(ctx, redisKey).Bytes()
		if err != nil {
			return nil, false
		}
		var target persistedVideoUploadTarget
		if json.Unmarshal(raw, &target) != nil || target.JobID == "" || target.OwnerHash == "" {
			return nil, false
		}
		if job, ok := h.lookupVideoJob(ctx, target.JobID, target.OwnerHash); ok && job != nil {
			return job, true
		}
		return nil, false
	}
	buildVideoUploads.Lock()
	defer buildVideoUploads.Unlock()
	target, ok := buildVideoUploads.items[key]
	if !ok || time.Now().After(target.expiresAt) {
		delete(buildVideoUploads.items, key)
		return nil, false
	}
	delete(buildVideoUploads.items, key)
	return target.job, target.job != nil
}

func (h *Handler) HandleVideoUpload(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodPut) {
		return
	}
	token := strings.TrimSpace(strings.TrimPrefix(r.URL.Path, "/media/uploads/"))
	if !videoUploadTokenPattern.MatchString(token) {
		http.NotFound(w, r)
		return
	}
	job, ok := h.consumeBuildVideoUpload(r.Context(), token)
	if !ok {
		http.NotFound(w, r)
		return
	}
	mimeType := strings.ToLower(strings.TrimSpace(strings.Split(r.Header.Get("Content-Type"), ";")[0]))
	if mimeType == "" || mimeType == "application/octet-stream" {
		mimeType = "video/mp4"
	}
	if !strings.HasPrefix(mimeType, "video/") {
		http.Error(w, "video content type required", http.StatusUnsupportedMediaType)
		return
	}
	raw, err := io.ReadAll(io.LimitReader(r.Body, maxConsoleVideoAssetBytes+1))
	if err != nil || len(raw) == 0 || len(raw) > maxConsoleVideoAssetBytes {
		http.Error(w, "invalid video upload", http.StatusBadRequest)
		return
	}
	name, err := h.cacheMediaBytes("xai-upload:"+job.ID, "video", raw, mimeType)
	if err != nil {
		http.Error(w, "failed to store video upload", http.StatusInternalServerError)
		return
	}
	videoJobsMu.Lock()
	job.ContentPath = filepath.Join(cacheBaseDir, "video", name)
	job.Status = "completed"
	job.Progress = 100
	job.CompletedAt = time.Now().Unix()
	videoJobsMu.Unlock()
	putVideoJob(job)
	h.persistVideoJob(context.Background(), job)
	w.WriteHeader(http.StatusNoContent)
}
