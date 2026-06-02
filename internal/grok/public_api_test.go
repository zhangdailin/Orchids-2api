package grok

import (
	"bytes"
	"github.com/goccy/go-json"
	"net/http"
	"net/http/httptest"
	"testing"

	"orchids-api/internal/config"
)

func TestHandlePublicVerify(t *testing.T) {
	h := &Handler{}
	req := httptest.NewRequest(http.MethodGet, "/v1/public/verify", nil)
	rec := httptest.NewRecorder()
	h.HandlePublicVerify(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d want=%d", rec.Code, http.StatusOK)
	}
}

func TestHandlePublicImagineConfig(t *testing.T) {
	h := &Handler{}
	req := httptest.NewRequest(http.MethodGet, "/v1/public/imagine/config", nil)
	rec := httptest.NewRecorder()
	h.HandlePublicImagineConfig(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d want=%d", rec.Code, http.StatusOK)
	}
	var out map[string]interface{}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if got, _ := out["final_min_bytes"].(float64); int(got) != 100000 {
		t.Fatalf("final_min_bytes=%v want=100000", out["final_min_bytes"])
	}
	if got, _ := out["medium_min_bytes"].(float64); int(got) != 30000 {
		t.Fatalf("medium_min_bytes=%v want=30000", out["medium_min_bytes"])
	}
	if got, _ := out["nsfw"].(bool); got != false {
		t.Fatalf("nsfw=%v want=false", out["nsfw"])
	}
}

func TestHandlePublicImagineConfig_FromConfig(t *testing.T) {
	nsfw := false
	h := &Handler{
		cfg: &config.Config{
			ImageFinalMinBytes:  22222,
			ImageMediumMinBytes: 11111,
			ImageNSFW:           &nsfw,
		},
	}
	req := httptest.NewRequest(http.MethodGet, "/v1/public/imagine/config", nil)
	rec := httptest.NewRecorder()
	h.HandlePublicImagineConfig(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d want=%d", rec.Code, http.StatusOK)
	}
	var out map[string]interface{}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if got, _ := out["final_min_bytes"].(float64); int(got) != 22222 {
		t.Fatalf("final_min_bytes=%v want=22222", out["final_min_bytes"])
	}
	if got, _ := out["medium_min_bytes"].(float64); int(got) != 11111 {
		t.Fatalf("medium_min_bytes=%v want=11111", out["medium_min_bytes"])
	}
	if got, _ := out["nsfw"].(bool); got != false {
		t.Fatalf("nsfw=%v want=false", out["nsfw"])
	}
}

func TestHandlePublicVideoStart_EmptyPrompt(t *testing.T) {
	h := &Handler{}
	req := httptest.NewRequest(http.MethodPost, "/v1/public/video/start", bytes.NewBufferString(`{"prompt":""}`))
	rec := httptest.NewRecorder()
	h.HandlePublicVideoStart(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status=%d want=%d", rec.Code, http.StatusBadRequest)
	}
}

func TestHandlePublicVideoStart_Success(t *testing.T) {
	h := &Handler{}
	raw := map[string]interface{}{
		"prompt":          "hello",
		"aspect_ratio":    "1280x720",
		"video_length":    6,
		"resolution_name": "480p",
		"preset":          "normal",
	}
	body, _ := json.Marshal(raw)
	req := httptest.NewRequest(http.MethodPost, "/v1/public/video/start", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	h.HandlePublicVideoStart(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d want=%d body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}
	var out map[string]interface{}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("invalid json: %v", err)
	}
	if out["task_id"] == "" {
		t.Fatalf("missing task_id: %+v", out)
	}
	if out["aspect_ratio"] != "16:9" {
		t.Fatalf("aspect_ratio=%v want=16:9", out["aspect_ratio"])
	}
}

func TestHandlePublicVideoSSE_TaskNotFound(t *testing.T) {
	h := &Handler{}
	req := httptest.NewRequest(http.MethodGet, "/v1/public/video/sse?task_id=not-found", nil)
	rec := httptest.NewRecorder()
	h.HandlePublicVideoSSE(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status=%d want=%d", rec.Code, http.StatusNotFound)
	}
}
