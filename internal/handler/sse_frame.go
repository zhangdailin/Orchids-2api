package handler

import (
	"encoding/json"
	"strconv"
	"unicode/utf8"
)

const (
	jsonHexDigits = "0123456789abcdef"
)

var (
	sseMessageStopBytes = []byte(`{"type":"message_stop"}`)
)

func canAppendJSONRawString(value string) bool {
	for i := 0; i < len(value); {
		b := value[i]
		if b < utf8.RuneSelf {
			if b < 0x20 || b == '\\' || b == '"' || b == '<' || b == '>' || b == '&' {
				return false
			}
			i++
			continue
		}
		r, size := utf8.DecodeRuneInString(value[i:])
		if r == utf8.RuneError && size == 1 {
			return false
		}
		if r == '\u2028' || r == '\u2029' {
			return false
		}
		i += size
	}
	return true
}

func appendJSONBytes(dst []byte, value string) ([]byte, error) {
	if canAppendJSONRawString(value) {
		dst = append(dst, '"')
		dst = append(dst, value...)
		dst = append(dst, '"')
		return dst, nil
	}

	originLen := len(dst)
	dst = append(dst, '"')
	start := 0
	for i := 0; i < len(value); {
		b := value[i]
		if b < utf8.RuneSelf {
			if b >= 0x20 && b != '\\' && b != '"' && b != '<' && b != '>' && b != '&' {
				i++
				continue
			}
			if start < i {
				dst = append(dst, value[start:i]...)
			}
			switch b {
			case '\\', '"':
				dst = append(dst, '\\', b)
			case '\b':
				dst = append(dst, '\\', 'b')
			case '\f':
				dst = append(dst, '\\', 'f')
			case '\n':
				dst = append(dst, '\\', 'n')
			case '\r':
				dst = append(dst, '\\', 'r')
			case '\t':
				dst = append(dst, '\\', 't')
			default:
				dst = append(dst, '\\', 'u', '0', '0')
				dst = append(dst, jsonHexDigits[b>>4], jsonHexDigits[b&0x0f])
			}
			i++
			start = i
			continue
		}
		r, size := utf8.DecodeRuneInString(value[i:])
		if r == utf8.RuneError && size == 1 {
			dst = dst[:originLen]
			quoted, err := json.Marshal(value)
			if err != nil {
				return nil, err
			}
			return append(dst, quoted...), nil
		}
		if r == '\u2028' || r == '\u2029' {
			if start < i {
				dst = append(dst, value[start:i]...)
			}
			if r == '\u2028' {
				dst = append(dst, '\\', 'u', '2', '0', '2', '8')
			} else {
				dst = append(dst, '\\', 'u', '2', '0', '2', '9')
			}
			i += size
			start = i
			continue
		}
		i += size
	}
	if start < len(value) {
		dst = append(dst, value[start:]...)
	}
	dst = append(dst, '"')
	return dst, nil
}

func appendSSEContentBlockStartToolUse(dst []byte, index int, id, name string) ([]byte, error) {
	dst = append(dst, `{"type":"content_block_start","index":`...)
	dst = strconv.AppendInt(dst, int64(index), 10)
	dst = append(dst, `,"content_block":{"type":"tool_use","id":`...)
	var err error
	dst, err = appendJSONBytes(dst, id)
	if err != nil {
		return nil, err
	}
	dst = append(dst, `,"name":`...)
	dst, err = appendJSONBytes(dst, name)
	if err != nil {
		return nil, err
	}
	dst = append(dst, `,"input":{}}}`...)
	return dst, nil
}

func appendSSEMessageStart(dst []byte, msgID, model string, inputTokens, outputTokens int) ([]byte, error) {
	dst = append(dst, `{"type":"message_start","message":{"id":`...)
	var err error
	dst, err = appendJSONBytes(dst, msgID)
	if err != nil {
		return nil, err
	}
	dst = append(dst, `,"type":"message","role":"assistant","content":[],"model":`...)
	dst, err = appendJSONBytes(dst, model)
	if err != nil {
		return nil, err
	}
	dst = append(dst, `,"usage":{"input_tokens":`...)
	dst = strconv.AppendInt(dst, int64(inputTokens), 10)
	dst = append(dst, `,"output_tokens":`...)
	dst = strconv.AppendInt(dst, int64(outputTokens), 10)
	dst = append(dst, `}}}`...)
	return dst, nil
}

func appendSSEContentBlockStartText(dst []byte, index int) ([]byte, error) {
	dst = append(dst, `{"type":"content_block_start","index":`...)
	dst = strconv.AppendInt(dst, int64(index), 10)
	dst = append(dst, `,"content_block":{"type":"text","text":""}}`...)
	return dst, nil
}

func appendSSEContentBlockStartThinking(dst []byte, index int, signature string) ([]byte, error) {
	dst = append(dst, `{"type":"content_block_start","index":`...)
	dst = strconv.AppendInt(dst, int64(index), 10)
	dst = append(dst, `,"content_block":{"type":"thinking","thinking":"","signature":`...)
	var err error
	dst, err = appendJSONBytes(dst, signature)
	if err != nil {
		return nil, err
	}
	dst = append(dst, `}}`...)
	return dst, nil
}

func appendSSEContentBlockDeltaInputJSON(dst []byte, index int, partialJSON string) ([]byte, error) {
	dst = append(dst, `{"type":"content_block_delta","index":`...)
	dst = strconv.AppendInt(dst, int64(index), 10)
	dst = append(dst, `,"delta":{"type":"input_json_delta","partial_json":`...)
	var err error
	dst, err = appendJSONBytes(dst, partialJSON)
	if err != nil {
		return nil, err
	}
	dst = append(dst, `}}`...)
	return dst, nil
}

func appendSSEContentBlockDeltaText(dst []byte, index int, text string) ([]byte, error) {
	dst = append(dst, `{"type":"content_block_delta","index":`...)
	dst = strconv.AppendInt(dst, int64(index), 10)
	dst = append(dst, `,"delta":{"type":"text_delta","text":`...)
	var err error
	dst, err = appendJSONBytes(dst, text)
	if err != nil {
		return nil, err
	}
	dst = append(dst, `}}`...)
	return dst, nil
}

func appendSSEContentBlockDeltaThinking(dst []byte, index int, thinking string) ([]byte, error) {
	dst = append(dst, `{"type":"content_block_delta","index":`...)
	dst = strconv.AppendInt(dst, int64(index), 10)
	dst = append(dst, `,"delta":{"type":"thinking_delta","thinking":`...)
	var err error
	dst, err = appendJSONBytes(dst, thinking)
	if err != nil {
		return nil, err
	}
	dst = append(dst, `}}`...)
	return dst, nil
}

func appendSSEContentBlockStop(dst []byte, index int) ([]byte, error) {
	dst = append(dst, `{"type":"content_block_stop","index":`...)
	dst = strconv.AppendInt(dst, int64(index), 10)
	dst = append(dst, '}')
	return dst, nil
}

func appendSSEMessageDelta(dst []byte, stopReason string, outputTokens int) ([]byte, error) {
	dst = append(dst, `{"type":"message_delta","delta":{"stop_reason":`...)
	var err error
	dst, err = appendJSONBytes(dst, stopReason)
	if err != nil {
		return nil, err
	}
	dst = append(dst, `},"usage":{"output_tokens":`...)
	dst = strconv.AppendInt(dst, int64(outputTokens), 10)
	dst = append(dst, `}}`...)
	return dst, nil
}

func marshalSSEMessageStartBytes(msgID, model string, inputTokens, outputTokens int) ([]byte, error) {
	return appendSSEMessageStart(make([]byte, 0, 192+len(msgID)+len(model)), msgID, model, inputTokens, outputTokens)
}

func marshalSSEContentBlockStartTextBytes(index int) ([]byte, error) {
	return appendSSEContentBlockStartText(make([]byte, 0, 96), index)
}

func marshalSSEContentBlockDeltaTextBytes(index int, text string) ([]byte, error) {
	return appendSSEContentBlockDeltaText(make([]byte, 0, 80+len(text)), index, text)
}

func marshalSSEContentBlockStopBytes(index int) ([]byte, error) {
	return appendSSEContentBlockStop(make([]byte, 0, 48), index)
}

func marshalSSEMessageDeltaBytes(stopReason string, outputTokens int) ([]byte, error) {
	return appendSSEMessageDelta(make([]byte, 0, 88+len(stopReason)), stopReason, outputTokens)
}

func marshalSSEMessageStopBytes() ([]byte, error) {
	return sseMessageStopBytes, nil
}

// appendAnthropicError renders the Anthropic protocol's in-band failure event.
//
// The wire protocol carries a failure as `event: error` with an
// {"type":"error","error":{...}} payload. It is the only report available once a
// stream has sent its message_start: the HTTP status is already 200 and can never
// be revisited, so a stream that invents an assistant message saying "the accounts
// have exhausted their quota" is indistinguishable from one that answered.
func appendAnthropicError(dst []byte, code, message string) ([]byte, error) {
	dst = append(dst, `{"type":"error","error":{"type":`...)
	var err error
	if dst, err = appendJSONBytes(dst, code); err != nil {
		return nil, err
	}
	dst = append(dst, `,"message":`...)
	if dst, err = appendJSONBytes(dst, message); err != nil {
		return nil, err
	}
	dst = append(dst, `}}`...)
	return dst, nil
}

func marshalAnthropicErrorBytes(code, message string) ([]byte, error) {
	return appendAnthropicError(make([]byte, 0, 64+len(code)+len(message)), code, message)
}

// appendOpenAIError renders the failure an OpenAI-compatible stream client looks
// for: a single data frame whose payload is an error object. The terminal [DONE]
// sentinel follows it, so a client that reads to the end of the stream terminates
// instead of waiting for a chunk that will never come.
func appendOpenAIError(dst []byte, code, message string) ([]byte, error) {
	dst = append(dst, `{"error":{"message":`...)
	var err error
	if dst, err = appendJSONBytes(dst, message); err != nil {
		return nil, err
	}
	dst = append(dst, `,"type":`...)
	if dst, err = appendJSONBytes(dst, code); err != nil {
		return nil, err
	}
	dst = append(dst, `,"code":`...)
	if dst, err = appendJSONBytes(dst, code); err != nil {
		return nil, err
	}
	dst = append(dst, `}}`...)
	return dst, nil
}

func marshalOpenAIErrorBytes(code, message string) ([]byte, error) {
	return appendOpenAIError(make([]byte, 0, 96+len(code)+len(message)), code, message)
}
