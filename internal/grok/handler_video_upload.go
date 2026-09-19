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

// maxBuildVideoUploadBytes bounds the callback PUT. It matches grok2api's
// 256 MiB middleware limit and answers 413, where this endpoint used to read up
// to 512 MiB and then report 400.
const maxBuildVideoUploadBytes = 256 << 20

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

// restoreBuildVideoUpload puts a consumed ticket back after the request it was
// consumed for failed. Without it a single failed upload permanently 404s the
// callback address the video job is waiting on.
func (h *Handler) restoreBuildVideoUpload(ctx context.Context, token string, job *videoJob) {
	if job == nil || strings.TrimSpace(token) == "" {
		return
	}
	digest := sha256.Sum256([]byte(token))
	key := hex.EncodeToString(digest[:])
	if h != nil && h.lb != nil && h.lb.Store != nil && h.lb.Store.RedisClient() != nil {
		raw, err := json.Marshal(persistedVideoUploadTarget{JobID: job.ID, OwnerHash: firstNonEmpty(job.OwnerHash, "anonymous")})
		if err != nil {
			return
		}
		redisKey := h.lb.Store.RedisPrefix() + "video_upload:" + key
		_ = h.lb.Store.RedisClient().Set(ctx, redisKey, raw, videoJobTTL).Err()
		return
	}
	buildVideoUploads.Lock()
	buildVideoUploads.items[key] = videoUploadTarget{job: job, expiresAt: time.Now().Add(videoJobTTL)}
	buildVideoUploads.Unlock()
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
	// Validate the request before spending the one-shot ticket: a rejected
	// content type or an oversized body must not destroy the upload address the
	// caller still needs (grok2api pre-checks and releases the ticket the same
	// way).
	mimeType := strings.ToLower(strings.TrimSpace(strings.Split(r.Header.Get("Content-Type"), ";")[0]))
	if mimeType == "" || mimeType == "application/octet-stream" {
		mimeType = "video/mp4"
	}
	if !strings.HasPrefix(mimeType, "video/") {
		writeGrokErrorCode(w, http.StatusUnsupportedMediaType, "unsupported_media_type", "video content type required")
		return
	}
	if r.ContentLength > maxBuildVideoUploadBytes {
		writeGrokErrorCode(w, http.StatusRequestEntityTooLarge, "request_too_large", "video upload exceeds the configured limit")
		return
	}
	job, ok := h.consumeBuildVideoUpload(r.Context(), token)
	if !ok {
		http.NotFound(w, r)
		return
	}
	raw, err := io.ReadAll(io.LimitReader(r.Body, maxBuildVideoUploadBytes+1))
	if err != nil || len(raw) == 0 || len(raw) > maxBuildVideoUploadBytes {
		h.restoreBuildVideoUpload(r.Context(), token, job)
		if len(raw) > maxBuildVideoUploadBytes {
			writeGrokErrorCode(w, http.StatusRequestEntityTooLarge, "request_too_large", "video upload exceeds the configured limit")
			return
		}
		writeGrokError(w, http.StatusBadRequest, "invalid video upload")
		return
	}
	name, err := h.cacheMediaBytes("xai-upload:"+job.ID, "video", raw, mimeType)
	if err != nil {
		h.restoreBuildVideoUpload(r.Context(), token, job)
		writeGrokError(w, http.StatusInternalServerError, "failed to store video upload")
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
