package warp

import (
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// HTTPStatusError 表示 Warp 上游返回了非预期 HTTP 状态码。
type HTTPStatusError struct {
	Operation       string
	StatusCode      int
	ErrorCode       string
	RetryAfterDelay time.Duration
	Body            string
}

func (e *HTTPStatusError) Error() string {
	if e == nil {
		return "warp request failed"
	}
	op := strings.TrimSpace(e.Operation)
	if op == "" {
		op = "request"
	}
	if e.RetryAfterDelay > 0 {
		return fmt.Sprintf("warp %s failed: HTTP %d%s (retry after %s)", op, e.StatusCode, formatWarpErrorCode(e.ErrorCode), e.RetryAfterDelay.Round(time.Second))
	}
	if body := strings.TrimSpace(e.Body); body != "" {
		if len(body) > 512 {
			body = body[:512] + "...[truncated]"
		}
		return fmt.Sprintf("warp %s failed: HTTP %d%s: %s", op, e.StatusCode, formatWarpErrorCode(e.ErrorCode), body)
	}
	return fmt.Sprintf("warp %s failed: HTTP %d%s", op, e.StatusCode, formatWarpErrorCode(e.ErrorCode))
}

func formatWarpErrorCode(code string) string {
	if code = strings.TrimSpace(code); code != "" {
		return " [" + code + "]"
	}
	return ""
}

func (e *HTTPStatusError) RetryAfter() time.Duration {
	if e == nil {
		return 0
	}
	return e.RetryAfterDelay
}

func HTTPStatusCode(err error) int {
	var statusErr *HTTPStatusError
	if errors.As(err, &statusErr) {
		return statusErr.StatusCode
	}
	return 0
}

func RetryAfter(err error) time.Duration {
	var statusErr *HTTPStatusError
	if errors.As(err, &statusErr) {
		return statusErr.RetryAfterDelay
	}
	return 0
}

func parseRetryAfterHeader(value string, now time.Time) time.Duration {
	v := strings.TrimSpace(value)
	if v == "" {
		return 0
	}
	if seconds, err := strconv.Atoi(v); err == nil {
		if seconds > 0 {
			return time.Duration(seconds) * time.Second
		}
		return 0
	}
	if ts, err := http.ParseTime(v); err == nil {
		d := ts.Sub(now)
		if d > 0 {
			return d
		}
	}
	return 0
}
