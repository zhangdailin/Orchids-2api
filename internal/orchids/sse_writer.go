package orchids

import (
	"github.com/goccy/go-json"

	"orchids-api/internal/upstream"
)

type SSEWriter struct {
	state     *requestState
	onMessage func(upstream.SSEMessage)
}

func NewSSEWriter(state *requestState, onMessage func(upstream.SSEMessage)) *SSEWriter {
	if onMessage == nil && (state == nil || state.directSSE == nil) {
		return nil
	}
	return &SSEWriter{state: state, onMessage: onMessage}
}

func (w *SSEWriter) directEmitter() upstream.DirectSSEEmitter {
	if w == nil || w.state == nil {
		return nil
	}
	return w.state.directSSE
}

func (w *SSEWriter) writeEvent(eventType string, data interface{}) error {
	if w == nil {
		return nil
	}

	if direct := w.directEmitter(); direct != nil {
		raw, err := json.Marshal(data)
		if err == nil {
			direct.WriteDirectSSE(eventType, raw, false)
			return nil
		}
	}
	if w.onMessage == nil {
		return nil
	}

	eventMap, ok := data.(map[string]interface{})
	if !ok {
		// fallback if it's not a map
		raw, _ := json.Marshal(data)
		_ = json.Unmarshal(raw, &eventMap)
	}
	if w.onMessage != nil {
		w.onMessage(upstream.SSEMessage{Type: eventType, Event: eventMap})
	}
	return nil
}

func (w *SSEWriter) WriteBlockStart(blockType int, index int, typeStr string, subIndex int) error {
	event := map[string]interface{}{
		"type":  "content_block_start",
		"index": w.state.blockCount,
	}

	switch blockType {
	case 4: // text block
		event["content_block"] = map[string]interface{}{
			"type": "text",
			"text": "",
		}
	case 8: // thinking block
		event["content_block"] = map[string]interface{}{
			"type":     "thinking",
			"thinking": "",
		}
	}

	w.state.blockCount++
	return w.writeEvent("content_block_start", event)
}

func (w *SSEWriter) EnsureTextBlockAndWriteDelta(delta string) error {
	event := map[string]interface{}{
		"type":  "content_block_delta",
		"index": w.state.blockCount - 1,
		"delta": map[string]interface{}{
			"type": "text_delta",
			"text": delta,
		},
	}
	if direct := w.directEmitter(); direct != nil {
		direct.ObserveTextDelta(delta)
	}
	return w.writeEvent("content_block_delta", event)
}

func (w *SSEWriter) WriteThinkingDelta(delta string) error {
	event := map[string]interface{}{
		"type":  "content_block_delta",
		"index": w.state.blockCount - 1,
		"delta": map[string]interface{}{
			"type":     "thinking_delta",
			"thinking": delta,
		},
	}
	if direct := w.directEmitter(); direct != nil {
		direct.ObserveThinkingDelta(delta)
	}
	return w.writeEvent("content_block_delta", event)
}

func (w *SSEWriter) CloseReasoningBlock() error {
	event := map[string]interface{}{
		"type":  "content_block_stop",
		"index": w.state.blockCount - 1,
	}
	return w.writeEvent("content_block_stop", event)
}

func (w *SSEWriter) WriteToolInputDelta() error {
	if w.state.pendingToolInput == "" {
		return nil
	}
	event := map[string]interface{}{
		"type":  "content_block_delta",
		"index": w.state.blockCount - 1,
		"delta": map[string]interface{}{
			"type":         "input_json_delta",
			"partial_json": w.state.pendingToolInput,
		},
	}
	w.state.pendingToolInput = ""
	return w.writeEvent("content_block_delta", event)
}

func (w *SSEWriter) StartToolUseBlock(toolID string, toolName string) error {
	event := map[string]interface{}{
		"type":  "content_block_start",
		"index": w.state.blockCount,
		"content_block": map[string]interface{}{
			"type":  "tool_use",
			"id":    toolID,
			"name":  toolName,
			"input": map[string]interface{}{},
		},
	}
	w.state.blockCount++
	return w.writeEvent("content_block_start", event)
}

func (w *SSEWriter) WriteMessageStart() error {
	if w == nil || w.state == nil || !w.state.stream || w.state.messageStarted {
		return nil
	}
	w.state.messageStarted = true
	event := map[string]interface{}{
		"type": "message_start",
		"message": map[string]interface{}{
			"id":      "",
			"type":    "message",
			"role":    "assistant",
			"model":   w.state.modelName,
			"content": []interface{}{},
			"usage": map[string]interface{}{
				"input_tokens":  0,
				"output_tokens": 0,
			},
		},
	}
	return w.writeEvent("message_start", event)
}

func (w *SSEWriter) WriteMessageEnd() error {
	if w == nil || w.state == nil {
		return nil
	}
	if w.state.finishSent {
		return nil
	}
	w.state.finishSent = true

	// Write message_delta with usage and finishReason
	finishReason := w.state.finishReason
	if finishReason == "" {
		finishReason = "end_turn"
	}

	usageEvent := map[string]interface{}{
		"input_tokens":  w.state.inputTokens,
		"output_tokens": w.state.outputTokens,
	}
	if w.state.cacheReadInputTokens > 0 {
		usageEvent["cache_read_input_tokens"] = w.state.cacheReadInputTokens
	}
	if w.state.cacheCreationInputTokens > 0 {
		usageEvent["cache_creation_input_tokens"] = w.state.cacheCreationInputTokens
	}

	deltaEvent := map[string]interface{}{
		"type": "message_delta",
		"delta": map[string]interface{}{
			"stop_reason": finishReason,
		},
		"usage": usageEvent,
	}

	if direct := w.directEmitter(); direct != nil {
		direct.ObserveUsage(int(w.state.inputTokens), int(w.state.outputTokens))
		direct.ObserveStopReason(finishReason)
		raw, err := json.Marshal(deltaEvent)
		if err == nil {
			direct.WriteDirectSSE("message_delta", raw, false)
		}

		stopEvent := map[string]interface{}{
			"type": "message_stop",
		}
		rawStop, err := json.Marshal(stopEvent)
		if err == nil {
			direct.WriteDirectSSE("message_stop", rawStop, true)
		}
		direct.FinishDirectSSE(finishReason)
		return nil
	}

	if err := w.writeEvent("message_delta", deltaEvent); err != nil {
		return err
	}

	stopEvent := map[string]interface{}{
		"type": "message_stop",
	}
	return w.writeEvent("message_stop", stopEvent)
}

func (w *SSEWriter) WriteError(code, message string) {
	if w == nil {
		return
	}
	if w.state != nil {
		if w.state.finishSent {
			return
		}
		w.state.finishSent = true
		w.state.finishReason = "error"
	}
	event := map[string]interface{}{
		"type": "error",
		"error": map[string]interface{}{
			"type":    "error",
			"code":    code,
			"message": message,
		},
	}
	if direct := w.directEmitter(); direct != nil {
		raw, _ := json.Marshal(event)
		direct.ObserveStopReason("error")
		direct.WriteDirectSSE("error", raw, true)
		direct.FinishDirectSSE("error")
	} else if w.onMessage != nil {
		w.onMessage(upstream.SSEMessage{Type: "error", Event: event})
	}
}
