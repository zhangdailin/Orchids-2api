package grok

import (
	"context"
	"encoding/base64"
	"fmt"
	"github.com/goccy/go-json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

const (
	imagineSessionTTL = 10 * time.Minute
	// Use single-image batches to improve "real-time waterfall" responsiveness.
	imagineBatchImageCount = 1
)

var imagineUpgrader = websocket.Upgrader{
	CheckOrigin: func(r *http.Request) bool {
		return true
	},
}

type imagineSession struct {
	Prompt      string
	AspectRatio string
	Model       string
	NSFW        *bool
	CreatedAt   time.Time
}

var (
	imagineSessionsMu sync.Mutex
	imagineSessions   = map[string]imagineSession{}
)

type imagineStartRequest struct {
	Prompt      string `json:"prompt"`
	AspectRatio string `json:"aspect_ratio"`
	Model       string `json:"model,omitempty"`
	NSFW        *bool  `json:"nsfw,omitempty"`
}

type imagineStopRequest struct {
	TaskIDs []string `json:"task_ids"`
}

type imagineImage struct {
	B64 string
	URL string
}

func imagineImageSizeFromAspectRatio(ratio string) string {
	switch resolveAspectRatio(strings.TrimSpace(ratio)) {
	case "9:16":
		return "720x1280"
	case "16:9":
		return "1280x720"
	case "3:2":
		return "1792x1024"
	case "2:3":
		return "1024x1792"
	default:
		return "1024x1024"
	}
}

func normalizeImagineImageURL(raw string) string {
	u := strings.TrimSpace(raw)
	if u == "" {
		return ""
	}
	if strings.HasPrefix(u, "/grok/v1/files/") || strings.HasPrefix(u, "/v1/files/") {
		return u
	}
	if strings.HasPrefix(u, "http://127.0.0.1:") || strings.HasPrefix(u, "http://localhost:") {
		if idx := strings.Index(u, "/grok/v1/files/"); idx >= 0 {
			return u[idx:]
		}
		if idx := strings.Index(u, "/v1/files/"); idx >= 0 {
			return u[idx:]
		}
	}
	return u
}

func imagineImageB64FromURL(raw string) string {
	u := normalizeImagineImageURL(raw)
	mediaType, fileName, ok := parseFilesPath(u)
	if !ok || mediaType != "image" || strings.TrimSpace(fileName) == "" {
		return ""
	}
	fullPath := filepath.Join(cacheBaseDir, mediaType, fileName)
	data, err := os.ReadFile(fullPath)
	if err != nil || len(data) == 0 {
		return ""
	}
	return base64.StdEncoding.EncodeToString(data)
}

func cleanupImagineSessionsLocked(now time.Time) {
	for id, session := range imagineSessions {
		if now.Sub(session.CreatedAt) > imagineSessionTTL {
			delete(imagineSessions, id)
		}
	}
}

func cloneBoolPtr(v *bool) *bool {
	if v == nil {
		return nil
	}
	out := *v
	return &out
}

func parseOptionalBool(raw interface{}) *bool {
	switch v := raw.(type) {
	case nil:
		return nil
	case bool:
		out := v
		return &out
	case string:
		s := strings.ToLower(strings.TrimSpace(v))
		if s == "" {
			return nil
		}
		switch s {
		case "1", "true", "yes", "y", "on":
			out := true
			return &out
		case "0", "false", "no", "n", "off":
			out := false
			return &out
		default:
			return nil
		}
	default:
		return nil
	}
}

func normalizeImagineModel(model string) string {
	raw := strings.ToLower(strings.TrimSpace(model))
	switch raw {
	case "speed", "fast", "lite":
		return "grok-imagine-image-lite"
	case "quality", "pro":
		return "grok-imagine-image-pro"
	}
	id := normalizeModelID(raw)
	if id == "" {
		return "grok-imagine-image-lite"
	}
	if !isImageGenerationModel(id) {
		return "grok-imagine-image-lite"
	}
	return id
}

func createImagineSession(prompt, aspectRatio string, model string, nsfw *bool) string {
	id := randomHex(16)
	if id == "" {
		id = fmt.Sprintf("%d", time.Now().UnixNano())
	}
	now := time.Now()

	imagineSessionsMu.Lock()
	defer imagineSessionsMu.Unlock()
	cleanupImagineSessionsLocked(now)
	imagineSessions[id] = imagineSession{
		Prompt:      strings.TrimSpace(prompt),
		AspectRatio: resolveAspectRatio(strings.TrimSpace(aspectRatio)),
		Model:       normalizeImagineModel(model),
		NSFW:        cloneBoolPtr(nsfw),
		CreatedAt:   now,
	}
	return id
}

func getImagineSession(taskID string) (imagineSession, bool) {
	id := strings.TrimSpace(taskID)
	if id == "" {
		return imagineSession{}, false
	}
	now := time.Now()
	imagineSessionsMu.Lock()
	defer imagineSessionsMu.Unlock()
	cleanupImagineSessionsLocked(now)
	session, ok := imagineSessions[id]
	if !ok {
		return imagineSession{}, false
	}
	if now.Sub(session.CreatedAt) > imagineSessionTTL {
		delete(imagineSessions, id)
		return imagineSession{}, false
	}
	return session, true
}

func deleteImagineSession(taskID string) {
	id := strings.TrimSpace(taskID)
	if id == "" {
		return
	}
	imagineSessionsMu.Lock()
	delete(imagineSessions, id)
	imagineSessionsMu.Unlock()
}

func deleteImagineSessions(taskIDs []string) int {
	removed := 0
	imagineSessionsMu.Lock()
	for _, raw := range taskIDs {
		id := strings.TrimSpace(raw)
		if id == "" {
			continue
		}
		if _, ok := imagineSessions[id]; ok {
			delete(imagineSessions, id)
			removed++
		}
	}
	imagineSessionsMu.Unlock()
	return removed
}

func ensureImageModelConfig(payload map[string]interface{}, modelID string) map[string]interface{} {
	if payload == nil {
		return nil
	}
	modelConfigOverride, _ := payload["modelConfigOverride"].(map[string]interface{})
	if modelConfigOverride == nil {
		modelConfigOverride = map[string]interface{}{}
		payload["modelConfigOverride"] = modelConfigOverride
	}
	modelMap, _ := modelConfigOverride["modelMap"].(map[string]interface{})
	if modelMap == nil {
		modelMap = map[string]interface{}{}
		modelConfigOverride["modelMap"] = modelMap
	}
	if model := strings.TrimSpace(modelID); model != "" {
		modelMap["imageGenModel"] = model
	}
	imageGenCfg, _ := modelMap["imageGenModelConfig"].(map[string]interface{})
	if imageGenCfg == nil {
		imageGenCfg = map[string]interface{}{}
		modelMap["imageGenModelConfig"] = imageGenCfg
	}
	return imageGenCfg
}

func ensureImageAspectRatio(payload map[string]interface{}, modelID, ratio string) {
	if payload == nil {
		return
	}
	if strings.TrimSpace(ratio) == "" {
		ratio = "2:3"
	}
	ratio = resolveAspectRatio(ratio)

	imageGenCfg := ensureImageModelConfig(payload, modelID)
	if imageGenCfg == nil {
		return
	}
	imageGenCfg["aspectRatio"] = ratio
}

func ensureImageNSFW(payload map[string]interface{}, modelID string, nsfw *bool) {
	if payload == nil || nsfw == nil {
		return
	}
	imageGenCfg := ensureImageModelConfig(payload, modelID)
	if imageGenCfg == nil {
		return
	}
	// Keep both key styles for compatibility with different upstream parsers.
	imageGenCfg["enableNsfw"] = *nsfw
	imageGenCfg["enable_nsfw"] = *nsfw
}

func (h *Handler) generateImagineBatch(ctx context.Context, prompt, aspectRatio, model string, n int, nsfw *bool) ([]imagineImage, int, error) {
	imagineModel := normalizeImagineModel(model)
	if err := h.ensureModelEnabled(ctx, imagineModel); err != nil {
		return nil, 0, err
	}
	spec, ok := ResolveModel(imagineModel)
	if !ok || !spec.IsImage {
		return nil, 0, fmt.Errorf("image model not supported")
	}
	if n < 1 {
		n = 1
	}

	startedAt := time.Now()
	size := imagineImageSizeFromAspectRatio(aspectRatio)
	urls, err := h.callLocalImagesGenerationsWithOptions(
		ctx,
		imagineModel,
		strings.TrimSpace(prompt),
		n,
		size,
		"url",
		nsfw,
	)
	if err != nil {
		return nil, 0, err
	}
	urls = normalizeGeneratedImageURLs(urls, n)
	if len(urls) == 0 {
		return nil, 0, fmt.Errorf("no image generated")
	}

	images := make([]imagineImage, 0, len(urls))
	for _, u := range urls {
		imgURL := normalizeImagineImageURL(u)
		if imgURL != "" {
			images = append(images, imagineImage{URL: imgURL})
		}
	}
	if len(images) == 0 {
		return nil, 0, fmt.Errorf("no usable image generated")
	}

	return images, int(time.Since(startedAt) / time.Millisecond), nil
}

func sleepWithContext(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return true
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

func (h *Handler) runImagineLoop(
	ctx context.Context,
	prompt string,
	aspectRatio string,
	model string,
	taskID string,
	deleteSessionOnExit bool,
	nsfw *bool,
	emit func(map[string]interface{}) bool,
) {
	runID := randomHex(12)
	if runID == "" {
		runID = fmt.Sprintf("%d", time.Now().UnixNano())
	}
	sequence := 0
	if !emit(map[string]interface{}{
		"type":         "status",
		"status":       "running",
		"prompt":       prompt,
		"aspect_ratio": aspectRatio,
		"model":        normalizeImagineModel(model),
		"run_id":       runID,
	}) {
		return
	}

	defer func() {
		_ = emit(map[string]interface{}{
			"type":   "status",
			"status": "stopped",
			"run_id": runID,
		})
		if deleteSessionOnExit && strings.TrimSpace(taskID) != "" {
			deleteImagineSession(taskID)
		}
	}()

	for {
		if ctx.Err() != nil {
			return
		}
		if strings.TrimSpace(taskID) != "" {
			if _, ok := getImagineSession(taskID); !ok {
				return
			}
		}

		images, elapsedMS, err := h.generateImagineBatch(ctx, prompt, aspectRatio, model, imagineBatchImageCount, nsfw)
		if err != nil {
			delay := imagineErrorRetryDelay(err)
			if !emit(map[string]interface{}{
				"type":    "error",
				"message": err.Error(),
				"code":    "internal_error",
			}) {
				return
			}
			if !sleepWithContext(ctx, delay) {
				return
			}
			continue
		}

		nowMillis := time.Now().UnixMilli()
		for _, img := range images {
			sequence++
			fileURL := normalizeImagineImageURL(img.URL)
			b64 := strings.TrimSpace(img.B64)
			if b64 == "" && fileURL != "" {
				b64 = imagineImageB64FromURL(fileURL)
			}
			if !emit(map[string]interface{}{
				"type":         "image",
				"b64_json":     b64,
				"file_url":     fileURL,
				"url":          fileURL,
				"sequence":     sequence,
				"created_at":   nowMillis,
				"elapsed_ms":   elapsedMS,
				"aspect_ratio": aspectRatio,
				"model":        normalizeImagineModel(model),
				"run_id":       runID,
			}) {
				return
			}
		}
	}
}

func imagineErrorRetryDelay(err error) time.Duration {
	if err == nil {
		return 1500 * time.Millisecond
	}
	msg := strings.ToLower(err.Error())
	if strings.Contains(msg, "429") ||
		strings.Contains(msg, "rate-limited") ||
		strings.Contains(msg, "cooling down") ||
		strings.Contains(msg, "no image generated") ||
		strings.Contains(msg, "no enabled accounts available for channel: grok") {
		return time.Minute
	}
	return 1500 * time.Millisecond
}

func (h *Handler) HandleAdminImagineStart(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req imagineStartRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid json", http.StatusBadRequest)
		return
	}
	prompt := strings.TrimSpace(req.Prompt)
	if prompt == "" {
		http.Error(w, "prompt cannot be empty", http.StatusBadRequest)
		return
	}
	ratio := resolveAspectRatio(strings.TrimSpace(req.AspectRatio))
	model := normalizeImagineModel(req.Model)
	taskID := createImagineSession(prompt, ratio, model, req.NSFW)
	out := map[string]interface{}{
		"task_id":      taskID,
		"aspect_ratio": ratio,
		"model":        model,
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(out)
}

func (h *Handler) HandleAdminImagineStop(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req imagineStopRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid json", http.StatusBadRequest)
		return
	}
	removed := deleteImagineSessions(req.TaskIDs)
	out := map[string]interface{}{
		"status":  "success",
		"removed": removed,
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(out)
}

func (h *Handler) HandleAdminImagineSSE(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	taskID := strings.TrimSpace(r.URL.Query().Get("task_id"))
	prompt := strings.TrimSpace(r.URL.Query().Get("prompt"))
	ratio := strings.TrimSpace(r.URL.Query().Get("aspect_ratio"))
	model := normalizeImagineModel(r.URL.Query().Get("model"))
	nsfw := parseOptionalBool(r.URL.Query().Get("nsfw"))

	if taskID != "" {
		session, ok := getImagineSession(taskID)
		if !ok {
			http.Error(w, "task not found", http.StatusNotFound)
			return
		}
		prompt = session.Prompt
		ratio = session.AspectRatio
		model = normalizeImagineModel(session.Model)
		if nsfw == nil {
			nsfw = cloneBoolPtr(session.NSFW)
		}
	}
	if prompt == "" {
		http.Error(w, "prompt cannot be empty", http.StatusBadRequest)
		return
	}
	ratio = resolveAspectRatio(ratio)

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	flusher, _ := w.(http.Flusher)

	emit := func(payload map[string]interface{}) bool {
		writeSSEBytes(w, "", encodeJSONBytes(payload))
		if flusher != nil {
			flusher.Flush()
		}
		return r.Context().Err() == nil
	}

	h.runImagineLoop(r.Context(), prompt, ratio, model, taskID, true, nsfw, emit)
}

func (h *Handler) HandleAdminImagineWS(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	taskID := strings.TrimSpace(r.URL.Query().Get("task_id"))
	if taskID != "" {
		if _, ok := getImagineSession(taskID); !ok {
			http.Error(w, "task not found", http.StatusNotFound)
			return
		}
	}

	conn, err := imagineUpgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	defer conn.Close()

	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()

	var writeMu sync.Mutex
	send := func(payload map[string]interface{}) bool {
		writeMu.Lock()
		defer writeMu.Unlock()
		if err := conn.WriteJSON(payload); err != nil {
			return false
		}
		return true
	}

	var runMu sync.Mutex
	var runCancel context.CancelFunc
	var runDone chan struct{}

	stopRun := func() {
		runMu.Lock()
		cancelFn := runCancel
		done := runDone
		runCancel = nil
		runDone = nil
		runMu.Unlock()

		if cancelFn != nil {
			cancelFn()
		}
		if done != nil {
			<-done
		}
	}
	defer stopRun()

	startRun := func(prompt, ratio, model string, nsfw *bool) {
		stopRun()
		runCtx, cancelFn := context.WithCancel(ctx)
		done := make(chan struct{})
		runMu.Lock()
		runCancel = cancelFn
		runDone = done
		runMu.Unlock()
		go func() {
			defer close(done)
			h.runImagineLoop(runCtx, prompt, ratio, model, taskID, false, nsfw, send)
		}()
	}

	for {
		var payload map[string]interface{}
		if err := conn.ReadJSON(&payload); err != nil {
			break
		}
		msgType := strings.ToLower(strings.TrimSpace(fmt.Sprint(payload["type"])))
		switch msgType {
		case "start":
			prompt := strings.TrimSpace(fmt.Sprint(payload["prompt"]))
			ratio := strings.TrimSpace(fmt.Sprint(payload["aspect_ratio"]))
			model := normalizeImagineModel(fmt.Sprint(payload["model"]))
			nsfw := parseOptionalBool(payload["nsfw"])
			if taskID != "" {
				if session, ok := getImagineSession(taskID); ok {
					if prompt == "" {
						prompt = session.Prompt
					}
					if ratio == "" {
						ratio = session.AspectRatio
					}
					model = normalizeImagineModel(session.Model)
					if nsfw == nil {
						nsfw = cloneBoolPtr(session.NSFW)
					}
				}
			}
			if prompt == "" {
				_ = send(map[string]interface{}{
					"type":    "error",
					"message": "prompt cannot be empty",
					"code":    "empty_prompt",
				})
				continue
			}
			if ratio == "" {
				ratio = "2:3"
			}
			ratio = resolveAspectRatio(ratio)
			startRun(prompt, ratio, model, nsfw)
		case "stop":
			stopRun()
		case "ping":
			_ = send(map[string]interface{}{"type": "pong"})
		default:
			_ = send(map[string]interface{}{
				"type":    "error",
				"message": "unknown command",
				"code":    "unknown_command",
			})
		}
	}

	if taskID != "" {
		deleteImagineSession(taskID)
	}
}
