package web

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestStaticHandlerCachePolicy(t *testing.T) {
	handler := StaticHandler()
	tests := []struct {
		path string
		want string
	}{
		{"/css/main.css?v=release", "public, max-age=31536000, immutable"},
		{"/js/common.js?v=release", "public, max-age=31536000, immutable"},
		{"/css/main.css", "no-cache"},
		{"/login.html", "no-cache"},
	}

	for _, tt := range tests {
		t.Run(tt.path, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			request := httptest.NewRequest(http.MethodGet, tt.path, nil)
			handler.ServeHTTP(recorder, request)
			if got := recorder.Header().Get("Cache-Control"); got != tt.want {
				t.Fatalf("Cache-Control = %q, want %q", got, tt.want)
			}
			if recorder.Code != http.StatusOK {
				t.Fatalf("status = %d, want %d", recorder.Code, http.StatusOK)
			}
		})
	}
}
