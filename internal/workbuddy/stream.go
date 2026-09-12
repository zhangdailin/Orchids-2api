package workbuddy

import (
	"bufio"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"io"
	"strings"
	"sync/atomic"
	"time"

	"github.com/goccy/go-json"

	"orchids-api/internal/upstream"
)

// streamResult accumulates what the upstream produced so the caller can decide
// whether the attempt was meaningful and which stop reason to report.
type streamResult struct {
	SawMeaningfulEvent bool
	ToolCallCount      int
	Usage              map[string]interface{}
	ThinkingSignature  string
}

// FinishReason maps the accumulated stream onto an Anthropic-style stop reason.
func (r streamResult) FinishReason() string {
	if r.ToolCallCount > 0 {
		return "tool_use"
	}
	return "end_turn"
}

var toolCallSequence atomic.Uint64

// NewToolCallID mints a local tool-call id for upstream deltas that omit one.
func NewToolCallID() string {
	return fmt.Sprintf("toolu_%d_%d", time.Now().UnixNano(), toolCallSequence.Add(1))
}

// streamChunk is one `data:` line of the SSE response.
type streamChunk struct {
	ID      string `json:"id"`
	Object  string `json:"object"`
	Model   string `json:"model"`
	Choices []struct {
		Index        int    `json:"index"`
		FinishReason string `json:"finish_reason"`
		Delta        struct {
			Role             string `json:"role"`
			Content          string `json:"content"`
			ReasoningContent string `json:"reasoning_content"`
			ToolCalls        []struct {
				Index    int    `json:"index"`
				ID       string `json:"id"`
				Type     string `json:"type"`
				Function struct {
					Name      string `json:"name"`
					Arguments string `json:"arguments"`
				} `json:"function"`
			} `json:"tool_calls"`
		} `json:"delta"`
	} `json:"choices"`
	Usage map[string]interface{} `json:"usage"`
	Error struct {
		Message string          `json:"message"`
		Code    json.RawMessage `json:"code"`
	} `json:"error"`
}

// toolCallAccumulator rebuilds tool calls from OpenAI-style deltas, where the
// name arrives in the first delta and the arguments are streamed afterwards.
type toolCallAccumulator struct {
	order []int
	calls map[int]*toolCallState
}

type toolCallState struct {
	ID        string
	Name      string
	Arguments strings.Builder
	Emitted   bool
}

func newToolCallAccumulator() *toolCallAccumulator {
	return &toolCallAccumulator{calls: map[int]*toolCallState{}}
}

func (a *toolCallAccumulator) add(index int, id, name, args string) *toolCallState {
	state, ok := a.calls[index]
	if !ok {
		state = &toolCallState{}
		a.calls[index] = state
		a.order = append(a.order, index)
	}
	if trimmed := strings.TrimSpace(id); trimmed != "" {
		state.ID = trimmed
	}
	if trimmed := strings.TrimSpace(name); trimmed != "" {
		state.Name = trimmed
	}
	state.Arguments.WriteString(args)
	return state
}

// pending returns not-yet-emitted complete tool calls in stream order.
func (a *toolCallAccumulator) pending() []*toolCallState {
	out := make([]*toolCallState, 0, len(a.order))
	for _, index := range a.order {
		state := a.calls[index]
		if state == nil || state.Emitted || state.Name == "" {
			continue
		}
		out = append(out, state)
	}
	return out
}

func (a *toolCallAccumulator) completeAll() []*toolCallState {
	pending := a.pending()
	for _, state := range pending {
		state.Emitted = true
	}
	return pending
}

// consumeStream parses the SSE body and forwards deltas to the caller.
func consumeStream(body io.Reader, onMessage func(upstream.SSEMessage)) (streamResult, error) {
	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	result := streamResult{}
	tools := newToolCallAccumulator()

	emitTools := func() {
		for _, state := range tools.completeAll() {
			result.SawMeaningfulEvent = true
			result.ToolCallCount++
			if onMessage == nil {
				continue
			}
			id := state.ID
			if id == "" {
				id = NewToolCallID()
			}
			onMessage(upstream.SSEMessage{Type: "model.tool-call", Event: map[string]interface{}{
				"toolCallId": id,
				"toolName":   state.Name,
				"input":      normalizeToolInput(state.Arguments.String()),
			}})
		}
	}

	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, ":") {
			continue
		}
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if payload == "[DONE]" {
			break
		}
		if payload == "" {
			continue
		}

		var chunk streamChunk
		if err := json.Unmarshal([]byte(payload), &chunk); err != nil {
			// A non-JSON data line is a protocol warning, not a transport
			// failure; keep consuming the stream.
			continue
		}
		if msg := strings.TrimSpace(chunk.Error.Message); msg != "" {
			return result, fmt.Errorf("workbuddy stream error: %s", msg)
		}
		if chunk.Usage != nil {
			if usage := normalizeUsage(chunk.Usage); len(usage) > 0 {
				result.Usage = usage
				result.SawMeaningfulEvent = true
				if onMessage != nil {
					onMessage(upstream.SSEMessage{Type: "model.tokens-used", Event: usage})
				}
			}
		}
		if len(chunk.Choices) == 0 {
			continue
		}
		delta := chunk.Choices[0].Delta

		if delta.ReasoningContent != "" {
			result.SawMeaningfulEvent = true
			if onMessage != nil {
				if result.ThinkingSignature == "" {
					result.ThinkingSignature = newThinkingSignature()
				}
				onMessage(upstream.SSEMessage{Type: "model.reasoning-delta", Event: map[string]interface{}{
					"delta":     delta.ReasoningContent,
					"signature": result.ThinkingSignature,
				}})
			}
		}
		if delta.Content != "" {
			result.SawMeaningfulEvent = true
			if onMessage != nil {
				onMessage(upstream.SSEMessage{Type: "model.text-delta", Event: map[string]interface{}{
					"delta": delta.Content,
				}})
			}
		}
		for _, call := range delta.ToolCalls {
			result.SawMeaningfulEvent = true
			tools.add(call.Index, call.ID, call.Function.Name, call.Function.Arguments)
			// Emit as soon as the call is complete; the upstream does not send
			// an explicit end marker per call.
			emitTools()
		}
	}
	if err := scanner.Err(); err != nil {
		return result, fmt.Errorf("failed to read workbuddy stream: %w", err)
	}
	emitTools()
	return result, nil
}

func newThinkingSignature() string {
	var raw [24]byte
	if _, err := rand.Read(raw[:]); err == nil {
		return "workbuddy-v1:" + base64.RawURLEncoding.EncodeToString(raw[:])
	}
	return fmt.Sprintf("workbuddy-v1:%d", time.Now().UnixNano())
}

// normalizeToolInput unwraps the OpenAI `{"arguments":"<json-string>"}` shape so
// downstream tool dispatch sees a plain JSON object.
func normalizeToolInput(raw string) string {
	return normalizeToolInputDepth(raw, 3)
}

func normalizeToolInputDepth(input string, depth int) string {
	if depth <= 0 {
		return strings.TrimSpace(input)
	}
	trimmed := strings.TrimSpace(input)
	if trimmed == "" || trimmed == "null" {
		return "{}"
	}
	var text string
	if json.Unmarshal([]byte(trimmed), &text) == nil {
		text = strings.TrimSpace(text)
		if text == "" {
			return "{}"
		}
		return normalizeToolInputDepth(text, depth-1)
	}
	if inner, ok := unwrapOpenAIArguments(trimmed); ok {
		return normalizeToolInputDepth(inner, depth-1)
	}
	return trimmed
}

func unwrapOpenAIArguments(input string) (string, bool) {
	var obj map[string]json.RawMessage
	if err := json.Unmarshal([]byte(input), &obj); err != nil {
		return "", false
	}
	rawArgs, ok := obj["arguments"]
	if !ok {
		return "", false
	}
	var text string
	if err := json.Unmarshal(rawArgs, &text); err != nil {
		return "", false
	}
	text = strings.TrimSpace(text)
	if text == "" || text == "null" {
		return "{}", true
	}
	if !json.Valid([]byte(text)) {
		return "", false
	}
	return text, true
}

// normalizeUsage maps the upstream usage object onto the key pair the shared
// stream handler consumes.
func normalizeUsage(raw map[string]interface{}) map[string]interface{} {
	if len(raw) == 0 {
		return nil
	}
	input, hasInput := firstUsageInt(raw, "prompt_tokens", "promptTokens", "input_tokens", "inputTokens")
	output, hasOutput := firstUsageInt(raw, "completion_tokens", "completionTokens", "output_tokens", "outputTokens")
	if !hasInput && !hasOutput {
		return nil
	}
	out := make(map[string]interface{}, 6)
	if hasInput {
		out["inputTokens"] = input
		out["input_tokens"] = input
	}
	if hasOutput {
		out["outputTokens"] = output
		out["output_tokens"] = output
	}
	if cached, ok := firstUsageInt(raw, "prompt_cache_hit_tokens", "cached_tokens"); ok {
		out["cacheReadTokens"] = cached
		out["cache_read_tokens"] = cached
	}
	if reasoning, ok := firstUsageInt(raw, "completion_thinking_tokens"); ok {
		out["reasoningTokens"] = reasoning
	}
	return out
}

func firstUsageInt(values map[string]interface{}, keys ...string) (int, bool) {
	for _, key := range keys {
		switch typed := values[key].(type) {
		case float64:
			return int(typed), true
		case int:
			return typed, true
		case json.Number:
			if parsed, err := typed.Int64(); err == nil {
				return int(parsed), true
			}
		}
	}
	return 0, false
}
