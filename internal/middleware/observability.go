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
	mu                                      sync.Mutex
	input, cached, output, reasoning, total int64
	usage                                   bool
	// costTicks sums every priced row the request produced, and priced marks that
	// at least one of them carried a price.
	costTicks                    int64
	priced                       bool
	attempts, failures, switches int64
	account                      int64
	providerReached              bool
	finalEvent                   *audit.Event
	journal                      audit.Logger
}

var requestJournal audit.Logger

func SetRequestAuditLogger(logger audit.Logger) { requestJournal = logger }

var detailedOutcomeRecorder func(context.Context, opsagg.Outcome)

func SetDetailedOutcomeRecorder(recorder func(context.Context, opsagg.Outcome)) {
	detailedOutcomeRecorder = recorder
}

type observedAuditLogger struct{ next audit.Logger }

func ObserveAuditLogger(next audit.Logger) audit.Logger { return observedAuditLogger{next} }
func (l observedAuditLogger) Log(ctx context.Context, e audit.Event) {
	deferJournal := false
	if box, ok := ctx.Value(requestObservationKey{}).(*requestObservation); ok {
		box.mu.Lock()
		// A request can produce more than one priced row (a retry, or a media
		// call after a text attempt), so the cost is summed rather than replaced.
		if e.CostInUSDTicks > 0 {
			box.costTicks += e.CostInUSDTicks
			box.priced = true
		}
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
				box.cached += int64(e.CachedInputTokens)
				box.output += int64(e.OutputTokens)
				box.reasoning += int64(e.ReasoningTokens)
				box.total += int64(e.TotalTokens)
				box.usage = true
			}
		} else if e.Action == "chat_request" || e.Action == "grok_request" {
			copy := e
			box.finalEvent, box.journal = &copy, l.next
			deferJournal = true
			box.providerReached = true
			if e.InputTokens > 0 || e.OutputTokens > 0 {
				box.input = int64(e.InputTokens)
				box.cached = int64(e.CachedInputTokens)
				box.output = int64(e.OutputTokens)
				box.reasoning = int64(e.ReasoningTokens)
				box.total = int64(e.TotalTokens)
				box.usage = true
			}
		}
		box.mu.Unlock()
	}
	if deferJournal {
		return
	}
	if id := GetRequestID(ctx); id != "" {
		e.RequestID = id
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
			ctx, capture := debug.WithCapture(r.Context(), GetRequestID(r.Context()))
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

// Complete exactly one request journal row, including failures before a handler
// reaches its normal audit call. Use the same duration as the metrics/capture.
func finishRequestJournal(r *http.Request, w *TracedResponseWriter, duration time.Duration, model string) {
	channel := inferenceRequestChannel(r)
	if channel == HTTPChannel {
		return
	}
	logger := requestJournal
	e := audit.Event{Kind: audit.KindRequest, Action: "http_request", Channel: channel, Model: model}
	if box, ok := r.Context().Value(requestObservationKey{}).(*requestObservation); ok {
		box.mu.Lock()
		if box.finalEvent != nil {
			e = *box.finalEvent
		}
		if box.journal != nil {
			logger = box.journal
		}
		box.mu.Unlock()
	}
	if logger == nil {
		return
	}
	e.RequestID, e.Duration = GetRequestID(r.Context()), duration.Milliseconds()
	e.Timestamp = time.Now()
	e.ClientIP, e.UserAgent = ClientIP(r), r.UserAgent()
	metadata := map[string]interface{}{}
	for k, v := range e.Metadata {
		metadata[k] = v
	}
	metadata["http_status"], metadata["path"], metadata["method"] = w.StatusCode, r.URL.Path, r.Method
	metadata["trace_id"] = GetTraceID(r.Context())
	e.Metadata = metadata
	if w.StreamFailed() {
		e.Status = "stream_error"
	} else if w.StatusCode >= 400 {
		e.Status = "error"
	} else if e.Status == "" {
		e.Status = "success"
	}
	if capture := debug.FromContext(r.Context()); capture != nil {
		capture.Append("6_request_events.jsonl", fmtJSON(e)+"\n")
	}
	logger.Log(r.Context(), e)
}
