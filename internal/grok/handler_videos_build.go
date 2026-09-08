package grok

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path/filepath"
	"strings"
	"time"

	"github.com/goccy/go-json"
)

const maxBuildVideoResponseBytes = 2 << 20

func (h *Handler) runBuildVideoCreateJob(ctx context.Context, job *videoJob, spec ModelSpec, cfg *VideoConfig) {
	baseCtx, cancel := context.WithTimeout(ctx, videoJobTTL)
	defer cancel()
	leaseCtx, lease, acquired, err := h.beginConsoleVideoJobLease(baseCtx, job)
	if err != nil {
		h.failVideoJobWithCode(job, "video_lease_unavailable", err)
		return
	}
	if !acquired {
		h.abandonConsoleVideoJob(job)
		return
	}
	defer lease.Close()

	sess, err := h.openCLIAccountSession(leaseCtx, nil, spec.UpstreamModel)
	if err != nil {
		h.handleConsoleVideoJobError(job, lease, err)
		return
	}
	defer sess.Close()
	videoJobsMu.Lock()
	job.Provider = ProviderBuild
	job.AccountID = sess.acc.ID
	videoJobsMu.Unlock()
	h.persistVideoJob(leaseCtx, job)

	payload, err := buildCLIVideoPayload(job, spec, cfg)
	if err != nil {
		h.handleConsoleVideoJobError(job, lease, err)
		return
	}
	response, err := h.cliClient.doResponsesAt(leaseCtx, sess.acc, "/videos/generations", payload)
	if err != nil {
		if upstreamHTTPResponseStatus(err) != http.StatusForbidden {
			h.handleConsoleVideoJobError(job, lease, err)
			return
		}
		uploadURL, uploadErr := h.registerBuildVideoUpload(job)
		if uploadErr != nil {
			h.handleConsoleVideoJobError(job, lease, fmt.Errorf("Build video primary route denied and fallback is unavailable: %w", uploadErr))
			return
		}
		fallbackPayload := cloneStringInterfaceMap(payload)
		fallbackPayload["model"] = "grok-imagine-video-1.5-preview"
		fallbackPayload["output"] = map[string]interface{}{"upload_url": uploadURL}
		response, err = h.cliClient.doFallbackRequest(leaseCtx, sess.acc, http.MethodPost, "/videos/generations", fallbackPayload)
		if err != nil {
			h.handleConsoleVideoJobError(job, lease, err)
			return
		}
		videoJobsMu.Lock()
		job.BuildFallback = true
		videoJobsMu.Unlock()
		h.persistVideoJob(leaseCtx, job)
	}
	body, err := readBuildVideoResponse(response)
	if err != nil {
		h.handleConsoleVideoJobError(job, lease, err)
		return
	}
	requestID, err := parseBuildVideoCreate(body)
	if err != nil {
		h.handleConsoleVideoJobError(job, lease, err)
		return
	}
	videoJobsMu.Lock()
	job.UpstreamRequestID = requestID
	job.Progress = max(job.Progress, 1)
	videoJobsMu.Unlock()
	h.persistVideoJob(leaseCtx, job)
	h.pollBuildVideoJob(leaseCtx, lease, job, sess, requestID)
}

func parseBuildVideoCreate(body []byte) (string, error) {
	var payload struct {
		RequestID string `json:"request_id"`
		ID        string `json:"id"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return "", fmt.Errorf("parse Build video create response: %w", err)
	}
	requestID := firstNonEmpty(strings.TrimSpace(payload.RequestID), strings.TrimSpace(payload.ID))
	if requestID == "" {
		return "", fmt.Errorf("Build video create response has no request_id")
	}
	return requestID, nil
}

func buildCLIVideoPayload(job *videoJob, spec ModelSpec, cfg *VideoConfig) (map[string]interface{}, error) {
	if job == nil || cfg == nil {
		return nil, fmt.Errorf("Build video request is unavailable")
	}
	payload := map[string]interface{}{
		"model":  firstNonEmpty(strings.TrimSpace(spec.UpstreamModel), "grok-imagine-video-1.5"),
		"prompt": strings.TrimSpace(job.Prompt), "duration": cfg.VideoLength,
		"aspect_ratio": cfg.AspectRatio, "resolution": cfg.ResolutionName,
	}
	if len(job.InputReferences) > 8 {
		return nil, fmt.Errorf("Build video supports at most 8 reference images")
	}
	if len(job.InputReferences) > 0 {
		references := make([]map[string]interface{}, 0, len(job.InputReferences))
		var total int64
		for _, value := range job.InputReferences {
			value = strings.TrimSpace(value)
			size, err := validateBuildVideoReference(value)
			if err != nil {
				return nil, err
			}
			total += size
			if total > maxResolvedMediaBytes {
				return nil, fmt.Errorf("combined inline reference images exceed 32 MiB")
			}
			references = append(references, map[string]interface{}{"image_url": value})
		}
		payload["reference_images"] = references
	}
	return payload, nil
}

func publicHTTPSURL(rawURL string) bool {
	parsed, err := url.Parse(strings.TrimSpace(rawURL))
	return err == nil && strings.EqualFold(parsed.Scheme, "https") && parsed.Host != "" && parsed.User == nil
}

func readBuildVideoResponse(response *http.Response) ([]byte, error) {
	if response == nil || response.Body == nil {
		return nil, fmt.Errorf("Build video returned an empty response")
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, maxBuildVideoResponseBytes+1))
	if err != nil {
		return nil, err
	}
	if len(body) > maxBuildVideoResponseBytes {
		return nil, fmt.Errorf("Build video response exceeds 2 MiB")
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return nil, fmt.Errorf("Build video returned status %d", response.StatusCode)
	}
	return body, nil
}

func (h *Handler) pollBuildVideoJob(ctx context.Context, lease *consoleVideoJobLease, job *videoJob, sess *chatAccountSession, requestID string) {
	for {
		if job.BuildFallback && h != nil && h.lb != nil && h.lb.Store != nil {
			owner := firstNonEmpty(strings.TrimSpace(job.OwnerHash), "anonymous")
			if stored, storedErr := h.lb.Store.GetStoredVideoJob(ctx, job.ID, owner); storedErr == nil && stored != nil && stored.Status == "completed" && strings.TrimSpace(stored.ContentPath) != "" {
				videoJobsMu.Lock()
				job.Status, job.Progress, job.ContentPath, job.CompletedAt = stored.Status, stored.Progress, stored.ContentPath, stored.CompletedAt
				videoJobsMu.Unlock()
				return
			}
		}
		videoJobsMu.Lock()
		completedByUpload := job.Status == "completed" && strings.TrimSpace(job.ContentPath) != ""
		videoJobsMu.Unlock()
		if completedByUpload {
			return
		}
		if lease != nil && lease.Lost() {
			h.abandonConsoleVideoJob(job)
			h.scheduleStoredConsoleVideoRetry(job)
			return
		}
		var response *http.Response
		var err error
		if job.BuildFallback {
			response, err = h.cliClient.doFallbackRequest(ctx, sess.acc, http.MethodGet, "/videos/"+url.PathEscape(requestID), nil)
		} else {
			response, err = h.cliClient.doResponseResource(ctx, sess.acc, http.MethodGet, "/videos/"+url.PathEscape(requestID), "")
		}
		if err != nil {
			h.handleConsoleVideoJobError(job, lease, err)
			return
		}
		body, err := readBuildVideoResponse(response)
		if err != nil {
			h.handleConsoleVideoJobError(job, lease, err)
			return
		}
		videoURL, done, err := parseConsoleVideoStatus(body, func(progress int) {
			h.updateVideoJobProgress(job, progress)
		})
		if err != nil {
			h.handleConsoleVideoJobError(job, lease, err)
			return
		}
		if done {
			if job.BuildFallback && strings.TrimSpace(videoURL) == "" {
				select {
				case <-ctx.Done():
					h.handleConsoleVideoJobError(job, lease, ctx.Err())
					return
				case <-time.After(consoleVideoPollInterval):
					continue
				}
			}
			h.completeBuildVideoJob(ctx, lease, job, videoURL)
			return
		}
		select {
		case <-ctx.Done():
			h.handleConsoleVideoJobError(job, lease, ctx.Err())
			return
		case <-time.After(consoleVideoPollInterval):
		}
	}
}

func (h *Handler) completeBuildVideoJob(ctx context.Context, lease *consoleVideoJobLease, job *videoJob, videoURL string) {
	if lease != nil && lease.Lost() {
		h.abandonConsoleVideoJob(job)
		h.scheduleStoredConsoleVideoRetry(job)
		return
	}
	raw, mimeType, err := h.cliClient.downloadTrustedBuildVideo(ctx, videoURL)
	if err != nil {
		h.handleConsoleVideoJobError(job, lease, err)
		return
	}
	name, err := h.cacheMediaBytes(videoURL, "video", raw, mimeType)
	if err != nil {
		h.handleConsoleVideoJobError(job, lease, err)
		return
	}
	videoJobsMu.Lock()
	job.Status = "completed"
	job.Progress = 100
	job.CompletedAt = time.Now().Unix()
	job.VideoURL = videoURL
	job.ContentPath = filepath.Join(cacheBaseDir, "video", name)
	videoJobsMu.Unlock()
	h.persistVideoJob(ctx, job)
}

func (h *Handler) resumeStoredBuildVideoJob(job *videoJob, timeout time.Duration) {
	if job == nil {
		return
	}
	baseCtx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	leaseCtx, lease, acquired, err := h.beginConsoleVideoJobLease(baseCtx, job)
	if err != nil || !acquired {
		h.abandonConsoleVideoJob(job)
		h.scheduleStoredConsoleVideoRetry(job)
		return
	}
	defer lease.Close()
	if job.AccountID <= 0 || strings.TrimSpace(job.UpstreamRequestID) == "" {
		h.failVideoJobWithCode(job, "video_resume_unavailable", fmt.Errorf("Build video task was interrupted before it became resumable"))
		return
	}
	sess, err := h.openCLIAccountSessionByID(leaseCtx, job.AccountID, "grok-imagine-video-1.5")
	if err != nil {
		h.failVideoJobWithCode(job, "video_resume_unavailable", err)
		return
	}
	defer sess.Close()
	h.updateVideoJobProgress(job, max(1, job.Progress))
	h.pollBuildVideoJob(leaseCtx, lease, job, sess, job.UpstreamRequestID)
}

func (c *CLIClient) downloadTrustedBuildVideo(ctx context.Context, rawURL string) ([]byte, string, error) {
	if c == nil || c.httpClient == nil || !trustedBuildVideoURL(rawURL) {
		return nil, "", fmt.Errorf("Build video content URL is not trusted")
	}
	client := *c.httpClient
	previousRedirect := c.httpClient.CheckRedirect
	client.CheckRedirect = func(request *http.Request, via []*http.Request) error {
		if !trustedBuildVideoURL(request.URL.String()) {
			return fmt.Errorf("Build video redirected to an untrusted URL")
		}
		if previousRedirect != nil {
			return previousRedirect(request, via)
		}
		if len(via) >= 10 {
			return fmt.Errorf("too many Build video redirects")
		}
		return nil
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, "", err
	}
	request.Header.Set("Accept", "video/*,*/*;q=0.8")
	response, err := client.Do(request)
	if err != nil {
		return nil, "", err
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return nil, "", fmt.Errorf("Build video download returned status %d", response.StatusCode)
	}
	raw, err := io.ReadAll(io.LimitReader(response.Body, maxConsoleVideoAssetBytes+1))
	if err != nil {
		return nil, "", err
	}
	if len(raw) == 0 || len(raw) > maxConsoleVideoAssetBytes {
		return nil, "", fmt.Errorf("Build video is empty or exceeds 512 MiB")
	}
	mimeType := strings.ToLower(strings.TrimSpace(strings.Split(response.Header.Get("Content-Type"), ";")[0]))
	if mimeType == "" || mimeType == "application/octet-stream" {
		mimeType = "video/mp4"
	}
	if !strings.HasPrefix(mimeType, "video/") {
		return nil, "", fmt.Errorf("Build video has invalid Content-Type %q", mimeType)
	}
	return raw, mimeType, nil
}

func trustedBuildVideoURL(rawURL string) bool {
	parsed, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil || parsed.Scheme != "https" || parsed.User != nil || parsed.Hostname() == "" {
		return false
	}
	host := strings.ToLower(parsed.Hostname())
	for _, suffix := range []string{"x.ai", "grok.com", "assets.grok.com", "cdn.x.ai", "videos.x.ai"} {
		if host == suffix || strings.HasSuffix(host, "."+suffix) {
			return true
		}
	}
	return false
}
