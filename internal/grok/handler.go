package grok

import (
	"context"
	"crypto/sha1"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"orchids-api/internal/config"
	"orchids-api/internal/loadbalancer"
	"orchids-api/internal/store"
)

const maxEditImageBytes = 50 * 1024 * 1024

var cacheBaseDir = filepath.Join("data", "tmp")

type Handler struct {
	cfg    *config.Config
	lb     *loadbalancer.LoadBalancer
	client *Client
}

func NewHandler(cfg *config.Config, lb *loadbalancer.LoadBalancer) *Handler {
	return &Handler{
		cfg:    cfg,
		lb:     lb,
		client: New(cfg),
	}
}

func (h *Handler) selectAccount(ctx context.Context) (*store.Account, string, error) {
	if h.lb == nil {
		return nil, "", fmt.Errorf("load balancer not configured")
	}
	acc, err := h.lb.GetNextAccountExcludingByChannel(ctx, nil, "grok")
	if err != nil {
		return nil, "", err
	}
	raw := strings.TrimSpace(acc.ClientCookie)
	if raw == "" {
		raw = strings.TrimSpace(acc.RefreshToken)
	}
	token := parseTokenValue(raw)
	if token == "" {
		return nil, "", fmt.Errorf("grok account token is empty")
	}
	return acc, token, nil
}

func (h *Handler) ensureModelEnabled(ctx context.Context, modelID string) error {
	if h == nil || h.lb == nil || h.lb.Store == nil {
		return nil
	}
	modelID = strings.TrimSpace(modelID)
	if modelID == "" {
		return nil
	}
	m, err := h.lb.Store.GetModelByModelID(ctx, modelID)
	if err != nil || m == nil {
		return fmt.Errorf("model not found")
	}
	if !m.Status.Enabled() {
		return fmt.Errorf("model not available")
	}
	mChannel := strings.TrimSpace(m.Channel)
	if mChannel == "" {
		mChannel = "grok"
	}
	if !strings.EqualFold(mChannel, "grok") {
		return fmt.Errorf("model not found")
	}
	return nil
}

func (h *Handler) trackAccount(acc *store.Account) func() {
	if h == nil || h.lb == nil || acc == nil || acc.ID == 0 {
		return func() {}
	}
	h.lb.AcquireConnection(acc.ID)
	return func() {
		h.lb.ReleaseConnection(acc.ID)
	}
}

func (h *Handler) markAccountStatus(ctx context.Context, acc *store.Account, err error) {
	if acc == nil || err == nil || h.lb == nil {
		return
	}
	status := classifyAccountStatusFromError(err.Error())
	if status == "" {
		return
	}
	h.lb.MarkAccountStatus(ctx, acc, status)
}

func (h *Handler) syncGrokQuota(acc *store.Account, headers http.Header) {
	if acc == nil || h.lb == nil || h.lb.Store == nil {
		return
	}
	info := parseRateLimitInfo(headers)
	if info == nil || (info.Limit <= 0 && info.Remaining <= 0) {
		return
	}
	limit := info.Limit
	remaining := info.Remaining
	if remaining < 0 {
		remaining = 0
	}
	if limit <= 0 && remaining > 0 {
		limit = remaining
	}
	acc.UsageLimit = float64(limit)
	acc.UsageCurrent = float64(remaining)
	if !info.ResetAt.IsZero() {
		acc.QuotaResetAt = info.ResetAt
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := h.lb.Store.UpdateAccount(ctx, acc); err != nil {
		slog.Warn("grok quota update failed", "account_id", acc.ID, "error", err)
	}
}

func (h *Handler) HandleModels(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	resp := map[string]interface{}{
		"object": "list",
		"data":   make([]map[string]interface{}, 0, len(SupportedModels)),
	}
	data := resp["data"].([]map[string]interface{})
	for _, m := range SupportedModels {
		data = append(data, map[string]interface{}{
			"id":       m.ID,
			"object":   "model",
			"created":  0,
			"owned_by": "grok",
		})
	}
	resp["data"] = data
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}

func (h *Handler) HandleChatCompletions(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req ChatCompletionsRequest
	raw, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "invalid body", http.StatusBadRequest)
		return
	}
	if err := json.Unmarshal(raw, &req); err != nil {
		http.Error(w, "invalid json", http.StatusBadRequest)
		return
	}
	if err := req.Validate(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if err := h.ensureModelEnabled(r.Context(), req.Model); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	spec, ok := ResolveModel(req.Model)
	if !ok {
		http.Error(w, "model not found", http.StatusBadRequest)
		return
	}

	acc, token, err := h.selectAccount(r.Context())
	if err != nil {
		http.Error(w, "no available grok token: "+err.Error(), http.StatusServiceUnavailable)
		return
	}
	release := h.trackAccount(acc)
	defer release()

	text, attachments, err := extractMessageAndAttachments(req.Messages, spec.IsVideo)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if strings.TrimSpace(text) == "" && len(attachments) == 0 {
		http.Error(w, "empty message", http.StatusBadRequest)
		return
	}

	fileAttachments, err := h.uploadAttachmentInputs(r.Context(), token, attachments)
	if err != nil {
		h.markAccountStatus(r.Context(), acc, err)
		http.Error(w, "attachment upload failed: "+err.Error(), http.StatusBadGateway)
		return
	}

	payload, err := h.buildChatPayload(r.Context(), token, spec, text, fileAttachments, req.VideoConfig)
	if err != nil {
		h.markAccountStatus(r.Context(), acc, err)
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	resp, err := h.client.doChat(r.Context(), token, payload)
	if err != nil {
		h.markAccountStatus(r.Context(), acc, err)
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()
	h.syncGrokQuota(acc, resp.Header)

	if req.Stream {
		h.streamChat(w, req.Model, spec, token, resp.Body)
		return
	}
	h.collectChat(w, req.Model, spec, token, resp.Body)
}

func (h *Handler) buildChatPayload(ctx context.Context, token string, spec ModelSpec, text string, fileAttachments []string, videoCfg *VideoConfig) (map[string]interface{}, error) {
	payload := h.client.chatPayload(spec, text, true)
	if len(fileAttachments) > 0 {
		payload["fileAttachments"] = fileAttachments
	}

	if !spec.IsVideo {
		return payload, nil
	}

	if videoCfg == nil {
		videoCfg = &VideoConfig{}
	}
	videoCfg.Normalize()

	postID, err := h.client.createMediaPost(ctx, token, "MEDIA_POST_TYPE_VIDEO", text, "")
	if err != nil {
		return nil, fmt.Errorf("create video post failed: %w", err)
	}

	modeFlag := videoPresetFlag(videoCfg.Preset)
	message := strings.TrimSpace(text)
	if modeFlag != "" {
		message = strings.TrimSpace(message + " " + modeFlag)
	}

	return map[string]interface{}{
		"temporary":        true,
		"modelName":        spec.UpstreamModel,
		"message":          message,
		"toolOverrides":    map[string]interface{}{"videoGen": true},
		"enableSideBySide": true,
		"deviceEnvInfo": map[string]interface{}{
			"darkModeEnabled":  false,
			"devicePixelRatio": 2,
			"screenWidth":      1920,
			"screenHeight":     1080,
			"viewportWidth":    1920,
			"viewportHeight":   1080,
		},
		"responseMetadata": map[string]interface{}{
			"modelConfigOverride": map[string]interface{}{
				"modelMap": map[string]interface{}{
					"videoGenModelConfig": map[string]interface{}{
						"aspectRatio":    videoCfg.AspectRatio,
						"parentPostId":   postID,
						"resolutionName": videoCfg.ResolutionName,
						"videoLength":    videoCfg.VideoLength,
					},
				},
			},
		},
	}, nil
}

func videoPresetFlag(preset string) string {
	switch strings.ToLower(strings.TrimSpace(preset)) {
	case "fun":
		return "--mode=extremely-crazy"
	case "normal":
		return "--mode=normal"
	case "spicy":
		return "--mode=extremely-spicy-or-crazy"
	case "", "custom":
		return "--mode=custom"
	default:
		return "--mode=custom"
	}
}

func (h *Handler) uploadAttachmentInputs(ctx context.Context, token string, inputs []AttachmentInput) ([]string, error) {
	if len(inputs) == 0 {
		return nil, nil
	}
	out := make([]string, 0, len(inputs))
	for _, item := range inputs {
		fileID, fileURI, err := h.uploadSingleInput(ctx, token, item.Data)
		if err != nil {
			return nil, err
		}
		id := strings.TrimSpace(fileID)
		if id == "" {
			id = strings.TrimSpace(fileURI)
		}
		if id != "" {
			out = append(out, id)
		}
	}
	return uniqueStrings(out), nil
}

func (h *Handler) uploadSingleInput(ctx context.Context, token, input string) (string, string, error) {
	data := strings.TrimSpace(input)
	if data == "" {
		return "", "", fmt.Errorf("empty attachment")
	}
	if isRemoteURL(data) {
		var err error
		data, err = fetchRemoteAsDataURI(data, 30*time.Second)
		if err != nil {
			return "", "", err
		}
	}
	filename, contentBase64, mime, err := parseDataURI(data)
	if err != nil {
		// Fallback for plain base64 payloads.
		filename = "file.bin"
		mime = "application/octet-stream"
		contentBase64 = data
	}
	return h.client.uploadFile(ctx, token, filename, mime, contentBase64)
}

func (h *Handler) streamChat(w http.ResponseWriter, model string, spec ModelSpec, token string, body io.Reader) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	flusher, _ := w.(http.Flusher)

	id := "chatcmpl_" + randomHex(8)
	sentRole := false
	lastMessage := ""
	sawToken := false
	sentAny := false

	emitChunk := func(delta map[string]interface{}, finish interface{}) {
		chunk := map[string]interface{}{
			"id":      id,
			"object":  "chat.completion.chunk",
			"created": time.Now().Unix(),
			"model":   model,
			"choices": []map[string]interface{}{
				{
					"index":         0,
					"delta":         delta,
					"logprobs":      nil,
					"finish_reason": finish,
				},
			},
		}
		writeSSE(w, "", encodeJSON(chunk))
		if flusher != nil {
			flusher.Flush()
		}
		sentAny = true
	}

	err := parseUpstreamLines(body, func(resp map[string]interface{}) error {
		if !sentRole {
			emitChunk(map[string]interface{}{"role": "assistant"}, nil)
			sentRole = true
		}
		if tokenDelta, ok := resp["token"].(string); ok && tokenDelta != "" {
			sawToken = true
			emitChunk(map[string]interface{}{"content": tokenDelta}, nil)
		}
		if mr, ok := resp["modelResponse"].(map[string]interface{}); ok {
			if msg, ok := mr["message"].(string); ok && strings.TrimSpace(msg) != "" && msg != lastMessage {
				lastMessage = msg
				if !sawToken {
					emitChunk(map[string]interface{}{"content": msg}, nil)
				}
				// Debug aid: Grok sometimes returns image cards/tool markup instead of URLs.
				// Log a small hint to help map where the real URLs live.
				if strings.Contains(msg, "<grok:render") || strings.Contains(msg, "tool_usage_card") {
					slog.Debug("grok message contains render/tool markup", "has_modelResponse", true)
				}
			}
			for _, u := range extractImageURLs(mr) {
				emitChunk(map[string]interface{}{"content": "\n![](" + u + ")"}, nil)
			}
			// Fallback: tool/card payloads may include image URLs outside of the known keys.
			for _, u := range extractRenderableImageLinks(mr) {
				emitChunk(map[string]interface{}{"content": "\n![](" + u + ")"}, nil)
			}
		}
		// Broader fallback: sometimes URLs live outside modelResponse.
		for _, u := range extractRenderableImageLinks(resp) {
			emitChunk(map[string]interface{}{"content": "\n![](" + u + ")"}, nil)
		}
		if spec.IsVideo {
			if progress, videoURL, _, ok := extractVideoProgress(resp); ok {
				if progress > 0 && progress < 100 {
					emitChunk(map[string]interface{}{"content": fmt.Sprintf("正在生成视频中，当前进度%d%%\n", progress)}, nil)
				}
				if progress >= 100 && strings.TrimSpace(videoURL) != "" {
					finalURL := strings.TrimSpace(videoURL)
					if name, err := h.cacheMediaURL(context.Background(), token, finalURL, "video"); err == nil && name != "" {
						finalURL = "/grok/v1/files/video/" + name
					}
					emitChunk(map[string]interface{}{"content": finalURL}, nil)
				}
			}
		}
		return nil
	})
	if err != nil {
		slog.Warn("grok stream parse failed", "error", err)
		if !sentAny {
			http.Error(w, "stream parse error: "+err.Error(), http.StatusBadGateway)
			return
		}
		emitChunk(map[string]interface{}{"content": "\n[上游响应解析失败]\n"}, nil)
	}

	emitChunk(map[string]interface{}{}, "stop")
	writeSSE(w, "", "[DONE]")
	if flusher != nil {
		flusher.Flush()
	}
}

func (h *Handler) collectChat(w http.ResponseWriter, model string, spec ModelSpec, token string, body io.Reader) {
	id := "chatcmpl_" + randomHex(8)
	var content strings.Builder
	lastMessage := ""
	sawToken := false
	videoURL := ""

	err := parseUpstreamLines(body, func(resp map[string]interface{}) error {
		if tokenDelta, ok := resp["token"].(string); ok && tokenDelta != "" {
			sawToken = true
			content.WriteString(tokenDelta)
		}
		if mr, ok := resp["modelResponse"].(map[string]interface{}); ok {
			if msg, ok := mr["message"].(string); ok && strings.TrimSpace(msg) != "" && msg != lastMessage {
				lastMessage = msg
				if !sawToken {
					content.WriteString(msg)
				}
				if strings.Contains(msg, "<grok:render") || strings.Contains(msg, "tool_usage_card") {
					slog.Debug("grok message contains render/tool markup", "has_modelResponse", true)
				}
			}
			for _, u := range extractImageURLs(mr) {
				content.WriteString("\n![](" + u + ")")
			}
			for _, u := range extractRenderableImageLinks(mr) {
				content.WriteString("\n![](" + u + ")")
			}
		}
		for _, u := range extractRenderableImageLinks(resp) {
			content.WriteString("\n![](" + u + ")")
		}
		if spec.IsVideo {
			if progress, vurl, _, ok := extractVideoProgress(resp); ok && progress >= 100 && strings.TrimSpace(vurl) != "" {
				videoURL = strings.TrimSpace(vurl)
			}
		}
		return nil
	})
	if err != nil {
		http.Error(w, "stream parse error: "+err.Error(), http.StatusBadGateway)
		return
	}

	if videoURL != "" {
		if name, err := h.cacheMediaURL(context.Background(), token, videoURL, "video"); err == nil && name != "" {
			videoURL = "/grok/v1/files/video/" + name
		}
		if content.Len() > 0 {
			content.WriteString("\n")
		}
		content.WriteString(videoURL)
	}

	resp := map[string]interface{}{
		"id":      id,
		"object":  "chat.completion",
		"created": time.Now().Unix(),
		"model":   model,
		"choices": []map[string]interface{}{
			{
				"index": 0,
				"message": map[string]interface{}{
					"role":    "assistant",
					"content": content.String(),
				},
				"finish_reason": "stop",
			},
		},
		"usage": map[string]interface{}{
			"prompt_tokens":     0,
			"completion_tokens": 0,
			"total_tokens":      0,
		},
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}

func normalizeImageResponseFormat(format string) string {
	switch strings.ToLower(strings.TrimSpace(format)) {
	case "b64_json", "base64":
		return "b64_json"
	case "url", "":
		return "url"
	default:
		return "url"
	}
}

func imageResponseField(format string) string {
	if normalizeImageResponseFormat(format) == "b64_json" {
		return "b64_json"
	}
	return "url"
}

func imageUsagePayload() map[string]interface{} {
	return map[string]interface{}{
		"total_tokens":  0,
		"input_tokens":  0,
		"output_tokens": 0,
		"input_tokens_details": map[string]interface{}{
			"text_tokens":  0,
			"image_tokens": 0,
		},
	}
}

func mediaExtFromMime(mediaType, mimeType, rawURL string) string {
	m := strings.ToLower(strings.TrimSpace(strings.Split(mimeType, ";")[0]))
	switch m {
	case "image/jpeg":
		return ".jpg"
	case "image/png":
		return ".png"
	case "image/webp":
		return ".webp"
	case "image/gif":
		return ".gif"
	case "video/mp4":
		return ".mp4"
	case "video/webm":
		return ".webm"
	}
	trim := strings.TrimSpace(rawURL)
	if idx := strings.Index(trim, "?"); idx >= 0 {
		trim = trim[:idx]
	}
	ext := strings.ToLower(strings.TrimSpace(filepath.Ext(trim)))
	if ext != "" && len(ext) <= 10 {
		return ext
	}
	if strings.EqualFold(mediaType, "video") {
		return ".mp4"
	}
	return ".jpg"
}

func (h *Handler) cacheMediaURL(ctx context.Context, token, rawURL, mediaType string) (string, error) {
	mediaType = strings.ToLower(strings.TrimSpace(mediaType))
	if mediaType != "video" {
		mediaType = "image"
	}
	data, mimeType, err := h.client.downloadAsset(ctx, token, rawURL)
	if err != nil {
		return "", err
	}
	return h.cacheMediaBytes(rawURL, mediaType, data, mimeType)
}

func (h *Handler) imageOutputValue(ctx context.Context, token, url, format string) (string, error) {
	if normalizeImageResponseFormat(format) == "url" {
		if name, err := h.cacheMediaURL(ctx, token, url, "image"); err == nil && name != "" {
			return "/grok/v1/files/image/" + name, nil
		}
		return strings.TrimSpace(url), nil
	}
	raw, _, err := h.client.downloadAsset(ctx, token, url)
	if err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(raw), nil
}

func (h *Handler) cacheMediaBytes(rawURL, mediaType string, data []byte, mimeType string) (string, error) {
	mediaType = strings.ToLower(strings.TrimSpace(mediaType))
	if mediaType != "video" {
		mediaType = "image"
	}
	if len(data) == 0 {
		return "", fmt.Errorf("empty media data")
	}

	dir := filepath.Join(cacheBaseDir, mediaType)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}

	sum := sha1.Sum([]byte(strings.TrimSpace(rawURL)))
	name := hex.EncodeToString(sum[:]) + mediaExtFromMime(mediaType, mimeType, rawURL)
	fullPath := filepath.Join(dir, name)

	if info, statErr := os.Stat(fullPath); statErr == nil && info.Mode().IsRegular() && info.Size() > 0 {
		return name, nil
	}

	tmp := fullPath + ".tmp-" + randomHex(4)
	if writeErr := os.WriteFile(tmp, data, 0o644); writeErr != nil {
		_ = os.Remove(tmp)
		return "", writeErr
	}
	if renameErr := os.Rename(tmp, fullPath); renameErr != nil {
		_ = os.Remove(tmp)
		return "", renameErr
	}
	return name, nil
}

func (h *Handler) streamImageGeneration(w http.ResponseWriter, body io.Reader, token, format string, n int) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	flusher, _ := w.(http.Flusher)

	field := imageResponseField(format)
	var urls []string
	targetIndex := -1

	_ = parseUpstreamLines(body, func(resp map[string]interface{}) error {
		if index, progress, ok := extractImageProgress(resp); ok {
			outIndex := index
			if n == 1 {
				if targetIndex < 0 {
					targetIndex = index
				}
				if index != targetIndex {
					return nil
				}
				outIndex = 0
			}
			data := map[string]interface{}{
				"type":     "image_generation.partial_image",
				field:      "",
				"index":    outIndex,
				"progress": progress,
			}
			writeSSE(w, "image_generation.partial_image", encodeJSON(data))
			if flusher != nil {
				flusher.Flush()
			}
		}
		if mr, ok := resp["modelResponse"].(map[string]interface{}); ok {
			urls = append(urls, extractImageURLs(mr)...)
		}
		return nil
	})

	urls = uniqueStrings(urls)
	if n > 0 && len(urls) > n {
		urls = urls[:n]
	}

	for i, u := range urls {
		val, err := h.imageOutputValue(context.Background(), token, u, format)
		if err != nil {
			slog.Warn("grok image stream convert failed", "url", u, "error", err)
			if field == "url" {
				val = u
			}
		}
		data := map[string]interface{}{
			"type":  "image_generation.completed",
			field:   val,
			"index": i,
			"usage": imageUsagePayload(),
		}
		writeSSE(w, "image_generation.completed", encodeJSON(data))
		if flusher != nil {
			flusher.Flush()
		}
	}
}

func (h *Handler) HandleImagesGenerations(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req ImagesGenerationsRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid json", http.StatusBadRequest)
		return
	}
	req.Normalize()
	req.ResponseFormat = normalizeImageResponseFormat(req.ResponseFormat)
	req.Prompt = strings.TrimSpace(req.Prompt)
	if req.Prompt == "" {
		http.Error(w, "prompt is required", http.StatusBadRequest)
		return
	}
	if req.N < 1 || req.N > 10 {
		http.Error(w, "n must be between 1 and 10", http.StatusBadRequest)
		return
	}
	if req.Stream && req.N > 2 {
		http.Error(w, "streaming is only supported when n=1 or n=2", http.StatusBadRequest)
		return
	}
	if err := h.ensureModelEnabled(r.Context(), req.Model); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	spec, ok := ResolveModel(req.Model)
	if !ok || !spec.IsImage || spec.ID == "grok-imagine-1.0-edit" {
		http.Error(w, "image model not supported", http.StatusBadRequest)
		return
	}

	acc, token, err := h.selectAccount(r.Context())
	if err != nil {
		http.Error(w, "no available grok token: "+err.Error(), http.StatusServiceUnavailable)
		return
	}
	release := h.trackAccount(acc)
	defer release()

	onePayload := h.client.chatPayload(spec, "Image Generation: "+req.Prompt, true)
	if req.Stream {
		resp, err := h.client.doChat(r.Context(), token, onePayload)
		if err != nil {
			h.markAccountStatus(r.Context(), acc, err)
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		defer resp.Body.Close()
		h.syncGrokQuota(acc, resp.Header)
		h.streamImageGeneration(w, resp.Body, token, req.ResponseFormat, req.N)
		return
	}

	callsNeeded := (req.N + 1) / 2
	if callsNeeded < 1 {
		callsNeeded = 1
	}

	var urls []string
	for i := 0; i < callsNeeded; i++ {
		resp, err := h.client.doChat(r.Context(), token, onePayload)
		if err != nil {
			h.markAccountStatus(r.Context(), acc, err)
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		h.syncGrokQuota(acc, resp.Header)
		err = parseUpstreamLines(resp.Body, func(line map[string]interface{}) error {
			if mr, ok := line["modelResponse"].(map[string]interface{}); ok {
				urls = append(urls, extractImageURLs(mr)...)
			}
			return nil
		})
		resp.Body.Close()
		if err != nil {
			http.Error(w, "stream parse error: "+err.Error(), http.StatusBadGateway)
			return
		}
	}

	urls = uniqueStrings(urls)
	if len(urls) == 0 {
		http.Error(w, "no image generated", http.StatusBadGateway)
		return
	}
	if len(urls) > req.N {
		urls = urls[:req.N]
	}

	field := imageResponseField(req.ResponseFormat)
	data := make([]map[string]interface{}, 0, len(urls))
	for _, u := range urls {
		val, err := h.imageOutputValue(r.Context(), token, u, req.ResponseFormat)
		if err != nil {
			slog.Warn("grok image convert failed", "url", u, "error", err)
			if field == "url" {
				val = u
			} else {
				val = ""
			}
		}
		data = append(data, map[string]interface{}{field: val})
	}

	out := map[string]interface{}{
		"created": time.Now().Unix(),
		"data":    data,
		"usage":   imageUsagePayload(),
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(out)
}

func (h *Handler) buildImageEditPayload(spec ModelSpec, prompt string, imageURLs []string, parentPostID string) map[string]interface{} {
	imageEditCfg := map[string]interface{}{
		"imageReferences": imageURLs,
	}
	if strings.TrimSpace(parentPostID) != "" {
		imageEditCfg["parentPostId"] = strings.TrimSpace(parentPostID)
	}
	return map[string]interface{}{
		"temporary":                 true,
		"modelName":                 spec.UpstreamModel,
		"message":                   strings.TrimSpace(prompt),
		"enableImageGeneration":     true,
		"returnImageBytes":          false,
		"returnRawGrokInXaiRequest": false,
		"enableImageStreaming":      true,
		"imageGenerationCount":      2,
		"forceConcise":              false,
		"toolOverrides":             map[string]interface{}{"imageGen": true},
		"enableSideBySide":          true,
		"sendFinalMetadata":         true,
		"isReasoning":               false,
		"disableTextFollowUps":      true,
		"responseMetadata": map[string]interface{}{
			"modelConfigOverride": map[string]interface{}{
				"modelMap": map[string]interface{}{
					"imageEditModel":       "imagine",
					"imageEditModelConfig": imageEditCfg,
				},
			},
		},
		"disableMemory":   false,
		"forceSideBySide": false,
	}
}

func isAllowedEditImageMime(mime string) bool {
	switch strings.ToLower(strings.TrimSpace(mime)) {
	case "image/png", "image/jpeg", "image/webp":
		return true
	default:
		return false
	}
}

func (h *Handler) HandleImagesEdits(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if err := r.ParseMultipartForm(80 << 20); err != nil {
		http.Error(w, "invalid multipart form", http.StatusBadRequest)
		return
	}

	prompt := strings.TrimSpace(r.FormValue("prompt"))
	if prompt == "" {
		http.Error(w, "prompt is required", http.StatusBadRequest)
		return
	}
	model := strings.TrimSpace(r.FormValue("model"))
	if model == "" {
		model = "grok-imagine-1.0-edit"
	}
	n := parseIntLoose(r.FormValue("n"), 1)
	if n < 1 || n > 10 {
		http.Error(w, "n must be between 1 and 10", http.StatusBadRequest)
		return
	}
	stream := parseBoolLoose(r.FormValue("stream"), false)
	if stream && n > 2 {
		http.Error(w, "streaming is only supported when n=1 or n=2", http.StatusBadRequest)
		return
	}
	responseFormat := normalizeImageResponseFormat(r.FormValue("response_format"))

	spec, ok := ResolveModel(model)
	if !ok || !spec.IsImage || spec.ID != "grok-imagine-1.0-edit" {
		http.Error(w, "image edit model not supported", http.StatusBadRequest)
		return
	}
	if err := h.ensureModelEnabled(r.Context(), model); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	files := r.MultipartForm.File["image"]
	if len(files) == 0 {
		files = r.MultipartForm.File["image[]"]
	}
	if len(files) == 0 {
		http.Error(w, "image is required", http.StatusBadRequest)
		return
	}
	if len(files) > 16 {
		http.Error(w, "too many images. maximum is 16", http.StatusBadRequest)
		return
	}

	acc, token, err := h.selectAccount(r.Context())
	if err != nil {
		http.Error(w, "no available grok token: "+err.Error(), http.StatusServiceUnavailable)
		return
	}
	release := h.trackAccount(acc)
	defer release()

	imageURLs := make([]string, 0, len(files))
	for _, fh := range files {
		file, err := fh.Open()
		if err != nil {
			http.Error(w, "failed to read image file", http.StatusBadRequest)
			return
		}
		data, err := io.ReadAll(io.LimitReader(file, maxEditImageBytes+1))
		file.Close()
		if err != nil {
			http.Error(w, "failed to read image file", http.StatusBadRequest)
			return
		}
		if len(data) == 0 {
			http.Error(w, "file content is empty", http.StatusBadRequest)
			return
		}
		if len(data) > maxEditImageBytes {
			http.Error(w, "image file too large. maximum is 50MB", http.StatusBadRequest)
			return
		}
		mime := strings.ToLower(strings.TrimSpace(fh.Header.Get("Content-Type")))
		if mime == "image/jpg" {
			mime = "image/jpeg"
		}
		if !isAllowedEditImageMime(mime) {
			mime = mimeFromFilename(strings.TrimSpace(fh.Filename))
			if mime == "image/jpg" {
				mime = "image/jpeg"
			}
		}
		if !isAllowedEditImageMime(mime) {
			http.Error(w, "unsupported image type. supported: png, jpg, webp", http.StatusBadRequest)
			return
		}

		dataURI := dataURIFromBytes(mime, data)
		_, fileURI, err := h.uploadSingleInput(r.Context(), token, dataURI)
		if err != nil {
			h.markAccountStatus(r.Context(), acc, err)
			http.Error(w, "image upload failed: "+err.Error(), http.StatusBadGateway)
			return
		}
		u := strings.TrimSpace(fileURI)
		if u == "" {
			http.Error(w, "image upload failed: empty file uri", http.StatusBadGateway)
			return
		}
		if !strings.HasPrefix(strings.ToLower(u), "http://") && !strings.HasPrefix(strings.ToLower(u), "https://") {
			u = "https://assets.grok.com/" + strings.TrimLeft(u, "/")
		}
		imageURLs = append(imageURLs, u)
	}

	parentPostID := ""
	if len(imageURLs) > 0 {
		if postID, err := h.client.createMediaPost(r.Context(), token, "MEDIA_POST_TYPE_IMAGE", "", imageURLs[0]); err == nil {
			parentPostID = postID
		} else {
			slog.Warn("grok image edit create post failed, continue without parentPostId", "error", err)
		}
	}

	rawPayload := h.buildImageEditPayload(spec, prompt, imageURLs, parentPostID)

	if stream {
		resp, err := h.client.doChat(r.Context(), token, rawPayload)
		if err != nil {
			h.markAccountStatus(r.Context(), acc, err)
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		defer resp.Body.Close()
		h.syncGrokQuota(acc, resp.Header)
		h.streamImageGeneration(w, resp.Body, token, responseFormat, n)
		return
	}

	callsNeeded := (n + 1) / 2
	if callsNeeded < 1 {
		callsNeeded = 1
	}

	var urls []string
	for i := 0; i < callsNeeded; i++ {
		resp, err := h.client.doChat(r.Context(), token, rawPayload)
		if err != nil {
			h.markAccountStatus(r.Context(), acc, err)
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		h.syncGrokQuota(acc, resp.Header)
		err = parseUpstreamLines(resp.Body, func(line map[string]interface{}) error {
			if mr, ok := line["modelResponse"].(map[string]interface{}); ok {
				urls = append(urls, extractImageURLs(mr)...)
			}
			return nil
		})
		resp.Body.Close()
		if err != nil {
			http.Error(w, "stream parse error: "+err.Error(), http.StatusBadGateway)
			return
		}
	}

	urls = uniqueStrings(urls)
	if len(urls) == 0 {
		http.Error(w, "no image generated", http.StatusBadGateway)
		return
	}
	if len(urls) > n {
		urls = urls[:n]
	}

	field := imageResponseField(responseFormat)
	data := make([]map[string]interface{}, 0, len(urls))
	for _, u := range urls {
		val, err := h.imageOutputValue(r.Context(), token, u, responseFormat)
		if err != nil {
			slog.Warn("grok image edit convert failed", "url", u, "error", err)
			if field == "url" {
				val = u
			}
		}
		data = append(data, map[string]interface{}{field: val})
	}

	out := map[string]interface{}{
		"created": time.Now().Unix(),
		"data":    data,
		"usage":   imageUsagePayload(),
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(out)
}

func sanitizeCachedFilename(raw string) string {
	name := strings.TrimSpace(raw)
	name = strings.ReplaceAll(name, "\\", "-")
	name = strings.ReplaceAll(name, "/", "-")
	name = strings.TrimSpace(name)
	if name == "" || strings.Contains(name, "..") {
		return ""
	}
	return name
}

func parseFilesPath(rawPath string) (mediaType string, fileName string, ok bool) {
	if !strings.HasPrefix(rawPath, "/grok/v1/files/") {
		return "", "", false
	}
	path := strings.TrimPrefix(rawPath, "/grok/v1/files/")
	path = strings.TrimSpace(path)
	if path == "" {
		return "", "", false
	}
	parts := strings.SplitN(path, "/", 2)
	if len(parts) != 2 {
		return "", "", false
	}
	mediaType = strings.ToLower(strings.TrimSpace(parts[0]))
	if mediaType != "image" && mediaType != "video" {
		return "", "", false
	}
	fileName = sanitizeCachedFilename(parts[1])
	if fileName == "" {
		return "", "", false
	}
	return mediaType, fileName, true
}

func (h *Handler) HandleFiles(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	mediaType, fileName, ok := parseFilesPath(r.URL.Path)
	if !ok {
		http.Error(w, "file not found", http.StatusNotFound)
		return
	}

	fullPath := filepath.Join(cacheBaseDir, mediaType, fileName)
	info, err := os.Stat(fullPath)
	if err != nil || !info.Mode().IsRegular() {
		http.Error(w, "file not found", http.StatusNotFound)
		return
	}

	ctype := mime.TypeByExtension(strings.ToLower(filepath.Ext(fileName)))
	if ctype == "" {
		if mediaType == "video" {
			ctype = "video/mp4"
		} else {
			ctype = "image/jpeg"
		}
	}
	w.Header().Set("Content-Type", ctype)
	w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	http.ServeFile(w, r, fullPath)
}

func (h *Handler) HandleAdminVoiceToken(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	voice := strings.TrimSpace(r.URL.Query().Get("voice"))
	if voice == "" {
		voice = "ara"
	}
	personality := strings.TrimSpace(r.URL.Query().Get("personality"))
	if personality == "" {
		personality = "assistant"
	}
	speed := 1.0
	if raw := strings.TrimSpace(r.URL.Query().Get("speed")); raw != "" {
		if v, err := strconv.ParseFloat(raw, 64); err == nil && v > 0 {
			speed = v
		}
	}

	acc, token, err := h.selectAccount(r.Context())
	if err != nil {
		http.Error(w, "no available grok token: "+err.Error(), http.StatusServiceUnavailable)
		return
	}

	data, err := h.client.getVoiceToken(r.Context(), token, voice, personality, speed)
	if err != nil {
		h.markAccountStatus(r.Context(), acc, err)
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	respToken, _ := data["token"].(string)
	respToken = strings.TrimSpace(respToken)
	if respToken == "" {
		http.Error(w, "upstream returned no voice token", http.StatusBadGateway)
		return
	}

	out := map[string]interface{}{
		"token":            respToken,
		"url":              "wss://livekit.grok.com",
		"participant_name": "",
		"room_name":        "",
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(out)
}

func (h *Handler) LogSummary() {
	slog.Info("grok go-native endpoints enabled")
}
