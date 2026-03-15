package orchids

import (
	"log/slog"

	"github.com/goccy/go-json"

	"orchids-api/internal/debug"
	"orchids-api/internal/upstream"
)

func dispatchOrchidsFastEvent(
	rawData []byte,
	envelope orchidsFastEnvelope,
	state *requestState,
	onMessage func(upstream.SSEMessage),
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
		if !markOrchidsResponseStarted(state) {
			return true, false
		}
		return true, false
	case EventReasoningCompleted:
		if logger != nil {
			logger.LogUpstreamSSE(envelope.Type, string(rawData))
		}
		markOrchidsCodingAgent(state)
		if endOrchidsReasoning(state) {
			onMessage(upstream.SSEMessage{Type: "model", Event: map[string]interface{}{"type": "reasoning-end", "id": "0"}})
		}
		return true, false
	case EventCodingAgentTokens:
		var msg orchidsFastTokensMessage
		if err := json.Unmarshal(rawData, &msg); err != nil {
			return false, false
		}
		if logger != nil {
			logger.LogUpstreamSSE(msg.Type, string(rawData))
		}
		emitOrchidsUsageEvent(msg.Data, onMessage)
		return true, false
	case EventReasoningChunk, EventOutputTextDelta, EventResponseChunk:
		var msg orchidsFastTextMessage
		if err := json.Unmarshal(rawData, &msg); err != nil {
			return false, false
		}
		text := extractOrchidsFastText(msg)
		if logger != nil {
			logger.LogUpstreamSSE(msg.Type, string(rawData))
		}
		if text == "" {
			return true, false
		}
		markOrchidsCodingAgent(state)
		if msg.Type == EventReasoningChunk {
			emitOrchidsReasoningDelta(state, onMessage, text)
			return true, false
		}
		emitOrchidsTextDelta(state, onMessage, msg.Type, text)
		return true, false
	case EventResponseDone:
		var msg orchidsFastResponseDone
		if err := json.Unmarshal(rawData, &msg); err != nil {
			return false, false
		}
		if logger != nil {
			logger.LogUpstreamSSE(msg.Type, string(rawData))
		}
		return true, handleOrchidsFastCompletion(msg, state, onMessage, clientTools)
	case EventCodingAgentEnd, EventComplete:
		if logger != nil {
			logger.LogUpstreamSSE(envelope.Type, string(rawData))
		}
		emitOrchidsCompletionTail(state, onMessage)
		return true, true
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
		return true, emitOrchidsModelEvent(event, state, onMessage, clientTools, event)
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
		emitOrchidsErrorEvent(onMessage, errCode, errMsg)
		return true, true
	default:
		return false, false
	}
}

func dispatchOrchidsDecodedEvent(
	msg map[string]interface{},
	rawData []byte,
	state *requestState,
	onMessage func(upstream.SSEMessage),
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
		if !markOrchidsResponseStarted(state) {
			return false
		}
		return false
	case EventCodingAgentStart, EventCodingAgentInit:
		if state.suppressStarts {
			return false
		}
		markOrchidsCodingAgent(state)
		onMessage(upstream.SSEMessage{Type: msgType, Event: msg, Raw: msg, RawJSON: cloneRawJSON(rawData)})
		return false
	case EventCodingAgentTokens:
		emitOrchidsTokensFromMessage(msg, onMessage)
		return false
	case EventCreditsExhausted:
		errCode, errMsg := extractOrchidsErrorPayload(msg)
		if errCode == "" {
			errCode = "credits_exhausted"
		}
		if errMsg == "" {
			errMsg = "You have run out of credits."
		}
		slog.Warn("Orchids credits exhausted", "code", errCode, "message", errMsg)
		setOrchidsErrorState(state, errCode, errMsg, true)
		emitOrchidsErrorEvent(onMessage, errCode, errMsg)
		return true
	case EventResponseDone, EventCodingAgentEnd, EventComplete:
		return handleOrchidsCompletionMessage(msgType, msg, state, onMessage, clientTools)
	case EventFS:
		return false
	case EventReasoningChunk:
		markOrchidsCodingAgent(state)
		emitOrchidsReasoningDelta(state, onMessage, extractOrchidsText(msg))
		return false
	case EventReasoningCompleted:
		markOrchidsCodingAgent(state)
		if endOrchidsReasoning(state) {
			onMessage(upstream.SSEMessage{Type: "model", Event: map[string]interface{}{"type": "reasoning-end", "id": "0"}})
		}
		return false
	case EventOutputTextDelta, EventResponseChunk:
		markOrchidsCodingAgent(state)
		emitOrchidsTextDelta(state, onMessage, msgType, extractOrchidsText(msg))
		return false
	case EventWriteStart, EventWriteContentStart, EventEditStart:
		return false
	case EventWriteChunk, EventEditChunk:
		return false
	case EventWriteCompleted:
		return false
	case EventEditCompleted, EventEditFileCompleted:
		return false
	case EventModel:
		return dispatchOrchidsModelMessage(msg, state, onMessage, clientTools)
	case "error":
		errCode, errMsg := extractOrchidsErrorPayload(msg)
		if errMsg == "" {
			errMsg = "unknown upstream error"
		}
		slog.Warn("Orchids upstream error event", "code", errCode, "message", errMsg)
		setOrchidsErrorState(state, errCode, errMsg, false)
		emitOrchidsErrorEvent(onMessage, errCode, errMsg)
		return true
	case EventTodoWriteStart, EventRunItemStream, EventToolCallOutput:
		return false
	default:
		return false
	}
}
