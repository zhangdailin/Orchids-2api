package grok

import (
	"context"
	"fmt"
	"github.com/goccy/go-json"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"
)

func (h *Handler) buildImageEditPayload(spec ModelSpec, prompt string, imageURLs []string, parentPostID string) map[string]interface{} {
	imageEditCfg := map[string]interface{}{
		"imageReferences": imageURLs,
	}
	if strings.TrimSpace(parentPostID) != "" {
		imageEditCfg["parentPostId"] = strings.TrimSpace(parentPostID)
	}
	temporary := true
	disableMemory := false
	customPersonality := ""
	if h != nil && h.cfg != nil {
		temporary = h.cfg.GrokChatTemporary()
		disableMemory = h.cfg.GrokChatDisableMemory(false)
		customPersonality = h.cfg.GrokChatCustomInstruction()
	}
	payload := map[string]interface{}{
		"temporary":                 temporary,
		"modelName":                 spec.UpstreamModel,
		"modelMode":                 spec.ModelMode,
		"message":                   strings.TrimSpace(prompt),
		"fileAttachments":           []string{},
		"imageAttachments":          []string{},
		"disableSearch":             false,
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
			"requestModelDetails": map[string]interface{}{
				"modelId": spec.UpstreamModel,
			},
		},
		"disableMemory":   disableMemory,
		"forceSideBySide": false,
		"deviceEnvInfo": map[string]interface{}{
			"darkModeEnabled":  false,
			"devicePixelRatio": 2,
			"screenWidth":      2056,
			"screenHeight":     1329,
			"viewportWidth":    2056,
			"viewportHeight":   1083,
		},
	}
	if customPersonality != "" {
		payload["customPersonality"] = customPersonality
	}
	return payload
}

func (h *Handler) buildImageEditRequestPayload(
	ctx context.Context,
	token string,
	spec ModelSpec,
	prompt string,
	inputs []imageEditUploadInput,
) (map[string]interface{}, error) {
	imageURLs := make([]string, 0, len(inputs))
	for _, in := range inputs {
		dataURI := dataURIFromBytes(in.mime, in.data)
		_, fileURI, err := h.uploadSingleInput(ctx, token, dataURI)
		if err != nil {
			return nil, fmt.Errorf("image upload failed: %w", err)
		}
		u := strings.TrimSpace(fileURI)
		if u == "" {
			return nil, fmt.Errorf("image upload failed: empty file uri")
		}
		if !strings.HasPrefix(strings.ToLower(u), "http://") && !strings.HasPrefix(strings.ToLower(u), "https://") {
			u = "https://assets.grok.com/" + strings.TrimLeft(u, "/")
		}
		imageURLs = append(imageURLs, u)
	}

	parentPostID := ""
	if len(imageURLs) > 0 {
		if postID, err := h.client.createMediaPost(ctx, token, "MEDIA_POST_TYPE_IMAGE", "", imageURLs[0]); err == nil {
			parentPostID = postID
		} else {
			slog.Warn("grok image edit create post failed, continue without parentPostId", "error", err)
		}
	}
	return h.buildImageEditPayload(spec, prompt, imageURLs, parentPostID), nil
}

func (h *Handler) buildImageEditPayloadFromInputs(
	ctx context.Context,
	token string,
	spec ModelSpec,
	prompt string,
	inputs []string,
) (map[string]interface{}, error) {
	imageURLs := make([]string, 0, len(inputs))
	for _, in := range inputs {
		raw := strings.TrimSpace(in)
		if raw == "" {
			continue
		}
		_, fileURI, err := h.uploadSingleInput(ctx, token, raw)
		if err != nil {
			return nil, fmt.Errorf("image upload failed: %w", err)
		}
		u := strings.TrimSpace(fileURI)
		if u == "" {
			return nil, fmt.Errorf("image upload failed: empty file uri")
		}
		if !strings.HasPrefix(strings.ToLower(u), "http://") && !strings.HasPrefix(strings.ToLower(u), "https://") {
			u = "https://assets.grok.com/" + strings.TrimLeft(u, "/")
		}
		imageURLs = append(imageURLs, u)
	}
	if len(imageURLs) == 0 {
		return nil, fmt.Errorf("image upload failed: empty image urls")
	}

	parentPostID := ""
	if postID, err := h.client.createMediaPost(ctx, token, "MEDIA_POST_TYPE_IMAGE", "", imageURLs[0]); err == nil {
		parentPostID = postID
	} else {
		slog.Warn("grok image edit create post failed, continue without parentPostId", "error", err)
	}
	return h.buildImageEditPayload(spec, prompt, imageURLs, parentPostID), nil
}

func (h *Handler) handleChatImageEdit(
	ctx context.Context,
	w http.ResponseWriter,
	req ChatCompletionsRequest,
	spec ModelSpec,
	prompt string,
	imageURLs []string,
	publicBase string,
) {
	if len(imageURLs) == 0 {
		http.Error(w, "image_url is required for image edits", http.StatusBadRequest)
		return
	}
	if len(imageURLs) > 16 {
		http.Error(w, "too many images. maximum is 16", http.StatusBadRequest)
		return
	}

	imageCfg := req.ImageConfig
	if imageCfg == nil {
		imageCfg = &ImageConfig{}
	}
	imageCfg.Normalize()
	n := imageCfg.N
	if n < 1 {
		n = 1
	}
	if n > 10 {
		n = 10
	}
	responseFormat := normalizeImageResponseFormat(imageCfg.ResponseFormat)

	sess, err := h.openChatAccountSession(ctx)
	if err != nil {
		http.Error(w, "no available grok token: "+err.Error(), http.StatusServiceUnavailable)
		return
	}
	defer sess.Close()

	rawPayload, err := h.buildImageEditPayloadFromInputs(ctx, sess.token, spec, prompt, imageURLs)
	if err != nil {
		h.markAccountStatus(ctx, sess.acc, err)
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	rebuildPayload := func(token string) (map[string]interface{}, error) {
		return h.buildImageEditPayloadFromInputs(ctx, token, spec, prompt, imageURLs)
	}

	if req.Stream {
		resp, err := h.doChatWithAutoSwitchRebuild(ctx, sess, &rawPayload, rebuildPayload)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		defer resp.Body.Close()
		h.syncGrokQuota(sess.acc, resp.Header)
		h.streamImageGeneration(w, resp.Body, sess.token, prompt, responseFormat, n, publicBase)
		return
	}

	callsNeeded := (n + 1) / 2
	if callsNeeded < 1 {
		callsNeeded = 1
	}

	var urls []string
	for i := 0; i < callsNeeded; i++ {
		resp, err := h.doChatWithAutoSwitchRebuild(ctx, sess, &rawPayload, rebuildPayload)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		h.syncGrokQuota(sess.acc, resp.Header)
		err = parseUpstreamLines(resp.Body, func(line map[string]interface{}) error {
			if mr := extractUpstreamModelResponse(line); mr != nil {
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

	urls = normalizeGeneratedImageURLs(urls, n)
	if len(urls) == 0 {
		http.Error(w, "no image generated", http.StatusBadGateway)
		return
	}

	field := imageResponseField(responseFormat)
	data := make([]map[string]interface{}, 0, len(urls))
	for _, u := range urls {
		val, err := h.imageOutputValue(ctx, sess.token, u, responseFormat)
		if err != nil {
			slog.Warn("grok image edit convert failed", "url", u, "error", err)
			if field == "url" {
				val = u
			} else {
				val = ""
			}
		}
		if field == "url" && publicBase != "" && strings.HasPrefix(val, "/") {
			val = publicBase + val
		}
		data = append(data, map[string]interface{}{
			field:            val,
			"revised_prompt": nil,
		})
	}

	out := map[string]interface{}{
		"created": time.Now().Unix(),
		"data":    data,
		"usage":   buildImageUsagePayload(prompt, len(data)),
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(out)
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
		model = "grok-imagine-image-edit"
	}
	model = normalizeModelID(model)
	if !isImageEditModel(model) {
		http.Error(w, "The model `grok-imagine-image-edit` is required for image edits.", http.StatusBadRequest)
		return
	}
	n := parseIntLoose(r.FormValue("n"), 1)
	if n < 1 || n > 10 {
		http.Error(w, "n must be between 1 and 10", http.StatusBadRequest)
		return
	}
	if _, err := normalizeImageSize(r.FormValue("size")); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	stream := parseBoolLoose(r.FormValue("stream"), false)
	if stream && n > 2 {
		http.Error(w, "streaming is only supported when n=1 or n=2", http.StatusBadRequest)
		return
	}
	responseFormat := normalizeImageResponseFormat(r.FormValue("response_format"))
	publicBase := detectPublicBaseURL(r)

	spec, ok := ResolveModel(model)
	if !ok || !spec.IsImage || !isImageEditModel(spec.ID) {
		http.Error(w, "The model `grok-imagine-image-edit` is required for image edits.", http.StatusBadRequest)
		return
	}
	if err := h.ensureModelEnabled(r.Context(), model); err != nil {
		http.Error(w, modelValidationMessage(model, err), http.StatusBadRequest)
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

	sess, err := h.openChatAccountSession(r.Context())
	if err != nil {
		http.Error(w, "no available grok token: "+err.Error(), http.StatusServiceUnavailable)
		return
	}
	defer sess.Close()

	uploads := make([]imageEditUploadInput, 0, len(files))
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
		uploads = append(uploads, imageEditUploadInput{
			mime: mime,
			data: data,
		})
	}

	rawPayload, err := h.buildImageEditRequestPayload(r.Context(), sess.token, spec, prompt, uploads)
	if err != nil {
		h.markAccountStatus(r.Context(), sess.acc, err)
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	rebuildPayload := func(token string) (map[string]interface{}, error) {
		return h.buildImageEditRequestPayload(r.Context(), token, spec, prompt, uploads)
	}

	if stream {
		resp, err := h.doChatWithAutoSwitchRebuild(r.Context(), sess, &rawPayload, rebuildPayload)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		defer resp.Body.Close()
		h.syncGrokQuota(sess.acc, resp.Header)
		h.streamImageGeneration(w, resp.Body, sess.token, prompt, responseFormat, n, publicBase)
		return
	}

	callsNeeded := (n + 1) / 2
	if callsNeeded < 1 {
		callsNeeded = 1
	}

	var urls []string
	for i := 0; i < callsNeeded; i++ {
		resp, err := h.doChatWithAutoSwitchRebuild(r.Context(), sess, &rawPayload, rebuildPayload)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		h.syncGrokQuota(sess.acc, resp.Header)
		err = parseUpstreamLines(resp.Body, func(line map[string]interface{}) error {
			if mr := extractUpstreamModelResponse(line); mr != nil {
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

	urls = normalizeGeneratedImageURLs(urls, n)
	if len(urls) == 0 {
		http.Error(w, "no image generated", http.StatusBadGateway)
		return
	}

	field := imageResponseField(responseFormat)
	data := make([]map[string]interface{}, 0, len(urls))
	for _, u := range urls {
		val, err := h.imageOutputValue(r.Context(), sess.token, u, responseFormat)
		if err != nil {
			slog.Warn("grok image edit convert failed", "url", u, "error", err)
			if field == "url" {
				val = u
			}
		}
		if field == "url" && publicBase != "" && strings.HasPrefix(val, "/") {
			val = publicBase + val
		}
		data = append(data, map[string]interface{}{
			field:            val,
			"revised_prompt": nil,
		})
	}

	out := map[string]interface{}{
		"created": time.Now().Unix(),
		"data":    data,
		"usage":   buildImageUsagePayload(prompt, len(data)),
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(out)
}
