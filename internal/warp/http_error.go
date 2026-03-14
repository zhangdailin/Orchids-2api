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
	Operation  string
	StatusCode int
	RetryAfter time.Duration
}

func (e *HTTPStatusError) Error() string {
	if e == nil {
		return "warp request failed"
	}
	op := strings.TrimSpace(e.Operation)
	if op == "" {
		op = "request"
	}
	if e.RetryAfter > 0 {
		return fmt.Sprintf("warp %s failed: HTTP %d (retry after %s)", op, e.StatusCode, e.RetryAfter.Round(time.Second))
	}
	return fmt.Sprintf("warp %s failed: HTTP %d", op, e.StatusCode)
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
		return statusErr.RetryAfter
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
