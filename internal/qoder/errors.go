package qoder

// 上游错误四分类（参照 qoder2api 的 internal/bridge/errors.go）
//
// 分类语义：
//   - 内容审核拒绝：上游安全审核命中（DataInspectionFailed 一类）。客户端状态
//     统一 400，永不重试 —— 重试只会让同一份被拒内容再打一遍上游，并顺带把
//     账号池里每个账号都标成限流。
//   - 瞬时可重试：418（上游把 provider 故障包装成 418）、5xx、4xx 带
//     provider_error，以及传输层 TLS/EOF/reset/超时。同账号退避重试。
//   - 客户端参数错：invalid_parameter_error 一类。重试无效，快速失败。
//   - 其余上游错误：按原样上抛。
//
// 401/403/429 各有专门路径（刷新凭证 / 额度 / 排队窗口），不归入瞬时重试类：
// 把它们当成抖动重试，等于在凭证已经失效时把整池账号逐个刷一遍。

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strings"
	"syscall"
	"time"
)

// ErrEmptyStream means the upstream opened a 200 stream and closed it without
// delivering a single usable delta or usage frame.
//
// It is deliberately not retryable: an empty stream is the upstream saying it
// has nothing for this request, and replaying it only doubles the load that
// produced the empty answer in the first place. Reporting it as a truncated
// success would hand the caller an empty assistant message.
var ErrEmptyStream = errors.New("empty upstream stream")

// ErrContentPolicy means the upstream's safety review refused the input.
var ErrContentPolicy = errors.New("upstream content policy rejected the request")

// ErrTransientUpstream marks a failure the same account can ride out with a
// short backoff.
var ErrTransientUpstream = errors.New("upstream transient error")

// ErrClientFault means the upstream rejected the request on its own merits. A
// replay is identical, so it must fail fast rather than take a healthy account
// out of rotation.
var ErrClientFault = errors.New("upstream rejected the request parameters")

// TransientMaxRetries is how many extra attempts a transient failure gets on
// the account that already holds the request, before the failure is handed to
// the shared handler's account-switching loop.
//
// Keeping it low matters: the shared loop multiplies every attempt by its own
// budget, and switching accounts on a provider-side hiccup takes a healthy
// account out of rotation for a problem that was never its own.
const TransientMaxRetries = 2

// TransientBackoff is the wait before retry number attempt (counted from 1).
var TransientBackoff = func(attempt int) time.Duration {
	if attempt < 1 {
		attempt = 1
	}
	return time.Duration(attempt) * time.Second
}

// transientStatusCodes are the statuses the upstream uses for its own provider
// faults: 418 is how it reports them, and 5xx is the plain form.
var transientStatusCodes = map[int]bool{418: true, 500: true, 502: true, 503: true, 504: true}

// clientFaultMarkers are request-side problems: retrying changes nothing.
var clientFaultMarkers = []string{
	"invalid_parameter_error",
	"invalid_request_error",
	"authentication_error",
	"permission_error",
	`"Range of `,
}

// contentPolicyMarkers are the upstream safety review's rejections.
var contentPolicyMarkers = []string{
	"DataInspectionFailed",
	"inappropriate content",
	"input text data may contain",
	"ContentFilter",
	"SensitiveContent",
}

// nonTransientTransportMarkers are configuration and certificate faults. A TLS
// handshake timeout is transient; an untrusted certificate is not.
var nonTransientTransportMarkers = []string{
	"unsupported protocol scheme",
	"invalid url",
	"unknown scheme",
	"x509:",
}

func containsAnyMarker(detail string, markers []string) bool {
	for _, marker := range markers {
		if strings.Contains(detail, marker) {
			return true
		}
	}
	return false
}

// IsContentPolicy reports whether the upstream refused the input on safety
// grounds rather than because of capacity or credentials.
func IsContentPolicy(detail string) bool {
	return containsAnyMarker(detail, contentPolicyMarkers)
}

// IsClientFault reports a request the upstream rejected on its own merits.
func IsClientFault(detail string) bool {
	return containsAnyMarker(detail, clientFaultMarkers)
}

// IsTransientUpstreamStatus reports whether an HTTP failure is a provider-side
// hiccup the same account can wait out.
//
// 401/403/429 are excluded on purpose: they are a credential, a permission and
// a capacity window respectively, and each already has its own path.
func IsTransientUpstreamStatus(status int, detail string) bool {
	if status == 401 || status == 403 || status == 429 {
		return false
	}
	if IsContentPolicy(detail) || IsClientFault(detail) {
		return false
	}
	if transientStatusCodes[status] || status >= 500 {
		return true
	}
	// A 4xx that names a provider fault is the upstream's own problem wearing a
	// client status.
	return status >= 400 && status < 500 && strings.Contains(detail, "provider_error")
}

// IsTransientTransport reports a connection-level hiccup. A caller-side
// cancellation is not one, and neither is a configuration or certificate fault.
func IsTransientTransport(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	detail := err.Error()
	if containsAnyMarker(strings.ToLower(detail), nonTransientTransportMarkers) {
		return false
	}
	var netErr net.Error
	if errors.As(err, &netErr) {
		// A timeout is the transient case; a permanent address or protocol
		// problem is not.
		return netErr.Timeout() || isTransientOpError(err)
	}
	if isTransientOpError(err) {
		return true
	}
	lower := strings.ToLower(detail)
	return strings.Contains(lower, "unexpected eof") ||
		strings.Contains(lower, "connection reset") ||
		strings.Contains(lower, "connection refused") ||
		strings.Contains(lower, "broken pipe") ||
		strings.Contains(lower, "tls:") ||
		strings.Contains(lower, "eof")
}

// isTransientOpError recognises the OS-level reset/abort errors net.Error does
// not always classify as timeouts.
func isTransientOpError(err error) bool {
	var opErr *net.OpError
	if errors.As(err, &opErr) {
		if opErr.Timeout() {
			return true
		}
		return isTransientSyscall(opErr.Err)
	}
	return isTransientSyscall(errors.Unwrap(err))
}

func isTransientSyscall(err error) bool {
	var errno syscall.Errno
	if errors.As(err, &errno) {
		switch errno {
		case syscall.ECONNRESET, syscall.ECONNABORTED, syscall.ECONNREFUSED,
			syscall.EPIPE, syscall.ETIMEDOUT, syscall.EHOSTUNREACH, syscall.ENETUNREACH:
			return true
		}
		return false
	}
	// A wrapped syscall error on a platform without the constant still reads as
	// a reset when it names one.
	if err == nil {
		return false
	}
	lower := strings.ToLower(err.Error())
	return strings.Contains(lower, "connection reset") ||
		strings.Contains(lower, "connection refused") ||
		strings.Contains(lower, "broken pipe") ||
		strings.Contains(lower, "timed out")
}

// contentPolicyError wraps a refusal in the sentinel the classifier and the
// account policy both recognise.
func contentPolicyError(detail string) error {
	detail = strings.TrimSpace(detail)
	if detail == "" {
		return fmt.Errorf("%w", ErrContentPolicy)
	}
	return fmt.Errorf("%w: %s", ErrContentPolicy, detail)
}

// transientError wraps a provider-side hiccup in its sentinel.
func transientError(detail string) error {
	detail = strings.TrimSpace(detail)
	if detail == "" {
		return fmt.Errorf("%w", ErrTransientUpstream)
	}
	return fmt.Errorf("%w: %s", ErrTransientUpstream, detail)
}

// isContentPolicyError reports whether an error chain carries the refusal.
func isContentPolicyError(err error) bool {
	return errors.Is(err, ErrContentPolicy)
}

// isTransientError reports whether an error chain asks for a same-account
// backoff retry.
func isTransientError(err error) bool {
	return errors.Is(err, ErrTransientUpstream)
}

// isEmptyStreamError reports whether an error chain is the empty-stream verdict.
func isEmptyStreamError(err error) bool {
	return errors.Is(err, ErrEmptyStream)
}
