package upstream

import "orchids-api/internal/prompt"

// UpstreamRequest is the shared request representation for upstream providers.
type UpstreamRequest struct {
	Prompt            string
	Model             string
	Messages          []prompt.Message
	System            []prompt.SystemItem
	Tools             []interface{}
	ToolChoice        interface{}
	ParallelToolCalls *bool
	NoTools           bool
	Attempt           int
	// RequestID identifies one downstream request across provider retries and
	// account switches. Providers that expose an upstream request-correlation
	// header can reuse it instead of making every retry look like a new turn.
	RequestID string
	// ConversationID is the explicit client conversation identifier. It stays
	// empty when the client did not provide one; providers must not substitute a
	// synthetic routing/session key for it.
	ConversationID string
	// TraceID preserves a client-provided trace independently from RequestID.
	// WorkBuddy uses RequestID to aggregate one turn and TraceID for diagnostics.
	TraceID              string
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
