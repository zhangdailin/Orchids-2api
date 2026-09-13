package upstream

import "orchids-api/internal/prompt"

// UpstreamRequest is the shared request representation for upstream providers.
type UpstreamRequest struct {
	Prompt               string
	Model                string
	Messages             []prompt.Message
	System               []prompt.SystemItem
	Tools                []interface{}
	NoTools              bool
	Attempt              int
	ChatSessionID        string
	Workdir              string // Dynamic local workdir override
	WarpCliAgentModel    string
	WarpComputerUseModel string
	WarpToolContexts     map[string]WarpToolContext
}

// WarpToolContext retains the upstream action identity that produced a
// downstream tool call. Warp requires the result to use the original action
// type and tool_call_id on the following request.
type WarpToolContext struct {
	Type  string
	Name  string
	Input string
}

// SSEMessage is the shared streaming event representation for upstream providers.
type SSEMessage struct {
	Type  string                 `json:"type"`
	Event map[string]interface{} `json:"event,omitempty"`
}
