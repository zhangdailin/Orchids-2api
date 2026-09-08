package grok

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/goccy/go-json"
)

const (
	maxConsoleImageResponseBytes = 128 << 20
	maxConsoleImageAssetBytes    = 32 << 20
)

func (h *Handler) serveConsoleImagesGeneration(ctx context.Context, w http.ResponseWriter, spec ModelSpec, req ImagesGenerationsRequest, publicBase string) {
	if req.Stream || imagePartialCount(req) != 0 {
		http.Error(w, "Grok Console image generation does not support stream or partial_images", http.StatusBadRequest)
		return
	}
	ratio, err := normalizeConsoleImageAspectRatio(req.AspectRatio, req.Size)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	resolution, quality, err := normalizeConsoleImageOptions(spec, req.Resolution, req.Quality)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	payload := map[string]interface{}{
		"model":           spec.ConsoleModel,
		"prompt":          req.Prompt,
		"n":               req.N,
		"response_format": req.ResponseFormat,
	}
	if ratio != "" {
		payload["aspect_ratio"] = ratio
	}
	if resolution != "" {
		payload["resolution"] = resolution
	}
	if quality != "" {
		payload["quality"] = quality
	}
	h.forwardConsoleImageRequest(ctx, w, spec.ID, "images/generations", payload, req.ResponseFormat, publicBase)
}

func (h *Handler) serveConsoleImagesEdit(ctx context.Context, w http.ResponseWriter, spec ModelSpec, prompt string, uploads []imageEditUploadInput, n int, aspectRatio, size, resolution, quality, responseFormat, publicBase string) {
	if len(uploads) < 1 || len(uploads) > 3 {
		http.Error(w, "Console image edit requires between 1 and 3 images", http.StatusBadRequest)
		return
	}
	ratio, err := normalizeConsoleImageAspectRatio(aspectRatio, size)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	resolution, quality, err = normalizeConsoleImageOptions(spec, resolution, quality)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	images := make([]map[string]interface{}, 0, len(uploads))
	for _, upload := range uploads {
		images = append(images, map[string]interface{}{"type": "image_url", "url": dataURIFromBytes(upload.mime, upload.data)})
	}
	payload := map[string]interface{}{
		"model": spec.ConsoleModel, "prompt": prompt, "n": n, "response_format": responseFormat,
	}
	if len(images) == 1 {
		payload["image"] = images[0]
	} else {
		payload["images"] = images
	}
	if ratio != "" {
		payload["aspect_ratio"] = ratio
	}
	if resolution != "" {
		payload["resolution"] = resolution
	}
	if quality != "" {
		payload["quality"] = quality
	}
	h.forwardConsoleImageRequest(ctx, w, spec.ID, "images/edits", payload, responseFormat, publicBase)
}

func normalizeConsoleImageOptions(spec ModelSpec, resolution, quality string) (string, string, error) {
	resolution = strings.ToLower(strings.TrimSpace(resolution))
	if resolution != "" && resolution != "1k" && resolution != "2k" {
		return "", "", fmt.Errorf("resolution must be 1k or 2k")
	}
	quality = strings.ToLower(strings.TrimSpace(quality))
	if quality != "" && quality != "low" && quality != "medium" {
		return "", "", fmt.Errorf("quality must be low or medium")
	}
	if quality != "" && !strings.EqualFold(spec.ConsoleModel, "grok-imagine-image-2.0") {
		return "", "", fmt.Errorf("quality is only supported by console/grok-imagine-image-2.0")
	}
	return resolution, quality, nil
}

func normalizeConsoleImageAspectRatio(aspectRatio, size string) (string, error) {
	value := strings.ToLower(strings.TrimSpace(aspectRatio))
	if value == "" {
		value = strings.ToLower(strings.TrimSpace(size))
	}
	if value == "" || value == "auto" {
		return "", nil
	}
	values := map[string]string{
		"1:1": "1:1", "16:9": "16:9", "9:16": "9:16", "4:3": "4:3", "3:4": "3:4",
		"3:2": "3:2", "2:3": "2:3", "2:1": "2:1", "1:2": "1:2",
		"1024x1024": "1:1", "1280x720": "16:9", "720x1280": "9:16",
		"1792x1024": "3:2", "1536x1024": "3:2", "1024x1792": "2:3", "1024x1536": "2:3",
	}
	if resolved := values[value]; resolved != "" {
		return resolved, nil
	}
	return "", fmt.Errorf("aspect_ratio or size is not supported by Grok Console")
}

func (h *Handler) forwardConsoleImageRequest(ctx context.Context, w http.ResponseWriter, modelID, path string, payload map[string]interface{}, responseFormat, publicBase string) {
	body, err := json.Marshal(payload)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	client := h.currentClient()
	if client == nil {
		http.Error(w, "Grok Console client is not configured", http.StatusServiceUnavailable)
		return
	}
	sess, err := h.openConsoleAccountSession(ctx, nil, modelID)
	if err != nil {
		http.Error(w, "no available Grok Console account: "+err.Error(), http.StatusServiceUnavailable)
		return
	}
	defer sess.Close()
	response, err := client.doConsoleDPoPRequestWithHeaders(withRateLimitAccount(ctx, sess.acc), sess.token, http.MethodPost, h.consoleURL(path), body, http.Header{
		"Content-Type": []string{"application/json"},
		"Accept":       []string{"application/json"},
	})
	if err != nil {
		if markAllGrokAccountStatuses(err) {
			h.markAccountStatus(ctx, sess.acc, err)
		}
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	defer response.Body.Close()
	data, err := io.ReadAll(io.LimitReader(response.Body, maxConsoleImageResponseBytes+1))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	if len(data) > maxConsoleImageResponseBytes {
		http.Error(w, "Console image response exceeds 128 MiB", http.StatusBadGateway)
		return
	}
	if normalizeImageResponseFormat(responseFormat) == "url" {
		data, err = h.localizeConsoleImageResponse(ctx, sess.token, data, publicBase)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(data)
}

func (h *Handler) localizeConsoleImageResponse(ctx context.Context, token string, data []byte, publicBase string) ([]byte, error) {
	var envelope map[string]interface{}
	if err := json.Unmarshal(data, &envelope); err != nil {
		return nil, fmt.Errorf("parse Console image response: %w", err)
	}
	items, ok := envelope["data"].([]interface{})
	if !ok || len(items) == 0 || len(items) > 10 {
		return nil, fmt.Errorf("Console image response is missing valid data")
	}
	for index, rawItem := range items {
		item, ok := rawItem.(map[string]interface{})
		if !ok {
			return nil, fmt.Errorf("Console image response item %d is invalid", index+1)
		}
		rawURL := strings.TrimSpace(fmt.Sprint(item["url"]))
		if rawURL == "" || rawURL == "<nil>" {
			return nil, fmt.Errorf("Console image response item %d has no URL", index+1)
		}
		raw, mimeType, err := h.downloadTrustedConsoleImage(ctx, token, rawURL)
		if err != nil {
			return nil, err
		}
		name, err := h.cacheMediaBytes(rawURL, "image", raw, mimeType)
		if err != nil {
			return nil, fmt.Errorf("cache Console image: %w", err)
		}
		localized := "/grok/v1/files/image/" + name
		if publicBase != "" {
			localized = publicBase + localized
		}
		item["url"] = localized
		item["mime_type"] = mimeType
	}
	return json.Marshal(envelope)
}

func (h *Handler) downloadTrustedConsoleImage(ctx context.Context, token, rawURL string) ([]byte, string, error) {
	client := h.currentClient()
	if client == nil || client.clientForAsset(true) == nil {
		return nil, "", fmt.Errorf("Console image asset client is not configured")
	}
	if !trustedConsoleImageURL(rawURL, h.consoleURL("")) {
		return nil, "", fmt.Errorf("Console image content URL is not trusted")
	}
	baseClient := client.clientForAsset(true)
	httpClient := *baseClient
	previousRedirect := baseClient.CheckRedirect
	httpClient.CheckRedirect = func(request *http.Request, via []*http.Request) error {
		if !trustedConsoleImageURL(request.URL.String(), h.consoleURL("")) {
			return fmt.Errorf("Console image redirected to an untrusted URL")
		}
		if previousRedirect != nil {
			return previousRedirect(request, via)
		}
		if len(via) >= 10 {
			return fmt.Errorf("too many Console image redirects")
		}
		return nil
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, "", err
	}
	request.Header = client.assetDownloadHeaders(token, rawURL)
	request.Header.Set("Accept", "image/*,*/*;q=0.8")
	request.Header.Set("Referer", "https://console.x.ai/")
	response, err := httpClient.Do(request)
	if err != nil {
		return nil, "", err
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return nil, "", fmt.Errorf("Console image download returned status %d", response.StatusCode)
	}
	raw, err := io.ReadAll(io.LimitReader(response.Body, maxConsoleImageAssetBytes+1))
	if err != nil {
		return nil, "", err
	}
	if len(raw) == 0 || len(raw) > maxConsoleImageAssetBytes {
		return nil, "", fmt.Errorf("Console image is empty or exceeds 32 MiB")
	}
	mimeType := strings.ToLower(strings.TrimSpace(strings.Split(response.Header.Get("Content-Type"), ";")[0]))
	if mimeType == "" || mimeType == "application/octet-stream" {
		mimeType = http.DetectContentType(raw)
	}
	if !strings.HasPrefix(mimeType, "image/") {
		return nil, "", fmt.Errorf("Console image has invalid Content-Type %q", mimeType)
	}
	return raw, mimeType, nil
}

func trustedConsoleImageURL(rawURL, consoleBase string) bool {
	parsed, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil || parsed.User != nil || parsed.Hostname() == "" {
		return false
	}
	host := strings.ToLower(parsed.Hostname())
	if parsed.Scheme == "https" && (host == "assets.grok.com" || host == "imagine-public.x.ai" || host == "imgen.x.ai") {
		return true
	}
	base, err := url.Parse(strings.TrimSpace(consoleBase))
	return err == nil && base.User == nil && (parsed.Scheme == "http" || parsed.Scheme == "https") &&
		strings.EqualFold(parsed.Scheme, base.Scheme) && strings.EqualFold(parsed.Host, base.Host)
}
