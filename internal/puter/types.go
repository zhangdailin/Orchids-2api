package puter

import (
	"strings"

	"github.com/goccy/go-json"
)

type Request struct {
	Interface string      `json:"interface"`
	Service   string      `json:"service"`
	TestMode  bool        `json:"test_mode"`
	Method    string      `json:"method"`
	Args      RequestArgs `json:"args"`
	AuthToken string      `json:"auth_token"`
}

type RequestArgs struct {
	Messages []Message `json:"messages"`
	Model    string    `json:"model"`
	Stream   bool      `json:"stream"`
}

type Message struct {
	Role    string `json:"role,omitempty"`
	Content string `json:"content"`
}

type StreamChunk struct {
	Type    string `json:"type"`
	Text    string `json:"text"`
	Delta   string `json:"delta"`
	Message string `json:"message"`
}

type ErrorResponse struct {
	Success *bool      `json:"success,omitempty"`
	Error   ErrorField `json:"error,omitempty"`
}

type ErrorPayload struct {
	Iface   string `json:"iface"`
	Code    string `json:"code"`
	Message string `json:"message"`
	Status  int    `json:"status"`
}

type ErrorField struct {
	Payload *ErrorPayload
	Message string
	raw     json.RawMessage
}

func (e *ErrorField) UnmarshalJSON(data []byte) error {
	e.Payload = nil
	e.Message = ""
	e.raw = append(e.raw[:0], data...)

	trimmed := strings.TrimSpace(string(data))
	if trimmed == "" || trimmed == "null" {
		return nil
	}

	var msg string
	if err := json.Unmarshal(data, &msg); err == nil {
		e.Message = strings.TrimSpace(msg)
		return nil
	}

	var payload ErrorPayload
	if err := json.Unmarshal(data, &payload); err != nil {
		return err
	}
	e.Payload = &payload
	return nil
}

func (e ErrorField) Present() bool {
	trimmed := strings.TrimSpace(string(e.raw))
	return trimmed != "" && trimmed != "null"
}

func (e ErrorField) AsPayload() *ErrorPayload {
	if e.Payload != nil {
		return e.Payload
	}
	if strings.TrimSpace(e.Message) == "" {
		return nil
	}
	return &ErrorPayload{Message: strings.TrimSpace(e.Message)}
}

type ParsedToolCall struct {
	Name  string          `json:"name"`
	ID    string          `json:"id,omitempty"`
	Input json.RawMessage `json:"input"`
}
