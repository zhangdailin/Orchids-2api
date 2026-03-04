// Package errors 提供统一的错误处理机制
package errors

import (
	"github.com/goccy/go-json"
	"net/http"
)

// AppError 表示应用层错误，包含错误码、消息和可选的原因
type AppError struct {
	Code       string `json:"code"`
	Message    string `json:"message"`
	HTTPStatus int    `json:"-"`
	Cause      error  `json:"-"`
}

// ToJSON 返回错误的 JSON 表示
func (e *AppError) ToJSON() []byte {
	data, _ := json.Marshal(map[string]interface{}{
		"type": "error",
		"error": map[string]string{
			"type":    e.Code,
			"message": e.Message,
		},
	})
	return data
}

// WriteResponse 将错误写入 HTTP 响应
func (e *AppError) WriteResponse(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(e.HTTPStatus)
	w.Write(e.ToJSON())
}

// 预定义错误码
const (
	CodeInvalidRequest    = "invalid_request_error"
	CodeAuthError         = "authentication_error"
	CodePermissionDenied  = "permission_denied"
	CodeNotFound          = "not_found"
	CodeOverloaded        = "overloaded_error"
	CodeUpstreamError     = "upstream_error"
	CodeInternalError     = "internal_error"
	CodeRateLimitExceeded = "rate_limit_exceeded"
	CodeTimeout           = "timeout_error"
	CodeCircuitOpen       = "circuit_breaker_open"
)

// 预定义错误实例
var (
	// 请求错误
	ErrInvalidRequest = &AppError{
		Code:       CodeInvalidRequest,
		Message:    "请求格式无效",
		HTTPStatus: http.StatusBadRequest,
	}
	ErrRequestBodyTooLarge = &AppError{
		Code:       CodeInvalidRequest,
		Message:    "请求体过大",
		HTTPStatus: http.StatusRequestEntityTooLarge,
	}
	ErrMethodNotAllowed = &AppError{
		Code:       CodeInvalidRequest,
		Message:    "方法不允许",
		HTTPStatus: http.StatusMethodNotAllowed,
	}

	// 认证错误
	ErrUnauthorized = &AppError{
		Code:       CodeAuthError,
		Message:    "认证失败",
		HTTPStatus: http.StatusUnauthorized,
	}
	ErrInvalidToken = &AppError{
		Code:       CodeAuthError,
		Message:    "无效的令牌",
		HTTPStatus: http.StatusUnauthorized,
	}
	ErrSessionExpired = &AppError{
		Code:       CodeAuthError,
		Message:    "会话已过期",
		HTTPStatus: http.StatusUnauthorized,
	}

	// 资源错误
	ErrAccountNotFound = &AppError{
		Code:       CodeNotFound,
		Message:    "账号不存在",
		HTTPStatus: http.StatusNotFound,
	}
	ErrModelNotFound = &AppError{
		Code:       CodeNotFound,
		Message:    "模型不存在",
		HTTPStatus: http.StatusNotFound,
	}
	ErrResourceNotFound = &AppError{
		Code:       CodeNotFound,
		Message:    "资源不存在",
		HTTPStatus: http.StatusNotFound,
	}

	// 服务错误
	ErrNoAvailableAccount = &AppError{
		Code:       CodeOverloaded,
		Message:    "没有可用账号",
		HTTPStatus: http.StatusServiceUnavailable,
	}
	ErrUpstreamUnavailable = &AppError{
		Code:       CodeUpstreamError,
		Message:    "上游服务不可用",
		HTTPStatus: http.StatusBadGateway,
	}
	ErrUpstreamTimeout = &AppError{
		Code:       CodeTimeout,
		Message:    "上游服务响应超时",
		HTTPStatus: http.StatusGatewayTimeout,
	}
	ErrCircuitBreakerOpen = &AppError{
		Code:       CodeCircuitOpen,
		Message:    "服务熔断中，请稍后重试",
		HTTPStatus: http.StatusServiceUnavailable,
	}

	// 限流错误
	ErrRateLimitExceeded = &AppError{
		Code:       CodeRateLimitExceeded,
		Message:    "请求频率超限",
		HTTPStatus: http.StatusTooManyRequests,
	}
	ErrConcurrencyLimitExceeded = &AppError{
		Code:       CodeRateLimitExceeded,
		Message:    "并发请求数超限",
		HTTPStatus: http.StatusTooManyRequests,
	}

	// 内部错误
	ErrInternal = &AppError{
		Code:       CodeInternalError,
		Message:    "内部服务错误",
		HTTPStatus: http.StatusInternalServerError,
	}
	ErrStoreNotConfigured = &AppError{
		Code:       CodeInternalError,
		Message:    "存储未配置",
		HTTPStatus: http.StatusInternalServerError,
	}
)

// New 创建新的应用错误
func New(code, message string, httpStatus int) *AppError {
	return &AppError{
		Code:       code,
		Message:    message,
		HTTPStatus: httpStatus,
	}
}
