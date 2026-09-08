package grok

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"time"

	"orchids-api/internal/store"
)

func (h *Handler) streamImageGeneration(w http.ResponseWriter, body io.Reader, token, prompt, format string, n int, publicBase string) {
	flusher := streamResponseHeaders(w)

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
			writeSSE(w, flusher, "image_generation.partial_image", encodeJSONBytes(data))
		}
		urls = appendImageResultURLs(urls, resp)
		return nil
	}); err != nil {
		writeSSEStreamError(w, flusher, nil, "stream parse error: "+err.Error())
		return
	}

	urls = normalizeGeneratedImageURLs(urls, n)
	if len(urls) == 0 {
		writeSSECodedError(w, flusher, "no image generated", "no_image_generated")
		return
	}

	for i, u := range urls {
		val, err := h.imageOutputValue(context.Background(), token, u, format)
		if err != nil {
			slog.Warn("grok image stream convert failed", "url", u, "error", err)
			if field == "url" && !mustCacheImageURL(u) {
				val = u
			} else {
				writeSSECodedError(w, flusher, "image cache failed: "+err.Error(), "image_cache_failed")
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
		writeSSE(w, flusher, "image_generation.completed", encodeJSONBytes(data))
	}
	writeSSE(w, flusher, "", []byte("[DONE]"))
}

func (h *Handler) HandleImagesGenerations(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodPost) {
		return
	}
	var req ImagesGenerationsRequest
	if !decodeJSONBody(w, r, &req) {
		return
	}
	req.Model = normalizeModelID(req.Model)
	req.Normalize()
	if !requireAPIKeyModel(w, r, req.Model) {
		return
	}
	rawResponseFormat := strings.ToLower(strings.TrimSpace(req.ResponseFormat))
	if rawResponseFormat != "" && rawResponseFormat != "url" && rawResponseFormat != "b64_json" && rawResponseFormat != "base64" {
		http.Error(w, "response_format must be url or b64_json", http.StatusBadRequest)
		return
	}
	req.ResponseFormat = normalizeImageResponseFormat(req.ResponseFormat)
	if !isImageGenerationModel(req.Model) {
		http.Error(w, "image generation model must be one of [grok-imagine-image-lite, grok-imagine-image, grok-imagine-image-2.0, grok-imagine-image-quality, grok-imagine-image-pro]", http.StatusBadRequest)
		return
	}
	ratio, ratioErr := normalizeImageAspectRatio(req.AspectRatio, req.Size)
	if ratioErr != nil {
		http.Error(w, ratioErr.Error(), http.StatusBadRequest)
		return
	}
	req.AspectRatio = ratio
	req.Size = strings.ToLower(strings.TrimSpace(req.Size))
	req.Prompt = strings.TrimSpace(req.Prompt)
	if req.Prompt == "" {
		http.Error(w, "prompt is required", http.StatusBadRequest)
		return
	}
	if req.N < 1 || req.N > 10 {
		http.Error(w, "n must be between 1 and 10", http.StatusBadRequest)
		return
	}
	if req.Stream && req.N != 1 {
		http.Error(w, "Streaming is only supported with n=1.", http.StatusBadRequest)
		return
	}
	if req.Stream && normalizeModelID(req.Model) == "grok-imagine-image-lite" {
		http.Error(w, "grok-imagine-image-lite does not support stream", http.StatusBadRequest)
		return
	}
	if req.PartialImages != nil {
		if *req.PartialImages < 0 || *req.PartialImages > 3 {
			http.Error(w, "partial_images must be between 0 and 3", http.StatusBadRequest)
			return
		}
		if *req.PartialImages > 0 && !req.Stream {
			http.Error(w, "partial_images requires stream=true", http.StatusBadRequest)
			return
		}
	}
	quality := strings.ToLower(strings.TrimSpace(req.Quality))
	if quality != "" && quality != "low" && quality != "medium" {
		http.Error(w, "quality must be low or medium", http.StatusBadRequest)
		return
	}
	req.Quality = quality
	if raw := strings.TrimSpace(string(req.StorageOptions)); raw != "" && raw != "null" {
		http.Error(w, "storage_options is not supported", http.StatusBadRequest)
		return
	}
	h.serveImagesGenerations(r.Context(), w, req, detectPublicBaseURL(r))
}

func (h *Handler) serveImagesGenerations(ctx context.Context, w http.ResponseWriter, req ImagesGenerationsRequest, publicBase string) {
	if err := h.ensureModelCapability(ctx, req.Model, store.CapabilityImage); err != nil {
		http.Error(w, modelValidationMessage(req.Model, err), http.StatusBadRequest)
		return
	}

	spec, ok := ResolveModel(req.Model)
	if !ok || !spec.IsImage || !isImageGenerationModel(spec.ID) {
		http.Error(w, fmt.Sprintf("The model `%s` is not supported for image generation. Supported: [grok-imagine-image-lite, grok-imagine-image, grok-imagine-image-2.0, grok-imagine-image-quality, grok-imagine-image-pro]", req.Model), http.StatusBadRequest)
		return
	}
	spec = h.applyPersistedRoute(ctx, spec)
	if spec.Upstream == UpstreamConsole {
		h.serveConsoleImagesGeneration(ctx, w, spec, req, publicBase)
		return
	}

	var sess *chatAccountSession
	var err error
	sess, err = h.openChatAccountSessionForModel(ctx, spec)
	if err != nil {
		http.Error(w, "no available grok token: "+err.Error(), http.StatusServiceUnavailable)
		return
	}
	defer func() {
		sess.Close()
	}()

	if req.Stream {
		if normalizeModelID(spec.ID) == "grok-imagine-image-lite" {
			h.streamAppChatImagesGeneration(ctx, w, sess, spec, req, publicBase)
			return
		}
		h.streamImagineWSGeneration(ctx, w, sess, spec, req)
		return
	}
	if normalizeModelID(spec.ID) != "grok-imagine-image-lite" {
		h.collectImagineWSGeneration(ctx, w, sess, spec, req, publicBase)
		return
	}
	urls, err := h.collectAppChatImageURLs(ctx, sess, spec, req, true)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	if len(urls) == 0 {
		http.Error(w, "no image generated", http.StatusBadGateway)
		return
	}

	h.writeImageResults(w, ctx, sess.token, req.Prompt, urls, req.ResponseFormat, publicBase, true)
}

func imagineWSProModel(modelID string) bool {
	switch normalizeModelID(modelID) {
	case "grok-imagine-image-2.0", "grok-imagine-image-quality", "grok-imagine-image-pro":
		return true
	default:
		return false
	}
}

func imagePartialCount(req ImagesGenerationsRequest) int {
	if req.PartialImages == nil {
		return 0
	}
	return *req.PartialImages
}

func (h *Handler) streamImagineWSGeneration(ctx context.Context, w http.ResponseWriter, sess *chatAccountSession, spec ModelSpec, req ImagesGenerationsRequest) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	flusher := streamResponseHeaders(w)
	nsfw := req.NSFW != nil && *req.NSFW
	events, errs := h.streamImagineWSImages(ctx, sess, req.Prompt, req.AspectRatio, req.N, nsfw, imagineWSProModel(spec.ID))
	partialLimit := imagePartialCount(req)
	partialIndex := 0
	completed := 0
	for ev := range events {
		if ev.Type == "progress" && partialIndex < partialLimit && (strings.TrimSpace(ev.Blob) != "" || strings.TrimSpace(ev.URL) != "") {
			value, err := h.imagineImageOutputValue(ctx, sess.token, ev, "b64_json")
			if err == nil && strings.TrimSpace(value) != "" {
				outputFormat := imageOutputFormatFromBase64(value)
				writeSSE(w, flusher, "image_generation.partial_image", encodeJSONBytes(map[string]interface{}{
					"type": "image_generation.partial_image", "b64_json": value,
					"created_at": time.Now().Unix(), "size": "auto", "quality": "auto",
					"background": "auto", "output_format": outputFormat, "partial_image_index": partialIndex,
				}))
				partialIndex++
			}
		}
		if !ev.Final || (strings.TrimSpace(ev.Blob) == "" && strings.TrimSpace(ev.URL) == "") {
			continue
		}
		value, err := h.imagineImageOutputValue(ctx, sess.token, ev, "b64_json")
		if err != nil {
			writeSSEStreamError(w, flusher, nil, "image download failed: "+err.Error())
			return
		}
		outputFormat := imageOutputFormatFromBase64(value)
		writeSSE(w, flusher, "image_generation.completed", encodeJSONBytes(map[string]interface{}{
			"type": "image_generation.completed", "b64_json": value,
			"created_at": time.Now().Unix(), "size": "auto", "quality": "auto",
			"background": "auto", "output_format": outputFormat,
		}))
		completed++
	}
	if err := <-errs; err != nil {
		writeSSEStreamError(w, flusher, nil, err.Error())
		return
	}
	if completed == 0 {
		writeSSECodedError(w, flusher, "no image generated", "no_image_generated")
		return
	}
	writeSSE(w, flusher, "", []byte("[DONE]"))
}

func (h *Handler) collectImagineWSGeneration(ctx context.Context, w http.ResponseWriter, sess *chatAccountSession, spec ModelSpec, req ImagesGenerationsRequest, publicBase string) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	nsfw := req.NSFW != nil && *req.NSFW
	events, errs := h.streamImagineWSImages(ctx, sess, req.Prompt, req.AspectRatio, req.N, nsfw, imagineWSProModel(spec.ID))
	field := imageResponseField(req.ResponseFormat)
	data := make([]map[string]interface{}, 0, req.N)
	for ev := range events {
		if !ev.Final || (strings.TrimSpace(ev.Blob) == "" && strings.TrimSpace(ev.URL) == "") {
			continue
		}
		value, err := h.imagineImageOutputValue(ctx, sess.token, ev, req.ResponseFormat)
		if err != nil {
			http.Error(w, "image conversion failed: "+err.Error(), http.StatusBadGateway)
			return
		}
		if field == "url" && publicBase != "" && strings.HasPrefix(value, "/") {
			value = publicBase + value
		}
		data = append(data, map[string]interface{}{field: value, "revised_prompt": nil})
	}
	if err := <-errs; err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	if len(data) == 0 {
		http.Error(w, "no image generated", http.StatusBadGateway)
		return
	}
	writeJSON(w, map[string]interface{}{
		"created": time.Now().Unix(), "data": data,
		"usage": buildImageUsagePayload(req.Prompt, len(data)),
	})
}

func (h *Handler) streamAppChatImagesGeneration(ctx context.Context, w http.ResponseWriter, sess *chatAccountSession, spec ModelSpec, req ImagesGenerationsRequest, publicBase string) {
	onePayload := h.client.appChatImagePayload(spec, req.Prompt, req.Size, req.N)
	ensureImageNSFW(onePayload)
	resp, err := h.doAppChatImageRequest(ctx, sess, spec, &onePayload, true)
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

func (h *Handler) collectAppChatImageURLs(ctx context.Context, sess *chatAccountSession, spec ModelSpec, req ImagesGenerationsRequest, allowSwitch bool) ([]string, error) {
	var urls []string
	var debugShapes []string
	var debugNoImage []string

	// Grok upstream may return only 2 images per call and may repeat.
	// To reach N, request 1 image per call without rewriting the user's prompt.
	maxAttempts := req.N * 2
	if maxAttempts < 4 {
		maxAttempts = 4
	}
	deadline := time.Now().Add(60 * time.Second)
	excludedAccountIDs := make([]int64, 0, maxAttempts)
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
		payload := h.client.appChatImagePayload(spec, req.Prompt, req.Size, count)
		ensureImageNSFW(payload)
		resp, err := h.doAppChatImageRequest(ctx, sess, spec, &payload, allowSwitch)
		if err != nil {
			slog.Warn("grok app-chat image upstream failed",
				"model", req.Model,
				"status", parseUpstreamStatus(err),
				"error", err,
			)
			return nil, err
		}
		imageLimitHit := false
		h.syncGrokQuota(sess.acc, resp.Header)
		err = parseUpstreamLines(resp.Body, func(line map[string]interface{}) error {
			if len(debugShapes) < 20 {
				debugShapes = append(debugShapes, imageDebugShape(line))
			}
			if len(debugNoImage) < 20 {
				debugNoImage = append(debugNoImage, appChatImageNoImageDiagnostics(line)...)
			}
			if isAppChatImageLimitResponse(line) {
				imageLimitHit = true
			}
			urls = append(urls, extractAppChatImageURLs(line)...)
			return nil
		})
		resp.Body.Close()
		if err != nil {
			return nil, fmt.Errorf("stream parse error: %w", err)
		}
		urls = normalizeGeneratedImageURLs(urls, 0)
		if len(urls) == 0 && imageLimitHit {
			err := fmt.Errorf("grok upstream status=429 body=image generation limit reached")
			h.markAccountStatus(ctx, sess.acc, err)
			if !allowSwitch || sess == nil || i == maxAttempts-1 {
				return nil, err
			}
			if sess.acc != nil && sess.acc.ID != 0 {
				if !slices.Contains(excludedAccountIDs, sess.acc.ID) {
					excludedAccountIDs = append(excludedAccountIDs, sess.acc.ID)
				}
				sess.Close()
				next, switchErr := h.openChatAccountSessionExcludingWithPools(ctx, excludedAccountIDs, sess.poolCandidates)
				if switchErr != nil {
					return nil, err
				}
				sess.acc = next.acc
				sess.token = next.token
				sess.poolCandidates = next.poolCandidates
				sess.release = next.release
			}
		}
	}
	urls = normalizeGeneratedImageURLs(urls, req.N)
	if len(urls) == 0 {
		slog.Warn("grok image generation returned no images",
			"model", req.Model,
			"attempts", maxAttempts,
			"event_shapes", uniqueStrings(debugShapes),
			"diagnostics", uniqueStrings(debugNoImage),
		)
		return nil, fmt.Errorf("no image generated")
	}
	return urls, nil
}

func (h *Handler) doAppChatImageRequest(ctx context.Context, sess *chatAccountSession, spec ModelSpec, payload *map[string]interface{}, allowSwitch bool) (*http.Response, error) {
	if payload == nil {
		return nil, fmt.Errorf("empty payload")
	}
	if normalizeModelID(spec.ID) == "grok-imagine-image-lite" {
		if allowSwitch {
			return h.doAutoSwitchRequest(ctx, sess, payload, nil, (*Client).doChat)
		}
		return h.doSingleAccountRequest(ctx, sess, *payload, markAllGrokAccountStatuses, (*Client).doChat)
	}
	if allowSwitch {
		return h.doAutoSwitchRequest(ctx, sess, payload, nil, (*Client).doAppChatCreateAndRespond)
	}
	return h.doSingleAccountRequest(ctx, sess, *payload, markAllGrokAccountStatuses, (*Client).doAppChatCreateAndRespond)
}
