package upstream

import (
	"github.com/goccy/go-json"

	"orchids-api/internal/prompt"
)

type DirectSSEEmitter interface {
	WriteDirectSSE(event string, payload []byte, final bool)
	ObserveTextDelta(text string)
	ObserveThinkingDelta(text string)
	ObserveToolCall(name, input string)
	ObserveUsage(inputTokens, outputTokens int)
	ObserveStopReason(stopReason string)
	FinishDirectSSE(stopReason string)
}

// UpstreamRequest 统一上游请求结构（Warp/Orchids 复用）
type UpstreamRequest struct {
	Prompt               string
	ChatHistory          []interface{}
	Model                string
	Stream               bool
	Messages             []prompt.Message
	System               []prompt.SystemItem
	Tools                []interface{}
	NoTools              bool
	NoThinking           bool
	TraceID              string
	Attempt              int
	ChatSessionID        string
	Workdir              string // Dynamic local workdir override
	ProjectID            string
	IsFirstPrompt        bool
	WarpCliAgentModel    string
	WarpComputerUseModel string
	DirectSSE            DirectSSEEmitter
}

// SSEMessage 统一上游 SSE 消息结构（Warp/Orchids 复用）
type SSEMessage struct {
	Type    string                 `json:"type"`
	Event   map[string]interface{} `json:"event,omitempty"`
	Raw     map[string]interface{} `json:"-"`
	RawJSON json.RawMessage        `json:"-"`
}
