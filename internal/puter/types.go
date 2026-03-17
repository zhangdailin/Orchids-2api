package puter

import "github.com/goccy/go-json"

type Request struct {
	Interface string      `json:"interface"`
	Driver    string      `json:"driver"`
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
	Type string `json:"type"`
	Text string `json:"text"`
}

type ErrorResponse struct {
	Success bool          `json:"success"`
	Error   *ErrorPayload `json:"error"`
}

type ErrorPayload struct {
	Iface   string `json:"iface"`
	Code    string `json:"code"`
	Message string `json:"message"`
	Status  int    `json:"status"`
}

type ParsedToolCall struct {
	Name  string          `json:"name"`
	ID    string          `json:"id,omitempty"`
	Input json.RawMessage `json:"input"`
}
