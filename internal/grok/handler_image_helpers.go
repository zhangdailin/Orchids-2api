package grok

import (
	"bytes"
	"context"
	"crypto/sha1"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"image"
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"
	"log/slog"
	"net/url"
	"os"
	"path/filepath"
	"strings"
)

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

func imageDimsFromBytes(data []byte) (int, int) {
	if len(data) == 0 {
		return 0, 0
	}
	cfg, _, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		return 0, 0
	}
	return cfg.Width, cfg.Height
}

func isLikelyRasterImageBytes(data []byte) bool {
	if len(data) < 4 {
		return false
	}
	if len(data) >= 3 && data[0] == 0xff && data[1] == 0xd8 && data[2] == 0xff {
		return true
	}
	if len(data) >= 8 &&
		data[0] == 0x89 && data[1] == 'P' && data[2] == 'N' && data[3] == 'G' &&
		data[4] == '\r' && data[5] == '\n' && data[6] == 0x1a && data[7] == '\n' {
		return true
	}
	if len(data) >= 6 && (string(data[:6]) == "GIF87a" || string(data[:6]) == "GIF89a") {
		return true
	}
	if len(data) >= 12 && string(data[:4]) == "RIFF" && string(data[8:12]) == "WEBP" {
		return true
	}
	return false
}

func (h *Handler) cacheMediaURL(ctx context.Context, token, rawURL, mediaType string) (string, error) {
	mediaType = strings.ToLower(strings.TrimSpace(mediaType))
	if mediaType != "video" {
		mediaType = "image"
	}
	trimURL := strings.TrimSpace(rawURL)
	lurl := strings.ToLower(trimURL)
	// Never cache known low-res thumbnail hosts; they lead to blurry results.
	if mediaType == "image" && strings.Contains(lurl, "encrypted-tbn0.gstatic.com") {
		return "", fmt.Errorf("skip thumbnail url")
	}
	// If the client can't reach Grok/X assets (common in some regions), caching through this server
	// is required for images to display at all.
	forceCache := mediaType == "image" && mustCacheImageURL(lurl)

	data, mimeType, err := h.client.downloadAsset(ctx, token, rawURL)
	if err != nil {
		return "", err
	}
	// Heuristic: avoid caching tiny/low-res images (often thumbnails/previews).
	if mediaType == "image" {
		if !isRasterImageMime(mimeType) {
			return "", fmt.Errorf("unsupported image mime type: %s", strings.TrimSpace(mimeType))
		}
		w, hgt := imageDimsFromBytes(data)
		if w <= 0 || hgt <= 0 {
			if !forceCache || !isLikelyRasterImageBytes(data) {
				return "", fmt.Errorf("unsupported image data")
			}
		}
		// For Grok/X image assets, caching is required for display (clients may not reach the CDN).
		if forceCache {
			// Always cache (even previews). We already avoid emitting -part-0 when full exists.
		} else {
			if (w > 0 && hgt > 0 && (w < 900 || hgt < 900)) || len(data) < 60*1024 {
				slog.Debug("skip caching low-res image", "url", trimURL, "bytes", len(data), "w", w, "h", hgt)
				return "", fmt.Errorf("skip low-res image")
			}
		}
	}
	return h.cacheMediaBytes(rawURL, mediaType, data, mimeType)
}

func isRasterImageMime(mimeType string) bool {
	switch strings.ToLower(strings.TrimSpace(strings.Split(mimeType, ";")[0])) {
	case "image/jpeg", "image/png", "image/webp", "image/gif":
		return true
	default:
		return false
	}
}

func mustCacheImageURL(rawURL string) bool {
	raw := strings.TrimSpace(rawURL)
	if raw == "" {
		return false
	}
	lower := strings.ToLower(raw)
	if strings.HasPrefix(lower, "/grok/v1/files/image/") || strings.HasPrefix(lower, "/v1/files/image/") {
		return false
	}
	if isLikelyImageAssetPath(raw) {
		return true
	}
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Hostname() == "" {
		return false
	}
	host := strings.ToLower(parsed.Hostname())
	if host == "assets.grok.com" || host == "grok.com" || strings.HasSuffix(host, ".grok.com") ||
		host == "x.ai" || strings.HasSuffix(host, ".x.ai") {
		return true
	}
	return false
}

func (h *Handler) imageOutputValue(ctx context.Context, token, url, format string) (string, error) {
	if normalizeImageResponseFormat(format) == "url" {
		trim := strings.TrimSpace(url)
		var cacheErr error
		// Stable contract: prefer full over -part-0. If we only got a preview URL,
		// try the full variant first.
		if strings.Contains(trim, "-part-0/") {
			full := strings.ReplaceAll(trim, "-part-0/", "/")
			if name, err := h.cacheMediaURL(ctx, token, full, "image"); err == nil && name != "" {
				return "/grok/v1/files/image/" + name, nil
			} else if err != nil {
				cacheErr = err
			}
		}
		if name, err := h.cacheMediaURL(ctx, token, trim, "image"); err == nil && name != "" {
			return "/grok/v1/files/image/" + name, nil
		} else if err != nil {
			cacheErr = err
		}
		if mustCacheImageURL(trim) {
			if cacheErr != nil {
				return "", fmt.Errorf("cache grok image locally: %w", cacheErr)
			}
			return "", fmt.Errorf("cache grok image locally failed")
		}
		return trim, nil
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
