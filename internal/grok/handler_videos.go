package grok

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/goccy/go-json"
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

func getVideoJob(id string) (*videoJob, bool) {
	id = strings.TrimSpace(id)
	if id == "" {
		return nil, false
	}
	videoJobsMu.Lock()
	defer videoJobsMu.Unlock()
	cleanupVideoJobsLocked(time.Now())
	job, ok := videoJobs[id]
	return job, ok
}

func (j *videoJob) toMap() map[string]interface{} {
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

func parseVideosRequest(r *http.Request) (VideosRequest, error) {
	var req VideosRequest
	contentType := strings.ToLower(r.Header.Get("Content-Type"))
	if strings.Contains(contentType, "multipart/form-data") {
		if err := r.ParseMultipartForm(80 << 20); err != nil {
			return req, err
		}
		req.Model = strings.TrimSpace(r.FormValue("model"))
		req.Prompt = strings.TrimSpace(r.FormValue("prompt"))
		req.Seconds = parseIntLoose(firstNonEmpty(r.FormValue("seconds"), r.FormValue("video_length")), 6)
		req.Size = strings.TrimSpace(firstNonEmpty(r.FormValue("size"), r.FormValue("aspect_ratio")))
		req.ResolutionName = strings.TrimSpace(r.FormValue("resolution_name"))
		req.Preset = strings.TrimSpace(r.FormValue("preset"))
		refs, err := readVideoInputReferenceFiles(r)
		if err != nil {
			return req, err
		}
		req.InputReferences = refs
		for _, key := range []string{"input_reference", "input_references", "input_reference[]"} {
			for _, value := range r.Form[key] {
				if s := strings.TrimSpace(value); s != "" {
					req.InputReferences = append(req.InputReferences, s)
				}
			}
		}
		req.InputReferences = uniqueStrings(req.InputReferences)
		return req, nil
	}
	if strings.Contains(contentType, "application/x-www-form-urlencoded") {
		if err := r.ParseForm(); err != nil {
			return req, err
		}
		req.Model = strings.TrimSpace(r.FormValue("model"))
		req.Prompt = strings.TrimSpace(r.FormValue("prompt"))
		req.Seconds = parseIntLoose(firstNonEmpty(r.FormValue("seconds"), r.FormValue("video_length")), 6)
		req.Size = strings.TrimSpace(firstNonEmpty(r.FormValue("size"), r.FormValue("aspect_ratio")))
		req.ResolutionName = strings.TrimSpace(r.FormValue("resolution_name"))
		req.Preset = strings.TrimSpace(r.FormValue("preset"))
		for _, key := range []string{"input_reference", "input_references", "input_reference[]"} {
			for _, value := range r.Form[key] {
				if s := strings.TrimSpace(value); s != "" {
					req.InputReferences = append(req.InputReferences, s)
				}
			}
		}
		req.InputReferences = uniqueStrings(req.InputReferences)
		return req, nil
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		return req, err
	}
	return req, nil
}

func readVideoInputReferenceFiles(r *http.Request) ([]string, error) {
	if r.MultipartForm == nil {
		return nil, nil
	}
	var out []string
	for _, key := range []string{"input_reference", "input_reference[]"} {
		for _, fh := range r.MultipartForm.File[key] {
			if len(out) >= 7 {
				return out, nil
			}
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

func videoConfigFromVideosRequest(req VideosRequest) (*VideoConfig, error) {
	cfg := &VideoConfig{
		AspectRatio:    "",
		VideoLength:    req.Seconds,
		ResolutionName: req.ResolutionName,
		Preset:         req.Preset,
		Size:           req.Size,
	}
	if strings.TrimSpace(cfg.Size) == "" {
		cfg.Size = "720x1280"
	}
	return validateVideoConfig(cfg)
}

func (h *Handler) HandleVideosCreate(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	req, err := parseVideosRequest(r)
	if err != nil {
		http.Error(w, "invalid request", http.StatusBadRequest)
		return
	}
	req.Model = normalizeModelID(firstNonEmpty(req.Model, "grok-imagine-video"))
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
	if err := h.ensureModelEnabled(r.Context(), req.Model); err != nil {
		http.Error(w, modelValidationMessage(req.Model, err), http.StatusBadRequest)
		return
	}
	cfg, err := videoConfigFromVideosRequest(req)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if len(req.InputReferences) > 7 {
		req.InputReferences = req.InputReferences[:7]
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
	}
	putVideoJob(job)
	go h.runVideoCreateJob(context.Background(), job, spec, cfg)
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(job.toMap())
}

func (h *Handler) runVideoCreateJob(ctx context.Context, job *videoJob, spec ModelSpec, cfg *VideoConfig) {
	update := func(status string, progress int) {
		videoJobsMu.Lock()
		defer videoJobsMu.Unlock()
		job.Status = status
		if progress >= 0 {
			job.Progress = clampProgress(progress)
		}
	}
	update("in_progress", 1)

	sess, err := h.openChatAccountSessionForModel(ctx, spec)
	if err != nil {
		h.failVideoJob(job, err)
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
}

func (h *Handler) failVideoJob(job *videoJob, err error) {
	videoJobsMu.Lock()
	defer videoJobsMu.Unlock()
	job.Status = "failed"
	job.Error = map[string]interface{}{
		"code":    "video_generation_failed",
		"message": err.Error(),
	}
}

func (h *Handler) HandleVideosRetrieve(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	videoID := videoIDFromPath(r.URL.Path)
	if idx := strings.Index(videoID, "/"); idx >= 0 {
		videoID = videoID[:idx]
	}
	job, ok := getVideoJob(videoID)
	if !ok {
		http.Error(w, "video not found", http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(job.toMap())
}

func (h *Handler) HandleVideosContent(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	videoID := videoIDFromPath(r.URL.Path)
	job, ok := getVideoJob(videoID)
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
