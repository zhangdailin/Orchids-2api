package middleware

import (
	"bytes"
	"io"
	"net/http"
	"strings"

	"orchids-api/internal/audit"
)

// operationLogger is the operation journal. It is injected once at startup so a
// single wrapper can cover every admin endpoint: adding per-handler calls would
// mean a new endpoint could silently ship unaudited.
var operationLogger audit.Logger

// SetOperationAuditLogger wires the journal used by adminSessionAudit.
func SetOperationAuditLogger(logger audit.Logger) {
	operationLogger = logger
}

// maxAuditBodyBytes bounds how much of a request body is buffered for the
// change summary. Admin payloads are small; the cap keeps a large import from
// turning into a large allocation.
const maxAuditBodyBytes = 32 << 10

// captureRequestBody replaces r.Body with a re-readable copy and returns the// bytes for summarising. Reads beyond maxAuditBodyBytes stop being captured
// while the handler still receives the complete body.
func captureRequestBody(r *http.Request) []byte {
	if r == nil || r.Body == nil {
		return nil
	}
	raw, err := io.ReadAll(io.LimitReader(r.Body, maxAuditBodyBytes+1))
	_ = r.Body.Close()
	if err != nil {
		// Rebuild an empty body so the handler can still answer 400 itself.
		r.Body = io.NopCloser(bytes.NewReader(nil))
		return nil
	}
	captured := raw
	if len(captured) > maxAuditBodyBytes {
		captured = captured[:maxAuditBodyBytes]
	}
	// The handler must see every byte, not just the captured prefix.
	r.Body = io.NopCloser(io.MultiReader(bytes.NewReader(raw), r.Body))
	r.ContentLength = int64(len(raw))
	return captured
}

// operationAction renders the journal action name for an admin request, e.g.
// "account.update" or "config.rotate_key". It is deliberately mechanical: the
// path is the source of truth, so a new endpoint is named without extra code.
func operationAction(method, path string) string {
	trimmed := strings.Trim(strings.TrimSpace(path), "/")
	trimmed = strings.TrimPrefix(trimmed, "api/")
	trimmed = strings.TrimPrefix(trimmed, "admin/")
	if trimmed == "" {
		return strings.ToLower(method) + ".root"
	}
	parts := strings.Split(trimmed, "/")
	resource := parts[0]
	switch resource {
	case "login", "logout":
		// Session endpoints read better as session.create / session.delete.
		if resource == "login" {
			return "session.create"
		}
		return "session.delete"
	}
	if len(parts) > 1 && parts[1] != "" {
		switch parts[1] {
		case "login":
			resource = "session"
		case "logout":
			resource = "session"
		default:
			resource = resource + "." + parts[1]
		}
	}
	resource = strings.ReplaceAll(resource, "/", ".")
	switch method {
	case http.MethodPost:
		return resource + ".create"
	case http.MethodPut, http.MethodPatch:
		return resource + ".update"
	case http.MethodDelete:
		return resource + ".delete"
	default:
		return resource + ".read"
	}
}

// operationTarget extracts the object an admin change touched.
func operationTarget(path string, query map[string][]string) string {
	trimmed := strings.Trim(strings.TrimSpace(path), "/")
	parts := strings.Split(trimmed, "/")
	if len(parts) >= 3 && isDigits(parts[2]) {
		return parts[1] + ":" + parts[2]
	}
	if len(parts) >= 2 && parts[0] == "api" && isDigits(parts[1]) {
		return parts[1]
	}
	for _, key := range []string{"id", "account_id", "key_id"} {
		if values := query[key]; len(values) > 0 && strings.TrimSpace(values[0]) != "" {
			return key + ":" + strings.TrimSpace(values[0])
		}
	}
	return ""
}

func isDigits(value string) bool {
	if value == "" {
		return false
	}
	for _, r := range value {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// adminSessionAudit wraps an admin handler so every change it performs is
// journalled with the actor, the source address, the object and a redacted
// change summary. Only mutating methods are recorded: reads would bury the
// changes an operator actually needs to trace.
func adminSessionAudit(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if operationLogger == nil || !isMutatingMethod(r.Method) {
			next(w, r)
			return
		}
		// A login body carries the admin password; record the attempt, never the
		// payload.
		isLogin := strings.HasSuffix(strings.Trim(r.URL.Path, "/"), "/login")
		var body []byte
		if !isLogin {
			body = captureRequestBody(r)
		}

		recorder := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next(recorder, r)

		details, redacted := audit.SummarizeChange(body)
		status := "success"
		if recorder.status >= 400 {
			status = "error"
		}
		operationLogger.Log(r.Context(), audit.Event{
			Kind:      audit.KindOperation,
			Action:    operationAction(r.Method, r.URL.Path),
			Actor:     adminActor(r),
			Target:    operationTarget(r.URL.Path, r.URL.Query()),
			ClientIP:  ClientIP(r),
			UserAgent: r.UserAgent(),
			Status:    status,
			Details:   details,
			Redacted:  redacted,
			Metadata: map[string]interface{}{
				"method": r.Method,
				"path":   r.URL.Path,
				"code":   recorder.status,
			},
		})
	}
}

func isMutatingMethod(method string) bool {
	switch method {
	case http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete:
		return true
	default:
		return false
	}
}

// adminActor names who performed the change. The admin session is a single
// shared credential, so the credential kind is the honest answer; a request
// signed with the static admin token is distinguished from a browser session.
func adminActor(r *http.Request) string {
	if r == nil {
		return "unknown"
	}
	if _, err := r.Cookie("session_token"); err == nil {
		return "admin-session"
	}
	if strings.TrimSpace(r.Header.Get("X-Admin-Token")) != "" {
		return "admin-token"
	}
	if strings.HasPrefix(strings.TrimSpace(r.Header.Get("Authorization")), "Bearer ") {
		return "admin-bearer"
	}
	return "admin"
}

// statusRecorder remembers the response status for the journal without
// interfering with handlers that stream or flush.
type statusRecorder struct {
	http.ResponseWriter
	status int
	wrote  bool
}

func (s *statusRecorder) WriteHeader(code int) {
	if !s.wrote {
		s.status = code
		s.wrote = true
	}
	s.ResponseWriter.WriteHeader(code)
}

func (s *statusRecorder) Write(payload []byte) (int, error) {
	if !s.wrote {
		s.wrote = true
	}
	return s.ResponseWriter.Write(payload)
}

func (s *statusRecorder) Flush() {
	if flusher, ok := s.ResponseWriter.(http.Flusher); ok {
		flusher.Flush()
	}
}

// Unwrap lets http.ResponseController reach the underlying writer.
func (s *statusRecorder) Unwrap() http.ResponseWriter { return s.ResponseWriter }
