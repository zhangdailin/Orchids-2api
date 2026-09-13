package debug

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// UpstreamAttempt owns immutable section names, so concurrent or retried calls
// cannot attribute a previous response to the latest request.
type UpstreamAttempt struct {
	capture *Capture
	prefix  string
	started time.Time
}

func BeginUpstream(ctx context.Context, method, url string, headers http.Header, body interface{}) *UpstreamAttempt {
	return beginUpstream(FromContext(ctx), method, url, headers, body)
}
func beginUpstream(c *Capture, method, url string, headers http.Header, body interface{}) *UpstreamAttempt {
	if c == nil {
		return nil
	}
	c.mu.Lock()
	c.attempts++
	id := c.attempts
	c.mu.Unlock()
	a := &UpstreamAttempt{capture: c, prefix: fmt.Sprintf("upstream_%03d_", id), started: time.Now()}
	if raw, ok := body.([]byte); ok {
		if json.Valid(raw) {
			body = json.RawMessage(raw)
		} else {
			body = string(raw)
		}
	}
	safeHeaders := map[string]string{}
	for k, v := range headers {
		safeHeaders[k] = strings.Join(v, ", ")
	}
	a.writeJSON("request.json", map[string]interface{}{"attempt": id, "method": method, "url": url, "headers": safeHeaders, "body": body})
	return a
}
func (a *UpstreamAttempt) writeJSON(suffix string, value interface{}) {
	if a == nil {
		return
	}
	raw, err := json.MarshalIndent(value, "", "  ")
	if err == nil {
		a.capture.Set(a.prefix+suffix, string(raw))
	}
}
func (a *UpstreamAttempt) Response(resp *http.Response, err error) {
	if a == nil {
		return
	}
	result := map[string]interface{}{"elapsed_ms": time.Since(a.started).Milliseconds()}
	if resp != nil {
		result["status"] = resp.StatusCode
		result["content_type"] = resp.Header.Get("Content-Type")
	}
	if err != nil {
		result["error"] = err.Error()
	}
	a.writeJSON("result.json", result)
}
func (a *UpstreamAttempt) Append(data string) {
	if a != nil {
		a.capture.Append(a.prefix+"response.txt", data)
	}
}
func (a *UpstreamAttempt) CaptureBody(body io.ReadCloser) io.ReadCloser {
	if a == nil || body == nil {
		return body
	}
	return &attemptBody{ReadCloser: body, attempt: a}
}

type attemptBody struct {
	io.ReadCloser
	attempt *UpstreamAttempt
}

func (b *attemptBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	b.attempt.Append(string(p[:n]))
	if err != nil && err != io.EOF {
		b.attempt.writeJSON("read_error.json", map[string]interface{}{"error": err.Error()})
	}
	return n, err
}
