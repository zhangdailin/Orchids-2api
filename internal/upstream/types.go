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
	// ReasoningEffort is the OpenAI-style effort hint the client asked for
	// (reasoning_effort, or the Anthropic thinking/output_config dialect mapped
	// onto the same coarse levels). Providers whose wire contract exposes an
	// effort or thinking switch forward it; the value stays empty when the
	// client did not state one.
	ReasoningEffort string
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
	// WarpContextWindowLimit is the base model's input-token window as the
	// account's own discovery declared it. Warp reads zero as "use the model's
	// default max", so a caller that already knows the real window states it
	// instead of leaving the upstream to fall back to a smaller default.
	WarpContextWindowLimit uint32
	WarpToolContexts       map[string]WarpToolContext
	// WarpTaskContext is the protobuf-encoded task state returned by Warp on
	// the preceding turn. Warp conversations are stateful but task state is
	// client-owned and must be round-tripped with tool results.
	WarpTaskContext []byte
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
