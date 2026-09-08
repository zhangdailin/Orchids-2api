package grok

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"orchids-api/internal/store"
)

const (
	mediaInputTTL             = 24 * time.Hour
	maxMediaInputBytes        = 20 << 20
	maxMediaInputRequestBytes = maxMediaInputBytes + (1 << 20)
	maxResolvedMediaBytes     = 32 << 20
	localMediaInputPrefix     = "orchids-media-input:"
)

func (h *Handler) HandleMediaInputs(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodPost) {
		return
	}
	if h == nil || h.lb == nil || h.lb.Store == nil {
		writeResponsesAPIError(w, http.StatusServiceUnavailable, "service_unavailable", "media input store is not configured")
		return
	}
	if !strings.HasPrefix(strings.ToLower(strings.TrimSpace(r.Header.Get("Content-Type"))), "multipart/form-data") {
		writeResponsesAPIError(w, http.StatusUnsupportedMediaType, "invalid_request", "media input upload requires multipart/form-data")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxMediaInputRequestBytes)
	if err := r.ParseMultipartForm(maxMediaInputRequestBytes); err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			writeResponsesAPIError(w, http.StatusRequestEntityTooLarge, "media_too_large", "media input exceeds 20 MiB")
			return
		}
		writeResponsesAPIError(w, http.StatusBadRequest, "invalid_request", "invalid media input upload")
		return
	}
	files := r.MultipartForm.File["file"]
	if len(files) != 1 {
		writeResponsesAPIError(w, http.StatusBadRequest, "invalid_request", "exactly one file is required")
		return
	}
	fileHeader := files[0]
	if fileHeader.Size <= 0 {
		writeResponsesAPIError(w, http.StatusBadRequest, "invalid_media", "media input cannot be empty")
		return
	}
	if fileHeader.Size > maxMediaInputBytes {
		writeResponsesAPIError(w, http.StatusRequestEntityTooLarge, "media_too_large", "media input exceeds 20 MiB")
		return
	}
	file, err := fileHeader.Open()
	if err != nil {
		writeResponsesAPIError(w, http.StatusBadRequest, "invalid_media", "failed to read media input")
		return
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, maxMediaInputBytes+1))
	if err != nil {
		writeResponsesAPIError(w, http.StatusBadRequest, "invalid_media", "failed to read media input")
		return
	}
	if len(data) > maxMediaInputBytes {
		writeResponsesAPIError(w, http.StatusRequestEntityTooLarge, "media_too_large", "media input exceeds 20 MiB")
		return
	}
	kind, mimeType, err := detectMediaInput(data, fileHeader.Header.Get("Content-Type"))
	if err != nil {
		writeResponsesAPIError(w, http.StatusBadRequest, "invalid_media", err.Error())
		return
	}
	id, err := newMediaInputID()
	if err != nil {
		writeResponsesAPIError(w, http.StatusInternalServerError, "internal_error", "failed to allocate media input")
		return
	}
	name, err := h.cacheMediaBytes("input:"+id, kind, data, mimeType)
	if err != nil {
		writeResponsesAPIError(w, http.StatusInternalServerError, "internal_error", "failed to cache media input")
		return
	}
	contentPath := filepath.Join(cacheBaseDir, kind, name)
	now := time.Now().UTC()
	input := &store.StoredMediaInput{
		ID: id, OwnerHash: videoRequestOwner(r), Kind: kind, MIMEType: mimeType,
		ContentPath: contentPath, SizeBytes: int64(len(data)), CreatedAt: now,
	}
	if err := h.lb.Store.SaveStoredMediaInput(r.Context(), input, mediaInputTTL); err != nil {
		_ = os.Remove(contentPath)
		writeResponsesAPIError(w, http.StatusServiceUnavailable, "service_unavailable", "failed to persist media input")
		return
	}
	writeJSONStatus(w, http.StatusCreated, map[string]interface{}{
		"file_id": id, "object": "file", "kind": kind, "mime_type": mimeType,
		"bytes": len(data), "created_at": now.Unix(), "expires_at": now.Add(mediaInputTTL).Format(time.RFC3339),
	})
}

func (h *Handler) HandleMediaInputResource(w http.ResponseWriter, r *http.Request) {
	if h == nil || h.lb == nil || h.lb.Store == nil {
		writeResponsesAPIError(w, http.StatusServiceUnavailable, "service_unavailable", "media input store is not configured")
		return
	}
	id := mediaInputIDFromPath(r.URL.Path)
	if !validMediaInputID(id) {
		http.Error(w, "media input not found", http.StatusNotFound)
		return
	}
	owner := videoRequestOwner(r)
	input, err := h.lb.Store.GetStoredMediaInput(r.Context(), id, owner)
	if err != nil {
		http.Error(w, "media input not found", http.StatusNotFound)
		return
	}
	switch r.Method {
	case http.MethodGet:
		writeJSON(w, map[string]interface{}{
			"file_id": input.ID, "object": "file", "kind": input.Kind, "mime_type": input.MIMEType,
			"bytes": input.SizeBytes, "created_at": input.CreatedAt.Unix(), "expires_at": input.ExpiresAt.Format(time.RFC3339),
		})
	case http.MethodDelete:
		if err := h.lb.Store.DeleteStoredMediaInput(r.Context(), id, owner); err != nil {
			http.Error(w, "media input not found", http.StatusNotFound)
			return
		}
		if validCachedMediaContentPath(input.ContentPath, input.Kind) {
			_ = os.Remove(input.ContentPath)
		}
		writeJSON(w, map[string]interface{}{"id": id, "object": "file", "deleted": true})
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func newMediaInputID() (string, error) {
	raw := make([]byte, 24)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	return "input_" + base64.RawURLEncoding.EncodeToString(raw), nil
}

func validMediaInputID(id string) bool {
	encoded, ok := strings.CutPrefix(strings.TrimSpace(id), "input_")
	if !ok {
		return false
	}
	raw, err := base64.RawURLEncoding.DecodeString(encoded)
	return err == nil && len(raw) == 24
}

func mediaInputIDFromPath(path string) string {
	for _, prefix := range []string{"/grok/v1/media/inputs/", "/v1/media/inputs/"} {
		if strings.HasPrefix(path, prefix) {
			return strings.Trim(strings.TrimPrefix(path, prefix), "/")
		}
	}
	return ""
}

func detectMediaInput(data []byte, declared string) (string, string, error) {
	if len(data) == 0 {
		return "", "", fmt.Errorf("media input cannot be empty")
	}
	detected := strings.ToLower(strings.TrimSpace(strings.Split(http.DetectContentType(data), ";")[0]))
	switch detected {
	case "image/jpeg", "image/png", "image/gif", "image/webp":
		return "image", detected, nil
	}
	if len(data) >= 12 && string(data[4:8]) == "ftyp" {
		declared = strings.ToLower(strings.TrimSpace(strings.Split(declared, ";")[0]))
		if declared == "video/quicktime" {
			return "video", declared, nil
		}
		return "video", "video/mp4", nil
	}
	if len(data) >= 4 && bytes.Equal(data[:4], []byte{0x1a, 0x45, 0xdf, 0xa3}) {
		return "video", "video/webm", nil
	}
	return "", "", fmt.Errorf("only valid jpeg, png, webp, gif, mp4, webm, or quicktime media is supported")
}

func validCachedMediaContentPath(path, kind string) bool {
	path = strings.TrimSpace(path)
	kind = strings.ToLower(strings.TrimSpace(kind))
	if path == "" || (kind != "image" && kind != "video") {
		return false
	}
	base, err := filepath.Abs(filepath.Join(cacheBaseDir, kind))
	if err != nil {
		return false
	}
	target, err := filepath.Abs(path)
	if err != nil {
		return false
	}
	relative, err := filepath.Rel(base, target)
	return err == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(os.PathSeparator))
}

func (h *Handler) resolveMediaInputDataURL(ctx context.Context, id, owner, expectedKind string) (string, int64, error) {
	if h == nil || h.lb == nil || h.lb.Store == nil {
		return "", 0, fmt.Errorf("media input store is not configured")
	}
	input, err := h.lb.Store.GetStoredMediaInput(ctx, id, owner)
	if err != nil {
		if errors.Is(err, store.ErrNoRows) {
			return "", 0, fmt.Errorf("file_id is unavailable or belongs to another API key")
		}
		return "", 0, fmt.Errorf("failed to load file_id")
	}
	if input.Kind != expectedKind {
		return "", 0, fmt.Errorf("file_id must reference %s media", expectedKind)
	}
	if input.SizeBytes <= 0 || input.SizeBytes > maxMediaInputBytes || !validCachedMediaContentPath(input.ContentPath, input.Kind) {
		return "", 0, fmt.Errorf("file_id media is invalid")
	}
	file, err := os.Open(input.ContentPath)
	if err != nil {
		return "", 0, fmt.Errorf("file_id media is unavailable")
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, maxMediaInputBytes+1))
	if err != nil || len(data) == 0 || len(data) > maxMediaInputBytes || int64(len(data)) != input.SizeBytes {
		return "", 0, fmt.Errorf("file_id media is invalid")
	}
	kind, mimeType, err := detectMediaInput(data, input.MIMEType)
	if err != nil || kind != expectedKind || !strings.EqualFold(mimeType, input.MIMEType) {
		return "", 0, fmt.Errorf("file_id media is invalid")
	}
	return dataURIFromBytes(mimeType, data), int64(len(data)), nil
}

func (h *Handler) resolveConsoleVideoFileIDs(ctx context.Context, payload map[string]interface{}, owner string) error {
	var resolvedBytes int64
	resolveObject := func(field string, object map[string]interface{}, expectedKind string) error {
		rawURL, _ := object["url"].(string)
		prefix := localMediaInputPrefix + expectedKind + ":"
		if !strings.HasPrefix(rawURL, prefix) {
			return nil
		}
		id := strings.TrimPrefix(rawURL, prefix)
		dataURL, size, err := h.resolveMediaInputDataURL(ctx, id, owner, expectedKind)
		if err != nil {
			return fmt.Errorf("%s.file_id: %w", field, err)
		}
		resolvedBytes += size
		if resolvedBytes > maxResolvedMediaBytes {
			return fmt.Errorf("combined file_id media exceeds 32 MiB")
		}
		object["url"] = dataURL
		return nil
	}
	if image, ok := payload["image"].(map[string]interface{}); ok {
		if err := resolveObject("image", image, "image"); err != nil {
			return err
		}
	}
	if video, ok := payload["video"].(map[string]interface{}); ok {
		if err := resolveObject("video", video, "video"); err != nil {
			return err
		}
	}
	if references, ok := payload["reference_images"].([]map[string]interface{}); ok {
		for index, reference := range references {
			if err := resolveObject(fmt.Sprintf("reference_images[%d]", index), reference, "image"); err != nil {
				return err
			}
		}
	}
	return nil
}

// Resolve private input IDs before scheduling a Build job. Never send a local
// path or another API key's asset upstream. Multipart data follows the same
// byte/type limits as stored uploads.
func (h *Handler) resolveBuildVideoReferences(ctx context.Context, references []string, owner string) ([]string, error) {
	resolved := make([]string, 0, len(references))
	var total int64
	for index, reference := range references {
		value := strings.TrimSpace(reference)
		id := strings.TrimPrefix(value, localMediaInputPrefix+"image:")
		if validMediaInputID(id) {
			var err error
			value, _, err = h.resolveMediaInputDataURL(ctx, id, owner, "image")
			if err != nil {
				return nil, fmt.Errorf("input_references[%d]: %w", index, err)
			}
		}
		size, err := validateBuildVideoReference(value)
		if err != nil {
			return nil, fmt.Errorf("input_references[%d]: %w", index, err)
		}
		total += size
		if total > maxResolvedMediaBytes {
			return nil, fmt.Errorf("combined inline reference images exceed 32 MiB")
		}
		resolved = append(resolved, value)
	}
	return resolved, nil
}

func validateBuildVideoReference(value string) (int64, error) {
	if publicHTTPSURL(value) {
		return 0, nil
	}
	_, encoded, declared, err := parseDataURI(value)
	if err != nil {
		return 0, fmt.Errorf("reference image must be an HTTPS URL, image data URI, or owned media input ID")
	}
	if len(encoded) > base64.StdEncoding.EncodedLen(maxMediaInputBytes) {
		return 0, fmt.Errorf("reference image exceeds 20 MiB")
	}
	data, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil || len(data) == 0 || len(data) > maxMediaInputBytes {
		return 0, fmt.Errorf("reference image data is invalid or exceeds 20 MiB")
	}
	kind, mimeType, err := detectMediaInput(data, declared)
	if err != nil || kind != "image" || !strings.EqualFold(mimeType, declared) {
		return 0, fmt.Errorf("reference image MIME type does not match its content")
	}
	return int64(len(data)), nil
}
