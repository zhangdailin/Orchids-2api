package puter

import (
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// HTTPError preserves response metadata needed by the generic retry policy.
type HTTPError struct {
	StatusCode int
	Header     http.Header
	Body       string
}

func (e *HTTPError) Error() string {
	if e == nil {
		return "puter API error"
	}
	return fmt.Sprintf("puter API error: status=%d, body=%s", e.StatusCode, strings.TrimSpace(e.Body))
}

// RetryAfter implements the handler's typed retry hint contract.
func (e *HTTPError) RetryAfter() time.Duration {
	if e == nil || e.Header == nil {
		return 0
	}
	value := strings.TrimSpace(e.Header.Get("Retry-After"))
	if value == "" {
		return 0
	}
	if seconds, err := strconv.Atoi(value); err == nil && seconds > 0 {
		return time.Duration(seconds) * time.Second
	}
	if when, err := http.ParseTime(value); err == nil {
		if delay := time.Until(when); delay > 0 {
			return delay
		}
	}
	return 0
}
