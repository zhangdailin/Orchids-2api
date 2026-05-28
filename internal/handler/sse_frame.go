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

func appendSSEMessageStartNoUsage(dst []byte, msgID, model string) ([]byte, error) {
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
	dst = append(dst, `}}`...)
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

func marshalSSEContentBlockStartToolUseBytes(index int, id, name string) ([]byte, error) {
	return appendSSEContentBlockStartToolUse(make([]byte, 0, 128+len(id)+len(name)), index, id, name)
}

func marshalSSEMessageStartBytes(msgID, model string, inputTokens, outputTokens int) ([]byte, error) {
	return appendSSEMessageStart(make([]byte, 0, 192+len(msgID)+len(model)), msgID, model, inputTokens, outputTokens)
}

func marshalSSEMessageStartNoUsageBytes(msgID, model string) ([]byte, error) {
	return appendSSEMessageStartNoUsage(make([]byte, 0, 128+len(msgID)+len(model)), msgID, model)
}

func marshalSSEContentBlockStartToolUse(index int, id, name string) (string, error) {
	raw, err := marshalSSEContentBlockStartToolUseBytes(index, id, name)
	if err != nil {
		return "", err
	}
	return string(raw), nil
}

func marshalSSEContentBlockStartTextBytes(index int) ([]byte, error) {
	return appendSSEContentBlockStartText(make([]byte, 0, 96), index)
}

func marshalSSEContentBlockStartText(index int) (string, error) {
	raw, err := marshalSSEContentBlockStartTextBytes(index)
	if err != nil {
		return "", err
	}
	return string(raw), nil
}

func marshalSSEContentBlockStartThinkingBytes(index int, signature string) ([]byte, error) {
	return appendSSEContentBlockStartThinking(make([]byte, 0, 112+len(signature)), index, signature)
}

func marshalSSEContentBlockStartThinking(index int, signature string) (string, error) {
	raw, err := marshalSSEContentBlockStartThinkingBytes(index, signature)
	if err != nil {
		return "", err
	}
	return string(raw), nil
}

func marshalSSEContentBlockDeltaInputJSONBytes(index int, partialJSON string) ([]byte, error) {
	return appendSSEContentBlockDeltaInputJSON(make([]byte, 0, 96+len(partialJSON)*2), index, partialJSON)
}

func marshalSSEContentBlockDeltaInputJSON(index int, partialJSON string) (string, error) {
	raw, err := marshalSSEContentBlockDeltaInputJSONBytes(index, partialJSON)
	if err != nil {
		return "", err
	}
	return string(raw), nil
}

func marshalSSEContentBlockDeltaTextBytes(index int, text string) ([]byte, error) {
	return appendSSEContentBlockDeltaText(make([]byte, 0, 80+len(text)), index, text)
}

func marshalSSEContentBlockDeltaText(index int, text string) (string, error) {
	raw, err := marshalSSEContentBlockDeltaTextBytes(index, text)
	if err != nil {
		return "", err
	}
	return string(raw), nil
}

func marshalSSEContentBlockDeltaThinkingBytes(index int, thinking string) ([]byte, error) {
	return appendSSEContentBlockDeltaThinking(make([]byte, 0, 88+len(thinking)), index, thinking)
}

func marshalSSEContentBlockDeltaThinking(index int, thinking string) (string, error) {
	raw, err := marshalSSEContentBlockDeltaThinkingBytes(index, thinking)
	if err != nil {
		return "", err
	}
	return string(raw), nil
}

func marshalSSEContentBlockStopBytes(index int) ([]byte, error) {
	return appendSSEContentBlockStop(make([]byte, 0, 48), index)
}

func marshalSSEContentBlockStop(index int) (string, error) {
	raw, err := marshalSSEContentBlockStopBytes(index)
	if err != nil {
		return "", err
	}
	return string(raw), nil
}

func marshalSSEMessageDeltaBytes(stopReason string, outputTokens int) ([]byte, error) {
	return appendSSEMessageDelta(make([]byte, 0, 88+len(stopReason)), stopReason, outputTokens)
}

func marshalSSEMessageDelta(stopReason string, outputTokens int) (string, error) {
	raw, err := marshalSSEMessageDeltaBytes(stopReason, outputTokens)
	if err != nil {
		return "", err
	}
	return string(raw), nil
}

func marshalSSEMessageStopBytes() ([]byte, error) {
	return sseMessageStopBytes, nil
}
