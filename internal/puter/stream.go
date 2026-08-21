package puter

import (
	"bufio"
	"fmt"
	"io"
	"strings"
	"sync/atomic"
	"time"

	"github.com/goccy/go-json"

	"orchids-api/internal/upstream"
)

type streamResult struct {
	SawMeaningfulEvent bool
	ToolCallCount      int
	Usage              map[string]int
}

func (r streamResult) FinishReason() string {
	if r.ToolCallCount > 0 {
		return "tool_use"
	}
	return "end_turn"
}

var puterToolCallSequence atomic.Uint64

func newToolCallID() string {
	return fmt.Sprintf("toolu_%d_%d", time.Now().UnixNano(), puterToolCallSequence.Add(1))
}

func consumePuterStream(body io.Reader, onMessage func(upstream.SSEMessage)) (streamResult, error) {
	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	result := streamResult{}

	for scanner.Scan() {
		line := normalizePuterStreamLine(scanner.Text())
		if line == "" {
			continue
		}

		var apiErr ErrorResponse
		if err := json.Unmarshal([]byte(line), &apiErr); err == nil && apiErr.Error.Present() {
			return result, formatPuterAPIError(apiErr.Error.AsPayload(), line)
		}

		var chunk StreamChunk
		if err := json.Unmarshal([]byte(line), &chunk); err != nil {
			continue
		}
		switch strings.ToLower(strings.TrimSpace(chunk.Type)) {
		case "text":
			result.SawMeaningfulEvent = true
			if onMessage != nil && chunk.Text != "" {
				onMessage(upstream.SSEMessage{Type: "model.text-delta", Event: map[string]interface{}{"delta": chunk.Text}})
			}
		case "reasoning":
			result.SawMeaningfulEvent = true
			if onMessage != nil && chunk.Reasoning != "" {
				onMessage(upstream.SSEMessage{Type: "model.reasoning-delta", Event: map[string]interface{}{"delta": chunk.Reasoning}})
			}
		case "tool_use":
			name := strings.TrimSpace(chunk.Name)
			if name == "" {
				return result, fmt.Errorf("puter stream returned tool_use without a name")
			}
			id := strings.TrimSpace(chunk.ID)
			if id == "" {
				id = newToolCallID()
			}
			result.SawMeaningfulEvent = true
			result.ToolCallCount++
			if onMessage != nil {
				onMessage(upstream.SSEMessage{Type: "model.tool-call", Event: map[string]interface{}{
					"toolCallId": id,
					"toolName":   name,
					"input":      normalizeStreamToolInput(chunk.Input),
				}})
			}
		case "usage":
			result.SawMeaningfulEvent = true
			result.Usage = normalizePuterUsage(chunk.Usage)
			if onMessage != nil && len(result.Usage) > 0 {
				event := make(map[string]interface{}, len(result.Usage))
				for key, value := range result.Usage {
					event[key] = value
				}
				onMessage(upstream.SSEMessage{Type: "model.tokens-used", Event: event})
			}
		case "error":
			return result, puterStreamError(chunk.Message, line)
		}
	}
	if err := scanner.Err(); err != nil {
		return result, fmt.Errorf("failed to read puter stream: %w", err)
	}
	return result, nil
}

// puterStreamError 把 puter 的错误事件归一成可分类的错误。puter 经常把 HTTP
// 状态码直接拼在 message 开头(如 "400 {json}"),而 errors.ClassifyUpstreamError
// 只识别 status=/HTTP 等模式,裸 "400" 会落成 unknown→可重试→无意义重试甚至换账号。
// 这里把开头的状态码改写为 status=xxx,让分类器正确归类:
// 400→client 不可重试、429→rate_limit、401→auth、5xx→server。
func puterStreamError(message, line string) error {
	message = strings.TrimSpace(message)
	if message == "" {
		message = line
	}
	if code, rest := splitLeadingHTTPStatus(message); code != "" {
		if rest == "" {
			return fmt.Errorf("puter stream error: status=%s", code)
		}
		return fmt.Errorf("puter stream error: status=%s, message=%s", code, rest)
	}
	return fmt.Errorf("puter stream error: %s", message)
}

// splitLeadingHTTPStatus 识别消息开头的三位 HTTP 状态码(如 "400 {...}")。
// 仅当前三位都是数字且后面紧跟空格或冒号时才认定,避免误判普通文本。
func splitLeadingHTTPStatus(message string) (code, rest string) {
	if len(message) < 3 {
		return "", message
	}
	for i := 0; i < 3; i++ {
		if message[i] < '0' || message[i] > '9' {
			return "", message
		}
	}
	if len(message) == 3 {
		return message, ""
	}
	if message[3] == ' ' || message[3] == ':' {
		return message[:3], strings.TrimSpace(message[4:])
	}
	return "", message
}

func normalizePuterStreamLine(line string) string {
	line = strings.TrimSpace(line)
	if strings.HasPrefix(strings.ToLower(line), "data:") {
		line = strings.TrimSpace(line[5:])
	}
	if line == "[DONE]" {
		return ""
	}
	return line
}

func normalizeStreamToolInput(raw json.RawMessage) string {
	return normalizeStreamToolInputDepth(string(raw), 4)
}

// normalizeStreamToolInputDepth 递归归一化上游 tool_use 的 input，最多解开 depth 层包装。
// 兼容三种形态：
//  1. {"query":"..."}            已展开的对象 → 原样返回
//  2. "{\"file_path\":\"...\"}"  整体是 JSON 字符串 → 去掉一层引号再归一化
//  3. {"arguments":"{\"...\"}"}  OpenAI 风格包装 → 解包 arguments 再归一化
//
// 某些 OpenAI 兼容驱动会以 {"arguments":"<json-string>"} 形式返回工具参数，
// 甚至把字符串又包一层字面引号；逐层解开直到稳定为纯 JSON 对象。
func normalizeStreamToolInputDepth(input string, depth int) string {
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
		return normalizeStreamToolInputDepth(text, depth-1)
	}
	if inner, ok := unwrapOpenAIToolArguments(trimmed); ok {
		return normalizeStreamToolInputDepth(inner, depth-1)
	}
	return trimmed
}

// unwrapOpenAIToolArguments 识别 {"arguments":"<json-string>"} 包装并返回其中的 JSON
// 字符串。仅当 arguments 字段存在、值为字符串且该字符串是合法 JSON 时才解包，
// 避免误伤本身就把 arguments 当作普通参数的本地工具。
func unwrapOpenAIToolArguments(input string) (string, bool) {
	var obj map[string]json.RawMessage
	if err := json.Unmarshal([]byte(input), &obj); err != nil {
		return "", false
	}
	rawArgs, ok := obj["arguments"]
	if !ok {
		return "", false
	}
	var s string
	if err := json.Unmarshal(rawArgs, &s); err != nil {
		return "", false
	}
	s = strings.TrimSpace(s)
	if s == "" || s == "null" {
		return "{}", true
	}
	if !json.Valid([]byte(s)) {
		return "", false
	}
	return s, true
}

func normalizePuterUsage(raw map[string]interface{}) map[string]int {
	if len(raw) == 0 {
		return nil
	}
	input, hasInput := firstUsageInt(raw, "inputTokens", "input_tokens", "promptTokens", "prompt_tokens")
	output, hasOutput := firstUsageInt(raw, "outputTokens", "output_tokens", "completionTokens", "completion_tokens")
	if !hasInput && !hasOutput {
		return nil
	}
	out := make(map[string]int, 4)
	if hasInput {
		out["inputTokens"] = input
		out["input_tokens"] = input
	}
	if hasOutput {
		out["outputTokens"] = output
		out["output_tokens"] = output
	}
	return out
}

func firstUsageInt(values map[string]interface{}, keys ...string) (int, bool) {
	for _, key := range keys {
		value, ok := values[key]
		if !ok {
			continue
		}
		switch typed := value.(type) {
		case float64:
			return int(typed), true
		case float32:
			return int(typed), true
		case int:
			return typed, true
		case int64:
			return int(typed), true
		case json.Number:
			parsed, err := typed.Int64()
			if err == nil {
				return int(parsed), true
			}
		}
	}
	return 0, false
}
