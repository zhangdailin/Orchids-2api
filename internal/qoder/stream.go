package qoder

import (
	"bufio"
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"time"

	"github.com/goccy/go-json"

	"orchids-api/internal/upstream"
)

// The upstream always answers with SSE, and the payload is doubly wrapped: each
// `data:` line is an envelope object whose `body` field is itself a JSON string
// holding an OpenAI-shaped chunk. Reading the envelope as the chunk silently
// produces an empty answer, so the two layers are unwrapped explicitly.
//
// Termination is also not the OpenAI one. The stream ends with `event:finish`;
// a `[DONE]` marker may appear only inside an envelope body. An EOF without
// either marker is a truncation and must be reported as an error — treating it
// as success would hand the client a silently cut-off answer.

// streamEnvelope is the outer SSE frame.
type streamEnvelope struct {
	Headers         map[string][]string `json:"headers"`
	Body            string              `json:"body"`
	StatusCodeValue int                 `json:"statusCodeValue"`
	StatusCode      string              `json:"statusCode"`
}

// streamChunk is the inner OpenAI-shaped chunk.
type streamChunk struct {
	ID      string `json:"id"`
	Object  string `json:"object"`
	Created int64  `json:"created"`
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
	Usage *streamUsage `json:"usage"`
	Error *struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

type streamUsage struct {
	PromptTokens        int `json:"prompt_tokens"`
	CompletionTokens    int `json:"completion_tokens"`
	TotalTokens         int `json:"total_tokens"`
	PromptTokensDetails *struct {
		CachedTokens    int `json:"cached_tokens"`
		CacheableTokens int `json:"cacheable_tokens"`
	} `json:"prompt_tokens_details"`
	CompletionTokensDetails *struct {
		ReasoningTokens int `json:"reasoning_tokens"`
	} `json:"completion_tokens_details"`
	// The gateway bills in credits and reports the discount separately; the
	// numbers are surfaced so a cost panel can show what a plan actually cost.
	Credits         *float64 `json:"credits"`
	OriginalCredits *float64 `json:"original_credits"`
}

// streamResult accumulates what one upstream stream produced, so the caller can
// decide whether the attempt was meaningful and which stop reason to report.
type streamResult struct {
	SawMeaningfulEvent bool
	ToolCallCount      int
	FinishReasonValue  string
	Usage              map[string]interface{}
	ThinkingSignature  string
	// DroppedToolCallCount counts calls the upstream reported through a reused
	// index. They are not forwarded because their arguments were incomplete, so
	// the count is the only evidence they existed.
	DroppedToolCallCount int
}

// FinishReason maps the accumulated stream onto an Anthropic-style stop reason.
func (r streamResult) FinishReason() string {
	switch strings.TrimSpace(r.FinishReasonValue) {
	case "tool_calls":
		return "tool_use"
	case "length":
		return "max_tokens"
	case "content_filter":
		return "refusal"
	}
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

// toolCallAccumulator rebuilds tool calls from streamed deltas, where the name
// arrives in the first delta and the arguments are streamed afterwards.
//
// The upstream sometimes reuses one index for calls that are only distinguished
// by id. One index can hold only one call, so the newest id wins: the displaced
// call is dropped and counted, because its argument buffer is incomplete and
// emitting a half-built call would hand the client invalid JSON. The drop count
// is what lets the caller tell "the model made one call" from "the model made a
// call this channel could not represent".
type toolCallAccumulator struct {
	order   []int
	indexed map[int]struct{}
	calls   map[int]*toolCallState
	dropped int
}

type toolCallState struct {
	ID        string
	Name      string
	Arguments strings.Builder
	Emitted   bool
}

func newToolCallAccumulator() *toolCallAccumulator {
	return &toolCallAccumulator{
		indexed: map[int]struct{}{},
		calls:   map[int]*toolCallState{},
	}
}

// add folds one delta into the call at index.
func (a *toolCallAccumulator) add(index int, id, name, args string) *toolCallState {
	trimmedID := strings.TrimSpace(id)
	state, ok := a.calls[index]
	if ok && trimmedID != "" && state.ID != "" && state.ID != trimmedID {
		// A different id at the same index is a different call; the index can
		// only carry one.
		if !state.Emitted && state.Name != "" {
			a.dropped++
		}
		state = nil
		ok = false
	}
	if !ok {
		state = &toolCallState{}
		a.calls[index] = state
		if _, tracked := a.indexed[index]; !tracked {
			// An index that was already closed keeps its slot: appending it
			// again would emit the same call twice on the next flush.
			a.order = append(a.order, index)
			a.indexed[index] = struct{}{}
		}
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

// pending returns the not-yet-emitted calls in stream order.
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

// sseFrame is one accumulated SSE event.
type sseFrame struct {
	event string
	data  string
}

// readSSE frames the upstream stream. A `data:` line continues the current
// frame until a blank line closes it, and an `event:` line names it.
func readSSE(reader io.Reader, fn func(sseFrame) bool) error {
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)

	var event, data strings.Builder
	flush := func() bool {
		if event.Len() == 0 && data.Len() == 0 {
			return true
		}
		frame := sseFrame{event: event.String(), data: data.String()}
		event.Reset()
		data.Reset()
		return fn(frame)
	}

	for scanner.Scan() {
		line := strings.TrimRight(scanner.Text(), "\r")
		switch {
		case line == "":
			if !flush() {
				return nil
			}
		case strings.HasPrefix(line, ":"):
			// Comment / keep-alive.
		case strings.HasPrefix(line, "event:"):
			if event.Len() > 0 {
				event.WriteByte(' ')
			}
			event.WriteString(strings.TrimSpace(strings.TrimPrefix(line, "event:")))
		case strings.HasPrefix(line, "data:"):
			value := strings.TrimPrefix(strings.TrimPrefix(line, "data:"), " ")
			if data.Len() > 0 {
				data.WriteByte('\n')
			}
			data.WriteString(value)
		}
	}
	if err := scanner.Err(); err != nil {
		return err
	}
	// A trailing frame without its blank-line terminator still counts.
	flush()
	return nil
}

// consumeStream parses the SSE body and forwards deltas to the caller.
func consumeStream(body io.Reader, onMessage func(upstream.SSEMessage)) (streamResult, error) {
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

	sawFinish := false
	var streamErr error

	readErr := readSSE(body, func(frame sseFrame) bool {
		if strings.EqualFold(strings.TrimSpace(frame.event), "finish") {
			sawFinish = true
			return false
		}
		if strings.EqualFold(strings.TrimSpace(frame.event), "error") {
			if streamErr == nil {
				streamErr = fmt.Errorf("qoder stream reported an error event")
			}
			return false
		}

		payload := strings.TrimSpace(frame.data)
		if payload == "" {
			return true
		}
		if payload == "[DONE]" {
			sawFinish = true
			return false
		}

		var envelope streamEnvelope
		if err := json.Unmarshal([]byte(payload), &envelope); err != nil {
			// A non-JSON data line is a protocol warning, not a transport
			// failure; keep consuming so a single bad frame does not discard an
			// otherwise good answer.
			return true
		}
		if envelope.StatusCodeValue != 0 && envelope.StatusCodeValue != http.StatusOK {
			var failure struct {
				Code    string `json:"code"`
				Message string `json:"message"`
			}
			_ = json.Unmarshal([]byte(envelope.Body), &failure)
			detail := strings.TrimSpace(failure.Message)
			if detail == "" {
				detail = fmt.Sprintf("upstream status %d", envelope.StatusCodeValue)
			}
			if stringOfCode(failure.Code) == busyCode {
				streamErr = fmt.Errorf("%w: %s", ErrBusy, detail)
			} else if envelope.StatusCodeValue == http.StatusUnauthorized || envelope.StatusCodeValue == http.StatusForbidden {
				streamErr = fmt.Errorf("%w: %s", errUpstreamUnauthorized, detail)
			} else {
				streamErr = fmt.Errorf("qoder upstream error: %s", detail)
			}
			return false
		}
		if strings.TrimSpace(envelope.Body) == "" {
			return true
		}
		if strings.TrimSpace(envelope.Body) == "[DONE]" {
			sawFinish = true
			return false
		}

		var chunk streamChunk
		if err := json.Unmarshal([]byte(envelope.Body), &chunk); err != nil {
			return true
		}
		if chunk.Error != nil && strings.TrimSpace(chunk.Error.Message) != "" {
			if stringOfCode(chunk.Error.Code) == busyCode {
				streamErr = fmt.Errorf("%w: %s", ErrBusy, chunk.Error.Message)
			} else {
				streamErr = fmt.Errorf("qoder stream error: %s", chunk.Error.Message)
			}
			return false
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
			return true
		}
		choice := chunk.Choices[0]
		delta := choice.Delta

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
		}
		if reason := strings.TrimSpace(choice.FinishReason); reason != "" && reason != "null" {
			result.FinishReasonValue = reason
			// Arguments can span several deltas, so calls are only emitted once
			// the choice says it is done.
			emitTools()
		}
		return true
	})

	if readErr != nil {
		return result, fmt.Errorf("read qoder stream: %w", readErr)
	}
	if streamErr != nil {
		return result, streamErr
	}
	emitTools()
	if !sawFinish {
		// An EOF without a finish event means the connection was cut
		// mid-answer. Reporting success here would truncate silently.
		return result, ErrStreamTruncated
	}
	result.DroppedToolCallCount = tools.dropped
	return result, nil
}

// busyCode is the gateway's queue/concurrency refusal. It arrives as a business
// code, sometimes under a 401 status, so it must be classified before the status
// is trusted.
const busyCode = "10605"

// errUpstreamUnauthorized marks an authentication failure that a token refresh
// can plausibly fix.
var errUpstreamUnauthorized = fmt.Errorf("qoder upstream rejected the credential")

func stringOfCode(raw string) string {
	return strings.TrimSpace(raw)
}

func newThinkingSignature() string {
	var raw [24]byte
	if _, err := rand.Read(raw[:]); err == nil {
		return "qoder-v1:" + base64.RawURLEncoding.EncodeToString(raw[:])
	}
	return fmt.Sprintf("qoder-v1:%d", time.Now().UnixNano())
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

// normalizeUsage maps the upstream usage object onto the key names the shared
// stream handler consumes, keeping the credit fields under their own names so
// a cost panel can read them.
func normalizeUsage(usage *streamUsage) map[string]interface{} {
	if usage == nil {
		return nil
	}
	out := make(map[string]interface{}, 8)
	if usage.PromptTokens > 0 || usage.CompletionTokens > 0 {
		out["inputTokens"] = usage.PromptTokens
		out["input_tokens"] = usage.PromptTokens
		out["outputTokens"] = usage.CompletionTokens
		out["output_tokens"] = usage.CompletionTokens
	}
	if usage.TotalTokens > 0 {
		out["totalTokens"] = usage.TotalTokens
	}
	if usage.PromptTokensDetails != nil {
		if usage.PromptTokensDetails.CachedTokens > 0 {
			out["cacheReadTokens"] = usage.PromptTokensDetails.CachedTokens
			out["cache_read_tokens"] = usage.PromptTokensDetails.CachedTokens
		}
		if usage.PromptTokensDetails.CacheableTokens > 0 {
			out["cacheWriteTokens"] = usage.PromptTokensDetails.CacheableTokens
			out["cache_creation_input_tokens"] = usage.PromptTokensDetails.CacheableTokens
		}
	}
	if usage.CompletionTokensDetails != nil && usage.CompletionTokensDetails.ReasoningTokens > 0 {
		out["reasoningTokens"] = usage.CompletionTokensDetails.ReasoningTokens
	}
	if usage.Credits != nil {
		out["credits"] = *usage.Credits
	}
	if usage.OriginalCredits != nil {
		out["original_credits"] = *usage.OriginalCredits
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// extractBodyMessage reads the human-readable message out of an error envelope.
func extractBodyMessage(raw []byte) string {
	var envelope struct {
		Body string `json:"body"`
	}
	if err := json.Unmarshal(bytes.TrimSpace(raw), &envelope); err != nil {
		return ""
	}
	var failure struct {
		Message string `json:"message"`
	}
	if err := json.Unmarshal([]byte(envelope.Body), &failure); err != nil {
		return ""
	}
	return strings.TrimSpace(failure.Message)
}
