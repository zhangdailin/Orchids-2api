// Package middleware 提供 HTTP 中间件
package middleware

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"log/slog"
	"net/http"
	"time"
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

// TraceFunc 是 TraceMiddleware 的 HandlerFunc 版本
func TraceFunc(next http.HandlerFunc) http.HandlerFunc {
	return TraceMiddleware(next).ServeHTTP
}

// GetTraceID 从 context 获取 trace ID
func GetTraceID(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	if traceID, ok := ctx.Value(traceIDKey{}).(string); ok {
		return traceID
	}
	return ""
}

// WithTraceID 创建带有 trace ID 的新 context
func WithTraceID(ctx context.Context, traceID string) context.Context {
	return context.WithValue(ctx, traceIDKey{}, traceID)
}

// LogWithTrace 返回带有 trace ID 的 logger
func LogWithTrace(ctx context.Context) *slog.Logger {
	traceID := GetTraceID(ctx)
	if traceID == "" {
		return slog.Default()
	}
	return slog.Default().With("trace_id", traceID)
}

// TracedResponseWriter 包装 ResponseWriter 以记录响应状态
type TracedResponseWriter struct {
	http.ResponseWriter
	StatusCode   int
	BytesWritten int64
}

// NewTracedResponseWriter 创建新的 TracedResponseWriter
func NewTracedResponseWriter(w http.ResponseWriter) *TracedResponseWriter {
	return &TracedResponseWriter{
		ResponseWriter: w,
		StatusCode:     http.StatusOK,
	}
}

// WriteHeader 实现 http.ResponseWriter
func (w *TracedResponseWriter) WriteHeader(code int) {
	w.StatusCode = code
	w.ResponseWriter.WriteHeader(code)
}

// Write 实现 http.ResponseWriter
func (w *TracedResponseWriter) Write(b []byte) (int, error) {
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

// LoggingMiddleware 记录请求日志，包含 trace ID 和耗时
func LoggingMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		traceID := GetTraceID(r.Context())

		// 包装 ResponseWriter
		wrapped := NewTracedResponseWriter(w)

		// 记录请求开始
		slog.Debug("Request started",
			"trace_id", traceID,
			"method", r.Method,
			"path", r.URL.Path,
			"remote_addr", r.RemoteAddr,
		)

		// 处理请求
		next.ServeHTTP(wrapped, r)

		// 记录请求完成
		duration := time.Since(start)
		level := slog.LevelInfo
		if wrapped.StatusCode >= 500 {
			level = slog.LevelError
		} else if wrapped.StatusCode >= 400 {
			level = slog.LevelWarn
		}

		slog.Log(r.Context(), level, "Request completed",
			"trace_id", traceID,
			"method", r.Method,
			"path", r.URL.Path,
			"status", wrapped.StatusCode,
			"bytes", wrapped.BytesWritten,
			"duration", duration,
		)
	})
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

// ChainFunc 链式组合多个中间件（HandlerFunc 版本）
func ChainFunc(middlewares ...func(http.HandlerFunc) http.HandlerFunc) func(http.HandlerFunc) http.HandlerFunc {
	return func(final http.HandlerFunc) http.HandlerFunc {
		for i := len(middlewares) - 1; i >= 0; i-- {
			final = middlewares[i](final)
		}
		return final
	}
}
