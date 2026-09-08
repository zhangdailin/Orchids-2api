package grok

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"mime/multipart"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/goccy/go-json"

	"orchids-api/internal/middleware"
	"orchids-api/internal/store"
)

const videoJobTTL = time.Hour

var (
	videoJobsMu sync.Mutex
	videoJobs   = map[string]*videoJob{}
)

func cleanupVideoJobsLocked(now time.Time) {
	for id, job := range videoJobs {
		if job == nil || now.Sub(time.Unix(job.CreatedAt, 0)) > videoJobTTL {
			delete(videoJobs, id)
		}
	}
}

func putVideoJob(job *videoJob) {
	videoJobsMu.Lock()
	defer videoJobsMu.Unlock()
	cleanupVideoJobsLocked(time.Now())
	videoJobs[job.ID] = job
}

func (j *videoJob) toMap() map[string]interface{} {
	if j.StandardAPI {
		return j.toStandardMap()
	}
	out := map[string]interface{}{
		"id":         j.ID,
		"object":     "video",
		"created_at": j.CreatedAt,
		"status":     j.Status,
		"model":      j.Model,
		"progress":   j.Progress,
		"prompt":     j.Prompt,
		"seconds":    fmt.Sprint(j.Seconds),
		"size":       j.Size,
		"quality":    j.Quality,
	}
	if j.CompletedAt > 0 {
		out["completed_at"] = j.CompletedAt
	}
	if strings.TrimSpace(j.VideoURL) != "" {
		out["video_url"] = j.VideoURL
	}
	if j.Status == "completed" && strings.TrimSpace(j.ID) != "" && strings.TrimSpace(j.ContentPath) != "" {
		out["content_url"] = "/grok/v1/videos/" + j.ID + "/content"
	}
	if j.Error != nil {
		out["error"] = j.Error
	}
	if strings.TrimSpace(j.RemixedFromID) != "" {
		out["remixed_from_video_id"] = j.RemixedFromID
	}
	return out
}

func (j *videoJob) toStandardMap() map[string]interface{} {
	if j == nil {
		return map[string]interface{}{"status": "failed", "error": map[string]interface{}{"code": "internal_error", "message": "video job is unavailable"}}
	}
	switch j.Status {
	case "completed":
		video := map[string]interface{}{
			"url":                "/v1/videos/" + j.ID + "/content",
			"respect_moderation": true,
		}
		if j.Operation == "generate" && j.Seconds > 0 {
			video["duration"] = j.Seconds
		}
		return map[string]interface{}{
			"status": "done", "model": j.Model, "progress": 100, "video": video,
		}
	case "failed":
		message := "video operation failed"
		if j.Error != nil {
			if value := strings.TrimSpace(fmt.Sprint(j.Error["message"])); value != "" && value != "<nil>" {
				message = value
			}
		}
		return map[string]interface{}{
			"status": "failed", "error": map[string]interface{}{"code": "internal_error", "message": message},
		}
	default:
		return map[string]interface{}{
			"status": "pending", "model": j.Model, "progress": min(99, clampProgress(j.Progress)),
		}
	}
}

func videoRequestOwner(r *http.Request) string {
	if r == nil {
		return "anonymous"
	}
	owner := strings.TrimSpace(middleware.APIKeyFingerprint(r.Context()))
	if owner == "" {
		return "anonymous"
	}
	return owner
}

func storedVideoJobFromRuntime(job *videoJob) *store.StoredVideoJob {
	if job == nil {
		return nil
	}
	owner := strings.TrimSpace(job.OwnerHash)
	if owner == "" {
		owner = "anonymous"
	}
	errorCode := ""
	errorMessage := ""
	if job.Error != nil {
		errorCode = strings.TrimSpace(fmt.Sprint(job.Error["code"]))
		errorMessage = strings.TrimSpace(fmt.Sprint(job.Error["message"]))
		if errorCode == "<nil>" {
			errorCode = ""
		}
		if errorMessage == "<nil>" {
			errorMessage = ""
		}
	}
	return &store.StoredVideoJob{
		ID: job.ID, OwnerHash: owner, AccountID: job.AccountID, Provider: job.Provider,
		Model: job.Model, Prompt: truncateVideoJobText(job.Prompt, 64<<10),
		Seconds: job.Seconds, Size: job.Size, Quality: job.Quality,
		Status: job.Status, Progress: job.Progress, VideoURL: job.VideoURL,
		ContentPath: job.ContentPath, UpstreamRequestID: job.UpstreamRequestID, RemixedFromID: job.RemixedFromID,
		Operation: job.Operation, StandardAPI: job.StandardAPI, BuildFallback: job.BuildFallback,
		ErrorCode: truncateVideoJobText(errorCode, 256), ErrorMessage: truncateVideoJobText(errorMessage, 4<<10),
		CreatedAt: job.CreatedAt, CompletedAt: job.CompletedAt,
	}
}

func truncateVideoJobText(value string, limit int) string {
	if limit <= 0 || len(value) <= limit {
		return value
	}
	end := limit
	for end > 0 && !utf8.ValidString(value[:end]) {
		end--
	}
	return value[:end]
}

func cloneVideoJob(job *videoJob) *videoJob {
	if job == nil {
		return nil
	}
	cloned := *job
	cloned.InputReferences = append([]string(nil), job.InputReferences...)
	if job.Error != nil {
		cloned.Error = make(map[string]interface{}, len(job.Error))
		for key, value := range job.Error {
			cloned.Error[key] = value
		}
	}
	return &cloned
}

func runtimeVideoJobFromStored(stored *store.StoredVideoJob) *videoJob {
	if stored == nil {
		return nil
	}
	job := &videoJob{
		ID: stored.ID, OwnerHash: stored.OwnerHash, AccountID: stored.AccountID, Provider: stored.Provider,
		Model: stored.Model, Prompt: stored.Prompt,
		Seconds: stored.Seconds, Size: stored.Size, Quality: stored.Quality,
		Status: stored.Status, Progress: stored.Progress, VideoURL: stored.VideoURL,
		ContentPath: stored.ContentPath, UpstreamRequestID: stored.UpstreamRequestID, RemixedFromID: stored.RemixedFromID,
		Operation: stored.Operation, StandardAPI: stored.StandardAPI, BuildFallback: stored.BuildFallback,
		CreatedAt: stored.CreatedAt, CompletedAt: stored.CompletedAt,
	}
	if stored.ErrorCode != "" || stored.ErrorMessage != "" {
		job.Error = map[string]interface{}{"code": stored.ErrorCode, "message": stored.ErrorMessage}
	}
	if !validPersistedVideoContentPath(job.ContentPath) {
		job.ContentPath = ""
	}
	return job
}

func validPersistedVideoContentPath(path string) bool {
	return strings.TrimSpace(path) == "" || validCachedMediaContentPath(path, "video")
}

func (h *Handler) persistVideoJob(ctx context.Context, job *videoJob) {
	if h == nil || h.lb == nil || h.lb.Store == nil || job == nil {
		return
	}
	videoJobsMu.Lock()
	snapshot := storedVideoJobFromRuntime(job)
	videoJobsMu.Unlock()
	if snapshot == nil {
		return
	}
	createdAt := time.Unix(snapshot.CreatedAt, 0)
	if snapshot.CreatedAt <= 0 {
		createdAt = time.Now()
	}
	ttl := time.Until(createdAt.Add(videoJobTTL))
	if ttl <= 0 {
		return
	}
	if ctx == nil {
		ctx = context.Background()
	}
	persistCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Second)
	defer cancel()
	if err := h.lb.Store.SaveStoredVideoJob(persistCtx, snapshot, ttl); err != nil {
		slog.Warn("failed to persist video job", "video_id", snapshot.ID, "status", snapshot.Status, "error", err)
	}
}

func (h *Handler) lookupVideoJob(ctx context.Context, id, owner string) (*videoJob, bool) {
	videoJobsMu.Lock()
	cleanupVideoJobsLocked(time.Now())
	if job, ok := videoJobs[strings.TrimSpace(id)]; ok {
		jobOwner := strings.TrimSpace(job.OwnerHash)
		if jobOwner == "" {
			jobOwner = "anonymous"
		}
		if jobOwner == owner {
			snapshot := cloneVideoJob(job)
			videoJobsMu.Unlock()
			return snapshot, true
		}
	}
	videoJobsMu.Unlock()
	if h == nil || h.lb == nil || h.lb.Store == nil {
		return nil, false
	}
	stored, err := h.lb.Store.GetStoredVideoJob(ctx, id, owner)
	if err != nil {
		return nil, false
	}
	job := runtimeVideoJobFromStored(stored)
	if job == nil {
		return nil, false
	}
	putVideoJob(job)
	return cloneVideoJob(job), true
}

func parseVideosRequest(r *http.Request) (VideosRequest, error) {
	var req VideosRequest
	contentType := strings.ToLower(r.Header.Get("Content-Type"))
	if strings.Contains(contentType, "multipart/form-data") {
		if err := r.ParseMultipartForm(80 << 20); err != nil {
			return req, err
		}
		refs, err := readVideoInputReferenceFiles(r)
		if err != nil {
			return req, err
		}
		req.InputReferences = refs
		fillVideosRequestFromForm(&req, r.Form)
		return req, nil
	}
	if strings.Contains(contentType, "application/x-www-form-urlencoded") {
		if err := r.ParseForm(); err != nil {
			return req, err
		}
		fillVideosRequestFromForm(&req, r.Form)
		return req, nil
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		return req, err
	}
	return req, nil
}

// fillVideosRequestFromForm copies the video request fields from form values.
// Multipart and urlencoded bodies share the same field layout.
func fillVideosRequestFromForm(req *VideosRequest, form url.Values) {
	req.Model = strings.TrimSpace(form.Get("model"))
	req.Prompt = strings.TrimSpace(form.Get("prompt"))
	req.Seconds = parseIntLoose(firstNonEmpty(form.Get("seconds"), form.Get("video_length")), 6)
	req.Size = strings.TrimSpace(firstNonEmpty(form.Get("size"), form.Get("aspect_ratio")))
	req.ResolutionName = strings.TrimSpace(form.Get("resolution_name"))
	req.Preset = strings.TrimSpace(form.Get("preset"))
	for _, key := range []string{"input_reference", "input_references", "input_reference[]"} {
		for _, value := range form[key] {
			if s := strings.TrimSpace(value); s != "" {
				req.InputReferences = append(req.InputReferences, s)
			}
		}
	}
	req.InputReferences = uniqueStrings(req.InputReferences)
}

func readVideoInputReferenceFiles(r *http.Request) ([]string, error) {
	if r.MultipartForm == nil {
		return nil, nil
	}
	var out []string
	for _, key := range []string{"input_reference", "input_reference[]"} {
		for _, fh := range r.MultipartForm.File[key] {
			dataURI, err := uploadFileHeaderToDataURI(fh)
			if err != nil {
				return nil, err
			}
			out = append(out, dataURI)
		}
	}
	return out, nil
}

func uploadFileHeaderToDataURI(fh *multipart.FileHeader) (string, error) {
	file, err := fh.Open()
	if err != nil {
		return "", fmt.Errorf("failed to read input_reference")
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, maxEditImageBytes+1))
	if err != nil {
		return "", fmt.Errorf("failed to read input_reference")
	}
	if len(data) == 0 {
		return "", fmt.Errorf("input_reference cannot be empty")
	}
	if len(data) > maxEditImageBytes {
		return "", fmt.Errorf("input_reference too large. maximum is 50MB")
	}
	mime := strings.TrimSpace(fh.Header.Get("Content-Type"))
	if mime == "" || mime == "application/octet-stream" {
		mime = mimeFromFilename(fh.Filename)
	}
	if !strings.HasPrefix(strings.ToLower(mime), "image/") {
		return "", fmt.Errorf("input_reference must be an image")
	}
	return dataURIFromBytes(mime, data), nil
}

func (h *Handler) HandleVideosCreate(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodPost) {
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 128<<20)
	defer func() {
		if r.MultipartForm != nil {
			_ = r.MultipartForm.RemoveAll()
		}
	}()
	req, err := parseVideosRequest(r)
	if err != nil {
		http.Error(w, "invalid request", http.StatusBadRequest)
		return
	}
	req.Model = normalizeModelID(firstNonEmpty(req.Model, "grok-imagine-video"))
	if !requireAPIKeyModel(w, r, req.Model) {
		return
	}
	req.Prompt = strings.TrimSpace(req.Prompt)
	if req.Prompt == "" {
		http.Error(w, "prompt cannot be empty", http.StatusBadRequest)
		return
	}
	spec, ok := ResolveModel(req.Model)
	if !ok || !spec.IsVideo {
		http.Error(w, fmt.Sprintf("Model %q is not a video model", req.Model), http.StatusBadRequest)
		return
	}
	spec = h.applyPersistedRoute(r.Context(), spec)
	if err := h.ensureModelCapability(r.Context(), req.Model, store.CapabilityVideo); err != nil {
		http.Error(w, modelValidationMessage(req.Model, err), http.StatusBadRequest)
		return
	}
	if spec.Upstream == UpstreamAppChat || (spec.Upstream == UpstreamAuto && !requiresConsoleResponses(spec)) {
		if len(req.InputReferences) > 0 {
			http.Error(w, "web video generation currently supports text-to-video only; use a Console/Build video model for image references", http.StatusBadRequest)
			return
		}
	}
	cfg, err := validateVideoConfigForModel(&VideoConfig{
		VideoLength:    req.Seconds,
		ResolutionName: req.ResolutionName,
		Preset:         req.Preset,
		Size:           firstNonEmpty(req.Size, "720x1280"),
	}, spec, len(req.InputReferences))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	maxReferences := 7
	if spec.Upstream == UpstreamCLI {
		maxReferences = 8
	}
	if len(req.InputReferences) > maxReferences {
		http.Error(w, fmt.Sprintf("input_references supports at most %d images for this model", maxReferences), http.StatusBadRequest)
		return
	}
	if spec.Upstream == UpstreamCLI {
		req.InputReferences, err = h.resolveBuildVideoReferences(r.Context(), req.InputReferences, videoRequestOwner(r))
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
	}
	job := &videoJob{
		ID:              "video_" + randomHex(16),
		Model:           req.Model,
		Prompt:          req.Prompt,
		Seconds:         cfg.VideoLength,
		Size:            firstNonEmpty(cfg.Size, req.Size, cfg.AspectRatio),
		Quality:         "standard",
		CreatedAt:       time.Now().Unix(),
		Status:          "queued",
		Progress:        0,
		InputReferences: req.InputReferences,
		Operation:       "generate",
		OwnerHash:       videoRequestOwner(r),
		PublicBaseURL:   detectPublicBaseURL(r),
	}
	putVideoJob(job)
	h.persistVideoJob(r.Context(), job)
	response := job.toMap()
	go h.runVideoCreateJob(context.Background(), job, spec, cfg)
	writeJSON(w, response)
}

func (h *Handler) runVideoCreateJob(ctx context.Context, job *videoJob, spec ModelSpec, cfg *VideoConfig) {
	update := func(status string, progress int) {
		videoJobsMu.Lock()
		job.Status = status
		if progress >= 0 {
			job.Progress = clampProgress(progress)
		}
		videoJobsMu.Unlock()
		h.persistVideoJob(ctx, job)
	}
	update("in_progress", 1)
	if spec.Upstream == UpstreamCLI && spec.IsVideo {
		h.runBuildVideoCreateJob(ctx, job, spec, cfg)
		return
	}

	// Retry opening a session with account switching — video generation
	// can take minutes and the 429 cooldown may clear while we wait.
	var sess *chatAccountSession
	var sessErr error
	for attempt := 0; attempt < 10; attempt++ {
		sess, sessErr = h.openChatAccountSessionForModel(ctx, spec)
		if sessErr == nil {
			break
		}
		time.Sleep(2 * time.Second)
	}
	if sessErr != nil {
		h.failVideoJob(job, sessErr)
		return
	}
	defer sess.Close()

	attachments := make([]AttachmentInput, 0, len(job.InputReferences))
	for _, ref := range job.InputReferences {
		attachments = append(attachments, AttachmentInput{Type: "image", Data: ref})
	}
	artifact, err := h.runVideoSegments(ctx, sess, spec, job.Prompt, attachments, cfg, nil, func(progress int) {
		update("in_progress", progress)
	})
	if err != nil {
		h.failVideoJob(job, err)
		return
	}
	raw, _, err := h.client.downloadAsset(ctx, sess.token, artifact.URL)
	if err != nil {
		h.failVideoJob(job, err)
		return
	}
	name, err := h.cacheMediaBytes(artifact.URL, "video", raw, "video/mp4")
	if err != nil {
		h.failVideoJob(job, err)
		return
	}
	videoJobsMu.Lock()
	job.Status = "completed"
	job.Progress = 100
	job.CompletedAt = time.Now().Unix()
	job.VideoURL = artifact.URL
	job.ContentPath = filepath.Join(cacheBaseDir, "video", name)
	job.RemixedFromID = artifact.VideoPostID
	videoJobsMu.Unlock()
	h.persistVideoJob(ctx, job)
}

func (h *Handler) failVideoJob(job *videoJob, err error) {
	h.failVideoJobWithCode(job, "video_generation_failed", err)
}

func (h *Handler) failVideoJobWithCode(job *videoJob, code string, err error) {
	videoJobsMu.Lock()
	job.Status = "failed"
	job.Error = map[string]interface{}{
		"code":    code,
		"message": err.Error(),
	}
	videoJobsMu.Unlock()
	h.persistVideoJob(context.Background(), job)
}

func (h *Handler) HandleVideosRetrieve(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodGet) {
		return
	}
	videoID := videoIDFromPath(r.URL.Path)
	if idx := strings.Index(videoID, "/"); idx >= 0 {
		videoID = videoID[:idx]
	}
	job, ok := h.lookupVideoJob(r.Context(), videoID, videoRequestOwner(r))
	if !ok {
		http.Error(w, "video not found", http.StatusNotFound)
		return
	}
	writeJSON(w, job.toMap())
}

func (h *Handler) HandleVideosContent(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodGet) {
		return
	}
	videoID := videoIDFromPath(r.URL.Path)
	job, ok := h.lookupVideoJob(r.Context(), videoID, videoRequestOwner(r))
	if !ok {
		http.Error(w, "video not found", http.StatusNotFound)
		return
	}
	if job.Status != "completed" || strings.TrimSpace(job.ContentPath) == "" {
		http.Error(w, "video content is not ready yet", http.StatusConflict)
		return
	}
	data, err := os.ReadFile(job.ContentPath)
	if err != nil {
		http.Error(w, "video content not found", http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "video/mp4")
	http.ServeContent(w, r, videoID+".mp4", time.Now(), bytes.NewReader(data))
}

func videoIDFromPath(path string) string {
	path = strings.TrimSpace(path)
	for _, prefix := range []string{"/grok/v1/videos/", "/v1/videos/"} {
		if strings.HasPrefix(path, prefix) {
			rest := strings.Trim(strings.TrimPrefix(path, prefix), "/")
			rest = strings.TrimPrefix(rest, "generations/")
			rest = strings.TrimSuffix(rest, "/content")
			rest = strings.Trim(rest, "/")
			if idx := strings.Index(rest, "/"); idx >= 0 {
				rest = rest[:idx]
			}
			return rest
		}
	}
	return ""
}
