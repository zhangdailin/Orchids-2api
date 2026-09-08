package grok

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"strings"

	"orchids-api/internal/store"
)

var imageEditPlaceholderRE = regexp.MustCompile(`(?i)@IMAGE(\d+)\b`)

// The Web edit wire format follows grok2api's mediaGenInput.imageToImage
// contract. inputAssets are metadata IDs, not download URLs.
func (h *Handler) buildImageEditPayload(spec ModelSpec, prompt string, assets []string, aspectRatio string) map[string]interface{} {
	input := map[string]interface{}{"prompt": strings.TrimSpace(prompt), "inputAssets": assets}
	if aspectRatio != "" {
		input["aspectRatio"] = aspectRatio
	}
	payload := map[string]interface{}{
		"modelName": spec.UpstreamModel, "message": strings.TrimSpace(prompt),
		"enableImageStreaming": true, "enableSideBySide": true, "sendFinalMetadata": true,
		"mediaGenInput": map[string]interface{}{"imageToImage": input},
	}
	if h != nil && h.cfg != nil {
		payload["temporary"] = h.cfg.GrokChatTemporary()
		payload["disableMemory"] = h.cfg.GrokChatDisableMemory(false)
		if instruction := h.cfg.GrokChatCustomInstruction(); instruction != "" {
			payload["customPersonality"] = instruction
		}
	}
	return payload
}

func (h *Handler) buildImageEditRequestPayload(ctx context.Context, token string, spec ModelSpec, prompt string, inputs []imageEditUploadInput, ratio string) (map[string]interface{}, error) {
	values := make([]string, 0, len(inputs))
	for _, input := range inputs {
		values = append(values, dataURIFromBytes(input.mime, input.data))
	}
	return h.buildImageEditPayloadFromInputs(ctx, token, spec, prompt, values, ratio)
}

func (h *Handler) buildImageEditPayloadFromInputs(ctx context.Context, token string, spec ModelSpec, prompt string, inputs []string, ratio string) (map[string]interface{}, error) {
	refs := make([]imageEditReference, 0, len(inputs))
	assets := make([]string, 0, len(inputs))
	for _, input := range inputs {
		fileID, _, err := h.uploadSingleInput(ctx, token, input)
		if err != nil {
			return nil, fmt.Errorf("image upload failed: %w", err)
		}
		fileID = strings.TrimSpace(fileID)
		if fileID == "" {
			return nil, fmt.Errorf("image upload returned no fileMetadataId")
		}
		assets = append(assets, fileID)
		refs = append(refs, imageEditReference{fileID: fileID})
	}
	if len(assets) == 0 {
		return nil, fmt.Errorf("image_url is required for image edits")
	}
	return h.buildImageEditPayload(spec, replaceImageEditPlaceholders(prompt, refs), assets, ratio), nil
}

func replaceImageEditPlaceholders(prompt string, refs []imageEditReference) string {
	if len(refs) == 0 || !strings.Contains(strings.ToUpper(prompt), "@IMAGE") {
		return prompt
	}
	return imageEditPlaceholderRE.ReplaceAllStringFunc(prompt, func(match string) string {
		groups := imageEditPlaceholderRE.FindStringSubmatch(match)
		if len(groups) != 2 {
			return match
		}
		idx, err := strconv.Atoi(groups[1])
		if err != nil || idx < 1 || idx > len(refs) {
			return match
		}
		fileID := strings.TrimSpace(refs[idx-1].fileID)
		if fileID == "" {
			return match
		}
		return "@" + fileID
	})
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
	if len(imageURLs) > 7 {
		imageURLs = imageURLs[len(imageURLs)-7:]
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
	if n > 2 {
		http.Error(w, "image_config.n must be between 1 and 2 for image edit", http.StatusBadRequest)
		return
	}
	responseFormat := normalizeImageResponseFormat(imageCfg.ResponseFormat)
	if _, err := normalizeImageEditSize(imageCfg.Size); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	ratio, _ := normalizeImageAspectRatio("", imageCfg.Size)

	sess, err := h.openChatAccountSessionForModel(ctx, spec)
	if err != nil {
		http.Error(w, "no available grok token: "+err.Error(), http.StatusServiceUnavailable)
		return
	}
	defer sess.Close()

	rawPayload, err := h.buildImageEditPayloadFromInputs(ctx, sess.token, spec, prompt, imageURLs, ratio)
	if err != nil {
		if skipExternalAttachmentFetchGrokAccountStatus(err) {
			h.markAccountStatus(ctx, sess.acc, err)
		}
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	rebuildPayload := func(token string) (map[string]interface{}, error) {
		return h.buildImageEditPayloadFromInputs(ctx, token, spec, prompt, imageURLs, ratio)
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

	urls, ok := h.collectImageChatURLs(ctx, w, sess, &rawPayload, rebuildPayload, n)
	if !ok {
		return
	}

	h.writeImageResults(w, ctx, sess.token, prompt, urls, responseFormat, publicBase, false)
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
	if !requireMethod(w, r, http.MethodPost) {
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
	if !requireAPIKeyModel(w, r, model) {
		return
	}
	spec, ok := ResolveModel(model)
	consoleEdit := ok && spec.IsImage && spec.Upstream == UpstreamConsole
	if !isImageEditModel(model) && !consoleEdit {
		http.Error(w, "image edit model must be grok-imagine-image-edit or a Console image model", http.StatusBadRequest)
		return
	}
	if r.MultipartForm != nil && len(r.MultipartForm.File["mask"]) > 0 {
		http.Error(w, "mask is not supported yet", http.StatusBadRequest)
		return
	}
	n := parseIntLoose(r.FormValue("n"), 1)
	maxN := 2
	if consoleEdit {
		maxN = 10
	}
	if n < 1 || n > maxN {
		http.Error(w, fmt.Sprintf("n must be between 1 and %d for image edit", maxN), http.StatusBadRequest)
		return
	}
	if consoleEdit {
		if _, err := normalizeConsoleImageAspectRatio(r.FormValue("aspect_ratio"), r.FormValue("size")); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
	} else {
		if _, err := normalizeImageEditSize(r.FormValue("size")); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if _, err := normalizeImageAspectRatio(r.FormValue("aspect_ratio"), r.FormValue("size")); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
	}
	stream := parseBoolLoose(r.FormValue("stream"), false)
	partialImages := parseIntLoose(r.FormValue("partial_images"), 0)
	if consoleEdit && (stream || partialImages != 0) {
		http.Error(w, "Grok Console image edit does not support stream or partial_images", http.StatusBadRequest)
		return
	}
	if stream && n > 2 {
		http.Error(w, "streaming is only supported when n=1 or n=2", http.StatusBadRequest)
		return
	}
	rawResponseFormat := strings.ToLower(strings.TrimSpace(r.FormValue("response_format")))
	if rawResponseFormat != "" && rawResponseFormat != "url" && rawResponseFormat != "b64_json" && rawResponseFormat != "base64" {
		http.Error(w, "response_format must be url or b64_json", http.StatusBadRequest)
		return
	}
	responseFormat := normalizeImageResponseFormat(rawResponseFormat)
	publicBase := detectPublicBaseURL(r)

	if !ok || !spec.IsImage || (!isImageEditModel(spec.ID) && spec.Upstream != UpstreamConsole) {
		http.Error(w, "image edit model is not supported", http.StatusBadRequest)
		return
	}
	spec = h.applyPersistedRoute(r.Context(), spec)
	if err := h.ensureModelCapability(r.Context(), model, store.CapabilityImageEdit); err != nil {
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
	if consoleEdit && len(files) > 3 {
		http.Error(w, "Console image edit supports at most 3 images", http.StatusBadRequest)
		return
	}
	if !consoleEdit && len(files) > 7 {
		files = files[len(files)-7:]
	}

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
	if consoleEdit {
		h.serveConsoleImagesEdit(r.Context(), w, spec, prompt, uploads, n,
			r.FormValue("aspect_ratio"), r.FormValue("size"), r.FormValue("resolution"), r.FormValue("quality"),
			responseFormat, publicBase)
		return
	}

	sess, err := h.openChatAccountSessionForModel(r.Context(), spec)
	if err != nil {
		http.Error(w, "no available grok token: "+err.Error(), http.StatusServiceUnavailable)
		return
	}
	defer sess.Close()

	ratio, _ := normalizeImageAspectRatio(r.FormValue("aspect_ratio"), r.FormValue("size"))
	rawPayload, err := h.buildImageEditRequestPayload(r.Context(), sess.token, spec, prompt, uploads, ratio)
	if err != nil {
		if skipExternalAttachmentFetchGrokAccountStatus(err) {
			h.markAccountStatus(r.Context(), sess.acc, err)
		}
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	rebuildPayload := func(token string) (map[string]interface{}, error) {
		return h.buildImageEditRequestPayload(r.Context(), token, spec, prompt, uploads, ratio)
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

	urls, ok := h.collectImageChatURLs(r.Context(), w, sess, &rawPayload, rebuildPayload, n)
	if !ok {
		return
	}

	h.writeImageResults(w, r.Context(), sess.token, prompt, urls, responseFormat, publicBase, false)
}
