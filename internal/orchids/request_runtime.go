package orchids

import (
	"context"

	"github.com/goccy/go-json"

	"orchids-api/internal/debug"
	"orchids-api/internal/upstream"
)

type requestRuntime struct {
	state  requestState
	writer *SSEWriter
}

func newOrchidsRequestRuntime(req upstream.UpstreamRequest, onMessage func(upstream.SSEMessage)) *requestRuntime {
	runtime := &requestRuntime{
		state: newOrchidsRequestState(req),
	}
	runtime.writer = NewSSEWriter(&runtime.state, onMessage)
	if runtime.state.stream && runtime.writer != nil {
		_ = runtime.writer.WriteMessageStart()
	}
	return runtime
}

func (r *requestRuntime) finalize(ctx context.Context) error {
	if r == nil {
		return nil
	}
	return finalizeOrchidsTransport(ctx, &r.state, r.writer)
}

func (r *requestRuntime) handleRawMessage(
	rawData []byte,
	logger *debug.Logger,
	clientTools []interface{},
) (handled bool, shouldBreak bool) {
	if r == nil {
		return false, false
	}
	var envelope orchidsFastEnvelope
	if err := json.Unmarshal(rawData, &envelope); err != nil {
		return false, false
	}
	return dispatchOrchidsFastEvent(rawData, envelope, &r.state, r.writer, logger, clientTools)
}

func (r *requestRuntime) handleDecodedMessage(
	msg map[string]interface{},
	rawData []byte,
	logger *debug.Logger,
	clientTools []interface{},
) bool {
	if r == nil {
		return false
	}
	return dispatchOrchidsDecodedEvent(msg, rawData, &r.state, r.writer, logger, clientTools)
}
