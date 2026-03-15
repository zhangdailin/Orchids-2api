package orchids

import (
	"log/slog"

	"github.com/goccy/go-json"

	"orchids-api/internal/debug"
)

func decodeOrchidsModelEvent(raw json.RawMessage) map[string]interface{} {
	if len(raw) == 0 {
		return nil
	}
	var event map[string]interface{}
	if err := json.Unmarshal(raw, &event); err != nil {
		return nil
	}
	return event
}

func dispatchOrchidsFastEvent(
	rawData []byte,
	envelope orchidsFastEnvelope,
	state *requestState,
	writer *SSEWriter,
	logger *debug.Logger,
	clientTools []interface{},
) (handled bool, shouldBreak bool) {
	switch envelope.Type {
	case EventConnected:
		if logger != nil {
			logger.LogUpstreamSSE(envelope.Type, string(rawData))
		}
		return true, false
	case EventResponseStarted:
		if logger != nil {
			logger.LogUpstreamSSE(envelope.Type, string(rawData))
		}
		if !state.responseStarted {
			state.StartResponse()
		}
		return true, false

	case EventModel:
		var msg orchidsFastModelMessage
		if err := json.Unmarshal(rawData, &msg); err != nil {
			return false, false
		}
		if logger != nil {
			logger.LogUpstreamSSE(msg.Type, string(rawData))
		}
		event := decodeOrchidsModelEvent(msg.Event)
		if len(event) == 0 {
			return true, false
		}

		err := handleModelEvent(state, writer, event, state.toolMapper, clientTools)
		if err != nil {
			slog.Error("error handling model event", "error", err)
		}
		// CodeFreeMax loops until finishReason is set. Since we stream, checking finishSent might not exist,
		// but we can check if finishReason is populated to break.
		shouldBreak := state.finishReason != ""
		if shouldBreak {
			_ = writer.WriteMessageEnd()
		}
		return true, shouldBreak
	case "error":
		var msg orchidsFastErrorMessage
		if err := json.Unmarshal(rawData, &msg); err != nil {
			return false, false
		}
		if logger != nil {
			logger.LogUpstreamSSE(msg.Type, string(rawData))
		}
		errCode, errMsg := extractOrchidsFastErrorPayload(msg)
		if errMsg == "" {
			errMsg = "unknown upstream error"
		}
		slog.Warn("Orchids upstream error event", "code", errCode, "message", errMsg)
		setOrchidsErrorState(state, errCode, errMsg, false)
		emitOrchidsErrorEvent(writer, errCode, errMsg)
		return true, true
	default:
		return false, false
	}
}

func dispatchOrchidsDecodedEvent(
	msg map[string]interface{},
	rawData []byte,
	state *requestState,
	writer *SSEWriter,
	logger *debug.Logger,
	clientTools []interface{},
) bool {
	msgType, _ := msg["type"].(string)
	if logger != nil {
		logger.LogUpstreamSSE(msgType, string(rawData))
	}

	switch msgType {
	case EventConnected:
		return false
	case EventResponseStarted:
		if !state.responseStarted {
			state.StartResponse()
		}
		return false

	case EventModel:
		modelEvent := msg
		if nested, ok := msg["event"].(map[string]interface{}); ok && len(nested) > 0 {
			modelEvent = nested
		}
		_ = handleModelEvent(state, writer, modelEvent, state.toolMapper, clientTools)

		isFinished := state.finishReason != ""
		if isFinished {
			_ = writer.WriteMessageEnd()
		}
		return isFinished
	case "error":
		errCode, errMsg := extractOrchidsErrorPayload(msg)
		if errMsg == "" {
			errMsg = "unknown upstream error"
		}
		slog.Warn("Orchids upstream error event", "code", errCode, "message", errMsg)
		setOrchidsErrorState(state, errCode, errMsg, false)
		emitOrchidsErrorEvent(writer, errCode, errMsg)
		return true

	default:
		return false
	}
}
