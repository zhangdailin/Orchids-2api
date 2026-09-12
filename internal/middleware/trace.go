// Package middleware 提供 HTTP 中间件
package middleware

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"time"

	"orchids-api/internal/logutil"
)

// TraceIDHeader 是请求追踪 ID 的 HTTP 头名称
const TraceIDHeader = "X-Trace-ID"

// RequestIDHeader 是请求 ID 的 HTTP 头名称（别名）
const RequestIDHeader = "X-Request-ID"

// traceIDKey 是 context 中存储 trace ID 的 key
type traceIDKey struct{}

// GenerateTraceID 生成一个新的 trace ID
func GenerateTraceID() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		// 降级到时间戳
		return hex.EncodeToString([]byte(time.Now().Format("20060102150405.000000")))
	}
	return hex.EncodeToString(b)
}

// TraceMiddleware 添加请求追踪功能
// 从请求头获取 trace ID，如果没有则生成新的
func TraceMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// 尝试从请求头获取 trace ID
		traceID := r.Header.Get(TraceIDHeader)
		if traceID == "" {
			traceID = r.Header.Get(RequestIDHeader)
		}
		if traceID == "" {
			traceID = GenerateTraceID()
		}

		// 将 trace ID 添加到响应头
		w.Header().Set(TraceIDHeader, traceID)

		// 将 trace ID 添加到 context
		ctx := context.WithValue(r.Context(), traceIDKey{}, traceID)

		// 继续处理请求
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// GetTraceID 从 context 获取 trace ID
func GetTraceID(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	traceID, _ := ctx.Value(traceIDKey{}).(string)
	return traceID
}

// TracedResponseWriter 包装 ResponseWriter 以记录响应状态
type TracedResponseWriter struct {
	http.ResponseWriter
	StatusCode   int
	BytesWritten int64
	// firstWriteAt is when the handler produced its first byte. It is what makes
	// time-to-first-token measurable, which is how the overview separates a slow
	// prefill from slow generation.
	firstWriteAt time.Time
}

// NewTracedResponseWriter 创建新的 TracedResponseWriter
func NewTracedResponseWriter(w http.ResponseWriter) *TracedResponseWriter {
	return &TracedResponseWriter{
		ResponseWriter: w,
		StatusCode:     http.StatusOK,
	}
}

// FirstWriteAt reports when the first response byte was produced, or the zero
// time when the handler never wrote anything.
func (w *TracedResponseWriter) FirstWriteAt() time.Time { return w.firstWriteAt }

// WriteHeader 实现 http.ResponseWriter
func (w *TracedResponseWriter) WriteHeader(code int) {
	if w.firstWriteAt.IsZero() {
		w.firstWriteAt = time.Now()
	}
	w.StatusCode = code
	w.ResponseWriter.WriteHeader(code)
}

// Write 实现 http.ResponseWriter
func (w *TracedResponseWriter) Write(b []byte) (int, error) {
	if w.firstWriteAt.IsZero() {
		w.firstWriteAt = time.Now()
	}
	n, err := w.ResponseWriter.Write(b)
	w.BytesWritten += int64(n)
	return n, err
}

// Flush 实现 http.Flusher
func (w *TracedResponseWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// Hijack 实现 http.Hijacker，保证 WebSocket 升级等场景可用。
func (w *TracedResponseWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	hj, ok := w.ResponseWriter.(http.Hijacker)
	if !ok {
		return nil, nil, fmt.Errorf("response writer does not support hijacking")
	}
	return hj.Hijack()
}

// LoggingMiddleware 记录请求日志，包含 trace ID 和耗时
func LoggingMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		traceID := GetTraceID(r.Context())

		// 包装 ResponseWriter
		wrapped := NewTracedResponseWriter(w)

		// The handler will publish the model it resolved; the hint must exist
		// before the handler runs because the request context is already cloned.
		requestCtx, readModel := RequestModelHint(r.Context())
		r = r.WithContext(requestCtx)

		if logutil.VerboseDiagnosticsEnabled() {
			slog.Debug("Request started",
				"trace_id", traceID,
				"method", r.Method,
				"path", r.URL.Path,
				"remote_addr", r.RemoteAddr,
			)
		}

		// 处理请求
		next.ServeHTTP(wrapped, r)

		// 记录请求完成
		duration := time.Since(start)
		// The operations overview counts every finished request, so this record
		// must happen before the log level decides whether to print a line.
		recordRequestOutcome(r, wrapped.StatusCode, duration, wrapped.FirstWriteAt(), readModel())

		level := slog.LevelDebug
		if wrapped.StatusCode >= 500 {
			level = slog.LevelError
		} else if wrapped.StatusCode >= 400 {
			level = slog.LevelWarn
		} else if !logutil.VerboseDiagnosticsEnabled() && !strings.HasPrefix(r.URL.Path, "/warp/") {
			return
		}
		userAgent := strings.TrimSpace(r.UserAgent())
		if len(userAgent) > 256 {
			userAgent = userAgent[:256]
		}

		slog.Log(r.Context(), level, "Request completed",
			"trace_id", traceID,
			"method", r.Method,
			"path", r.URL.Path,
			"status", wrapped.StatusCode,
			"bytes", wrapped.BytesWritten,
			"duration", duration,
			"remote_ip", ClientIP(r),
			"user_agent", userAgent,
		)
		// One request-finished event per request: a request retried upstream is
		// still one request, and no handler has to remember to record itself.
		// (The observation itself is taken above, before the log-level branch.)
	})
}

// RequestOutcomeRecorder receives one observation per finished HTTP request.
type RequestOutcomeRecorder func(channel, model, statusClass string, durationMS, firstTokenMS int64)

var requestOutcomeRecorder RequestOutcomeRecorder

// SetRequestOutcomeRecorder wires the operations overview into the request path.
func SetRequestOutcomeRecorder(recorder RequestOutcomeRecorder) {
	requestOutcomeRecorder = recorder
}

// requestModelContextKey carries the model a handler resolved for this request.
// The request path cannot know the model before the body is parsed (and the
// middleware must not re-read the body), so the handler publishes it on the
// context and the latency recorder picks it up when the request finishes.
type requestModelContextKey struct{}

// requestModelHintBox is a mutable slot: the handler runs after the middleware
// cloned the request context, so a plain context value would not be visible.
type requestModelHintBox struct {
	model string
}

// WithRequestModel publishes the resolved model for outcome recording. It is the
// only way the per-model figures in the operations overview get a name.
func WithRequestModel(ctx context.Context, model string) context.Context {
	if ctx == nil || strings.TrimSpace(model) == "" {
		return ctx
	}
	if box, ok := ctx.Value(requestModelContextKey{}).(*requestModelHintBox); ok && box != nil {
		box.model = strings.TrimSpace(model)
	}
	return ctx
}

// RequestModelHint returns a context that lets the wrapped handler publish its
// model, plus a reader for the value once the handler has run.
func RequestModelHint(ctx context.Context) (context.Context, func() string) {
	box := &requestModelHintBox{}
	return context.WithValue(ctx, requestModelContextKey{}, box), func() string { return box.model }
}

// ProbeHeader marks a request as a synthetic probe. The probe loop sets it; the
// metric recorder then counts the request apart from real traffic so an injected
// failure cannot distort the user-facing success rate.
const ProbeHeader = "X-Orchids-Probe"

func recordRequestOutcome(r *http.Request, status int, duration time.Duration, firstWrite time.Time, model string) {
	if requestOutcomeRecorder == nil || r == nil {
		return
	}
	durationMS := duration.Milliseconds()
	firstTokenMS := int64(0)
	if !firstWrite.IsZero() {
		firstTokenMS = firstWrite.Sub(requestStartOf(r, duration)).Milliseconds()
	}
	if strings.TrimSpace(r.Header.Get(ProbeHeader)) != "" {
		requestOutcomeRecorder(ProbeChannel, probeModel, httpStatusClass(status), durationMS, firstTokenMS)
		return
	}
	requestOutcomeRecorder(
		requestChannel(r.URL.Path),
		model,
		httpStatusClass(status),
		durationMS,
		firstTokenMS,
	)
}

// Reserved synthetic-traffic labels. They are not routable models, so a client
// cannot use them to move its own traffic out of the real figures.
const (
	// ProbeChannel is the label synthetic probes are recorded under. The probe
	// loop journals the same value, so the overview and the log centre name the
	// same thing.
	ProbeChannel = "probe"
	// HTTPChannel is the label for requests that are not inference traffic.
	HTTPChannel = "http"

	probeModel = "__probe__"
)

// requestStartOf reconstructs the request start from the measured duration. The
// trace middleware owns the clock; keeping the derivation here avoids threading
// a start time through every wrapper.
func requestStartOf(_ *http.Request, duration time.Duration) time.Time {
	return time.Now().Add(-duration)
}

// requestChannel maps a request path to the channel it belongs to. The mapping
// mirrors how handlers journal the same request, so the overview's channel and
// the log centre's channel always agree. Anything that is not inference traffic
// (the admin UI, health checks, a public scanner probing paths) is labelled
// "http" and is kept out of the channel matrix: it is not a provider.
func requestChannel(path string) string {
	trimmed := strings.Trim(path, "/")
	parts := strings.Split(trimmed, "/")
	if len(parts) == 0 {
		return "http"
	}
	switch parts[0] {
	case "warp", "puter", "workbuddy", "grok":
		return parts[0]
	case "v1":
		// The unified /v1 routes are served by the Grok handler. Keep the id
		// stable across both prefixes so one model's figures do not split in two.
		return "grok"
	default:
		return HTTPChannel
	}
}

// requestModelHint extracts a model from the request when it is cheap to do so.
// Request bodies are not re-read here: the log centre shows the model from the
// handler's own audit event, and the overview counts by channel.
func httpStatusClass(status int) string {
	switch {
	case status >= 200 && status < 300:
		return "2xx"
	case status >= 300 && status < 400:
		return "3xx"
	case status >= 400 && status < 500:
		return "4xx"
	case status >= 500 && status < 600:
		return "5xx"
	default:
		return "unknown"
	}
}

// Chain 链式组合多个中间件
func Chain(middlewares ...func(http.Handler) http.Handler) func(http.Handler) http.Handler {
	return func(final http.Handler) http.Handler {
		for i := len(middlewares) - 1; i >= 0; i-- {
			final = middlewares[i](final)
		}
		return final
	}
}
