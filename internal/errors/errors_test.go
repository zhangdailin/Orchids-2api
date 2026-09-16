package errors

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestAppError_ToJSON(t *testing.T) {
	err := New("invalid_request_error", "请求格式无效", http.StatusBadRequest)
	json := string(err.ToJSON())

	if json == "" {
		t.Error("ToJSON() returned empty string")
	}
	if !strings.Contains(json, `"type":"error"`) {
		t.Errorf("ToJSON() missing type field: %s", json)
	}
	if !strings.Contains(json, `"type":"invalid_request_error"`) {
		t.Errorf("ToJSON() missing error type: %s", json)
	}
}

func TestAppError_WriteResponse(t *testing.T) {
	err := New("invalid_request_error", "请求格式无效", http.StatusBadRequest)
	w := httptest.NewRecorder()

	err.WriteResponse(w)

	if w.Code != http.StatusBadRequest {
		t.Errorf("WriteResponse() status = %d, want %d", w.Code, http.StatusBadRequest)
	}
	if ct := w.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("WriteResponse() Content-Type = %q, want %q", ct, "application/json")
	}
}

func TestNew(t *testing.T) {
	err := New("custom_code", "custom message", http.StatusTeapot)

	if err.Code != "custom_code" {
		t.Errorf("New() code = %v, want %v", err.Code, "custom_code")
	}
	if err.Message != "custom message" {
		t.Errorf("New() message = %v, want %v", err.Message, "custom message")
	}
	if err.HTTPStatus != http.StatusTeapot {
		t.Errorf("New() status = %v, want %v", err.HTTPStatus, http.StatusTeapot)
	}
}
