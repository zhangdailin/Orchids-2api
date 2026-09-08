package grok

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/goccy/go-json"

	"orchids-api/internal/store"
)

const (
	maxConsoleVideoRequestBytes  = 64 << 20
	maxConsoleVideoResponseBytes = 1 << 20
	maxConsoleVideoAssetBytes    = 512 << 20
	consoleVideoPollInterval     = 2 * time.Second
)

var consoleVideoLeaseTTL = 30 * time.Second

type consoleVideoJobLease struct {
	h      *Handler
	jobID  string
	owner  string
	holder string
	ttl    time.Duration
	cancel context.CancelFunc
	stop   chan struct{}
	done   chan struct{}
	noop   bool
	lost   atomic.Bool
	once   sync.Once
}

func (l *consoleVideoJobLease) Lost() bool { return l != nil && l.lost.Load() }

func (l *consoleVideoJobLease) Close() {
	if l == nil {
		return
	}
	l.once.Do(func() {
		if !l.noop {
			close(l.stop)
			<-l.done
		}
		l.cancel()
		if l.noop || l.Lost() || l.h == nil || l.h.lb == nil || l.h.lb.Store == nil {
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_, _ = l.h.lb.Store.ReleaseVideoJobLease(ctx, l.jobID, l.owner, l.holder)
	})
}

func (h *Handler) beginConsoleVideoJobLease(parent context.Context, job *videoJob) (context.Context, *consoleVideoJobLease, bool, error) {
	if parent == nil {
		parent = context.Background()
	}
	leaseCtx, cancel := context.WithCancel(parent)
	owner := strings.TrimSpace(job.OwnerHash)
	if owner == "" {
		owner = "anonymous"
	}
	if h == nil || h.lb == nil || h.lb.Store == nil {
		return leaseCtx, &consoleVideoJobLease{cancel: cancel, noop: true}, true, nil
	}
	holder := strings.TrimSpace(h.instanceID) + ":" + randomHex(8)
	if strings.Trim(holder, ":") == "" {
		holder = "grok-video:" + randomUUID()
	}
	acquireCtx, acquireCancel := context.WithTimeout(context.WithoutCancel(parent), 3*time.Second)
	acquired, err := h.lb.Store.AcquireVideoJobLease(acquireCtx, job.ID, owner, holder, consoleVideoLeaseTTL)
	acquireCancel()
	if err != nil || !acquired {
		cancel()
		return nil, nil, acquired, err
	}
	lease := &consoleVideoJobLease{
		h: h, jobID: job.ID, owner: owner, holder: holder, ttl: consoleVideoLeaseTTL, cancel: cancel,
		stop: make(chan struct{}), done: make(chan struct{}),
	}
	go lease.heartbeat()
	return leaseCtx, lease, true, nil
}

func (l *consoleVideoJobLease) heartbeat() {
	defer close(l.done)
	ticker := time.NewTicker(l.ttl / 3)
	defer ticker.Stop()
	for {
		select {
		case <-l.stop:
			return
		case <-ticker.C:
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			refreshed, err := l.h.lb.Store.RefreshVideoJobLease(ctx, l.jobID, l.owner, l.holder, l.ttl)
			cancel()
			if err != nil || !refreshed {
				l.lost.Store(true)
				l.cancel()
				return
			}
		}
	}
}

type consoleVideoOperation string

const (
	consoleVideoGenerate consoleVideoOperation = "generate"
	consoleVideoEdit     consoleVideoOperation = "edit"
	consoleVideoExtend   consoleVideoOperation = "extend"
)

type consoleVideoInput struct {
	URL    string `json:"url"`
	FileID string `json:"file_id"`
}

type consoleVideoReferenceAudio struct {
	VoiceID string `json:"voice_id"`
}

type consoleVideoAPIRequest struct {
	Model           string                       `json:"model"`
	Prompt          string                       `json:"prompt"`
	Duration        json.RawMessage              `json:"duration"`
	AspectRatio     string                       `json:"aspect_ratio"`
	Resolution      string                       `json:"resolution"`
	Image           *consoleVideoInput           `json:"image"`
	ReferenceImages []consoleVideoInput          `json:"reference_images"`
	ReferenceAudios []consoleVideoReferenceAudio `json:"reference_audios"`
	Video           *consoleVideoInput           `json:"video"`
	Output          json.RawMessage              `json:"output"`
	StorageOptions  json.RawMessage              `json:"storage_options"`
}

type preparedConsoleVideoRequest struct {
	model      string
	prompt     string
	duration   int
	resolution string
	payload    map[string]interface{}
}

type consoleVideoRequestError struct {
	code    string
	message string
}

func (e *consoleVideoRequestError) Error() string { return e.message }

func (h *Handler) HandleConsoleVideosGenerate(w http.ResponseWriter, r *http.Request) {
	h.handleConsoleVideoCreate(w, r, consoleVideoGenerate)
}

func (h *Handler) HandleConsoleVideosEdit(w http.ResponseWriter, r *http.Request) {
	h.handleConsoleVideoCreate(w, r, consoleVideoEdit)
}

func (h *Handler) HandleConsoleVideosExtend(w http.ResponseWriter, r *http.Request) {
	h.handleConsoleVideoCreate(w, r, consoleVideoExtend)
}

func (h *Handler) handleConsoleVideoCreate(w http.ResponseWriter, r *http.Request, operation consoleVideoOperation) {
	if !requireMethod(w, r, http.MethodPost) {
		return
	}
	mediaType, _, contentTypeErr := mime.ParseMediaType(strings.TrimSpace(r.Header.Get("Content-Type")))
	if contentTypeErr != nil || !strings.EqualFold(mediaType, "application/json") {
		writeResponsesAPIError(w, http.StatusUnsupportedMediaType, "invalid_request", "standard video operations require application/json")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxConsoleVideoRequestBytes)
	body, err := io.ReadAll(r.Body)
	if err != nil {
		writeResponsesAPIError(w, http.StatusRequestEntityTooLarge, "request_too_large", "video request exceeds 64 MiB")
		return
	}
	var request consoleVideoAPIRequest
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&request) != nil {
		writeResponsesAPIError(w, http.StatusBadRequest, "invalid_request", "invalid video JSON request")
		return
	}
	var trailing interface{}
	if err := decoder.Decode(&trailing); err != io.EOF {
		writeResponsesAPIError(w, http.StatusBadRequest, "invalid_request", "video request must contain one JSON object")
		return
	}
	prepared, err := prepareConsoleVideoRequest(request, operation)
	if err != nil {
		code := "invalid_request"
		var requestErr *consoleVideoRequestError
		if errors.As(err, &requestErr) && strings.TrimSpace(requestErr.code) != "" {
			code = requestErr.code
		}
		writeResponsesAPIError(w, http.StatusBadRequest, code, err.Error())
		return
	}
	if !requireAPIKeyModel(w, r, prepared.model) {
		return
	}
	spec, ok := ResolveModel(prepared.model)
	if !ok || !spec.IsVideo {
		writeResponsesAPIError(w, http.StatusBadRequest, "invalid_model", fmt.Sprintf("model %s does not support video", prepared.model))
		return
	}
	if err := h.ensureModelCapability(r.Context(), prepared.model, store.CapabilityVideo); err != nil {
		writeResponsesAPIError(w, http.StatusBadRequest, "invalid_model", modelValidationMessage(prepared.model, err))
		return
	}
	if err := h.resolveConsoleVideoFileIDs(r.Context(), prepared.payload, videoRequestOwner(r)); err != nil {
		writeResponsesAPIError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	sess, err := h.openConsoleVideoAccountSession(r.Context(), prepared.model)
	if err != nil {
		writeResponsesAPIError(w, http.StatusServiceUnavailable, "account_unavailable", "no available Grok Console video account: "+err.Error())
		return
	}
	job := &videoJob{
		ID:          "video_" + randomHex(16),
		Model:       prepared.model,
		Prompt:      prepared.prompt,
		Seconds:     prepared.duration,
		Size:        prepared.resolution,
		Quality:     prepared.resolution,
		CreatedAt:   time.Now().Unix(),
		Status:      "queued",
		Progress:    0,
		Operation:   string(operation),
		StandardAPI: true,
		OwnerHash:   videoRequestOwner(r),
		Provider:    ProviderConsole,
	}
	if sess.acc != nil {
		job.AccountID = sess.acc.ID
	}
	baseCtx, baseCancel := context.WithTimeout(context.Background(), videoJobTTL)
	leaseCtx, lease, acquired, leaseErr := h.beginConsoleVideoJobLease(baseCtx, job)
	if leaseErr != nil || !acquired {
		baseCancel()
		sess.Close()
		writeResponsesAPIError(w, http.StatusServiceUnavailable, "video_lease_unavailable", "failed to acquire video task lease")
		return
	}
	putVideoJob(job)
	h.persistVideoJob(r.Context(), job)
	go func() {
		defer baseCancel()
		h.runConsoleVideoJobWithLease(leaseCtx, lease, job, sess, operation, prepared.payload)
	}()
	writeJSON(w, map[string]interface{}{"request_id": job.ID})
}

func prepareConsoleVideoRequest(request consoleVideoAPIRequest, operation consoleVideoOperation) (preparedConsoleVideoRequest, error) {
	model := normalizeModelID(request.Model)
	if model == "" {
		return preparedConsoleVideoRequest{}, fmt.Errorf("model is required")
	}
	if hasConsoleVideoJSONValue(request.Output) {
		return preparedConsoleVideoRequest{}, &consoleVideoRequestError{code: "unsupported_parameter", message: "output.upload_url is not supported"}
	}
	if hasConsoleVideoJSONValue(request.StorageOptions) {
		return preparedConsoleVideoRequest{}, &consoleVideoRequestError{code: "unsupported_parameter", message: "storage_options is not supported"}
	}
	prompt := strings.TrimSpace(request.Prompt)
	payload := map[string]interface{}{"model": model}
	prepared := preparedConsoleVideoRequest{model: model, prompt: prompt, payload: payload}
	if operation != consoleVideoGenerate && model != "grok-imagine-video" {
		return preparedConsoleVideoRequest{}, fmt.Errorf("%s only supports grok-imagine-video", operation)
	}

	if operation == consoleVideoGenerate {
		duration, err := parseConsoleVideoDuration(request.Duration, 8)
		if err != nil || duration < 1 || duration > 15 {
			return preparedConsoleVideoRequest{}, fmt.Errorf("duration must be an integer from 1 to 15")
		}
		aspectRatio := strings.TrimSpace(request.AspectRatio)
		if aspectRatio == "" {
			aspectRatio = "16:9"
		}
		if !validConsoleVideoAspectRatio(aspectRatio) {
			return preparedConsoleVideoRequest{}, fmt.Errorf("aspect_ratio must be one of 1:1, 16:9, 9:16, 4:3, 3:4, 3:2, or 2:3")
		}
		resolution := strings.ToLower(strings.TrimSpace(request.Resolution))
		if resolution == "" {
			resolution = "720p"
		}
		if resolution != "480p" && resolution != "720p" && resolution != "1080p" {
			return preparedConsoleVideoRequest{}, fmt.Errorf("resolution must be 480p, 720p, or 1080p")
		}
		if resolution == "1080p" && model != "grok-imagine-video-1.5" {
			return preparedConsoleVideoRequest{}, fmt.Errorf("%s does not support 1080p", model)
		}
		imageURL := ""
		if request.Image != nil {
			value, err := consoleVideoInputURL(*request.Image, "image", "image")
			if err != nil {
				return preparedConsoleVideoRequest{}, err
			}
			imageURL = value
		}
		if len(request.ReferenceImages) > 7 {
			return preparedConsoleVideoRequest{}, fmt.Errorf("reference_images cannot exceed 7 items")
		}
		references := make([]map[string]interface{}, 0, len(request.ReferenceImages))
		for _, input := range request.ReferenceImages {
			value, err := consoleVideoInputURL(input, "reference_images", "image")
			if err != nil {
				return preparedConsoleVideoRequest{}, err
			}
			references = append(references, map[string]interface{}{"url": value})
		}
		if len(request.ReferenceAudios) > 3 {
			return preparedConsoleVideoRequest{}, fmt.Errorf("reference_audios cannot exceed 3 items")
		}
		audios := make([]map[string]interface{}, 0, len(request.ReferenceAudios))
		for index, input := range request.ReferenceAudios {
			voiceID := strings.TrimSpace(input.VoiceID)
			if voiceID == "" {
				return preparedConsoleVideoRequest{}, fmt.Errorf("reference_audios[%d].voice_id is required", index)
			}
			audios = append(audios, map[string]interface{}{"voice_id": voiceID})
		}
		if imageURL != "" && (len(references) > 0 || len(audios) > 0) {
			return preparedConsoleVideoRequest{}, fmt.Errorf("image cannot be combined with reference_images or reference_audios")
		}
		hasReferences := len(references) > 0 || len(audios) > 0
		if hasReferences && prompt == "" {
			return preparedConsoleVideoRequest{}, fmt.Errorf("reference video generation requires prompt")
		}
		if hasReferences && resolution == "1080p" {
			return preparedConsoleVideoRequest{}, fmt.Errorf("reference video generation supports at most 720p")
		}
		if model == "grok-imagine-video" && len(references) > 0 && duration > 10 {
			return preparedConsoleVideoRequest{}, fmt.Errorf("grok-imagine-video reference generation supports at most 10 seconds")
		}
		if prompt == "" && imageURL == "" && !hasReferences {
			return preparedConsoleVideoRequest{}, fmt.Errorf("text video generation requires prompt; image generation may omit it")
		}
		if request.Video != nil {
			return preparedConsoleVideoRequest{}, fmt.Errorf("video generation does not accept video input")
		}
		payload["duration"] = duration
		payload["aspect_ratio"] = aspectRatio
		payload["resolution"] = resolution
		if prompt != "" {
			payload["prompt"] = prompt
		}
		if imageURL != "" {
			payload["image"] = map[string]interface{}{"url": imageURL}
		}
		if len(references) > 0 {
			payload["reference_images"] = references
		}
		if len(audios) > 0 {
			payload["reference_audios"] = audios
		}
		prepared.duration = duration
		prepared.resolution = resolution
		return prepared, nil
	}

	if prompt == "" {
		return preparedConsoleVideoRequest{}, fmt.Errorf("video %s requires prompt", operation)
	}
	if request.Video == nil {
		return preparedConsoleVideoRequest{}, fmt.Errorf("video %s requires video", operation)
	}
	videoURL, err := consoleVideoInputURL(*request.Video, "video", "video")
	if err != nil {
		return preparedConsoleVideoRequest{}, err
	}
	if request.Image != nil || len(request.ReferenceImages) > 0 || len(request.ReferenceAudios) > 0 {
		return preparedConsoleVideoRequest{}, fmt.Errorf("video %s does not accept image, reference_images, or reference_audios", operation)
	}
	if strings.TrimSpace(request.AspectRatio) != "" || strings.TrimSpace(request.Resolution) != "" {
		return preparedConsoleVideoRequest{}, fmt.Errorf("video %s does not accept aspect_ratio or resolution", operation)
	}
	payload["prompt"] = prompt
	payload["video"] = map[string]interface{}{"url": videoURL}
	if operation == consoleVideoEdit {
		if hasConsoleVideoJSONValue(request.Duration) {
			return preparedConsoleVideoRequest{}, fmt.Errorf("video edit does not support duration")
		}
		return prepared, nil
	}
	duration, err := parseConsoleVideoDuration(request.Duration, 6)
	if err != nil || duration < 2 || duration > 10 {
		return preparedConsoleVideoRequest{}, fmt.Errorf("video extension duration must be an integer from 2 to 10")
	}
	payload["duration"] = duration
	prepared.duration = duration
	return prepared, nil
}

func consoleVideoInputURL(input consoleVideoInput, field, mediaType string) (string, error) {
	urlValue := strings.TrimSpace(input.URL)
	fileID := strings.TrimSpace(input.FileID)
	if (urlValue == "") == (fileID == "") {
		return "", fmt.Errorf("%s must provide exactly one of url or file_id", field)
	}
	if fileID != "" {
		if !validMediaInputID(fileID) {
			return "", fmt.Errorf("%s.file_id is invalid", field)
		}
		return localMediaInputPrefix + mediaType + ":" + fileID, nil
	}
	if !validConsoleVideoMediaInputURL(urlValue, mediaType) {
		return "", fmt.Errorf("%s.url must be an HTTPS URL or %s data URL", field, mediaType)
	}
	return urlValue, nil
}

func validConsoleVideoMediaInputURL(value, mediaType string) bool {
	value = strings.TrimSpace(value)
	lower := strings.ToLower(value)
	if strings.HasPrefix(lower, "data:"+mediaType+"/") {
		return strings.Contains(lower, ";base64,")
	}
	parsed, err := url.Parse(value)
	return err == nil && parsed.Scheme == "https" && parsed.Host != "" && parsed.User == nil
}

func parseConsoleVideoDuration(raw json.RawMessage, defaultValue int) (int, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return defaultValue, nil
	}
	var number int
	if json.Unmarshal(trimmed, &number) == nil {
		return number, nil
	}
	var text string
	if json.Unmarshal(trimmed, &text) != nil {
		return 0, fmt.Errorf("duration must be an integer or integer string")
	}
	parsed, err := strconv.Atoi(strings.TrimSpace(text))
	if err != nil {
		return 0, fmt.Errorf("duration must be an integer or integer string")
	}
	return parsed, nil
}

func hasConsoleVideoJSONValue(raw json.RawMessage) bool {
	trimmed := bytes.TrimSpace(raw)
	return len(trimmed) > 0 && !bytes.Equal(trimmed, []byte("null"))
}

func validConsoleVideoAspectRatio(value string) bool {
	switch value {
	case "1:1", "16:9", "9:16", "4:3", "3:4", "3:2", "2:3":
		return true
	default:
		return false
	}
}

func (h *Handler) openConsoleVideoAccountSession(ctx context.Context, model string) (*chatAccountSession, error) {
	if h == nil || h.lb == nil {
		return nil, fmt.Errorf("load balancer not configured")
	}
	account, err := h.lb.GetNextAccountExcludingByChannelWithTrackerFilter(ctx, nil, "grok", h.connTracker, func(account *store.Account) bool {
		return isGrokConsoleAccount(account) && AccountSupportsModel(account, model) && h.routeAllowsAccount(ctx, model, account.ID)
	})
	if err != nil {
		return nil, err
	}
	token := grokSSOTokenRaw(account)
	if NormalizeSSOToken(token) == "" {
		return nil, fmt.Errorf("grok console account token is empty")
	}
	release, reserved := h.reserveAccount(account)
	if !reserved {
		return nil, fmt.Errorf("grok console account is at its concurrency limit")
	}
	return &chatAccountSession{acc: account, token: token, release: release}, nil
}

func (h *Handler) recoverStoredConsoleVideoJobs(ctx context.Context) {
	if h == nil || h.lb == nil || h.lb.Store == nil {
		return
	}
	storedJobs, err := h.lb.Store.ListStoredVideoJobs(ctx)
	if err != nil {
		return
	}
	for _, stored := range storedJobs {
		status := strings.ToLower(strings.TrimSpace(stored.Status))
		if status != "queued" && status != "in_progress" && status != "pending" && status != "processing" {
			continue
		}
		videoJobsMu.Lock()
		_, alreadyRunning := videoJobs[stored.ID]
		videoJobsMu.Unlock()
		if alreadyRunning {
			continue
		}
		job := runtimeVideoJobFromStored(stored)
		if job == nil {
			continue
		}
		putVideoJob(job)
		remaining := time.Until(time.Unix(job.CreatedAt, 0).Add(videoJobTTL))
		if remaining <= 0 {
			continue
		}
		go h.resumeStoredVideoJob(job, remaining)
	}
}

func (h *Handler) abandonConsoleVideoJob(job *videoJob) {
	if job == nil {
		return
	}
	videoJobsMu.Lock()
	if current, ok := videoJobs[job.ID]; ok && current == job {
		delete(videoJobs, job.ID)
	}
	videoJobsMu.Unlock()
}

func (h *Handler) handleConsoleVideoJobError(job *videoJob, lease *consoleVideoJobLease, err error) {
	if lease != nil && lease.Lost() {
		h.abandonConsoleVideoJob(job)
		h.scheduleStoredConsoleVideoRetry(job)
		return
	}
	h.failVideoJob(job, err)
}

func (h *Handler) resumeStoredConsoleVideoJob(job *videoJob, timeout time.Duration) {
	baseCtx, baseCancel := context.WithTimeout(context.Background(), timeout)
	defer baseCancel()
	leaseCtx, lease, acquired, err := h.beginConsoleVideoJobLease(baseCtx, job)
	if err != nil || !acquired {
		h.abandonConsoleVideoJob(job)
		h.scheduleStoredConsoleVideoRetry(job)
		return
	}
	defer lease.Close()
	if !job.StandardAPI || job.Provider != ProviderConsole || job.AccountID <= 0 || strings.TrimSpace(job.UpstreamRequestID) == "" {
		h.failVideoJobWithCode(job, "video_resume_unavailable", fmt.Errorf("video task was interrupted before it became resumable"))
		return
	}
	account, accountErr := h.lb.Store.GetAccount(leaseCtx, job.AccountID)
	if accountErr != nil || account == nil || !account.Enabled || !isGrokConsoleAccount(account) || !AccountSupportsModel(account, job.Model) {
		h.failVideoJobWithCode(job, "video_resume_unavailable", fmt.Errorf("the original Console video account is unavailable"))
		return
	}
	token := grokSSOTokenRaw(account)
	if NormalizeSSOToken(token) == "" {
		h.failVideoJobWithCode(job, "video_resume_unavailable", fmt.Errorf("the original Console video account token is unavailable"))
		return
	}
	release, reserved := h.reserveAccount(account)
	if !reserved {
		h.failVideoJobWithCode(job, "video_resume_unavailable", fmt.Errorf("the original Console video account is at its concurrency limit"))
		return
	}
	sess := &chatAccountSession{acc: account, token: token, release: release}
	defer sess.Close()
	h.updateVideoJobProgress(job, max(1, job.Progress))
	h.pollConsoleVideoJob(leaseCtx, lease, job, sess, job.UpstreamRequestID)
}

func (h *Handler) scheduleStoredConsoleVideoRetry(job *videoJob) {
	if h == nil || h.lb == nil || h.lb.Store == nil || job == nil {
		return
	}
	delay := consoleVideoLeaseTTL / 3
	if delay < time.Second {
		delay = time.Second
	}
	time.AfterFunc(delay, func() {
		stored, err := h.lb.Store.GetStoredVideoJob(context.Background(), job.ID, job.OwnerHash)
		if err != nil {
			return
		}
		status := strings.ToLower(strings.TrimSpace(stored.Status))
		if status != "queued" && status != "in_progress" && status != "pending" && status != "processing" {
			return
		}
		remaining := time.Until(time.Unix(stored.CreatedAt, 0).Add(videoJobTTL))
		if remaining <= 0 {
			return
		}
		videoJobsMu.Lock()
		_, alreadyRunning := videoJobs[stored.ID]
		videoJobsMu.Unlock()
		if alreadyRunning {
			return
		}
		retryJob := runtimeVideoJobFromStored(stored)
		if retryJob == nil {
			return
		}
		putVideoJob(retryJob)
		go h.resumeStoredVideoJob(retryJob, remaining)
	})
}

func (h *Handler) resumeStoredVideoJob(job *videoJob, timeout time.Duration) {
	if job != nil && job.Provider == ProviderBuild {
		h.resumeStoredBuildVideoJob(job, timeout)
		return
	}
	h.resumeStoredConsoleVideoJob(job, timeout)
}

func (h *Handler) runConsoleVideoJobWithLease(ctx context.Context, lease *consoleVideoJobLease, job *videoJob, sess *chatAccountSession, operation consoleVideoOperation, payload map[string]interface{}) {
	defer sess.Close()
	defer lease.Close()
	h.updateVideoJobProgress(job, 1)
	body, err := json.Marshal(payload)
	if err != nil {
		h.handleConsoleVideoJobError(job, lease, err)
		return
	}
	createPath := "videos/generations"
	if operation == consoleVideoEdit {
		createPath = "videos/edits"
	} else if operation == consoleVideoExtend {
		createPath = "videos/extensions"
	}
	created, err := h.doConsoleVideoJSON(ctx, sess, http.MethodPost, createPath, body)
	if err != nil {
		h.handleConsoleVideoJobError(job, lease, err)
		return
	}
	var createdPayload struct {
		RequestID string `json:"request_id"`
	}
	if json.Unmarshal(created, &createdPayload) != nil || strings.TrimSpace(createdPayload.RequestID) == "" {
		h.handleConsoleVideoJobError(job, lease, fmt.Errorf("Console video create response is missing request_id"))
		return
	}
	upstreamID := strings.TrimSpace(createdPayload.RequestID)
	videoJobsMu.Lock()
	job.UpstreamRequestID = upstreamID
	if sess.acc != nil {
		job.AccountID = sess.acc.ID
	}
	job.Provider = ProviderConsole
	videoJobsMu.Unlock()
	h.persistVideoJob(ctx, job)
	h.pollConsoleVideoJob(ctx, lease, job, sess, upstreamID)
}

func (h *Handler) pollConsoleVideoJob(ctx context.Context, lease *consoleVideoJobLease, job *videoJob, sess *chatAccountSession, upstreamID string) {
	for {
		statusBody, pollErr := h.doConsoleVideoJSON(ctx, sess, http.MethodGet, "videos/"+url.PathEscape(upstreamID), nil)
		if pollErr != nil {
			h.handleConsoleVideoJobError(job, lease, pollErr)
			return
		}
		videoURL, done, parseErr := parseConsoleVideoStatus(statusBody, func(progress int) {
			h.updateVideoJobProgress(job, min(99, progress))
		})
		if parseErr != nil {
			h.handleConsoleVideoJobError(job, lease, parseErr)
			return
		}
		if done {
			data, mimeType, downloadErr := h.currentClient().downloadTrustedConsoleVideo(ctx, sess.token, videoURL, h.consoleURL(""))
			if downloadErr != nil {
				h.handleConsoleVideoJobError(job, lease, downloadErr)
				return
			}
			name, cacheErr := h.cacheMediaBytes(videoURL, "video", data, mimeType)
			if cacheErr != nil {
				h.handleConsoleVideoJobError(job, lease, cacheErr)
				return
			}
			videoJobsMu.Lock()
			job.Status = "completed"
			job.Progress = 100
			job.CompletedAt = time.Now().Unix()
			job.VideoURL = videoURL
			job.ContentPath = filepath.Join(cacheBaseDir, "video", name)
			job.RemixedFromID = upstreamID
			videoJobsMu.Unlock()
			h.persistVideoJob(ctx, job)
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

func (h *Handler) updateVideoJobProgress(job *videoJob, progress int) {
	videoJobsMu.Lock()
	job.Status = "in_progress"
	job.Progress = clampProgress(progress)
	videoJobsMu.Unlock()
	h.persistVideoJob(context.Background(), job)
}

func (h *Handler) doConsoleVideoJSON(ctx context.Context, sess *chatAccountSession, method, path string, body []byte) ([]byte, error) {
	if h == nil || h.currentClient() == nil || sess == nil {
		return nil, fmt.Errorf("Console video client is not configured")
	}
	response, err := h.currentClient().doConsoleDPoPRequestWithHeaders(withRateLimitAccount(ctx, sess.acc), sess.token, method, h.consoleURL(path), body, http.Header{
		"Content-Type": []string{"application/json"},
		"Accept":       []string{"application/json"},
	})
	if err != nil {
		if markAllGrokAccountStatuses(err) {
			h.markAccountStatus(ctx, sess.acc, err)
		}
		return nil, err
	}
	defer response.Body.Close()
	data, err := io.ReadAll(io.LimitReader(response.Body, maxConsoleVideoResponseBytes+1))
	if err != nil {
		return nil, err
	}
	if len(data) > maxConsoleVideoResponseBytes {
		return nil, fmt.Errorf("Console video response exceeds 1 MiB")
	}
	return data, nil
}

func parseConsoleVideoStatus(body []byte, progress func(int)) (string, bool, error) {
	var payload struct {
		Status   string `json:"status"`
		Progress int    `json:"progress"`
		Video    struct {
			URL string `json:"url"`
		} `json:"video"`
		Error interface{} `json:"error"`
	}
	if json.Unmarshal(body, &payload) != nil {
		return "", false, fmt.Errorf("invalid Console video status response")
	}
	if progress != nil && payload.Progress > 0 {
		progress(payload.Progress)
	}
	switch strings.ToLower(strings.TrimSpace(payload.Status)) {
	case "done", "completed", "succeeded", "success", "ready":
		videoURL := strings.TrimSpace(payload.Video.URL)
		if videoURL == "" {
			return "", false, fmt.Errorf("Console video completed without a content URL")
		}
		return videoURL, true, nil
	case "failed", "expired", "cancelled", "canceled", "error":
		message := safeConsoleVideoErrorValue(payload.Error)
		if message == "" {
			message = strings.TrimSpace(payload.Status)
		}
		return "", false, fmt.Errorf("Console video failed: %s", message)
	case "pending", "processing", "in_progress", "queued":
		return "", false, nil
	default:
		return "", false, fmt.Errorf("invalid Console video status %q", strings.TrimSpace(payload.Status))
	}
}

func safeConsoleVideoErrorValue(value interface{}) string {
	switch typed := value.(type) {
	case map[string]interface{}:
		for _, key := range []string{"message", "msg", "code", "type", "detail", "error_description"} {
			if text := safeConsoleVideoErrorValue(typed[key]); text != "" {
				return text
			}
		}
	case []interface{}:
		for _, item := range typed {
			if text := safeConsoleVideoErrorValue(item); text != "" {
				return text
			}
		}
	case string:
		return safeConsoleVideoErrorText(typed)
	case json.Number, float64, float32, int, int64, int32, uint, uint64, uint32, bool:
		return safeConsoleVideoErrorText(fmt.Sprint(typed))
	}
	return ""
}

func safeConsoleVideoErrorText(value string) string {
	value = strings.Join(strings.Fields(value), " ")
	lower := strings.ToLower(value)
	for _, sensitive := range []string{
		"authorization", "cookie", "bearer ", "access_token", "access-token",
		"refresh_token", "refresh-token", "api_key", "api-key", "sso-rw", "cf_clearance",
	} {
		if strings.Contains(lower, sensitive) {
			return "upstream rejected the video request"
		}
	}
	const limit = 160
	if len(value) <= limit {
		return value
	}
	end := limit
	for end > 0 && !utf8.ValidString(value[:end]) {
		end--
	}
	return value[:end]
}

func (c *Client) downloadTrustedConsoleVideo(ctx context.Context, token, rawURL, consoleBase string) ([]byte, string, error) {
	if c == nil {
		return nil, "", fmt.Errorf("grok client is not configured")
	}
	if !trustedConsoleVideoURL(rawURL, consoleBase) {
		return nil, "", fmt.Errorf("Console video content URL is not trusted")
	}
	baseClient := c.clientForAsset(true)
	if baseClient == nil {
		return nil, "", fmt.Errorf("Console video asset client is not configured")
	}
	client := *baseClient
	previousRedirect := baseClient.CheckRedirect
	client.CheckRedirect = func(request *http.Request, via []*http.Request) error {
		if !trustedConsoleVideoURL(request.URL.String(), consoleBase) {
			return fmt.Errorf("Console video redirected to an untrusted URL")
		}
		if previousRedirect != nil {
			return previousRedirect(request, via)
		}
		if len(via) >= 10 {
			return fmt.Errorf("too many Console video redirects")
		}
		return nil
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, "", err
	}
	request.Header = c.assetDownloadHeaders(token, rawURL)
	request.Header.Set("Referer", "https://console.x.ai/")
	response, err := client.Do(request)
	if err != nil {
		return nil, "", err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, "", fmt.Errorf("Console video download returned status %d", response.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, maxConsoleVideoAssetBytes+1))
	if err != nil {
		return nil, "", err
	}
	if len(data) > maxConsoleVideoAssetBytes {
		return nil, "", fmt.Errorf("Console video asset exceeds 512 MiB")
	}
	mimeType := strings.TrimSpace(strings.Split(response.Header.Get("Content-Type"), ";")[0])
	if mimeType == "" || mimeType == "application/octet-stream" {
		mimeType = "video/mp4"
	}
	if !strings.HasPrefix(strings.ToLower(mimeType), "video/") {
		return nil, "", fmt.Errorf("Console video asset has invalid Content-Type %q", mimeType)
	}
	return data, mimeType, nil
}

func trustedConsoleVideoURL(rawURL, consoleBase string) bool {
	parsed, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil || parsed.User != nil || parsed.Hostname() == "" {
		return false
	}
	host := strings.ToLower(strings.TrimSpace(parsed.Hostname()))
	if parsed.Scheme == "https" && (host == "vidgen.x.ai" || strings.HasSuffix(host, ".vidgen.x.ai")) {
		return true
	}
	base, err := url.Parse(strings.TrimSpace(consoleBase))
	return err == nil && base.User == nil && (parsed.Scheme == "http" || parsed.Scheme == "https") &&
		strings.EqualFold(parsed.Scheme, base.Scheme) && strings.EqualFold(parsed.Host, base.Host)
}
