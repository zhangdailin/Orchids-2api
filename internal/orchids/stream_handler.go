package orchids

import (
	"crypto/rand"
	"encoding/hex"
	"strings"

	"github.com/goccy/go-json"
)

func getStringField(m map[string]interface{}, key string) (string, bool) {
	v, ok := m[key]
	if !ok {
		return "", false
	}
	s, ok := v.(string)
	if !ok {
		return "", false
	}
	return strings.TrimSpace(s), true
}

// The old methods for state are replaced by these StreamState-compatible ones:

func (s *requestState) ShouldSkipModelEvent() bool {
	return false
}

func (s *requestState) StartResponse() {
	s.responseStarted = true
}

func (s *requestState) StartReasoning() {
	s.reasoningStarted = true
}

func (s *requestState) SetInputTokens(tokens int64) {
	s.inputTokens = tokens
}

func (s *requestState) SetOutputTokens(tokens int64) {
	s.outputTokens = tokens
}

func (s *requestState) SetCacheReadInputTokens(tokens int64) {
	s.cacheReadInputTokens = tokens
}

func (s *requestState) SetFinishReason(reason string) {
	s.finishReason = reason
}

func (s *requestState) EndToolCall() *orchidsToolCallState {
	tc := s.currentToolCall
	s.currentToolCall = nil
	return tc
}

func (s *requestState) AppendToolInput(delta string) {
	s.pendingToolInput += delta
}

func (s *requestState) UpdatePendingToolInput(parsed interface{}) {
	if s.currentToolCall != nil {
		if data, err := json.Marshal(parsed); err == nil {
			s.currentToolCall.input = string(data)
		}
	}
}

func handleModelEvent(
	state *requestState,
	writer *SSEWriter,
	event map[string]interface{},
	toolMapper *ToolMapper,
	clientTools []interface{},
) error {
	if state.ShouldSkipModelEvent() {
		return nil
	}

	dataRaw, ok := event["data"]
	if !ok {
		return nil
	}
	data, ok := dataRaw.(map[string]interface{})
	if !ok {
		return nil
	}

	eventType, ok := getStringField(data, "type")
	if !ok || eventType == "" {
		return nil
	}

	switch eventType {
	case "finish":
		return handleFinishEvent(state, writer, data)

	case "text-delta":
		delta, ok := getStringField(data, "delta")
		if !ok || delta == "" {
			return nil
		}
		return writer.EnsureTextBlockAndWriteDelta(delta)

	case "text-start":
		if !state.responseStarted {
			state.StartResponse()
			return writer.WriteBlockStart(4, 0, "text", 0)
		}

	case "reasoning-end":
		return writer.CloseReasoningBlock()

	case "tool-input-end":
		return handleToolInputEnd(state, writer)

	case "reasoning-delta":
		delta, ok := getStringField(data, "delta")
		if !ok || delta == "" {
			return nil
		}
		if state.reasoningStarted {
			return writer.WriteThinkingDelta(delta)
		}

	case "reasoning-start":
		if !state.reasoningStarted {
			state.StartReasoning()
			return writer.WriteBlockStart(8, 0, "thinking", 0)
		}

	case "tool-input-delta":
		delta, ok := getStringField(data, "delta")
		if !ok || delta == "" {
			return nil
		}
		state.AppendToolInput(delta)
		if state.pendingToolInput != "" {
			return writer.WriteToolInputDelta()
		}

	case "tool-input-start":
		return handleToolInputStart(state, writer, data, toolMapper, clientTools)

	case "tool-call":
		return handleToolCallEvent(state, writer, data, toolMapper, clientTools)

	case "tokens-used":
		if value, _, ok := parseOrchidsUsageValue(data, "inputTokens", "input_tokens"); ok {
			state.SetInputTokens(value)
		}
		if value, _, ok := parseOrchidsUsageValue(data, "outputTokens", "output_tokens"); ok {
			state.SetOutputTokens(value)
		}
	}

	return nil
}

func handleFinishEvent(state *requestState, writer *SSEWriter, data map[string]interface{}) error {
	stopReason, _ := getStringField(data, "stop_reason")
	if stopReason == "" {
		stopReason, _ = getStringField(data, "finishReason")
	}

	if usageRaw, ok := data["usage"]; ok {
		if usage, ok := usageRaw.(map[string]interface{}); ok {
			if v, _, ok := parseOrchidsUsageValue(usage, "inputTokens", "input_tokens"); ok {
				state.SetInputTokens(v)
			}
			if v, _, ok := parseOrchidsUsageValue(usage, "outputTokens", "output_tokens"); ok {
				state.SetOutputTokens(v)
			}
			if v, _, ok := parseOrchidsUsageValue(usage, "cache_read_input_tokens"); ok {
				state.SetCacheReadInputTokens(v)
			}
		}
	}

	switch stopReason {
	case "tool-calls", "tool_use":
		state.SetFinishReason("tool_use")
	case "stop", "end_turn":
		state.SetFinishReason("end_turn")
	default:
		if stopReason != "" {
			state.SetFinishReason(stopReason)
		} else {
			state.SetFinishReason("end_turn")
		}
	}

	return nil
}

func parseOrchidsUsageValue(usage map[string]interface{}, keys ...string) (int64, float64, bool) {
	for _, key := range keys {
		if v, ok := usage[key]; ok {
			switch typed := v.(type) {
			case float64:
				return int64(typed), typed, true
			case int:
				return int64(typed), float64(typed), true
			case int32:
				return int64(typed), float64(typed), true
			case int64:
				return typed, float64(typed), true
			}
		}
	}
	return 0, 0, false
}

func handleToolInputEnd(state *requestState, writer *SSEWriter) error {
	tc := state.EndToolCall()
	if tc == nil {
		return nil
	}

	if tc.inputLength > 0 || tc.input != "" {
		var parsed interface{}
		// Actually use accumulated pending input if we are mirroring it
		accumulated := state.pendingToolInput
		if accumulated != "" {
			tc.input = accumulated
		}
		if err := json.Unmarshal([]byte(tc.input), &parsed); err == nil {
			state.UpdatePendingToolInput(parsed)
		}
	}

	if !tc.hasInput && tc.input != "" {
		if err := writer.WriteToolInputDelta(); err != nil {
			return err
		}
	}

	// Build and write content_block_stop
	event := map[string]interface{}{
		"type":  "content_block_stop",
		"index": writer.state.blockCount - 1,
	}

	if direct := writer.directEmitter(); direct != nil {
		direct.ObserveToolCall(tc.name, strings.TrimSpace(tc.input))
	}

	return writer.writeEvent("content_block_stop", event)
}

func handleToolInputStart(
	state *requestState,
	writer *SSEWriter,
	data map[string]interface{},
	toolMapper *ToolMapper,
	clientTools []interface{},
) error {
	toolID, _ := getStringField(data, "id")
	if toolID == "" {
		toolID, _ = getStringField(data, "toolCallId")
	}

	toolName, _ := getStringField(data, "tool_name")
	if toolName == "" {
		toolName, _ = getStringField(data, "toolName")
	}

	if toolID == "" {
		buf := make([]byte, 6)
		if _, err := rand.Read(buf); err == nil {
			encoded := make([]byte, hex.EncodedLen(len(buf)))
			hex.Encode(encoded, buf)
			toolID = "toolu_" + string(encoded)
		}
	}

	mappedName := toolName
	if len(clientTools) > 0 && toolName != "" {
		mappedName = MapToolNameToClient(toolName, clientTools, toolMapper)
	}

	if err := writer.StartToolUseBlock(toolID, mappedName); err != nil {
		return err
	}

	state.currentToolCall = &orchidsToolCallState{
		id:   toolID,
		name: mappedName,
	}

	// Reset pending tool input for new call
	state.pendingToolInput = ""

	return nil
}

// handleToolCallEvent handles a complete "tool-call" event from the decoded path.
// Unlike the streaming tool-input-start/delta/end sequence, this event contains
// the complete tool call data in a single event.
func handleToolCallEvent(
	state *requestState,
	writer *SSEWriter,
	data map[string]interface{},
	toolMapper *ToolMapper,
	clientTools []interface{},
) error {
	toolName, _ := getStringField(data, "tool_name")
	if toolName == "" {
		toolName, _ = getStringField(data, "toolName")
	}
	callID, _ := getStringField(data, "callId")
	if callID == "" {
		callID, _ = getStringField(data, "id")
	}
	// If no call ID, drop the event (consistent with test expectation)
	if callID == "" {
		return nil
	}

	// Map tool name to client name
	mappedName := toolName
	if len(clientTools) > 0 && toolName != "" {
		mappedName = MapToolNameToClient(toolName, clientTools, toolMapper)
	}

	// Emit content_block_start
	if err := writer.StartToolUseBlock(callID, mappedName); err != nil {
		return err
	}

	// Build input JSON
	var inputJSON string
	if inputRaw, ok := data["input"]; ok {
		// Transform tool input
		if inputMap, ok := inputRaw.(map[string]interface{}); ok {
			transformed := TransformToolInput(toolName, mappedName, inputMap)
			if encoded, err := json.Marshal(transformed); err == nil {
				inputJSON = string(encoded)
			}
		} else {
			var normalized map[string]interface{}
			if encoded, err := json.Marshal(inputRaw); err == nil {
				if err := json.Unmarshal(encoded, &normalized); err == nil && normalized != nil {
					transformed := TransformToolInput(toolName, mappedName, normalized)
					if encoded, err := json.Marshal(transformed); err == nil {
						inputJSON = string(encoded)
					}
				} else {
					inputJSON = string(encoded)
				}
			}
		}
	}

	// Emit content_block_delta with input_json_delta
	if inputJSON != "" {
		state.pendingToolInput = inputJSON
		if err := writer.WriteToolInputDelta(); err != nil {
			return err
		}
	}

	// Observe tool call for direct SSE
	if direct := writer.directEmitter(); direct != nil {
		direct.ObserveToolCall(mappedName, strings.TrimSpace(inputJSON))
	}

	// Emit content_block_stop
	event := map[string]interface{}{
		"type":  "content_block_stop",
		"index": writer.state.blockCount - 1,
	}
	return writer.writeEvent("content_block_stop", event)
}

func setOrchidsErrorState(state *requestState, code, message string, quota bool) {
	if quota {
		state.errorMsg = "no remaining quota: " + message
		return
	}
	if code != "" {
		state.errorMsg = code + ": " + message
		return
	}
	state.errorMsg = message
}

func emitOrchidsErrorEvent(writer *SSEWriter, code, message string) {
	if writer == nil {
		return
	}
	writer.WriteError(code, message)
}
