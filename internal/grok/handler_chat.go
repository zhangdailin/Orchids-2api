package grok

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"
)

func detectPublicBaseURL(r *http.Request) string {
	proto := strings.TrimSpace(r.Header.Get("X-Forwarded-Proto"))
	if proto == "" {
		if r.TLS != nil {
			proto = "https"
		} else {
			proto = "http"
		}
	}
	host := strings.TrimSpace(r.Header.Get("X-Forwarded-Host"))
	if host == "" {
		host = strings.TrimSpace(r.Host)
	}
	if host == "" {
		return ""
	}
	return proto + "://" + host
}

func (h *Handler) defaultChatStream() bool {
	if h == nil || h.cfg == nil {
		return true
	}
	return h.cfg.ChatDefaultStream()
}

func (h *Handler) applyDefaultChatStream(req *ChatCompletionsRequest) {
	if req == nil {
		return
	}
	if req.StreamProvided {
		return
	}
	req.Stream = h.defaultChatStream()
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
	req.Model = normalizeModelID(req.Model)
	if req.ImageConfig != nil {
		req.ImageConfig.Normalize()
	}
	h.applyDefaultChatStream(&req)
	if err := req.Validate(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if err := h.ensureModelEnabled(r.Context(), req.Model); err != nil {
		msg := strings.TrimSpace(err.Error())
		if strings.EqualFold(msg, "model not found") && h.tryAutoRegisterModel(r.Context(), req.Model) {
			if retryErr := h.ensureModelEnabled(r.Context(), req.Model); retryErr != nil {
				http.Error(w, modelValidationMessage(req.Model, retryErr), http.StatusBadRequest)
				return
			}
		} else {
			http.Error(w, modelValidationMessage(req.Model, err), http.StatusBadRequest)
			return
		}
	}

	spec, ok := ResolveModelOrDynamic(req.Model)
	if !ok {
		http.Error(w, modelNotFoundMessage(req.Model), http.StatusBadRequest)
		return
	}

	publicBase := detectPublicBaseURL(r)
	if spec.IsImage {
		prompt, imageURLs := extractPromptAndImageURLs(req.Messages)
		prompt = strings.TrimSpace(prompt)
		if prompt == "" {
			http.Error(w, "prompt is required", http.StatusBadRequest)
			return
		}
		imageCfg := req.ImageConfig
		if imageCfg == nil {
			imageCfg = &ImageConfig{}
			imageCfg.Normalize()
		}
		if imageCfg.N < 1 || imageCfg.N > 10 {
			http.Error(w, "image_config.n must be between 1 and 10", http.StatusBadRequest)
			return
		}
		if req.Stream && imageCfg.N > 2 {
			http.Error(w, "streaming is only supported when image_config.n=1 or n=2", http.StatusBadRequest)
			return
		}
		if imageCfg.ResponseFormat == "" {
			imageCfg.ResponseFormat = "url"
		}
		if imageCfg.ResponseFormat != "" {
			imageCfg.ResponseFormat = normalizeImageResponseFormat(imageCfg.ResponseFormat)
		}
		if imageCfg.Size != "" {
			size, err := normalizeImageSize(imageCfg.Size)
			if err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			imageCfg.Size = size
		} else {
			imageCfg.Size = "1024x1024"
		}

		if spec.ID == "grok-imagine-1.0-edit" {
			if len(imageURLs) == 0 {
				http.Error(w, "image_url is required for image edits", http.StatusBadRequest)
				return
			}
			// Keep grok2api compatibility in chat mode: use the last provided image as edit source.
			editInputs := imageURLs
			if len(editInputs) > 1 {
				editInputs = editInputs[len(editInputs)-1:]
			}
			h.handleChatImageEdit(r.Context(), w, req, spec, prompt, editInputs)
			return
		}

		genReq := ImagesGenerationsRequest{
			Model:          req.Model,
			Prompt:         prompt,
			N:              imageCfg.N,
			Size:           imageCfg.Size,
			Stream:         req.Stream,
			ResponseFormat: imageCfg.ResponseFormat,
		}
		h.serveImagesGenerations(r.Context(), w, genReq, publicBase)
		return
	}

	text, attachments, err := extractMessageAndAttachments(req.Messages, spec.IsVideo)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	userPrompt := strings.TrimSpace(extractLastUserText(req.Messages))
	if userPrompt == "" {
		userPrompt = strings.TrimSpace(text)
	}
	slog.Debug("grok chat prompt context",
		"model", req.Model,
		"history_chars", utf8.RuneCountInString(strings.TrimSpace(text)),
		"latest_user_chars", utf8.RuneCountInString(userPrompt),
		"attachments", len(attachments),
		"stream", req.Stream,
	)
	if strings.TrimSpace(text) == "" && len(attachments) == 0 {
		http.Error(w, "empty message", http.StatusBadRequest)
		return
	}

	if spec.IsVideo {
		if cfg, err := validateVideoConfig(req.VideoConfig); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		} else {
			req.VideoConfig = cfg
		}
	}

	sess, err := h.openChatAccountSession(r.Context())
	if err != nil {
		http.Error(w, "no available grok token: "+err.Error(), http.StatusServiceUnavailable)
		return
	}
	defer sess.Close()

	buildPayload := func(token string) (map[string]interface{}, error) {
		fileAttachments := []string(nil)
		if !spec.IsVideo {
			var upErr error
			fileAttachments, upErr = h.uploadAttachmentInputs(r.Context(), token, attachments)
			if upErr != nil {
				return nil, fmt.Errorf("attachment upload failed: %w", upErr)
			}
		}
		return h.buildChatPayload(r.Context(), token, spec, text, fileAttachments, attachments, req.VideoConfig, &req)
	}

	payload, err := buildPayload(sess.token)
	if err != nil {
		h.markAccountStatus(r.Context(), sess.acc, err)
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}

	resp, err := h.doChatWithAutoSwitchRebuild(r.Context(), sess, &payload, buildPayload)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()
	h.syncGrokQuota(sess.acc, resp.Header)

	hasAttachments := len(attachments) > 0
	if req.Stream {
		h.streamChat(w, req.Model, spec, sess.token, publicBase, hasAttachments, text, resp.Body)
		return
	}
	h.collectChat(w, req.Model, spec, sess.token, publicBase, hasAttachments, text, resp.Body)
}

func (h *Handler) buildChatPayload(
	ctx context.Context,
	token string,
	spec ModelSpec,
	text string,
	fileAttachments []string,
	attachmentInputs []AttachmentInput,
	videoCfg *VideoConfig,
	req *ChatCompletionsRequest,
) (map[string]interface{}, error) {
	payload := h.client.chatPayload(spec, text, true, 0)
	if len(fileAttachments) > 0 {
		payload["fileAttachments"] = fileAttachments
	}
	if req != nil {
		override := map[string]interface{}{}
		if req.Temperature != nil {
			override["temperature"] = *req.Temperature
		}
		if req.TopP != nil {
			override["topP"] = *req.TopP
		}
		if req.ReasoningEffort != nil {
			if v := strings.TrimSpace(*req.ReasoningEffort); v != "" {
				override["reasoningEffort"] = strings.ToLower(v)
			}
		}
		if len(override) > 0 {
			responseMetadata, _ := payload["responseMetadata"].(map[string]interface{})
			if responseMetadata == nil {
				responseMetadata = map[string]interface{}{}
				payload["responseMetadata"] = responseMetadata
			}
			modelConfigOverride, _ := responseMetadata["modelConfigOverride"].(map[string]interface{})
			if modelConfigOverride == nil {
				modelConfigOverride = map[string]interface{}{}
				responseMetadata["modelConfigOverride"] = modelConfigOverride
			}
			for k, v := range override {
				modelConfigOverride[k] = v
			}
		}
	}

	if !spec.IsVideo {
		return payload, nil
	}

	if videoCfg == nil {
		videoCfg = &VideoConfig{}
	}
	videoCfg.Normalize()

	postType := "MEDIA_POST_TYPE_VIDEO"
	postPrompt := text
	postMediaURL := ""
	for _, item := range attachmentInputs {
		if !strings.EqualFold(strings.TrimSpace(item.Type), "image") {
			continue
		}
		if strings.TrimSpace(item.Data) == "" {
			continue
		}
		_, fileURI, upErr := h.uploadSingleInput(ctx, token, item.Data)
		if upErr != nil {
			return nil, fmt.Errorf("video image upload failed: %w", upErr)
		}
		u := strings.TrimSpace(fileURI)
		if u == "" {
			return nil, fmt.Errorf("video image upload failed: empty file uri")
		}
		if !strings.HasPrefix(strings.ToLower(u), "http://") && !strings.HasPrefix(strings.ToLower(u), "https://") {
			u = "https://assets.grok.com/" + strings.TrimLeft(u, "/")
		}
		postType = "MEDIA_POST_TYPE_IMAGE"
		postPrompt = ""
		postMediaURL = u
		break
	}

	postID, err := h.client.createMediaPost(ctx, token, postType, postPrompt, postMediaURL)
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

type streamMarkupFilter struct {
	pending  string
	inTool   bool
	inRender bool
}

// Streaming sanitizer/tokenizer.
// Goal: never leak tool/render markup, never corrupt UTF-8, and keep streaming responsive.
func (f *streamMarkupFilter) feed(chunk string) string {
	if f == nil || chunk == "" {
		return ""
	}
	f.pending += chunk
	// Bound memory
	if len(f.pending) > 64*1024 {
		f.pending = f.pending[len(f.pending)-64*1024:]
	}

	const toolStart = "xai:tool_usage_card"
	const toolEnd = "</xai:tool_usage_card>"
	const renderStart = "<grok:render"
	const renderEnd = "</grok:render>"

	var out strings.Builder

	for {
		lower := strings.ToLower(f.pending)

		if f.inTool {
			end := strings.Index(lower, toolEnd)
			if end < 0 {
				// wait for more data
				break
			}
			raw := f.pending[:end+len(toolEnd)]
			if line := extractToolUsageCardText(raw); line != "" {
				if out.Len() > 0 && !strings.HasSuffix(out.String(), "\n") {
					out.WriteString("\n")
				}
				out.WriteString(line)
				out.WriteString("\n")
			}
			f.pending = f.pending[end+len(toolEnd):]
			f.inTool = false
			continue
		}
		if f.inRender {
			end := strings.Index(lower, renderEnd)
			if end < 0 {
				break
			}
			f.pending = f.pending[end+len(renderEnd):]
			f.inRender = false
			continue
		}

		idxTool := strings.Index(lower, toolStart)
		idxRender := strings.Index(lower, renderStart)
		idx := -1
		kind := ""
		if idxTool >= 0 {
			idx = idxTool
			kind = "tool"
		}
		if idxRender >= 0 && (idx < 0 || idxRender < idx) {
			idx = idxRender
			kind = "render"
		}

		if idx < 0 {
			// No markers. Emit everything except a tail to avoid cutting potential markers.
			keep := 512
			if len(f.pending) <= keep {
				break
			}
			safe := validUTF8Prefix(f.pending[:len(f.pending)-keep])
			safe = stripLeadingAngleNoise(sanitizeText(safe))
			if safe != "" {
				out.WriteString(safe)
			}
			f.pending = f.pending[len(f.pending)-keep:]
			break
		}

		// Emit prefix before marker
		prefix := validUTF8Prefix(f.pending[:idx])
		prefix = stripLeadingAngleNoise(sanitizeText(prefix))
		if prefix != "" {
			out.WriteString(prefix)
		}
		f.pending = f.pending[idx:]
		if kind == "tool" {
			f.inTool = true
		} else {
			f.inRender = true
		}
	}

	return out.String()
}

func (f *streamMarkupFilter) flush() string {
	if f == nil {
		return ""
	}
	if f.inTool || f.inRender {
		return ""
	}
	if strings.TrimSpace(f.pending) == "" {
		return ""
	}
	out := stripLeadingAngleNoise(sanitizeText(stripToolAndRenderMarkup(validUTF8Prefix(f.pending))))
	f.pending = ""
	return out
}

func validUTF8Prefix(s string) string {
	if s == "" || utf8.ValidString(s) {
		return s
	}
	// Trim bytes until valid UTF-8.
	b := []byte(s)
	for len(b) > 0 {
		b = b[:len(b)-1]
		if utf8.Valid(b) {
			return string(b)
		}
	}
	return ""
}

// NOTE: streamMarkupFilter.feed is implemented earlier in this file.

func (h *Handler) streamChat(w http.ResponseWriter, model string, spec ModelSpec, token string, publicBase string, hasAttachments bool, userPrompt string, body io.Reader) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	flusher, _ := w.(http.Flusher)

	id := "chatcmpl_" + randomHex(8)
	sentRole := false
	lastMessage := ""
	sentAny := false
	var rawAll strings.Builder
	// Image URL stream handling: prefer full image variants over -part-0 previews.
	seenFull := map[string]bool{}
	pendingPart := map[string]string{}
	emitted := map[string]bool{}

	var mf *streamMarkupFilter
	if !hasAttachments {
		mf = &streamMarkupFilter{}
	}

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

	emitImageURL := func(raw string) {
		raw = strings.TrimSpace(raw)
		if raw == "" {
			return
		}
		// Track preview vs full variants on the raw upstream URL.
		if strings.Contains(raw, "-part-0/") {
			full := strings.ReplaceAll(raw, "-part-0/", "/")
			if seenFull[full] {
				return
			}
			pendingPart[full] = raw
			return
		}
		seenFull[raw] = true
		if emitted[raw] {
			return
		}

		val, errV := h.imageOutputValue(context.Background(), token, raw, "url")
		if errV != nil || strings.TrimSpace(val) == "" {
			val = raw
		}
		if publicBase != "" && strings.HasPrefix(val, "/") {
			val = publicBase + val
		}
		if md := formatImageMarkdown(val); md != "" {
			emitChunk(map[string]interface{}{"content": md}, nil)
			emitted[raw] = true
		}
	}

	err := parseUpstreamLines(body, func(resp map[string]interface{}) error {
		if !sentRole {
			emitChunk(map[string]interface{}{"role": "assistant"}, nil)
			sentRole = true
		}
		if tokenDelta, ok := resp["token"].(string); ok && tokenDelta != "" {
			rawAll.WriteString(tokenDelta)
			if mf == nil {
				// Vision Q/A path: keep text intact but strip full tool/render blocks when present.
				cleaned := stripToolAndRenderMarkup(tokenDelta)
				cleaned = stripLeadingAngleNoise(sanitizeText(cleaned))
				if cleaned != "" {
					emitChunk(map[string]interface{}{"content": cleaned}, nil)
				}
			}
			// Text streaming path (mf != nil): do NOT emit token deltas.
			// Grok token stream can include partial UTF-8 boundaries and markup noise; rely on modelResponse.message instead.
		}
		if mr, ok := resp["modelResponse"].(map[string]interface{}); ok {
			if msg, ok := mr["message"].(string); ok && strings.TrimSpace(msg) != "" && msg != lastMessage {
				lastMessage = msg
				rawAll.WriteString(msg)
				if mf == nil {
					cleaned := stripToolAndRenderMarkup(msg)
					cleaned = stripLeadingAngleNoise(sanitizeText(cleaned))
					if cleaned != "" {
						emitChunk(map[string]interface{}{"content": cleaned}, nil)
					}
				} else {
					// Text streaming path: feed full messages into the filter (handles tool/render blocks) and emit the cleaned text.
					if cleaned := mf.feed(msg); cleaned != "" {
						cleaned = stripLeadingAngleNoise(cleaned)
						if cleaned != "" {
							emitChunk(map[string]interface{}{"content": cleaned}, nil)
						}
					}
				}
				if strings.Contains(msg, "<grok:render") || strings.Contains(msg, "tool_usage_card") {
					slog.Debug("grok message contains render/tool markup", "has_modelResponse", true)
				}
			}
			for _, u := range extractImageURLs(mr) {
				emitImageURL(u)
			}
			// Fallback: tool/card payloads may include image URLs outside of the known keys.
			for _, u := range extractRenderableImageLinks(mr) {
				emitImageURL(u)
			}
			// Extra fallback: scan for asset-like strings inside cards / embedded JSON.
			for _, p := range collectAssetLikeStrings(mr, 80) {
				p = strings.TrimSpace(p)
				if p == "" {
					continue
				}
				if isLikelyImageURL(p) {
					emitImageURL(p)
					continue
				}
				if isLikelyImageAssetPath(p) {
					emitImageURL("https://assets.grok.com/" + strings.TrimPrefix(p, "/"))
					continue
				}
			}
		}
		// Broader fallback: sometimes URLs live outside modelResponse.
		for _, u := range extractImageURLs(resp) {
			emitImageURL(u)
		}
		for _, u := range extractRenderableImageLinks(resp) {
			emitImageURL(u)
		}
		for _, p := range collectAssetLikeStrings(resp, 120) {
			p = strings.TrimSpace(p)
			if p == "" {
				continue
			}
			if isLikelyImageURL(p) {
				emitImageURL(p)
				continue
			}
			if isLikelyImageAssetPath(p) {
				emitImageURL("https://assets.grok.com/" + strings.TrimPrefix(p, "/"))
				continue
			}
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

	// Flush any remaining buffered text (avoids "no content" when stream ends quickly).
	if mf != nil {
		if tail := mf.flush(); tail != "" {
			tail = stripLeadingAngleNoise(tail)
			if tail != "" {
				emitChunk(map[string]interface{}{"content": tail}, nil)
			}
		}
	}
	// Emit any pending part-0 previews only if we never saw a full variant.
	// Try to fetch/emit the full variant first; if it doesn't exist, fall back to the preview.
	for full, part := range pendingPart {
		if seenFull[full] {
			continue
		}
		// Try full (cache through this server for client reachability).
		if name, err := h.cacheMediaURL(context.Background(), token, full, "image"); err == nil && name != "" {
			val := "/grok/v1/files/image/" + name
			if publicBase != "" && strings.HasPrefix(val, "/") {
				val = publicBase + val
			}
			if md := formatImageMarkdown(val); md != "" {
				emitChunk(map[string]interface{}{"content": md}, nil)
			}
			continue
		}
		// Fall back to preview.
		emitImageURL(part)
	}

	// Final pass: scan accumulated raw text for any image URLs / asset paths that were only present
	// inside tool/render markup and might not have been captured by structured parsers.
	for _, u := range extractImageURLsFromText(rawAll.String()) {
		emitImageURL(u)
	}
	for _, p := range extractGrokAssetPathsFromText(rawAll.String()) {
		emitImageURL("https://assets.grok.com/" + strings.TrimPrefix(p, "/"))
	}

	emitChunk(map[string]interface{}{}, "stop")
	writeSSE(w, "", "[DONE]")
	if flusher != nil {
		flusher.Flush()
	}
}

func (h *Handler) collectChat(w http.ResponseWriter, model string, spec ModelSpec, token string, publicBase string, hasAttachments bool, userPrompt string, body io.Reader) {
	id := "chatcmpl_" + randomHex(8)
	var content strings.Builder
	lastMessage := ""
	sawToken := false
	videoURL := ""
	var imageCandidates []string

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
			imageCandidates = append(imageCandidates, extractImageURLs(mr)...)
			imageCandidates = append(imageCandidates, extractRenderableImageLinks(mr)...)
		}
		imageCandidates = append(imageCandidates, extractRenderableImageLinks(resp)...)
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

	finalContent := stripToolAndRenderMarkup(content.String())
	finalContent = stripLeadingAngleNoise(sanitizeText(finalContent))

	// Append any collected image links as Markdown, after text cleanup.
	imgs := normalizeImageURLs(imageCandidates, 8)
	for _, u := range imgs {
		val, errV := h.imageOutputValue(context.Background(), token, u, "url")
		if errV != nil || strings.TrimSpace(val) == "" {
			val = u
		}
		if publicBase != "" && strings.HasPrefix(val, "/") {
			val = publicBase + val
		}
		finalContent += formatImageMarkdown(val)
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
					"content": finalContent,
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
