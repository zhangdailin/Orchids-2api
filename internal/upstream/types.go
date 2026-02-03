package upstream

import "orchids-api/internal/prompt"

// UpstreamRequest 统一上游请求结构（Warp/Orchids 复用）
type UpstreamRequest struct {
	Prompt        string
	ChatHistory   []interface{}
	Model         string
	Messages      []prompt.Message
	System        []prompt.SystemItem
	Tools         []interface{}
	NoTools       bool
	NoThinking    bool
	ChatSessionID string
	Workdir       string // Dynamic local workdir override
}

// SSEMessage 统一上游 SSE 消息结构（Warp/Orchids 复用）
type SSEMessage struct {
	Type  string                 `json:"type"`
	Event map[string]interface{} `json:"event,omitempty"`
	Raw   map[string]interface{} `json:"-"`
}
