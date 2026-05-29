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
			if field == "url" {
				val = u
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
	events, errs := h.streamImagineWSImages(ctx, sess, req.Prompt, resolveAspectRatio(req.Size), req.N, nsfw, pro)
	field := imageResponseField(req.ResponseFormat)
	data := make([]map[string]interface{}, 0, req.N)
	for ev := range events {
		if !ev.Final {
			continue
		}
		val, err := h.imagineImageOutputValue(ctx, sess.token, ev, req.ResponseFormat)
		if err != nil {
			slog.Warn("grok imagine ws convert failed", "url", ev.URL, "image_id", ev.ImageID, "error", err)
			if field == "url" {
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
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
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
				if field == "url" {
					val = ev.URL
				} else {
					val = ""
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

	sess, err := h.openChatAccountSessionForModel(ctx, spec)
	if err != nil {
		http.Error(w, "no available grok token: "+err.Error(), http.StatusServiceUnavailable)
		return
	}
	defer sess.Close()

	nsfw := req.NSFW
	if nsfw == nil {
		v := true
		if h != nil && h.cfg != nil {
			v = h.cfg.PublicImagineNSFW()
		}
		nsfw = &v
	}

	if imageModelUsesImagineWS(req.Model) {
		h.serveImagineWSImages(ctx, w, sess, req, publicBase, *nsfw, imageModelUsesProImagineWS(req.Model))
		return
	}

	onePayload := h.client.chatPayload(spec, req.Prompt, true, req.N)
	prepareAppChatImageGenerationPayload(onePayload, req.N)
	ensureImageAspectRatio(onePayload, resolveAspectRatio(req.Size))
	ensureImageNSFW(onePayload, nsfw)
	if req.Stream {
		resp, err := h.doChatSingleAccount(ctx, sess, onePayload)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		defer resp.Body.Close()
		h.syncGrokQuota(sess.acc, resp.Header)
		h.streamImageGeneration(w, resp.Body, sess.token, req.Prompt, req.ResponseFormat, req.N, publicBase)
		return
	}

	var urls []string
	var debugHTTP []string
	var debugAsset []string
	var debugShapes []string

	// Grok upstream may return only 2 images per call and may repeat.
	// To reach N, request 1 image per call without rewriting the user's prompt.
	maxAttempts := req.N * 4
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
		payload := h.client.chatPayload(spec, strings.TrimSpace(req.Prompt), true, 1)
		prepareAppChatImageGenerationPayload(payload, 1)
		ensureImageAspectRatio(payload, resolveAspectRatio(req.Size))
		ensureImageNSFW(payload, nsfw)
		resp, err := h.doChatSingleAccount(ctx, sess, payload)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		h.syncGrokQuota(sess.acc, resp.Header)
		err = parseUpstreamLines(resp.Body, func(line map[string]interface{}) error {
			if len(debugShapes) < 20 {
				debugShapes = append(debugShapes, imageDebugShape(line))
			}
			if mr := extractUpstreamModelResponse(line); mr != nil {
				debugHTTP = append(debugHTTP, collectHTTPStrings(mr, 50)...)
				debugAsset = append(debugAsset, collectAssetLikeStrings(mr, 100)...)
			}
			urls = appendImageResultURLs(urls, line)
			debugHTTP = append(debugHTTP, collectHTTPStrings(line, 50)...)
			debugAsset = append(debugAsset, collectAssetLikeStrings(line, 100)...)
			return nil
		})
		resp.Body.Close()
		if err != nil {
			http.Error(w, "stream parse error: "+err.Error(), http.StatusBadGateway)
			return
		}
		urls = normalizeGeneratedImageURLs(urls, 0)
	}
	urls = normalizeGeneratedImageURLs(urls, req.N)
	if len(urls) == 0 {
		urls = appendImageCandidates(urls, uniqueStrings(debugHTTP), uniqueStrings(debugAsset), req.N)
	}
	if len(urls) == 0 {
		slog.Warn("grok image generation returned no images",
			"model", req.Model,
			"attempts", maxAttempts,
			"event_shapes", uniqueStrings(debugShapes),
			"http_candidates", len(uniqueStrings(debugHTTP)),
			"asset_candidates", len(uniqueStrings(debugAsset)),
		)
		http.Error(w, "no image generated", http.StatusBadGateway)
		return
	}

	field := imageResponseField(req.ResponseFormat)
	data := make([]map[string]interface{}, 0, len(urls))
	for _, u := range urls {
		val, err := h.imageOutputValue(ctx, sess.token, u, req.ResponseFormat)
		if err != nil {
			slog.Warn("grok image convert failed", "url", u, "error", err)
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
		"usage":   buildImageUsagePayload(req.Prompt, len(data)),
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(out)
}

func prepareAppChatImageGenerationPayload(payload map[string]interface{}, count int) {
	if payload == nil {
		return
	}
	if count < 1 {
		count = 1
	}
	payload["enableImageGeneration"] = true
	payload["enableImageStreaming"] = true
	payload["imageGenerationCount"] = count
	payload["responseMetadata"] = map[string]interface{}{}
	toolOverrides, _ := payload["toolOverrides"].(map[string]interface{})
	if toolOverrides == nil {
		toolOverrides = map[string]interface{}{}
		payload["toolOverrides"] = toolOverrides
	}
	toolOverrides["imageGen"] = false
	toolOverrides["webSearch"] = false
	toolOverrides["xSearch"] = false
	toolOverrides["xMediaSearch"] = false
	toolOverrides["trendsSearch"] = false
	toolOverrides["xPostAnalyze"] = false
}
