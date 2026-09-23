package puter

import (
	"strings"

	"github.com/goccy/go-json"
)

type request struct {
	Interface string      `json:"interface"`
	Service   string      `json:"service"`
	TestMode  bool        `json:"test_mode"`
	Method    string      `json:"method"`
	Args      requestArgs `json:"args"`
	AuthToken string      `json:"auth_token"`
}

type requestArgs struct {
	Messages          []message     `json:"messages"`
	Model             string        `json:"model"`
	Stream            bool          `json:"stream"`
	Tools             []interface{} `json:"tools,omitempty"`
	ToolChoice        interface{}   `json:"tool_choice,omitempty"`
	ParallelToolCalls *bool         `json:"parallel_tool_calls,omitempty"`
	// ReasoningEffort carries the OpenAI-style effort hint for providers whose
	// gateway reads it (DeepSeek reasoning mode). Empty when the client did not
	// state one.
	ReasoningEffort string `json:"reasoning_effort,omitempty"`
	// EnableThinking is the explicit reasoning toggle. The pointer is nil when
	// the client did not state a preference, so untouched requests stay
	// byte-identical to the previous wire shape.
	EnableThinking *bool `json:"enable_thinking,omitempty"`
}

type message struct {
	Role             string     `json:"role"`
	Content          string     `json:"content"`
	ReasoningContent string     `json:"reasoning_content,omitempty"`
	ToolCalls        []toolCall `json:"tool_calls,omitempty"`
	ToolCallID       string     `json:"tool_call_id,omitempty"`
}

type toolCall struct {
	ID       string           `json:"id"`
	Type     string           `json:"type"`
	Function toolCallFunction `json:"function"`
}

type toolCallFunction struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

type streamChunk struct {
	Type      string                 `json:"type"`
	Text      string                 `json:"text,omitempty"`
	Reasoning string                 `json:"reasoning,omitempty"`
	ID        string                 `json:"id,omitempty"`
	Name      string                 `json:"name,omitempty"`
	Input     json.RawMessage        `json:"input,omitempty"`
	Usage     map[string]interface{} `json:"usage,omitempty"`
	Message   string                 `json:"message,omitempty"`
	Error     errorField             `json:"error,omitempty"`
}

type MonthlyUsage struct {
	AllowanceInfo UsageAllowanceInfo `json:"allowanceInfo"`
}

type UsageAllowanceInfo struct {
	Remaining           float64 `json:"remaining"`
	MonthUsageAllowance float64 `json:"monthUsageAllowance"`
}

type errorPayload struct {
	Iface   string `json:"iface"`
	Code    string `json:"code"`
	Message string `json:"message"`
	Status  int    `json:"status"`
}

type errorField struct {
	Payload *errorPayload
	Message string
}

func (e *errorField) UnmarshalJSON(data []byte) error {
	e.Payload = nil
	e.Message = ""

	trimmed := strings.TrimSpace(string(data))
	if trimmed == "" || trimmed == "null" {
		return nil
	}

	var msg string
	if err := json.Unmarshal(data, &msg); err == nil {
		e.Message = strings.TrimSpace(msg)
		return nil
	}

	var payload errorPayload
	if err := json.Unmarshal(data, &payload); err != nil {
		return err
	}
	e.Payload = &payload
	return nil
}

func (e errorField) Present() bool {
	return e.Payload != nil || strings.TrimSpace(e.Message) != ""
}

func (e errorField) AsPayload() *errorPayload {
	if e.Payload != nil {
		return e.Payload
	}
	if strings.TrimSpace(e.Message) == "" {
		return nil
	}
	return &errorPayload{Message: strings.TrimSpace(e.Message)}
}
