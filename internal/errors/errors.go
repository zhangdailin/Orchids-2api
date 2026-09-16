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

// New 创建新的应用错误
func New(code, message string, httpStatus int) *AppError {
	return &AppError{
		Code:       code,
		Message:    message,
		HTTPStatus: httpStatus,
	}
}
