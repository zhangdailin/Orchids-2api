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

func (h *Handler) streamImageGeneration(w http.ResponseWriter, body io.Reader, token, prompt, format string, n int, publicBase string) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	flusher, _ := w.(http.Flusher)

	field := imageResponseField(format)
	var urls []string
	targetIndex := -1

	if err := parseUpstreamLines(body, func(resp map[string]interface{}) error {
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
			writeSSEBytes(w, "image_generation.partial_image", encodeJSONBytes(data))
			if flusher != nil {
				flusher.Flush()
			}
		}
		urls = appendImageResultURLs(urls, resp)
		return nil
	}); err != nil {
		writeSSEError(w, "stream parse error: "+err.Error(), "server_error", "stream_error")
		writeSSEBytes(w, "", []byte("[DONE]"))
		if flusher != nil {
			flusher.Flush()
		}
		return
	}

	urls = normalizeGeneratedImageURLs(urls, n)
	if len(urls) == 0 {
		writeSSEError(w, "no image generated", "server_error", "no_image_generated")
		writeSSEBytes(w, "", []byte("[DONE]"))
		if flusher != nil {
			flusher.Flush()
		}
		return
	}

	for i, u := range urls {
		val, err := h.imageOutputValue(context.Background(), token, u, format)
		if err != nil {
			slog.Warn("grok image stream convert failed", "url", u, "error", err)
			if field == "url" && !mustCacheImageURL(u) {
				val = u
			} else {
				writeSSEError(w, "image cache failed: "+err.Error(), "server_error", "image_cache_failed")
				writeSSEBytes(w, "", []byte("[DONE]"))
				if flusher != nil {
					flusher.Flush()
				}
				return
			}
		}
		if field == "url" && publicBase != "" && strings.HasPrefix(val, "/") {
			val = publicBase + val
		}
		data := map[string]interface{}{
			"type":           "image_generation.completed",
			field:            val,
			"index":          i,
			"revised_prompt": nil,
			"usage":          buildImageUsagePayload(prompt, len(urls)),
		}
		writeSSEBytes(w, "image_generation.completed", encodeJSONBytes(data))
		if flusher != nil {
			flusher.Flush()
		}
	}
	writeSSEBytes(w, "", []byte("[DONE]"))
	if flusher != nil {
		flusher.Flush()
	}
}

func (h *Handler) serveImagineWSImages(ctx context.Context, w http.ResponseWriter, sess *chatAccountSession, req ImagesGenerationsRequest, publicBase string, nsfw bool, pro bool) {
	if req.Stream {
		h.streamImagineWSImageGeneration(ctx, w, sess, req, publicBase, nsfw, pro)
		return
	}
	field := imageResponseField(req.ResponseFormat)
	data := make([]map[string]interface{}, 0, req.N)
	used := make([]int64, 0, 3)
	maxAttempts := 2
	if h != nil && h.cfg != nil && h.cfg.AccountSwitchCount > 0 {
		maxAttempts = h.cfg.AccountSwitchCount
	}
	if maxAttempts < 1 {
		maxAttempts = 1
	}
	for attempt := 0; attempt < maxAttempts; attempt++ {
		if sess != nil && sess.acc != nil && sess.acc.ID != 0 {
			used = append(used, sess.acc.ID)
		}
		events, errs := h.streamImagineWSImages(ctx, sess, req.Prompt, resolveAspectRatio(req.Size), req.N, nsfw, pro)
		data = data[:0]
		for ev := range events {
			if !ev.Final {
				continue
			}
			val, err := h.imagineImageOutputValue(ctx, sess.token, ev, req.ResponseFormat)
			if err != nil {
				slog.Warn("grok imagine ws convert failed", "url", ev.URL, "image_id", ev.ImageID, "error", err)
				if field == "url" && !mustCacheImageURL(ev.URL) {
					val = ev.URL
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
		if err := <-errs; err != nil {
			if !shouldSwitchGrokAccount(err) || attempt == maxAttempts-1 {
				http.Error(w, err.Error(), http.StatusBadGateway)
				return
			}
			spec, ok := ResolveModel(req.Model)
			if !ok {
				http.Error(w, "image generation model not found", http.StatusBadRequest)
				return
			}
			nextSess, switchErr := h.openChatAccountSessionForModelExcluding(ctx, used, spec)
			if switchErr != nil {
				http.Error(w, fmt.Sprintf("account switch failed: %v (original: %v)", switchErr, err), http.StatusBadGateway)
				return
			}
			sess.Close()
			sess = nextSess
			continue
		}
		break
	}
	if len(data) == 0 {
		http.Error(w, "no image generated", http.StatusBadGateway)
		return
	}
	out := map[string]interface{}{
		"created": time.Now().Unix(),
		"data":    data,
		"usage":   buildImageUsagePayload(req.Prompt, len(data)),
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(out)
}

func (h *Handler) streamImagineWSImageGeneration(ctx context.Context, w http.ResponseWriter, sess *chatAccountSession, req ImagesGenerationsRequest, publicBase string, nsfw bool, pro bool) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	flusher, _ := w.(http.Flusher)
	field := imageResponseField(req.ResponseFormat)
	events, errs := h.streamImagineWSImages(ctx, sess, req.Prompt, resolveAspectRatio(req.Size), req.N, nsfw, pro)
	completed := 0
	for ev := range events {
		switch ev.Type {
		case "progress":
			data := map[string]interface{}{
				"type":     "image_generation.partial_image",
				field:      "",
				"index":    ev.Order,
				"image_id": ev.ImageID,
				"progress": ev.Progress,
			}
			writeSSEBytes(w, "image_generation.partial_image", encodeJSONBytes(data))
		case "image":
			if !ev.Final {
				continue
			}
			val, err := h.imagineImageOutputValue(ctx, sess.token, ev, req.ResponseFormat)
			if err != nil {
				slog.Warn("grok imagine ws stream convert failed", "url", ev.URL, "image_id", ev.ImageID, "error", err)
				if field == "url" && !mustCacheImageURL(ev.URL) {
					val = ev.URL
				} else {
					writeSSEError(w, "image cache failed: "+err.Error(), "server_error", "image_cache_failed")
					writeSSEBytes(w, "", []byte("[DONE]"))
					if flusher != nil {
						flusher.Flush()
					}
					return
				}
			}
			if field == "url" && publicBase != "" && strings.HasPrefix(val, "/") {
				val = publicBase + val
			}
			data := map[string]interface{}{
				"type":           "image_generation.completed",
				field:            val,
				"index":          completed,
				"image_id":       ev.ImageID,
				"revised_prompt": nil,
				"usage":          buildImageUsagePayload(req.Prompt, req.N),
			}
			completed++
			writeSSEBytes(w, "image_generation.completed", encodeJSONBytes(data))
		case "moderated":
			data := map[string]interface{}{
				"type":     "image_generation.moderated",
				"index":    ev.Order,
				"image_id": ev.ImageID,
			}
			writeSSEBytes(w, "image_generation.moderated", encodeJSONBytes(data))
		}
		if flusher != nil {
			flusher.Flush()
		}
	}
	if err := <-errs; err != nil {
		writeSSEError(w, err.Error(), "server_error", "imagine_ws_error")
	}
	writeSSEBytes(w, "", []byte("[DONE]"))
	if flusher != nil {
		flusher.Flush()
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
	req.Model = normalizeModelID(req.Model)
	req.Normalize()
	req.ResponseFormat = normalizeImageResponseFormat(req.ResponseFormat)
	if !isImageGenerationModel(req.Model) {
		http.Error(w, "image generation model must be one of [grok-imagine-image-lite, grok-imagine-image, grok-imagine-image-pro]", http.StatusBadRequest)
		return
	}
	normalizedSize, sizeErr := normalizeImageSize(req.Size)
	if sizeErr != nil {
		http.Error(w, sizeErr.Error(), http.StatusBadRequest)
		return
	}
	req.Size = normalizedSize
	req.Prompt = strings.TrimSpace(req.Prompt)
	if req.Prompt == "" {
		http.Error(w, "prompt is required", http.StatusBadRequest)
		return
	}
	maxN := 10
	if normalizeModelID(req.Model) == "grok-imagine-image-lite" {
		maxN = 4
	}
	if req.N < 1 || req.N > maxN {
		http.Error(w, fmt.Sprintf("n must be between 1 and %d", maxN), http.StatusBadRequest)
		return
	}
	if req.Stream && req.N > 2 {
		http.Error(w, "streaming is only supported when n=1 or n=2", http.StatusBadRequest)
		return
	}
	h.serveImagesGenerations(r.Context(), w, req, detectPublicBaseURL(r))
}

func (h *Handler) serveImagesGenerations(ctx context.Context, w http.ResponseWriter, req ImagesGenerationsRequest, publicBase string) {
	if err := h.ensureModelEnabled(ctx, req.Model); err != nil {
		http.Error(w, modelValidationMessage(req.Model, err), http.StatusBadRequest)
		return
	}

	spec, ok := ResolveModel(req.Model)
	if !ok || !spec.IsImage || !isImageGenerationModel(spec.ID) {
		http.Error(w, fmt.Sprintf("The model `%s` is not supported for image generation. Supported: [grok-imagine-image-lite, grok-imagine-image, grok-imagine-image-pro]", req.Model), http.StatusBadRequest)
		return
	}

	var sess *chatAccountSession
	var err error
	if spec.ID == "grok-imagine-image-lite" {
		sess, err = h.openAppChatImageAccountSessionForModelExcluding(ctx, nil, spec)
		if err != nil && strings.Contains(err.Error(), "no enabled accounts available") {
			err = fmt.Errorf("no app-chat browser-cookie grok account available; import a full grok.com browser cookie with sso, sso-rw and x-userid/grok_device_id")
		}
	} else {
		sess, err = h.openChatAccountSessionForModel(ctx, spec)
	}
	if err != nil {
		http.Error(w, "no available grok token: "+err.Error(), http.StatusServiceUnavailable)
		return
	}
	defer func() {
		sess.Close()
	}()

	nsfw := req.NSFW
	if nsfw == nil {
		v := true
		if h != nil && h.cfg != nil {
			v = h.cfg.PublicImagineNSFW()
		}
		nsfw = &v
	}

	if req.Stream {
		h.streamAppChatImagesGeneration(ctx, w, sess, spec, req, publicBase, nsfw)
		return
	}
	urls, err := h.collectAppChatImageURLs(ctx, sess, spec, req, nsfw, true)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	if len(urls) == 0 {
		http.Error(w, "no image generated", http.StatusBadGateway)
		return
	}

	field := imageResponseField(req.ResponseFormat)
	data := make([]map[string]interface{}, 0, len(urls))
	for _, u := range urls {
		val, err := h.imageOutputValue(ctx, sess.token, u, req.ResponseFormat)
		if err != nil {
			slog.Warn("grok image convert failed", "url", u, "error", err)
			if field == "url" && !mustCacheImageURL(u) {
				val = u
			} else {
				http.Error(w, "image cache failed: "+err.Error(), http.StatusBadGateway)
				return
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
		"usage":   buildImageUsagePayload(req.Prompt, len(data)),
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(out)
}

func (h *Handler) streamAppChatImagesGeneration(ctx context.Context, w http.ResponseWriter, sess *chatAccountSession, spec ModelSpec, req ImagesGenerationsRequest, publicBase string, nsfw *bool) {
	onePayload := h.client.appChatImagePayload(spec, req.Prompt, req.Size, req.N)
	ensureImageNSFW(onePayload, spec.UpstreamModel, nsfw)
	resp, err := h.doAppChatCreateAndRespondWithAutoSwitchRebuildWithStatusPolicy(ctx, sess, &onePayload, nil, skipAppChatImageGrokAccountStatus)
	if err != nil {
		slog.Warn("grok app-chat image stream upstream failed",
			"model", req.Model,
			"status", parseUpstreamStatus(err),
			"error", err,
		)
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()
	h.syncGrokQuota(sess.acc, resp.Header)
	h.streamImageGeneration(w, resp.Body, sess.token, req.Prompt, req.ResponseFormat, req.N, publicBase)
}

func (h *Handler) collectAppChatImageURLs(ctx context.Context, sess *chatAccountSession, spec ModelSpec, req ImagesGenerationsRequest, nsfw *bool, allowSwitch bool) ([]string, error) {
	var urls []string
	var debugHTTP []string
	var debugAsset []string
	var debugShapes []string
	var debugNoImage []string

	// Grok upstream may return only 2 images per call and may repeat.
	// To reach N, request 1 image per call without rewriting the user's prompt.
	maxAttempts := req.N * 2
	promptVariants := grokAppChatImagePrompts(req.Prompt)
	if maxAttempts < 4 {
		maxAttempts = 4
	}
	deadline := time.Now().Add(60 * time.Second)
	for i := 0; i < maxAttempts; i++ {
		cur := normalizeGeneratedImageURLs(urls, 0)
		if len(cur) >= req.N {
			urls = cur
			break
		}
		if time.Now().After(deadline) {
			break
		}
		count := req.N
		prompt := strings.TrimSpace(req.Prompt)
		if len(promptVariants) > 0 {
			prompt = promptVariants[promptVariantIndex(i, promptVariants)]
		}
		payload := h.client.appChatImagePayload(spec, prompt, req.Size, count)
		ensureImageNSFW(payload, spec.UpstreamModel, nsfw)
		var resp *http.Response
		var err error
		if allowSwitch {
			resp, err = h.doAppChatCreateAndRespondWithAutoSwitchRebuildWithStatusPolicy(ctx, sess, &payload, nil, skipAppChatImageGrokAccountStatus)
		} else {
			resp, err = h.doAppChatCreateAndRespondSingleAccountWithStatusPolicy(ctx, sess, payload, skipAppChatImageGrokAccountStatus)
		}
		if err != nil {
			slog.Warn("grok app-chat image upstream failed",
				"model", req.Model,
				"status", parseUpstreamStatus(err),
				"error", err,
			)
			return nil, err
		}
		h.syncGrokQuota(sess.acc, resp.Header)
		err = parseUpstreamLines(resp.Body, func(line map[string]interface{}) error {
			if len(debugShapes) < 20 {
				debugShapes = append(debugShapes, imageDebugShape(line))
			}
			if len(debugNoImage) < 20 {
				debugNoImage = append(debugNoImage, appChatImageNoImageDiagnostics(line)...)
			}
			urls = append(urls, extractAppChatImageURLs(line)...)
			return nil
		})
		resp.Body.Close()
		if err != nil {
			return nil, fmt.Errorf("stream parse error: %w", err)
		}
		urls = normalizeGeneratedImageURLs(urls, 0)
	}
	urls = normalizeGeneratedImageURLs(urls, req.N)
	if len(urls) == 0 {
		slog.Warn("grok image generation returned no images",
			"model", req.Model,
			"attempts", maxAttempts,
			"event_shapes", uniqueStrings(debugShapes),
			"diagnostics", uniqueStrings(debugNoImage),
			"http_candidates", len(uniqueStrings(debugHTTP)),
			"asset_candidates", len(uniqueStrings(debugAsset)),
		)
		return nil, fmt.Errorf("no image generated")
	}
	return urls, nil
}

func promptVariantIndex(i int, variants []string) int {
	if len(variants) <= 1 || i <= 0 {
		return 0
	}
	if i >= len(variants) {
		return len(variants) - 1
	}
	return i
}

func grokAppChatImagePrompts(prompt string) []string {
	first := grokAppChatImagePrompt(prompt)
	if first == "" {
		return nil
	}
	variants := []string{first}
	if looksLikeShortChinesePortraitPrompt(prompt) {
		variants = append(variants, "Draw a safe-for-work portrait photo of an adult woman, fully clothed, non-sexual, tasteful fashion style, natural lighting, high quality.")
	}
	return uniqueStrings(variants)
}

func looksLikeShortChinesePortraitPrompt(prompt string) bool {
	p := strings.TrimSpace(prompt)
	if p == "" || len([]rune(p)) > 18 {
		return false
	}
	hasChinese := false
	for _, r := range p {
		if r >= '\u4e00' && r <= '\u9fff' {
			hasChinese = true
			break
		}
	}
	if !hasChinese {
		return false
	}
	lower := strings.ToLower(p)
	return strings.Contains(lower, "美女") ||
		strings.Contains(lower, "女生") ||
		strings.Contains(lower, "女孩") ||
		strings.Contains(lower, "女人") ||
		strings.Contains(lower, "人像") ||
		strings.Contains(lower, "照片")
}

func grokAppChatImagePrompt(prompt string) string {
	prompt = strings.TrimSpace(prompt)
	if prompt == "" {
		return prompt
	}
	return prompt
}
