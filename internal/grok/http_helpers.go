package grok

import (
	"io"
	"net/http"
	"strings"

	"github.com/goccy/go-json"

	"orchids-api/internal/debug"
	"orchids-api/internal/middleware"
)

var (
	grokSSEEventPrefixBytes = []byte("event: ")
	grokSSEDataPrefixBytes  = []byte("data: ")
	grokSSENewlineBytes     = []byte("\n")
	grokSSEFrameSuffixBytes = []byte("\n\n")
)

// requireMethod writes the standard 405 response and returns false when the
// request method does not match. Handlers use it as:
//
//	if !requireMethod(w, r, http.MethodGet) {
//		return
//	}
func requireMethod(w http.ResponseWriter, r *http.Request, method string) bool {
	if r.Method != method {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return false
	}
	return true
}

// writeJSON writes v to w as a JSON response with the application/json content
// type.
func writeJSON(w http.ResponseWriter, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func writeJSONStatus(w http.ResponseWriter, status int, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// decodeJSONBody decodes the request body into v and writes the standard
// 400 response on failure.
func decodeJSONBody(w http.ResponseWriter, r *http.Request, v interface{}) bool {
	if err := json.NewDecoder(r.Body).Decode(v); err != nil {
		http.Error(w, "invalid json", http.StatusBadRequest)
		return false
	}
	return true
}

func requireAPIKeyModel(w http.ResponseWriter, r *http.Request, model string) bool {
	if middleware.APIKeyAllowsModel(r.Context(), model) {
		return true
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusForbidden)
	_ = json.NewEncoder(w).Encode(map[string]interface{}{
		"error": map[string]interface{}{
			"message": "API key is not allowed to use model " + strings.TrimSpace(model),
			"type":    "permission_error",
			"code":    "model_not_allowed",
		},
	})
	return false
}

// requireGrokStore writes the standard 503 response and returns false when the
// handler has no account store. Admin handlers use it as:
//
//	if !requireGrokStore(w, h) {
//		return
//	}
func requireGrokStore(w http.ResponseWriter, h *Handler) bool {
	if h == nil || h.lb == nil || h.lb.Store == nil {
		http.Error(w, "store not configured", http.StatusServiceUnavailable)
		return false
	}
	return true
}

// requireGrokClient writes the standard 503 response and returns false when
// the handler has no grok client.
func requireGrokClient(w http.ResponseWriter, h *Handler) bool {
	if h == nil || h.client == nil {
		http.Error(w, "grok client not configured", http.StatusServiceUnavailable)
		return false
	}
	return true
}

// streamResponseHeaders writes the standard SSE headers and returns the
// response flusher (possibly nil).
func streamResponseHeaders(w http.ResponseWriter) http.Flusher {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	flusher, _ := w.(http.Flusher)
	return flusher
}

// writeSSEEventName writes an SSE event name, preferring StringWriter when available.
func writeSSEEventName(w http.ResponseWriter, event string) {
	if sw, ok := w.(io.StringWriter); ok {
		_, _ = sw.WriteString(event)
		return
	}
	_, _ = w.Write([]byte(event))
}

// writeSSEBytes sends a raw SSE frame without flushing.
func writeSSEBytes(w http.ResponseWriter, event string, data []byte) {
	if event != "" {
		_, _ = w.Write(grokSSEEventPrefixBytes)
		writeSSEEventName(w, event)
		_, _ = w.Write(grokSSENewlineBytes)
	}
	_, _ = w.Write(grokSSEDataPrefixBytes)
	_, _ = w.Write(data)
	_, _ = w.Write(grokSSEFrameSuffixBytes)
}

// writeSSEError sends an OpenAI-style SSE error event (no flush, no [DONE]).
//
// An SSE error is written after the 200 status line is already committed, so the
// HTTP status can no longer describe the outcome. The response writer is told
// about it instead, which is how the operations overview counts a stream that
// died after starting as a failure rather than a success.
func writeSSEError(w http.ResponseWriter, message, errType, code string) {
	middleware.MarkStreamFailure(w)
	payload := map[string]interface{}{
		"error": map[string]interface{}{
			"message": strings.TrimSpace(message),
			"type":    strings.TrimSpace(errType),
			"code":    strings.TrimSpace(code),
		},
	}
	writeSSEBytes(w, "error", encodeJSONBytes(payload))
}

// writeSSE sends an SSE frame and flushes when the writer supports it.
func writeSSE(w http.ResponseWriter, flusher http.Flusher, event string, data []byte) {
	writeSSEBytes(w, event, data)
	if flusher != nil {
		flusher.Flush()
	}
}

// writeSSELog sends an SSE frame, mirrors it to the debug logger, and flushes.
func writeSSELog(w http.ResponseWriter, flusher http.Flusher, logger *debug.Logger, raw []byte) {
	writeSSEBytes(w, "", raw)
	if logger != nil {
		logger.LogOutputSSE("", string(raw))
	}
	if flusher != nil {
		flusher.Flush()
	}
}

// writeSSEStreamError sends the SSE error frame, the [DONE] terminator, and
// flushes, mirroring both to the debug logger when one is set.
func writeSSEStreamError(w http.ResponseWriter, flusher http.Flusher, logger *debug.Logger, msg string) {
	writeSSEError(w, msg, "server_error", "stream_error")
	writeSSEBytes(w, "", []byte("[DONE]"))
	if logger != nil {
		logger.LogOutputSSE("error", msg)
		logger.LogOutputSSE("", "[DONE]")
	}
	if flusher != nil {
		flusher.Flush()
	}
}

// writeSSECodedError sends a typed SSE error frame followed by [DONE] and flushes.
// Use this when the error code is not the generic stream_error.
func writeSSECodedError(w http.ResponseWriter, flusher http.Flusher, message, code string) {
	writeSSEError(w, message, "server_error", code)
	writeSSE(w, flusher, "", []byte("[DONE]"))
}
