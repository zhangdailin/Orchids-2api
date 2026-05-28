package orchids

import "orchids-api/internal/upstream"

type requestState struct {
	textStarted              bool
	reasoningStarted         bool
	blockCount               int
	finishSent               bool
	sawToolCall              bool
	stream                   bool
	responseStarted          bool
	messageStarted           bool
	modelName                string
	finishReason             string
	pendingToolInput         string
	currentToolCall          *orchidsToolCallState
	inputTokens              int64
	outputTokens             int64
	cacheReadInputTokens     int64
	cacheCreationInputTokens int64
	errorMsg                 string
	directSSE                upstream.DirectSSEEmitter
	toolMapper               *ToolMapper
}

type orchidsToolCallState struct {
	id          string
	name        string
	input       string
	hasInput    bool
	inputLength int64
}

func newOrchidsRequestState(req upstream.UpstreamRequest) requestState {
	modelName := normalizeOrchidsAgentModel(req.Model)
	return requestState{
		stream:     req.Stream,
		modelName:  modelName,
		blockCount: 8,
		directSSE:  req.DirectSSE,
		toolMapper: buildClientToolMapper(req.Tools),
	}
}
