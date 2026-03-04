package middleware

import (
	"bufio"
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gorilla/websocket"
)

type hijackableRecorder struct {
	*httptest.ResponseRecorder
	hijackErr error
	hijacked  bool
}

func (h *hijackableRecorder) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	h.hijacked = true
	return nil, nil, h.hijackErr
}

func TestGenerateTraceID(t *testing.T) {
	t.Run("generates unique IDs", func(t *testing.T) {
		ids := make(map[string]bool)
		for i := 0; i < 1000; i++ {
			id := GenerateTraceID()
			if ids[id] {
				t.Errorf("duplicate trace ID generated: %s", id)
			}
			ids[id] = true
		}
	})

	t.Run("generates valid hex string", func(t *testing.T) {
		id := GenerateTraceID()
		if len(id) != 32 { // 16 bytes = 32 hex chars
			t.Errorf("trace ID length = %d, want 32", len(id))
		}
		for _, c := range id {
			if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')) {
				t.Errorf("invalid character in trace ID: %c", c)
			}
		}
	})
}

func TestTraceMiddleware(t *testing.T) {
	t.Run("generates trace ID when not provided", func(t *testing.T) {
		handler := TraceMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			traceID := GetTraceID(r.Context())
			if traceID == "" {
				t.Error("trace ID not found in context")
			}
			w.WriteHeader(http.StatusOK)
		}))

		req := httptest.NewRequest("GET", "/", nil)
		w := httptest.NewRecorder()

		handler.ServeHTTP(w, req)

		if w.Header().Get(TraceIDHeader) == "" {
			t.Error("trace ID not set in response header")
		}
	})

	t.Run("uses provided trace ID", func(t *testing.T) {
		expectedID := "test-trace-id-123"
		handler := TraceMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			traceID := GetTraceID(r.Context())
			if traceID != expectedID {
				t.Errorf("trace ID = %q, want %q", traceID, expectedID)
			}
		}))

		req := httptest.NewRequest("GET", "/", nil)
		req.Header.Set(TraceIDHeader, expectedID)
		w := httptest.NewRecorder()

		handler.ServeHTTP(w, req)

		if w.Header().Get(TraceIDHeader) != expectedID {
			t.Errorf("response trace ID = %q, want %q", w.Header().Get(TraceIDHeader), expectedID)
		}
	})

	t.Run("uses X-Request-ID as fallback", func(t *testing.T) {
		expectedID := "request-id-456"
		handler := TraceMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			traceID := GetTraceID(r.Context())
			if traceID != expectedID {
				t.Errorf("trace ID = %q, want %q", traceID, expectedID)
			}
		}))

		req := httptest.NewRequest("GET", "/", nil)
		req.Header.Set(RequestIDHeader, expectedID)
		w := httptest.NewRecorder()

		handler.ServeHTTP(w, req)
	})
}

func TestGetTraceID(t *testing.T) {
	t.Run("returns empty for context without trace ID", func(t *testing.T) {
		ctx := context.Background()
		if got := GetTraceID(ctx); got != "" {
			t.Errorf("GetTraceID(ctx) = %q, want empty", got)
		}
	})

	t.Run("returns trace ID from context", func(t *testing.T) {
		expected := "test-trace-id"
		ctx := context.WithValue(context.Background(), traceIDKey{}, expected)
		if got := GetTraceID(ctx); got != expected {
			t.Errorf("GetTraceID(ctx) = %q, want %q", got, expected)
		}
	})
}

func TestTracedResponseWriter(t *testing.T) {
	t.Run("tracks status code", func(t *testing.T) {
		w := httptest.NewRecorder()
		traced := NewTracedResponseWriter(w)

		traced.WriteHeader(http.StatusNotFound)

		if traced.StatusCode != http.StatusNotFound {
			t.Errorf("StatusCode = %d, want %d", traced.StatusCode, http.StatusNotFound)
		}
	})

	t.Run("default status is 200", func(t *testing.T) {
		w := httptest.NewRecorder()
		traced := NewTracedResponseWriter(w)

		if traced.StatusCode != http.StatusOK {
			t.Errorf("default StatusCode = %d, want %d", traced.StatusCode, http.StatusOK)
		}
	})

	t.Run("tracks bytes written", func(t *testing.T) {
		w := httptest.NewRecorder()
		traced := NewTracedResponseWriter(w)

		traced.Write([]byte("hello"))
		traced.Write([]byte(" world"))

		if traced.BytesWritten != 11 {
			t.Errorf("BytesWritten = %d, want 11", traced.BytesWritten)
		}
	})

	t.Run("flush works", func(t *testing.T) {
		w := httptest.NewRecorder()
		traced := NewTracedResponseWriter(w)

		// Should not panic
		traced.Flush()
	})

	t.Run("hijack delegates to underlying writer", func(t *testing.T) {
		w := &hijackableRecorder{ResponseRecorder: httptest.NewRecorder()}
		traced := NewTracedResponseWriter(w)

		_, _, err := traced.Hijack()
		if err != nil {
			t.Fatalf("Hijack() error = %v, want nil", err)
		}
		if !w.hijacked {
			t.Fatal("Hijack() should delegate to underlying writer")
		}
	})

	t.Run("hijack fails when underlying writer does not support it", func(t *testing.T) {
		w := httptest.NewRecorder()
		traced := NewTracedResponseWriter(w)

		_, _, err := traced.Hijack()
		if err == nil {
			t.Fatal("Hijack() should fail when underlying writer is not hijackable")
		}
		if !strings.Contains(err.Error(), "does not support hijacking") {
			t.Fatalf("Hijack() unexpected error: %v", err)
		}
	})
}

func TestLoggingMiddleware(t *testing.T) {
	handler := LoggingMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("OK"))
	}))

	// Add trace middleware first
	handler = TraceMiddleware(handler)

	req := httptest.NewRequest("GET", "/test", nil)
	w := httptest.NewRecorder()

	handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("status code = %d, want %d", w.Code, http.StatusOK)
	}
}

func TestLoggingMiddleware_WebSocketUpgrade(t *testing.T) {
	upgrader := websocket.Upgrader{
		CheckOrigin: func(r *http.Request) bool { return true },
	}

	handler := TraceMiddleware(LoggingMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Errorf("Upgrade() error = %v", err)
			return
		}
		defer conn.Close()
		_ = conn.WriteMessage(websocket.TextMessage, []byte("ok"))
	})))

	server := httptest.NewServer(handler)
	defer server.Close()

	wsURL := "ws" + strings.TrimPrefix(server.URL, "http")
	conn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatalf("Dial() error = %v", err)
	}
	defer conn.Close()

	_, msg, err := conn.ReadMessage()
	if err != nil {
		t.Fatalf("ReadMessage() error = %v", err)
	}
	if string(msg) != "ok" {
		t.Fatalf("message = %q, want %q", string(msg), "ok")
	}
}

func TestChain(t *testing.T) {
	var order []string

	m1 := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			order = append(order, "m1-before")
			next.ServeHTTP(w, r)
			order = append(order, "m1-after")
		})
	}

	m2 := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			order = append(order, "m2-before")
			next.ServeHTTP(w, r)
			order = append(order, "m2-after")
		})
	}

	final := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		order = append(order, "handler")
	})

	chained := Chain(m1, m2)(final)

	req := httptest.NewRequest("GET", "/", nil)
	w := httptest.NewRecorder()

	chained.ServeHTTP(w, req)

	expected := []string{"m1-before", "m2-before", "handler", "m2-after", "m1-after"}
	if len(order) != len(expected) {
		t.Fatalf("order length = %d, want %d", len(order), len(expected))
	}
	for i, v := range expected {
		if order[i] != v {
			t.Errorf("order[%d] = %q, want %q", i, order[i], v)
		}
	}
}
