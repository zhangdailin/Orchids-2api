package orchids

import (
	"github.com/goccy/go-json"

	"orchids-api/internal/upstream"
)

type requestState struct {
	textStarted         bool
	reasoningStarted    bool
	blockCount          int
	finishSent          bool
	sawToolCall         bool
	stream              bool
	responseStarted     bool
	messageStarted      bool
	toolCallHasInput    bool
	modelName           string
	finishReason        string
	pendingToolInput    string
	currentToolCall     *orchidsToolCallState
	inputTokens          int64
	outputTokens         int64
	cacheReadInputTokens int64
	errorMsg            string
	directSSE           upstream.DirectSSEEmitter
	toolMapper          *ToolMapper
	emittedToolCallIDs  map[string]struct{}
}

type orchidsToolCallState struct {
	id          string
	name        string
	input       string
	hasInput    bool
	inputLength int64
}

func cloneRawJSON(data []byte) json.RawMessage {
	if len(data) == 0 {
		return nil
	}
	return json.RawMessage(data)
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
