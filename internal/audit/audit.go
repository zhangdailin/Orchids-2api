package audit

import (
	"context"
	"log/slog"
	"strings"
	"time"

	"github.com/goccy/go-json"
	"github.com/redis/go-redis/v9"
)

// Kind separates the three journals the log centre shows. They share one stream
// (so retention is a single knob) and are filtered by this label.
type Kind string

const (
	// KindRequest records an inference request: what the caller asked for, which
	// channel and account served it, and how it ended.
	KindRequest Kind = "request"
	// KindOperation records an administrative change: who changed what, from
	// where, and how it ended.
	KindOperation Kind = "operation"
	// KindSystem records lifecycle and background decisions that explain why an
	// account changed state (refresh verdicts, probe results).
	KindSystem Kind = "system"
)

// Event represents a single audit log entry.
type Event struct {
	Timestamp time.Time `json:"timestamp"`
	// Kind is the journal this event belongs to (request/operation/system).
	Kind      Kind   `json:"kind,omitempty"`
	RequestID string `json:"request_id,omitempty"`
	Action    string `json:"action"`
	// Actor is the operator or credential that caused a management change. It is
	// empty for inference traffic, which is identified by APIKeyID instead.
	Actor     string `json:"actor,omitempty"`
	APIKeyID  int64  `json:"api_key_id,omitempty"`
	AccountID int64  `json:"account_id,omitempty"`
	Model     string `json:"model,omitempty"`
	Channel   string `json:"channel,omitempty"`
	Provider  string `json:"provider,omitempty"`
	Attempt   int    `json:"attempt,omitempty"`
	// UpstreamAttempts counts how many upstream tries one client request took.
	// A retry that succeeded is not a failed user request.
	UpstreamAttempts  int    `json:"upstream_attempts,omitempty"`
	InputTokens       int    `json:"input_tokens,omitempty"`
	OutputTokens      int    `json:"output_tokens,omitempty"`
	CachedInputTokens int    `json:"cached_input_tokens,omitempty"`
	ReasoningTokens   int    `json:"reasoning_tokens,omitempty"`
	ClientIP          string `json:"client_ip,omitempty"`
	UserAgent         string `json:"user_agent,omitempty"`
	Duration          int64  `json:"duration_ms,omitempty"`
	// FirstTokenMS is time-to-first-token. It is kept apart from Duration so a
	// slow prefill is distinguishable from slow generation.
	FirstTokenMS int64 `json:"first_token_ms,omitempty"`
	// Target names the object a management change touched (account id, key id).
	Target  string `json:"target,omitempty"`
	Status  string `json:"status"`
	Error   string `json:"error,omitempty"`
	Details string `json:"details,omitempty"`
	// Redacted lists the request fields that were masked before persisting, so a
	// reader knows a change summary is incomplete by design.
	Redacted []string               `json:"redacted,omitempty"`
	Metadata map[string]interface{} `json:"metadata,omitempty"`
}

// Logger is the audit logging interface.
type Logger interface {
	Log(ctx context.Context, event Event)
}

// --- Redis Stream Implementation ---

// RedisLogger writes audit events to a Redis Stream with async buffering.
type RedisLogger struct {
	client    *redis.Client
	streamKey string
	maxLen    int64
	eventCh   chan Event
	done      chan struct{}
}

// NewRedisLogger creates an audit logger backed by Redis Streams.
func NewRedisLogger(client *redis.Client, prefix string, maxLen int64) *RedisLogger {
	if maxLen <= 0 {
		maxLen = 10000
	}
	l := &RedisLogger{
		client:    client,
		streamKey: prefix + "audit:log",
		maxLen:    maxLen,
		eventCh:   make(chan Event, 256),
		done:      make(chan struct{}),
	}
	go l.writeLoop()
	return l
}

// StreamKey is the Redis key holding every journal entry.
func (l *RedisLogger) StreamKey() string {
	if l == nil {
		return ""
	}
	return l.streamKey
}

func (l *RedisLogger) Log(_ context.Context, event Event) {
	if event.Timestamp.IsZero() {
		event.Timestamp = time.Now()
	}
	if event.Kind == "" {
		event.Kind = KindRequest
	}
	select {
	case l.eventCh <- event:
	default:
		// Channel full, drop event to avoid blocking request path
		slog.Warn("Audit log buffer full, dropping event", "action", event.Action, "kind", event.Kind)
	}
}

func (l *RedisLogger) Close() {
	close(l.eventCh)
	<-l.done
}

func (l *RedisLogger) writeLoop() {
	defer close(l.done)
	for event := range l.eventCh {
		data, err := json.Marshal(event)
		if err != nil {
			continue
		}
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		l.client.XAdd(ctx, &redis.XAddArgs{
			Stream: l.streamKey,
			MaxLen: l.maxLen,
			Approx: true,
			Values: map[string]interface{}{
				"data":   string(data),
				"action": event.Action,
				"status": event.Status,
				"kind":   string(event.Kind),
			},
		}).Err()
		cancel()
	}
}

// --- Nop Implementation ---

// NopLogger discards all audit events.
type NopLogger struct{}

func NewNopLogger() *NopLogger                      { return &NopLogger{} }
func (l *NopLogger) Log(_ context.Context, _ Event) {}

// redactedKeys are request-body fields whose value must never reach the log:
// they are live credentials or secrets. The key stays visible (name and type)
// so a reader still sees WHAT was changed, just not the secret itself.
var redactedKeys = map[string]bool{
	"password": true, "admin_pass": true, "admin_token": true, "secret": true,
	"token": true, "api_key": true, "apikey": true, "authorization": true,
	"client_cookie": true, "session_cookie": true, "refresh_token": true,
	"oauth_access_token": true, "oauth_refresh_token": true, "access_token": true,
	"client_uat": true, "device_id": true, "session_id": true,
	"workbuddy_access_token": true, "workbuddy_refresh_token": true,
	"public_key": true, "app_key": true,
}

const redactedPlaceholder = "<redacted>"

// Redact sanitises a parsed request body for the operation journal. Credential
// fields are replaced, not dropped, so the summary still shows which knob moved.
func Redact(value interface{}) (interface{}, []string) {
	return redactValue(value, "")
}

func redactValue(value interface{}, prefix string) (interface{}, []string) {
	switch typed := value.(type) {
	case map[string]interface{}:
		out := make(map[string]interface{}, len(typed))
		redacted := make([]string, 0, 2)
		for key, item := range typed {
			path := key
			if prefix != "" {
				path = prefix + "." + key
			}
			if redactedKeys[strings.ToLower(strings.TrimSpace(key))] {
				out[key] = redactedPlaceholder
				redacted = append(redacted, path)
				continue
			}
			clean, nested := redactValue(item, path)
			out[key] = clean
			redacted = append(redacted, nested...)
		}
		return out, redacted
	case []interface{}:
		out := make([]interface{}, 0, len(typed))
		redacted := make([]string, 0)
		for index, item := range typed {
			clean, nested := redactValue(item, prefix)
			out = append(out, clean)
			redacted = append(redacted, nested...)
			_ = index
		}
		return out, redacted
	default:
		return value, nil
	}
}

// SummarizeChange renders a redacted, size-bounded change summary for the
// operation journal. It returns the summary and the list of masked fields.
func SummarizeChange(body []byte) (string, []string) {
	trimmed := strings.TrimSpace(string(body))
	if trimmed == "" {
		return "", nil
	}
	var parsed interface{}
	if err := json.Unmarshal(body, &parsed); err != nil {
		// Not JSON (form post, plain text): keep the shape, never the content.
		return "<" + contentTypeOf(trimmed) + " body, " + itoa(len(trimmed)) + " bytes>", nil
	}
	clean, redacted := Redact(parsed)
	encoded, err := json.Marshal(clean)
	if err != nil {
		return "", redacted
	}
	summary := string(encoded)
	const maxSummary = 2048
	if len(summary) > maxSummary {
		summary = summary[:maxSummary] + "…"
	}
	return summary, redacted
}

func contentTypeOf(raw string) string {
	if strings.HasPrefix(raw, "{") || strings.HasPrefix(raw, "[") {
		return "json"
	}
	if strings.Contains(raw, "=") {
		return "form"
	}
	return "text"
}

func itoa(value int) string {
	if value == 0 {
		return "0"
	}
	digits := make([]byte, 0, 10)
	for value > 0 {
		digits = append([]byte{byte('0' + value%10)}, digits...)
		value /= 10
	}
	return string(digits)
}
