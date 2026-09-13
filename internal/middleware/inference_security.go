package middleware

import (
	"encoding/json"
	"net/http"
)

// BearerAPIKeyAuth cannot be disabled by configuration or replaced by a cookie,
// query parameter, raw Authorization value or x-api-key header.
func BearerAPIKeyAuth(validate APIKeyValidator, limiter *RateLimiter, next http.HandlerFunc) http.HandlerFunc {
	validated := APIKeyAuth(nil, validate, next)
	return func(w http.ResponseWriter, r *http.Request) {
		if APIKeyID(r.Context()) > 0 {
			next(w, r)
			return
		}
		if limiter != nil && !limiter.Allow(ClientIP(r)) {
			w.Header().Set("Retry-After", "60")
			writeAPIKeyError(w, http.StatusTooManyRequests, "Request rate limit exceeded", "rate_limit_exceeded")
			return
		}
		if bearerToken(r) == "" {
			writeAPIKeyError(w, http.StatusUnauthorized, "Authorization: Bearer is required", "invalid_api_key")
			return
		}
		validated(w, r)
	}
}

// InferenceErrors strips upstream error bodies without buffering successful
// responses or SSE. Status codes, Retry-After and tracing headers are preserved.
func InferenceErrors(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		next(&inferenceErrorWriter{TracedResponseWriter: NewTracedResponseWriter(w)}, r)
	}
}

type inferenceErrorWriter struct {
	*TracedResponseWriter
	committed bool
	rejected  bool
}

func (w *inferenceErrorWriter) WriteHeader(status int) {
	if w.committed {
		return
	}
	if status >= 100 && status < 200 && status != http.StatusSwitchingProtocols {
		w.ResponseWriter.WriteHeader(status)
		return
	}
	w.committed = true
	w.rejected = status >= 400
	if w.rejected {
		w.Header().Del("Content-Length")
		w.Header().Del("Content-Encoding")
		w.Header().Set("Content-Type", "application/json")
	}
	w.TracedResponseWriter.WriteHeader(status)
	if w.rejected {
		message := http.StatusText(status)
		if message == "" {
			message = "Request failed"
		}
		payload, _ := json.Marshal(map[string]interface{}{"error": map[string]interface{}{"message": message, "type": "request_error", "code": status}})
		_, _ = w.TracedResponseWriter.Write(payload)
	}
}
func (w *inferenceErrorWriter) Write(p []byte) (int, error) {
	if !w.committed {
		w.WriteHeader(http.StatusOK)
	}
	if w.rejected {
		return len(p), nil
	}
	return w.TracedResponseWriter.Write(p)
}
func (w *inferenceErrorWriter) Flush() {
	if !w.committed {
		w.WriteHeader(http.StatusOK)
	}
	w.TracedResponseWriter.Flush()
}
