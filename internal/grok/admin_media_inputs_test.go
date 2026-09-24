package grok

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/goccy/go-json"
	"orchids-api/internal/middleware"
)

func adminMediaTestHandler(t *testing.T) *Handler {
	t.Helper()
	oldBase := cacheBaseDir
	cacheBaseDir = t.TempDir()
	t.Cleanup(func() { cacheBaseDir = oldBase })
	h, store, mini := setupValidationHandler(t)
	t.Cleanup(func() {
		_ = store.Close()
		mini.Close()
	})
	return h
}

func testPNG(t *testing.T) []byte {
	t.Helper()
	data, err := base64.StdEncoding.DecodeString("iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mNk+A8AAQUBAScY42YAAAAASUVORK5CYII=")
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func TestAdminMediaInputUploadAliasResponseAndType(t *testing.T) {
	h := adminMediaTestHandler(t)
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	part, err := writer.CreateFormFile("file", "pixel.png")
	if err != nil {
		t.Fatal(err)
	}
	_, _ = part.Write(testPNG(t))
	_ = writer.Close()

	req := httptest.NewRequest(http.MethodPost, "/api/admin/v1/media/inputs/upload", &body)
	req.Header.Set("Content-Type", writer.FormDataContentType())
	rec := httptest.NewRecorder()
	h.HandleAdminMediaInputs(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var response struct {
		Data struct {
			FileID    string `json:"fileId"`
			Kind      string `json:"kind"`
			MIMEType  string `json:"mimeType"`
			SizeBytes int64  `json:"sizeBytes"`
			ExpiresAt string `json:"expiresAt"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if !validMediaInputID(response.Data.FileID) || response.Data.Kind != "image" || response.Data.MIMEType != "image/png" || response.Data.SizeBytes == 0 || response.Data.ExpiresAt == "" {
		t.Fatalf("unexpected camelCase envelope: %s", rec.Body.String())
	}

	var junk bytes.Buffer
	junkWriter := multipart.NewWriter(&junk)
	junkPart, _ := junkWriter.CreateFormFile("file", "fake.png")
	_, _ = junkPart.Write([]byte("not an image"))
	_ = junkWriter.Close()
	badReq := httptest.NewRequest(http.MethodPost, "/api/admin/v1/media/inputs/upload", &junk)
	badReq.Header.Set("Content-Type", junkWriter.FormDataContentType())
	badRec := httptest.NewRecorder()
	h.HandleAdminMediaInputs(badRec, badReq)
	if badRec.Code != http.StatusBadRequest || !strings.Contains(badRec.Body.String(), "invalidMedia") {
		t.Fatalf("invalid type status=%d body=%s", badRec.Code, badRec.Body.String())
	}
}

func TestAdminMediaInputImportSavesAndBoundsContent(t *testing.T) {
	h := adminMediaTestHandler(t)
	oldFetcher := adminMediaInputFetcher
	t.Cleanup(func() { adminMediaInputFetcher = oldFetcher })
	adminMediaInputFetcher = func(context.Context, string) ([]byte, string, error) {
		return testPNG(t), "image/png", nil
	}
	req := httptest.NewRequest(http.MethodPost, "/api/admin/v1/media/inputs/import", strings.NewReader(`{"url":"https://example.com/input.png"}`))
	rec := httptest.NewRecorder()
	h.HandleAdminMediaInputImport(rec, req)
	if rec.Code != http.StatusCreated || !strings.Contains(rec.Body.String(), `"fileId":"input_`) {
		t.Fatalf("import status=%d body=%s", rec.Code, rec.Body.String())
	}

	adminMediaInputFetcher = func(context.Context, string) ([]byte, string, error) {
		return nil, "", errAdminMediaTooLarge
	}
	largeReq := httptest.NewRequest(http.MethodPost, "/api/admin/v1/media/inputs/import", strings.NewReader(`{"url":"https://example.com/large.png"}`))
	largeRec := httptest.NewRecorder()
	h.HandleAdminMediaInputImport(largeRec, largeReq)
	if largeRec.Code != http.StatusRequestEntityTooLarge || !strings.Contains(largeRec.Body.String(), "mediaTooLarge") {
		t.Fatalf("large import status=%d body=%s", largeRec.Code, largeRec.Body.String())
	}

	adminMediaInputFetcher = func(context.Context, string) ([]byte, string, error) {
		return []byte("text payload"), "text/plain", nil
	}
	typeReq := httptest.NewRequest(http.MethodPost, "/api/admin/v1/media/inputs/import", strings.NewReader(`{"url":"https://example.com/not-media"}`))
	typeRec := httptest.NewRecorder()
	h.HandleAdminMediaInputImport(typeRec, typeReq)
	if typeRec.Code != http.StatusBadRequest || !strings.Contains(typeRec.Body.String(), "invalidMedia") {
		t.Fatalf("invalid import type status=%d body=%s", typeRec.Code, typeRec.Body.String())
	}
}

func TestAdminMediaInputImportBlocksPrivateURLBeforeFetch(t *testing.T) {
	h := adminMediaTestHandler(t)
	oldFetcher := adminMediaInputFetcher
	t.Cleanup(func() { adminMediaInputFetcher = oldFetcher })
	adminMediaInputFetcher = fetchAdminMediaInput
	for _, target := range []string{"http://127.0.0.1/private", "http://169.254.169.254/latest/meta-data"} {
		req := httptest.NewRequest(http.MethodPost, "/api/admin/v1/media/inputs/import", strings.NewReader(`{"url":`+string(mustJSON(target))+`}`))
		rec := httptest.NewRecorder()
		h.HandleAdminMediaInputImport(rec, req)
		if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "mediaURLBlocked") {
			t.Fatalf("target=%s status=%d body=%s", target, rec.Code, rec.Body.String())
		}
	}
}

func mustJSON(value string) []byte {
	data, _ := json.Marshal(value)
	return data
}

func TestClientMediaInputImportUsesKeyOwnerAndPlainEnvelope(t *testing.T) {
	h := adminMediaTestHandler(t)
	oldFetcher := adminMediaInputFetcher
	t.Cleanup(func() { adminMediaInputFetcher = oldFetcher })
	adminMediaInputFetcher = func(context.Context, string) ([]byte, string, error) {
		return testPNG(t), "image/png", nil
	}
	validator := func(context.Context, string) (*middleware.APIKeyPrincipal, error) {
		return &middleware.APIKeyPrincipal{ID: 73}, nil
	}
	wrapped := middleware.APIKeyAuthWithRequest(func(*http.Request) bool { return true }, validator, h.HandleMediaInputImport)
	req := httptest.NewRequest(http.MethodPost, "/v1/media/inputs/import", strings.NewReader(`{"url":"https://example.com/input.png"}`))
	req.Header.Set("Authorization", "Bearer client-key")
	rec := httptest.NewRecorder()
	wrapped(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var response struct {
		FileID string `json:"file_id"`
		Kind   string `json:"kind"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if !validMediaInputID(response.FileID) || response.Kind != "image" || strings.Contains(rec.Body.String(), `"data"`) {
		t.Fatalf("unexpected response: %s", rec.Body.String())
	}

	ownerReq := httptest.NewRequest(http.MethodGet, "/", nil)
	ownerReq.Header.Set("Authorization", "Bearer client-key")
	ownerRec := httptest.NewRecorder()
	var owner string
	middleware.APIKeyAuthWithRequest(func(*http.Request) bool { return true }, validator, func(_ http.ResponseWriter, r *http.Request) {
		owner = videoRequestOwner(r)
	})(ownerRec, ownerReq)
	if _, _, err := h.resolveMediaInputDataURL(context.Background(), response.FileID, owner, "image"); err != nil {
		t.Fatalf("owner cannot resolve imported file: %v", err)
	}
	if _, _, err := h.resolveMediaInputDataURL(context.Background(), response.FileID, "another-owner", "image"); err == nil {
		t.Fatal("another owner unexpectedly resolved imported file")
	}
}

func TestClientMediaInputImportBlocksPrivateURL(t *testing.T) {
	h := adminMediaTestHandler(t)
	oldFetcher := adminMediaInputFetcher
	t.Cleanup(func() { adminMediaInputFetcher = oldFetcher })
	adminMediaInputFetcher = fetchAdminMediaInput
	req := httptest.NewRequest(http.MethodPost, "/v1/media/inputs/import", strings.NewReader(`{"url":"http://127.0.0.1/private"}`))
	rec := httptest.NewRecorder()
	h.HandleMediaInputImport(rec, req)
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "media_url_blocked") {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
}

func TestAdminCacheProtectsMediaInputFiles(t *testing.T) {
	oldBase := cacheBaseDir
	cacheBaseDir = t.TempDir()
	t.Cleanup(func() { cacheBaseDir = oldBase })
	dir := filepath.Join(cacheBaseDir, "image")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	inputName := mediaInputFilePrefix + "protected.png"
	for name, data := range map[string][]byte{inputName: []byte("input"), "generated.png": []byte("generated")} {
		if err := os.WriteFile(filepath.Join(dir, name), data, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	h := &Handler{}
	listRec := httptest.NewRecorder()
	h.HandleAdminCacheList(listRec, httptest.NewRequest(http.MethodGet, "/api/v1/admin/cache/list?type=image", nil))
	if listRec.Code != http.StatusOK || strings.Contains(listRec.Body.String(), inputName) || !strings.Contains(listRec.Body.String(), "generated.png") {
		t.Fatalf("cache list exposed input: %s", listRec.Body.String())
	}

	deleteRec := httptest.NewRecorder()
	h.HandleAdminCacheItemDelete(deleteRec, httptest.NewRequest(http.MethodPost, "/api/v1/admin/cache/item/delete", strings.NewReader(`{"type":"image","name":"`+inputName+`"}`)))
	if deleteRec.Code != http.StatusBadRequest {
		t.Fatalf("input delete status=%d body=%s", deleteRec.Code, deleteRec.Body.String())
	}
	if _, err := os.Stat(filepath.Join(dir, inputName)); err != nil {
		t.Fatalf("input deleted: %v", err)
	}

	clearRec := httptest.NewRecorder()
	h.HandleAdminCacheClear(clearRec, httptest.NewRequest(http.MethodPost, "/api/v1/admin/cache/clear", strings.NewReader(`{"type":"image"}`)))
	if clearRec.Code != http.StatusOK {
		t.Fatalf("clear status=%d body=%s", clearRec.Code, clearRec.Body.String())
	}
	if _, err := os.Stat(filepath.Join(dir, inputName)); err != nil {
		t.Fatalf("input cleared: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "generated.png")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("generated file remains: %v", err)
	}
}
