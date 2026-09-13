package middleware

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"orchids-api/internal/audit"
	"orchids-api/internal/debug"
	"orchids-api/internal/opsagg"
)

type requestObservationKey struct{}
type requestObservation struct {
	mu                           sync.Mutex
	input, output                int64
	usage                        bool
	attempts, failures, switches int64
	account                      int64
	providerReached              bool
}

var detailedOutcomeRecorder func(context.Context, opsagg.Outcome)

func SetDetailedOutcomeRecorder(recorder func(context.Context, opsagg.Outcome)) {
	detailedOutcomeRecorder = recorder
}

type observedAuditLogger struct{ next audit.Logger }

func ObserveAuditLogger(next audit.Logger) audit.Logger { return observedAuditLogger{next} }
func (l observedAuditLogger) Log(ctx context.Context, e audit.Event) {
	if box, ok := ctx.Value(requestObservationKey{}).(*requestObservation); ok {
		box.mu.Lock()
		if strings.HasSuffix(e.Action, "upstream_attempt") {
			box.providerReached = true
			box.attempts++
			if e.Status != "ok" && e.Status != "success" {
				box.failures++
			}
			if box.account != 0 && e.AccountID != 0 && box.account != e.AccountID {
				box.switches++
			}
			if e.AccountID != 0 {
				box.account = e.AccountID
			}
			// Native Responses reports its usage on the completed upstream attempt.
			if e.InputTokens > 0 || e.OutputTokens > 0 {
				box.input += int64(e.InputTokens)
				box.output += int64(e.OutputTokens)
				box.usage = true
			}
		} else if e.Action == "chat_request" || e.Action == "grok_request" {
			box.providerReached = true
			if e.InputTokens > 0 || e.OutputTokens > 0 {
				box.input = int64(e.InputTokens)
				box.output = int64(e.OutputTokens)
				box.usage = true
			}
		}
		box.mu.Unlock()
	}
	if capture := debug.FromContext(ctx); capture != nil {
		raw, _ := json.Marshal(e)
		capture.Append("6_request_events.jsonl", string(raw)+"\n")
	}
	if l.next != nil {
		l.next.Log(ctx, e)
	}
}

// Diagnostics captures only inference traffic. Tee readers preserve streaming
// and body limits; storage failures never replace a successful client response.
func Diagnostics(store *debug.DiagnosticStore, enabled func() bool) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if store == nil || enabled == nil || !enabled() || r.Method != http.MethodPost || requestChannel(r.URL.Path) == HTTPChannel {
				next.ServeHTTP(w, r)
				return
			}
			contentType := strings.ToLower(strings.TrimSpace(r.Header.Get("Content-Type")))
			if contentType != "" && !strings.Contains(contentType, "application/json") {
				next.ServeHTTP(w, r)
				return
			}
			ctx, capture := debug.WithCapture(r.Context(), GetTraceID(r.Context()))
			r = r.WithContext(ctx)
			if r.Body != nil {
				r.Body = &diagnosticReader{ReadCloser: r.Body, capture: capture, name: "1_http_request.json"}
			}
			writer := &diagnosticWriter{TracedResponseWriter: NewTracedResponseWriter(w), capture: capture}
			defer func() {
				capture.Set("6_http_summary.json", fmtJSON(map[string]interface{}{"status": writer.StatusCode, "bytes": writer.BytesWritten, "stream_failed": writer.StreamFailed()}))
				saveCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
				defer cancel()
				if err := store.Save(saveCtx, capture.Bundle()); err != nil {
					slog.Warn("Could not save request diagnostics", "error", err)
				}
			}()
			next.ServeHTTP(writer, r)
		})
	}
}
func fmtJSON(value interface{}) string { raw, _ := json.Marshal(value); return string(raw) }

type diagnosticReader struct {
	io.ReadCloser
	capture *debug.Capture
	name    string
}

func (r *diagnosticReader) Read(p []byte) (int, error) {
	n, err := r.ReadCloser.Read(p)
	r.capture.Append(r.name, string(p[:n]))
	return n, err
}

type diagnosticWriter struct {
	*TracedResponseWriter
	capture *debug.Capture
}

func (w *diagnosticWriter) Write(p []byte) (int, error) {
	n, err := w.TracedResponseWriter.Write(p)
	w.capture.Append("5_http_response.txt", string(p[:n]))
	return n, err
}

func RecordUpstreamAttempt(ctx context.Context, accountID int64, failed bool) {
	if box, ok := ctx.Value(requestObservationKey{}).(*requestObservation); ok {
		box.mu.Lock()
		defer box.mu.Unlock()
		box.providerReached = true
		box.attempts++
		if failed {
			box.failures++
		}
		if box.account != 0 && accountID != 0 && box.account != accountID {
			box.switches++
		}
		if accountID != 0 {
			box.account = accountID
		}
	}
}
