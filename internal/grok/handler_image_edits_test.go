package grok

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestDecodeImageEditJSONAcceptsImageAndImages(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/v1/images/edits", strings.NewReader(`{
		"model":"grok-imagine-image-edit",
		"prompt":"combine them",
		"image":{"url":"https://example.com/one.png"},
		"images":[{"file_id":"input_abcdefghijklmnopqrstuvwxyz012345"}]
	}`))
	rec := httptest.NewRecorder()

	decoded, ok := decodeImageEditJSON(rec, req)
	if !ok {
		t.Fatalf("decodeImageEditJSON() failed: status=%d body=%s", rec.Code, rec.Body.String())
	}
	inputs, err := imageEditJSONInputs(decoded)
	if err != nil {
		t.Fatalf("imageEditJSONInputs() error = %v", err)
	}
	if len(inputs) != 2 || inputs[0].URL != "https://example.com/one.png" || inputs[1].FileID != "input_abcdefghijklmnopqrstuvwxyz012345" {
		t.Fatalf("inputs = %#v", inputs)
	}
}

func TestImageEditJSONInputsRequireExactlyOneReference(t *testing.T) {
	tests := []struct {
		name  string
		input imageEditJSONInput
		want  string
	}{
		{name: "empty", input: imageEditJSONInput{}, want: "exactly one"},
		{name: "both", input: imageEditJSONInput{URL: "https://example.com/a.png", FileID: "input_abcdefghijklmnopqrstuvwxyz012345"}, want: "exactly one"},
		{name: "bad file id", input: imageEditJSONInput{FileID: "file_123"}, want: "file_id is invalid"},
		{name: "bad URL scheme", input: imageEditJSONInput{URL: "file:///etc/passwd"}, want: "HTTP(S) URL"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := imageEditJSONInputs(&imageEditJSONRequest{Image: &test.input})
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error = %v, want substring %q", err, test.want)
			}
		})
	}
}

func TestResolveImageEditJSONInputsUsesOwnedMediaStore(t *testing.T) {
	h, store, mini := setupValidationHandler(t)
	defer func() {
		_ = store.Close()
		mini.Close()
	}()

	_, err := h.resolveImageEditJSONInputs(context.Background(), []imageEditJSONInput{{
		FileID: "input_abcdefghijklmnopqrstuvwxyz012345",
	}}, "owner")
	if err == nil || !strings.Contains(err.Error(), "unavailable or belongs to another API key") {
		t.Fatalf("error = %v", err)
	}
}

func TestImageEditInputsToUploadsKeepsSSRFGuard(t *testing.T) {
	_, err := (&Handler{}).imageEditInputsToUploads(context.Background(), []string{"http://127.0.0.1/input.png"})
	if err == nil || !strings.Contains(err.Error(), "remote url is not allowed") {
		t.Fatalf("error = %v", err)
	}
}

func TestDecodeImageEditJSONRejectsTrailingValue(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/v1/images/edits", strings.NewReader(`{"prompt":"edit","image":{"url":"https://example.com/a.png"}} {}`))
	rec := httptest.NewRecorder()

	if _, ok := decodeImageEditJSON(rec, req); ok {
		t.Fatal("decodeImageEditJSON() accepted trailing JSON")
	}
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "one JSON object") {
		t.Fatalf("status=%d body=%q", rec.Code, rec.Body.String())
	}
}
