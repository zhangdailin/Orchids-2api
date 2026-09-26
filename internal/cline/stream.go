package cline

import (
	"bufio"
	cryptorand "crypto/rand"
	"encoding/base64"
	"fmt"
	"html"
	"io"
	"regexp"
	"strings"
	"sync/atomic"
	"time"

	"github.com/goccy/go-json"

	"orchids-api/internal/upstream"
	"orchids-api/internal/util"
)

// streamResult accumulates what the upstream produced so the caller can decide
// whether the attempt was meaningful and which stop reason to report.
type streamResult struct {
	SawMeaningfulEvent   bool
	ToolCallCount        int
	UpstreamFinishReason string
	Usage                map[string]interface{}
	// ThinkingSignature is one stable signature for every reasoning delta of
	// the stream, generated on first sight so a multi-delta block stays signed.
	ThinkingSignature string
}

// FinishReason maps the accumulated stream onto an Anthropic-style stop reason.
func (r streamResult) FinishReason() string {
	if r.ToolCallCount > 0 {
		return "tool_use"
	}
	switch strings.ToLower(strings.TrimSpace(r.UpstreamFinishReason)) {
	case "length", "max_tokens", "max_token_limit":
		return "max_tokens"
	case "stop", "stop_sequence", "end_turn", "":
		return "end_turn"
	default:
		return r.UpstreamFinishReason
	}
}

var toolCallSequence atomic.Uint64

// NewToolCallID mints a local tool-call id for upstream deltas that omit one.
func NewToolCallID() string {
	return fmt.Sprintf("toolu_%d_%d", time.Now().UnixNano(), toolCallSequence.Add(1))
}

// newThinkingSignature mints the per-stream thinking signature the Anthropic
// surface attaches to a thinking block. The prefix names the channel so a
// signature can be attributed when it round-trips in history replay.
func newThinkingSignature() string {
	var raw [24]byte
	if _, err := cryptorand.Read(raw[:]); err == nil {
		return "cline-v1:" + base64.RawURLEncoding.EncodeToString(raw[:])
	}
	return fmt.Sprintf("cline-v1:%d", time.Now().UnixNano())
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
			// GLM-compatible gateways have emitted the same channel under
			// `reasoning` or `thinking`; keep all aliases so the reasoning block
			// is not silently lost at the Cline boundary.
			Reasoning string `json:"reasoning"`
			Thinking  string `json:"thinking"`
			ToolCalls []struct {
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
	order []*toolCallState
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
	trimmedID := strings.TrimSpace(id)
	state, ok := a.calls[index]
	if ok && (state.Emitted || (trimmedID != "" && state.ID != "" && state.ID != trimmedID)) {
		state = nil
		ok = false
	}
	if !ok {
		state = &toolCallState{}
		a.calls[index] = state
		a.order = append(a.order, state)
	}
	if trimmedID != "" {
		state.ID = trimmedID
	}
	if trimmed := strings.TrimSpace(name); trimmed != "" {
		state.Name = trimmed
	}
	state.Arguments.WriteString(args)
	return state
}

// completeAll drains every not-yet-emitted call in stream order.
//
// Draining is what makes it safe to call twice: the finish-time emit and the
// end-of-stream flush both run, and without clearing the accumulator the second
// call would hand the same tool call to the client a second time.
func (a *toolCallAccumulator) completeAll() []*toolCallState {
	out := make([]*toolCallState, 0, len(a.order))
	for _, state := range a.order {
		if state == nil || state.Emitted || state.Name == "" {
			continue
		}
		state.Emitted = true
		out = append(out, state)
	}
	a.order = a.order[:0]
	a.calls = map[int]*toolCallState{}
	return out
}

var clineTextToolBlockRE = regexp.MustCompile(`(?s)<tool_call>\s*(.*?)\s*</tool_call>`)
var clineTextToolArgumentRE = regexp.MustCompile(`(?s)<arg_key>\s*([^<]+?)\s*</arg_key>\s*<arg_value>\s*(.*?)\s*</arg_value>`)
var clineTextToolNameRE = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_.-]*$`)

const maxClineTextToolBufferBytes = 1 << 20

// parseClineTextToolCalls converts the textual fallbacks emitted by some Cline
// models (notably z-ai/glm) when the gateway returns a tool call inside the text
// delta instead of OpenAI tool_calls deltas. Two formats have been observed:
//
//   - <tool_call>bash:ignored</arg_value><arg_key>command</arg_key>...</tool_call>
//   - <tool_call>glob<tool_call>glob: *<tool_call>args: {"pattern":"*"}...
//
// The first format's value before arg_key is an unlabelled duplicate and is
// intentionally ignored.
func parseClineTextToolCalls(text string) (string, []toolCall) {
	visible, calls := parseClineClosedTextToolCalls(text)
	if len(calls) > 0 {
		return visible, calls
	}
	return parseClineRepeatedTagToolCall(text)
}

func parseClineClosedTextToolCalls(text string) (string, []toolCall) {
	matches := clineTextToolBlockRE.FindAllStringSubmatchIndex(text, -1)
	if len(matches) == 0 {
		return text, nil
	}
	calls := make([]toolCall, 0, len(matches))
	var visible strings.Builder
	last := 0
	for _, match := range matches {
		if len(match) < 4 {
			continue
		}
		visible.WriteString(text[last:match[0]])
		body := text[match[2]:match[3]]
		colon := strings.IndexByte(body, ':')
		if colon <= 0 {
			visible.WriteString(text[match[0]:match[1]])
			last = match[1]
			continue
		}
		name := strings.TrimSpace(body[:colon])
		args := map[string]interface{}{}
		for _, arg := range clineTextToolArgumentRE.FindAllStringSubmatch(body[colon+1:], -1) {
			if len(arg) != 3 {
				continue
			}
			key := strings.TrimSpace(html.UnescapeString(arg[1]))
			value := html.UnescapeString(strings.TrimSpace(arg[2]))
			var typed interface{}
			if json.Unmarshal([]byte(value), &typed) == nil {
				args[key] = typed
			} else {
				args[key] = value
			}
		}
		if name == "" || len(args) == 0 {
			visible.WriteString(text[match[0]:match[1]])
			last = match[1]
			continue
		}
		raw, err := json.Marshal(args)
		if err != nil {
			visible.WriteString(text[match[0]:match[1]])
			last = match[1]
			continue
		}
		calls = append(calls, toolCall{ID: NewToolCallID(), Type: "function", Function: toolCallFunction{Name: name, Arguments: string(raw)}})
		last = match[1]
	}
	visible.WriteString(text[last:])
	return visible.String(), calls
}

func parseClineRepeatedTagToolCall(text string) (string, []toolCall) {
	const marker = "<tool_call>"
	first := strings.Index(text, marker)
	if first < 0 {
		return text, nil
	}
	segments := strings.Split(text[first+len(marker):], marker)
	if len(segments) < 1 {
		return text, nil
	}
	calls := make([]toolCall, 0, len(segments))
	for index := 0; index < len(segments); {
		segment := strings.TrimSpace(segments[index])
		// Compact GLM fallback: <tool_call>glob,{"pattern":"*"}
		if comma := strings.IndexByte(segment, ','); comma > 0 {
			name := strings.TrimSpace(segment[:comma])
			arguments := strings.TrimSpace(segment[comma+1:])
			if clineTextToolNameRE.MatchString(name) && json.Valid([]byte(arguments)) {
				calls = append(calls, toolCall{ID: NewToolCallID(), Type: "function", Function: toolCallFunction{Name: name, Arguments: arguments}})
				index++
				continue
			}
		}
		if !clineTextToolNameRE.MatchString(segment) {
			index++
			continue
		}
		name := segment
		args := map[string]interface{}{}
		cursor := index + 1
		for ; cursor < len(segments); cursor++ {
			part := strings.TrimSpace(segments[cursor])
			if clineTextToolNameRE.MatchString(part) {
				break
			}
			colon := strings.IndexByte(part, ':')
			if colon <= 0 {
				continue
			}
			key := strings.TrimSpace(part[:colon])
			value := strings.TrimSpace(part[colon+1:])
			if key == "" || value == "" || strings.EqualFold(key, name) || strings.EqualFold(key, "run_in_background") {
				continue
			}
			if strings.EqualFold(key, "args") || strings.EqualFold(key, "arguments") {
				var object map[string]interface{}
				if json.Unmarshal([]byte(value), &object) == nil {
					for objectKey, objectValue := range object {
						args[objectKey] = objectValue
					}
				}
				continue
			}
			var typed interface{}
			if json.Unmarshal([]byte(value), &typed) == nil {
				args[key] = typed
			} else {
				args[key] = value
			}
		}
		if len(args) > 0 {
			raw, err := json.Marshal(args)
			if err == nil {
				calls = append(calls, toolCall{ID: NewToolCallID(), Type: "function", Function: toolCallFunction{Name: name, Arguments: string(raw)}})
			}
		}
		if cursor <= index {
			index++
		} else {
			index = cursor
		}
	}
	if len(calls) == 0 {
		return text, nil
	}
	return text[:first], calls
}

// clineThinkingSplitter separates GLM-style in-content <think> blocks from
// answer text, including tags split across SSE frames. Unmatched tag prefixes
// are held for at most len("</think>") bytes and flushed at end of stream.
// A complete opening tag is required before treating anything as reasoning.
type clineThinkingSplitter struct {
	thinking bool
	pending  string
}

func (s *clineThinkingSplitter) feed(content string, final bool, emitReasoning, emitText func(string)) {
	s.pending += content
	for len(s.pending) > 0 {
		tag := "<think>"
		emit := emitText
		if s.thinking {
			tag = "</think>"
			emit = emitReasoning
		}
		if idx := strings.Index(s.pending, tag); idx >= 0 {
			emit(s.pending[:idx])
			s.pending = s.pending[idx+len(tag):]
			s.thinking = !s.thinking
			continue
		}
		// Retain only the longest suffix that could be the beginning of a
		// delimiter; everything else is safe to emit right now.
		keep := 0
		if !final {
			for n := min(len(s.pending), len(tag)-1); n > 0; n-- {
				if strings.HasSuffix(s.pending, tag[:n]) {
					keep = n
					break
				}
			}
		}
		emit(s.pending[:len(s.pending)-keep])
		s.pending = s.pending[len(s.pending)-keep:]
		break
	}
}

// consumeStream parses the SSE body and forwards deltas to the caller.
func consumeStream(body io.Reader, toolsEnabled bool, onMessage func(upstream.SSEMessage)) (streamResult, error) {
	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	result := streamResult{}
	tools := newToolCallAccumulator()
	var pendingText strings.Builder
	var thinkingSplitter clineThinkingSplitter
	sawNativeTools := false
	sawFinish := false

	emitText := func(text string) {
		if text == "" {
			return
		}
		result.SawMeaningfulEvent = true
		if onMessage != nil {
			onMessage(upstream.SSEMessage{Type: "model.text-delta", Event: map[string]interface{}{
				"delta": text,
			}})
		}
	}

	emitReasoning := func(reasoning string) {
		if reasoning == "" {
			return
		}
		result.SawMeaningfulEvent = true
		if onMessage != nil {
			// One signature per stream also covers reasoning embedded in content.
			if result.ThinkingSignature == "" {
				result.ThinkingSignature = newThinkingSignature()
			}
			onMessage(upstream.SSEMessage{Type: "model.reasoning-delta", Event: map[string]interface{}{
				"delta":     reasoning,
				"signature": result.ThinkingSignature,
			}})
		}
	}

	emitContent := func(content string) {
		if content == "" {
			return
		}
		if toolsEnabled && !sawNativeTools {
			pendingText.WriteString(content)
			if pendingText.Len() > maxClineTextToolBufferBytes {
				emitText(pendingText.String())
				pendingText.Reset()
			}
		} else {
			emitText(content)
		}
	}

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
				"input":      util.NormalizeToolInput(state.Arguments.String()),
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
			sawFinish = true
			break
		}
		if payload == "" {
			continue
		}

		// Some chunks arrive wrapped in {"data":{...}}; the delta has to be read
		// out of the envelope before it can be decoded as a chunk.
		var chunk streamChunk
		if err := json.Unmarshal([]byte(payload), &chunk); err != nil || !chunkLooksDecoded(payload) {
			unwrapped, ok := unwrapEnvelope(payload)
			if !ok {
				return result, fmt.Errorf("cline stream protocol error: invalid chunk")
			}
			if err := json.Unmarshal([]byte(unwrapped), &chunk); err != nil || !chunkLooksDecoded(unwrapped) {
				return result, fmt.Errorf("cline stream protocol error: invalid wrapped chunk")
			}
		}
		if msg := strings.TrimSpace(chunk.Error.Message); msg != "" {
			return result, fmt.Errorf("cline stream error: %s", msg)
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

		// Providers are not consistent about the GLM reasoning field name.
		// Normalize aliases before emitting the shared reasoning event.
		reasoning := delta.ReasoningContent
		if reasoning == "" {
			reasoning = delta.Reasoning
		}
		if reasoning == "" {
			reasoning = delta.Thinking
		}
		if reasoning != "" {
			emitReasoning(reasoning)
		}
		if delta.Content != "" {
			thinkingSplitter.feed(delta.Content, false, emitReasoning, emitContent)
		}
		for _, call := range delta.ToolCalls {
			result.SawMeaningfulEvent = true
			if !sawNativeTools {
				sawNativeTools = true
				// Textual tool markup and native deltas are duplicate encodings.
				// Preserve ordinary prose but strip any complete fallback blocks.
				if pendingText.Len() > 0 {
					visible, _ := parseClineTextToolCalls(pendingText.String())
					emitText(visible)
					pendingText.Reset()
				}
			}
			tools.add(call.Index, call.ID, call.Function.Name, call.Function.Arguments)
		}
		// OpenAI-style tool arguments can span several deltas. Emitting on the
		// first delta loses every later fragment and produces invalid JSON. A
		// non-empty finish reason closes the choice; [DONE]/EOF is handled below.
		finishReason := strings.TrimSpace(chunk.Choices[0].FinishReason)
		if finishReason != "" {
			result.UpstreamFinishReason = finishReason
			sawFinish = true
			emitTools()
		}
	}
	if err := scanner.Err(); err != nil {
		return result, fmt.Errorf("failed to read cline stream: %w", err)
	}
	if !sawFinish {
		// EOF before either an explicit finish_reason or [DONE] is a truncated
		// attempt. Returning success would silently accept a partial answer.
		return result, ErrStreamTruncated
	}
	thinkingSplitter.feed("", true, emitReasoning, emitContent)
	if toolsEnabled && !sawNativeTools && pendingText.Len() > 0 {
		visible, calls := parseClineTextToolCalls(pendingText.String())
		emitText(visible)
		for index, call := range calls {
			tools.add(index, call.ID, call.Function.Name, call.Function.Arguments)
		}
		pendingText.Reset()
	}
	emitTools()
	return result, nil
}

// chunkLooksDecoded reports whether a payload decoded as a chunk rather than
// merely as JSON. An envelope also decodes into streamChunk — every field is
// missing — so an object carrying only `data` has to be recognized and unwrapped.
func chunkLooksDecoded(payload string) bool {
	var probe map[string]json.RawMessage
	if err := json.Unmarshal([]byte(payload), &probe); err != nil {
		return false
	}
	for _, key := range []string{"choices", "usage", "id", "object", "model"} {
		if _, ok := probe[key]; ok {
			return true
		}
	}
	return false
}

// unwrapEnvelope reads the inner object of a {"data":{...}} chunk.
func unwrapEnvelope(payload string) (string, bool) {
	var envelope struct {
		Data json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal([]byte(payload), &envelope); err != nil {
		return "", false
	}
	inner := strings.TrimSpace(string(envelope.Data))
	if inner == "" || inner[0] != '{' {
		return "", false
	}
	return inner, true
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
	if reasoning, ok := reasoningUsage(raw); ok {
		out["reasoningTokens"] = reasoning
		out["reasoning_tokens"] = reasoning
	}
	return out
}

func reasoningUsage(raw map[string]interface{}) (int, bool) {
	if value, ok := firstUsageInt(raw, "reasoning_tokens", "reasoningTokens", "completion_thinking_tokens"); ok {
		return value, true
	}
	for _, key := range []string{"completion_tokens_details", "completionTokensDetails", "output_tokens_details", "outputTokensDetails"} {
		if details, ok := raw[key].(map[string]interface{}); ok {
			if value, found := firstUsageInt(details, "reasoning_tokens", "reasoningTokens", "thinking_tokens", "thinkingTokens"); found {
				return value, true
			}
		}
	}
	return 0, false
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
