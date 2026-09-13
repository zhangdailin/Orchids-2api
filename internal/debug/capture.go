package debug

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"
)

const DiagnosticRetention = 24 * time.Hour
const maxCaptureBytes = 64 << 10
const maxCaptureSections = 16
const maxDiagnosticBundles = 512

type Section struct {
	Name      string `json:"name"`
	Payload   string `json:"payload"`
	Bytes     int    `json:"bytes"`
	Truncated bool   `json:"truncated"`
}
type Bundle struct {
	RequestID  string    `json:"request_id"`
	Sections   []Section `json:"sections"`
	Bytes      int       `json:"bytes"`
	DurationMS int64     `json:"duration_ms"`
	Truncated  bool      `json:"truncated"`
}
type Capture struct {
	mu        sync.Mutex
	sections  map[string]*Section
	started   time.Time
	requestID string
}
type captureKey struct{}

func WithCapture(ctx context.Context, requestID string) (context.Context, *Capture) {
	c := &Capture{sections: map[string]*Section{}, started: time.Now(), requestID: requestID}
	return context.WithValue(ctx, captureKey{}, c), c
}
func FromContext(ctx context.Context) *Capture {
	if ctx == nil {
		return nil
	}
	c, _ := ctx.Value(captureKey{}).(*Capture)
	return c
}
func NewForContext(ctx context.Context, enabled, sse bool) *Logger {
	if c := FromContext(ctx); c != nil {
		return &Logger{enabled: true, sseEnabled: true, capture: c, startTime: time.Now()}
	}
	return New(enabled, sse)
}
func (c *Capture) Append(name, text string) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	s := c.sections[name]
	if s == nil {
		if len(c.sections) >= maxCaptureSections {
			return
		}
		s = &Section{Name: name}
		c.sections[name] = s
	}
	remaining := maxCaptureBytes - len(s.Payload)
	if len(text) > remaining {
		text = text[:remaining]
		s.Truncated = true
	}
	s.Payload += text
}
func (c *Capture) Set(name, text string) {
	if c == nil {
		return
	}
	c.mu.Lock()
	delete(c.sections, name)
	c.mu.Unlock()
	c.Append(name, text)
}

// Preserve prompts and token counts while masking credential fields and bearer
// strings. The same sanitizer applies to JSON, SSE fragments and plain text.
var credentialPattern = regexp.MustCompile(`(?i)("(?:[a-z0-9_-]*(?:authorization|cookie|api[_-]?key|token|password|secret|session|sso|signature|private[_-]?key)[a-z0-9_-]*)"\s*:\s*)"(?:\\.|[^"\\])*(?:"|$)`)
var bearerPattern = regexp.MustCompile(`(?i)\bBearer\s+[A-Za-z0-9._~+/=-]+`)
var urlPasswordPattern = regexp.MustCompile(`([a-zA-Z][a-zA-Z0-9+.-]*://)([^/@:\s]+):([^/@\s]+)@`)
var opaquePattern = regexp.MustCompile(`\b(?:sk-[A-Za-z0-9_-]{8,}|eyJ[A-Za-z0-9_.-]{16,})\b`)

func sanitizeCapture(text string) string {
	text = credentialPattern.ReplaceAllString(text, `${1}"[REDACTED]"`)
	text = bearerPattern.ReplaceAllString(text, "Bearer [REDACTED]")
	text = urlPasswordPattern.ReplaceAllString(text, "${1}${2}:[REDACTED]@")
	return opaquePattern.ReplaceAllString(text, "[REDACTED]")
}
func (c *Capture) Bundle() Bundle {
	c.mu.Lock()
	defer c.mu.Unlock()
	b := Bundle{RequestID: c.requestID, DurationMS: time.Since(c.started).Milliseconds(), Sections: []Section{}}
	for _, s := range c.sections {
		copy := *s
		copy.Payload = sanitizeCapture(strings.ToValidUTF8(copy.Payload, "�"))
		copy.Bytes = len(copy.Payload)
		b.Bytes += copy.Bytes
		b.Truncated = b.Truncated || copy.Truncated
		b.Sections = append(b.Sections, copy)
	}
	sort.Slice(b.Sections, func(i, j int) bool { return b.Sections[i].Name < b.Sections[j].Name })
	return b
}

type DiagnosticStore struct {
	client *redis.Client
	prefix string
}

func NewDiagnosticStore(client *redis.Client, prefix string) *DiagnosticStore {
	return &DiagnosticStore{client: client, prefix: prefix + "diagnostics:"}
}
func (s *DiagnosticStore) key(id string) string {
	hash := sha256.Sum256([]byte(id))
	return s.prefix + hex.EncodeToString(hash[:])
}
func (s *DiagnosticStore) Save(ctx context.Context, b Bundle) error {
	if s == nil || s.client == nil || b.RequestID == "" {
		return nil
	}
	raw, err := json.Marshal(b)
	if err != nil {
		return err
	}
	names := make([]string, 0, len(b.Sections))
	for _, sec := range b.Sections {
		names = append(names, sec.Name)
	}
	index, _ := json.Marshal(map[string]interface{}{"metadata": map[string]interface{}{"sections": names, "bytes": b.Bytes, "truncated": b.Truncated, "retention": "24 小时，最多 512 个请求"}})
	key := s.key(b.RequestID)
	// Bound total storage as well as individual bodies and TTL, atomically across replicas.
	return s.client.Eval(ctx, `redis.call('SET',KEYS[1],ARGV[1],'EX',ARGV[3]); redis.call('SET',KEYS[2],ARGV[2],'EX',ARGV[3]); redis.call('ZADD',KEYS[3],ARGV[4],KEYS[1]); local n=redis.call('ZCARD',KEYS[3])-tonumber(ARGV[5]); if n>0 then local old=redis.call('ZRANGE',KEYS[3],0,n-1); for _,k in ipairs(old) do redis.call('DEL',k,k..':index'); redis.call('ZREM',KEYS[3],k); end end; redis.call('EXPIRE',KEYS[3],ARGV[3]); return 1`, []string{key, key + ":index", s.prefix + "order"}, string(raw), string(index), int(DiagnosticRetention.Seconds()), time.Now().UnixMilli(), maxDiagnosticBundles).Err()
}
func (s *DiagnosticStore) Get(ctx context.Context, id string) (*Bundle, error) {
	if s == nil || s.client == nil {
		return nil, nil
	}
	raw, err := s.client.Get(ctx, s.key(id)).Bytes()
	if err == redis.Nil {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var b Bundle
	if err = json.Unmarshal(raw, &b); err != nil {
		return nil, fmt.Errorf("decode diagnostic bundle: %w", err)
	}
	return &b, nil
}
func (s *DiagnosticStore) Indexes(ctx context.Context, ids []string) (map[string]interface{}, error) {
	out := map[string]interface{}{}
	if s == nil || s.client == nil || len(ids) == 0 {
		return out, nil
	}
	keys := make([]string, len(ids))
	for i, id := range ids {
		keys[i] = s.key(id) + ":index"
	}
	values, err := s.client.MGet(ctx, keys...).Result()
	if err != nil {
		return nil, err
	}
	for i, v := range values {
		if raw, ok := v.(string); ok {
			var index interface{}
			if json.Unmarshal([]byte(raw), &index) == nil {
				out[ids[i]] = index
			}
		}
	}
	return out, nil
}

// CaptureBody tees the decoded upstream body without buffering or changing Close.
func CaptureBody(ctx context.Context, body io.ReadCloser) io.ReadCloser {
	if c := FromContext(ctx); c != nil && body != nil {
		return &capturedBody{ReadCloser: body, capture: c}
	}
	return body
}

type capturedBody struct {
	io.ReadCloser
	capture *Capture
}

func (b *capturedBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	b.capture.Append("4_upstream_sse.jsonl", string(p[:n]))
	return n, err
}
