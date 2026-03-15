package warp

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"hash/fnv"
	"io"
	"math"
	"path/filepath"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/goccy/go-json"
	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/structpb"

	"orchids-api/internal/debug"
	"orchids-api/internal/orchids"
	"orchids-api/internal/upstream"
)

type toolCall struct {
	ID    string
	Name  string
	Input string
}

type finishInfo struct {
	InputTokens  int
	OutputTokens int
}

type nonProtobufStreamError struct {
	Kind    string
	Preview string
}

func (e *nonProtobufStreamError) Error() string {
	if e == nil {
		return "warp returned non-protobuf response"
	}
	if e.Preview == "" {
		return fmt.Sprintf("warp returned non-protobuf %s response", e.Kind)
	}
	return fmt.Sprintf("warp returned non-protobuf %s response: %s", e.Kind, e.Preview)
}

type decoder struct {
	data []byte
	pos  int
}

func (d *decoder) eof() bool {
	return d.pos >= len(d.data)
}

func (d *decoder) readVarint() (uint64, error) {
	var x uint64
	var s uint
	for {
		if d.pos >= len(d.data) {
			return 0, io.ErrUnexpectedEOF
		}
		b := d.data[d.pos]
		d.pos++
		if b < 0x80 {
			if s >= 64 || (s == 63 && b > 1) {
				return 0, errors.New("varint overflow")
			}
			return x | uint64(b)<<s, nil
		}
		x |= uint64(b&0x7f) << s
		s += 7
		if s > 64 {
			return 0, errors.New("varint overflow")
		}
	}
}

func (d *decoder) readKey() (int, int, error) {
	v, err := d.readVarint()
	if err != nil {
		return 0, 0, err
	}
	return int(v >> 3), int(v & 0x7), nil
}

func (d *decoder) readBytes() ([]byte, error) {
	l, err := d.readVarint()
	if err != nil {
		return nil, err
	}
	if l > math.MaxInt32 {
		return nil, errors.New("length overflow")
	}
	length := int(l)
	if d.pos+length > len(d.data) {
		return nil, io.ErrUnexpectedEOF
	}
	b := d.data[d.pos : d.pos+length]
	d.pos += length
	return b, nil
}

func (d *decoder) skip(wire int) error {
	switch wire {
	case 0:
		_, err := d.readVarint()
		return err
	case 1:
		if d.pos+8 > len(d.data) {
			return io.ErrUnexpectedEOF
		}
		d.pos += 8
		return nil
	case 2:
		_, err := d.readBytes()
		return err
	case 5:
		if d.pos+4 > len(d.data) {
			return io.ErrUnexpectedEOF
		}
		d.pos += 4
		return nil
	default:
		return fmt.Errorf("unsupported wire type %d", wire)
	}
}

type parsedEvent struct {
	ConversationID  string
	TextDeltas      []string
	ReasoningDeltas []string
	ToolCalls       []toolCall
	Finish          *finishInfo
	Error           string
}

func (e *parsedEvent) hasSignals() bool {
	if e == nil {
		return false
	}
	return strings.TrimSpace(e.ConversationID) != "" ||
		len(e.TextDeltas) > 0 ||
		len(e.ReasoningDeltas) > 0 ||
		len(e.ToolCalls) > 0 ||
		e.Finish != nil ||
		strings.TrimSpace(e.Error) != ""
}

func processStreamBody(ctx context.Context, reader io.Reader, onMessage func(upstream.SSEMessage), logger *debug.Logger) error {
	if closer, ok := reader.(io.Closer); ok {
		done := make(chan struct{})
		go func() {
			select {
			case <-ctx.Done():
				_ = closer.Close()
			case <-done:
			}
		}()
		defer close(done)
	}

	br := bufio.NewReaderSize(reader, 64*1024)
	sawFrame := false
	sawToolCall := false
	toolCallIndex := 0

	for {
		frame, err := readFrame(br)
		if err != nil {
			var nonProtoErr *nonProtobufStreamError
			if errors.As(err, &nonProtoErr) && nonProtoErr.Kind == "sse" {
				return processSSEStreamBody(ctx, br, onMessage, logger)
			}
			if err == io.EOF {
				break
			}
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return err
		}
		sawFrame = true
		if logger != nil {
			logger.LogUpstreamSSE("warp_frame", fmt.Sprintf("bytes=%d", len(frame)))
		}
		_, done, err := emitWarpPayload(frame, onMessage, &sawToolCall, &toolCallIndex)
		if err != nil {
			return err
		}
		if done {
			return nil
		}
	}

	if !sawFrame {
		return fmt.Errorf("warp stream ended without protobuf frames")
	}

	reason := "end_turn"
	if sawToolCall {
		reason = "tool_use"
	}
	onMessage(upstream.SSEMessage{
		Type:  "model.finish",
		Event: map[string]interface{}{"finishReason": reason},
	})
	return nil
}

func processSSEStreamBody(ctx context.Context, reader *bufio.Reader, onMessage func(upstream.SSEMessage), logger *debug.Logger) error {
	var dataBuilder strings.Builder
	dataEventCount := 0
	parsedEventCount := 0
	sawToolCall := false
	toolCallIndex := 0
	finishSent := false

	flush := func() error {
		if dataBuilder.Len() == 0 {
			return nil
		}
		data := dataBuilder.String()
		dataBuilder.Reset()
		dataEventCount++
		if logger != nil {
			logger.LogUpstreamSSE("warp_data", data)
		}

		payloadBytes, err := decodeWarpPayload(data)
		if err != nil {
			if logger != nil {
				logger.LogUpstreamSSE("warp_decode_error", err.Error())
			}
			return nil
		}

		handled, done, err := emitWarpPayload(payloadBytes, onMessage, &sawToolCall, &toolCallIndex)
		if err != nil {
			return err
		}
		if handled {
			parsedEventCount++
		}
		if done {
			finishSent = true
		}
		return nil
	}

	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		line, err := reader.ReadString('\n')
		if err != nil {
			if errors.Is(err, io.EOF) {
				if flushErr := flush(); flushErr != nil {
					return flushErr
				}
				break
			}
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return err
		}

		line = strings.TrimRight(line, "\r\n")
		if line == "" {
			if err := flush(); err != nil {
				return err
			}
			if finishSent {
				return nil
			}
			continue
		}
		if strings.HasPrefix(line, ":") {
			continue
		}
		if strings.HasPrefix(line, "data:") {
			dataBuilder.WriteString(strings.TrimSpace(line[5:]))
		}
	}

	if dataEventCount == 0 {
		return fmt.Errorf("warp stream ended without any SSE data events")
	}
	if parsedEventCount == 0 {
		return fmt.Errorf("warp stream received %d SSE data events but none parsed", dataEventCount)
	}
	if !finishSent {
		reason := "end_turn"
		if sawToolCall {
			reason = "tool_use"
		}
		onMessage(upstream.SSEMessage{
			Type:  "model.finish",
			Event: map[string]interface{}{"finishReason": reason},
		})
	}
	return nil
}

func decodeWarpPayload(data string) ([]byte, error) {
	if data == "" {
		return nil, fmt.Errorf("empty payload")
	}
	if decoded, err := base64.RawURLEncoding.DecodeString(data); err == nil {
		return decoded, nil
	}
	if decoded, err := base64.URLEncoding.DecodeString(data); err == nil {
		return decoded, nil
	}
	return base64.StdEncoding.DecodeString(data)
}

func emitWarpPayload(frame []byte, onMessage func(upstream.SSEMessage), sawToolCall *bool, toolCallIndex *int) (bool, bool, error) {
	if parsed, err := parseResponseEvent(frame); err == nil && parsed.hasSignals() {
		if parsed.ConversationID != "" {
			onMessage(upstream.SSEMessage{
				Type:  "model.conversation_id",
				Event: map[string]interface{}{"id": parsed.ConversationID},
			})
		}
		for _, delta := range parsed.TextDeltas {
			if strings.TrimSpace(delta) == "" {
				continue
			}
			onMessage(upstream.SSEMessage{
				Type:  "model.text-delta",
				Event: map[string]interface{}{"delta": delta},
			})
		}
		for _, delta := range parsed.ReasoningDeltas {
			if strings.TrimSpace(delta) == "" {
				continue
			}
			onMessage(upstream.SSEMessage{
				Type:  "model.reasoning-delta",
				Event: map[string]interface{}{"delta": delta},
			})
		}
		for _, call := range parsed.ToolCalls {
			*sawToolCall = true
			onMessage(upstream.SSEMessage{
				Type: "model.tool-call",
				Event: map[string]interface{}{
					"toolCallId": call.ID,
					"toolName":   call.Name,
					"input":      call.Input,
				},
			})
		}
		if parsed.Error != "" {
			return true, false, fmt.Errorf("warp stream error: %s", parsed.Error)
		}
		if parsed.Finish != nil {
			finish := map[string]interface{}{
				"finishReason": "end_turn",
			}
			if *sawToolCall {
				finish["finishReason"] = "tool_use"
			}
			if parsed.Finish.InputTokens > 0 || parsed.Finish.OutputTokens > 0 {
				finish["usage"] = map[string]interface{}{
					"inputTokens":  parsed.Finish.InputTokens,
					"outputTokens": parsed.Finish.OutputTokens,
				}
			}
			onMessage(upstream.SSEMessage{Type: "model.finish", Event: finish})
			return true, true, nil
		}
		return true, false, nil
	}

	fields := parseRawProtobuf(frame)
	handled := false

	for _, value := range fields[1] {
		if text := extractStringValue(value); text != "" {
			handled = true
			onMessage(upstream.SSEMessage{
				Type:  "model.text-delta",
				Event: map[string]interface{}{"delta": text},
			})
		}
	}

	for _, value := range fields[2] {
		if text := extractStringValue(value); text != "" {
			handled = true
			onMessage(upstream.SSEMessage{
				Type:  "model.reasoning-delta",
				Event: map[string]interface{}{"delta": text},
			})
		}
	}

	for _, value := range fields[3] {
		payload, ok := value.([]byte)
		if !ok || len(payload) == 0 {
			continue
		}
		calls := parseToolCalls(parseRawProtobuf(payload), *toolCallIndex)
		for _, call := range calls {
			handled = true
			*sawToolCall = true
			*toolCallIndex = *toolCallIndex + 1
			onMessage(upstream.SSEMessage{
				Type: "model.tool-call",
				Event: map[string]interface{}{
					"toolCallId": call.ID,
					"toolName":   call.Name,
					"input":      call.Input,
				},
			})
		}
	}

	if errMsg := firstNonEmptyString(fields[6]); errMsg != "" {
		return true, false, fmt.Errorf("warp stream error: %s", errMsg)
	}

	if isDone(fields[4]) {
		finish := map[string]interface{}{
			"finishReason": "end_turn",
		}
		if *sawToolCall {
			finish["finishReason"] = "tool_use"
		}
		if usage := parseUsage(fields[5]); usage != nil {
			finish["usage"] = map[string]interface{}{
				"inputTokens":  usage.InputTokens,
				"outputTokens": usage.OutputTokens,
			}
		}
		onMessage(upstream.SSEMessage{Type: "model.finish", Event: finish})
		return true, true, nil
	}

	return handled, false, nil
}

func readFrame(reader *bufio.Reader) ([]byte, error) {
	header, err := reader.Peek(4)
	if err != nil {
		return nil, err
	}
	size := binary.BigEndian.Uint32(header)
	if size == 0 {
		if _, err := reader.Discard(4); err != nil {
			return nil, err
		}
		return nil, io.EOF
	}
	if size > 16*1024*1024 {
		if preview, kind, ok := sniffNonProtobufResponse(reader); ok {
			return nil, &nonProtobufStreamError{
				Kind:    kind,
				Preview: preview,
			}
		}
		return nil, fmt.Errorf("warp protobuf frame too large: %d", size)
	}
	if _, err := reader.Discard(4); err != nil {
		return nil, err
	}
	frame := make([]byte, size)
	if _, err := io.ReadFull(reader, frame); err != nil {
		return nil, err
	}
	return frame, nil
}

func parseResponseEvent(data []byte) (*parsedEvent, error) {
	d := decoder{data: data}
	out := &parsedEvent{}
	for !d.eof() {
		field, wire, err := d.readKey()
		if err != nil {
			return out, err
		}
		switch field {
		case 1:
			if wire != 2 {
				if err := d.skip(wire); err != nil {
					return out, err
				}
				continue
			}
			payload, err := d.readBytes()
			if err != nil {
				return out, err
			}
			parseStreamInit(payload, out)
		case 2:
			if wire != 2 {
				if err := d.skip(wire); err != nil {
					return out, err
				}
				continue
			}
			payload, err := d.readBytes()
			if err != nil {
				return out, err
			}
			parseClientActions(payload, out)
		case 3:
			if wire != 2 {
				if err := d.skip(wire); err != nil {
					return out, err
				}
				continue
			}
			payload, err := d.readBytes()
			if err != nil {
				return out, err
			}
			parseNestedStreamFinished(payload, out)
		case 4:
			if wire != 2 {
				if err := d.skip(wire); err != nil {
					return out, err
				}
				continue
			}
			payload, err := d.readBytes()
			if err != nil {
				return out, err
			}
			out.Error = parseNestedStreamError(payload)
		default:
			if err := d.skip(wire); err != nil {
				return out, err
			}
		}
	}
	return out, nil
}

func parseStreamInit(data []byte, out *parsedEvent) {
	d := decoder{data: data}
	for !d.eof() {
		field, wire, err := d.readKey()
		if err != nil {
			return
		}
		if field == 1 && wire == 2 {
			payload, err := d.readBytes()
			if err != nil {
				return
			}
			out.ConversationID = string(payload)
			continue
		}
		_ = d.skip(wire)
	}
}

func parseClientActions(data []byte, out *parsedEvent) {
	d := decoder{data: data}
	for !d.eof() {
		field, wire, err := d.readKey()
		if err != nil {
			return
		}
		if field == 1 && wire == 2 {
			payload, err := d.readBytes()
			if err != nil {
				return
			}
			parseClientAction(payload, out)
			continue
		}
		_ = d.skip(wire)
	}
}

func parseClientAction(data []byte, out *parsedEvent) {
	d := decoder{data: data}
	for !d.eof() {
		field, wire, err := d.readKey()
		if err != nil {
			return
		}
		switch field {
		case 1:
			if wire != 2 {
				_ = d.skip(wire)
				continue
			}
			payload, err := d.readBytes()
			if err != nil {
				return
			}
			parseCreateTask(payload, out)
		case 3:
			if wire != 2 {
				_ = d.skip(wire)
				continue
			}
			payload, err := d.readBytes()
			if err != nil {
				return
			}
			parseAddMessages(payload, out)
		case 4:
			if wire != 2 {
				_ = d.skip(wire)
				continue
			}
			payload, err := d.readBytes()
			if err != nil {
				return
			}
			parseUpdateTaskMessage(payload, out)
		case 5:
			if wire != 2 {
				_ = d.skip(wire)
				continue
			}
			payload, err := d.readBytes()
			if err != nil {
				return
			}
			parseAppendToMessage(payload, out)
		default:
			_ = d.skip(wire)
		}
	}
}

func parseAppendToMessage(data []byte, out *parsedEvent) {
	d := decoder{data: data}
	for !d.eof() {
		field, wire, err := d.readKey()
		if err != nil {
			return
		}
		if field == 1 && wire == 2 {
			payload, err := d.readBytes()
			if err != nil {
				return
			}
			parseMessage(payload, out)
			continue
		}
		_ = d.skip(wire)
	}
}

func parseAddMessages(data []byte, out *parsedEvent) {
	d := decoder{data: data}
	for !d.eof() {
		field, wire, err := d.readKey()
		if err != nil {
			return
		}
		if field == 2 && wire == 2 {
			payload, err := d.readBytes()
			if err != nil {
				return
			}
			parseMessage(payload, out)
			continue
		}
		_ = d.skip(wire)
	}
}

func parseUpdateTaskMessage(data []byte, out *parsedEvent) {
	d := decoder{data: data}
	for !d.eof() {
		field, wire, err := d.readKey()
		if err != nil {
			return
		}
		if field == 1 && wire == 2 {
			payload, err := d.readBytes()
			if err != nil {
				return
			}
			parseMessage(payload, out)
			continue
		}
		_ = d.skip(wire)
	}
}

func parseCreateTask(data []byte, out *parsedEvent) {
	d := decoder{data: data}
	for !d.eof() {
		field, wire, err := d.readKey()
		if err != nil {
			return
		}
		if field == 1 && wire == 2 {
			payload, err := d.readBytes()
			if err != nil {
				return
			}
			parseTask(payload, out)
			continue
		}
		_ = d.skip(wire)
	}
}

func parseTask(data []byte, out *parsedEvent) {
	d := decoder{data: data}
	for !d.eof() {
		field, wire, err := d.readKey()
		if err != nil {
			return
		}
		if field == 5 && wire == 2 {
			payload, err := d.readBytes()
			if err != nil {
				return
			}
			parseMessage(payload, out)
			continue
		}
		_ = d.skip(wire)
	}
}

func parseMessage(data []byte, out *parsedEvent) {
	d := decoder{data: data}
	for !d.eof() {
		field, wire, err := d.readKey()
		if err != nil {
			return
		}
		switch field {
		case 3:
			if wire != 2 {
				_ = d.skip(wire)
				continue
			}
			payload, err := d.readBytes()
			if err != nil {
				return
			}
			parseAgentOutput(payload, out)
		case 4:
			if wire != 2 {
				_ = d.skip(wire)
				continue
			}
			payload, err := d.readBytes()
			if err != nil {
				return
			}
			parseNestedToolCall(payload, out)
		default:
			_ = d.skip(wire)
		}
	}
}

func parseAgentOutput(data []byte, out *parsedEvent) {
	d := decoder{data: data}
	for !d.eof() {
		field, wire, err := d.readKey()
		if err != nil {
			return
		}
		if wire != 2 {
			_ = d.skip(wire)
			continue
		}
		payload, err := d.readBytes()
		if err != nil {
			return
		}
		switch field {
		case 1:
			out.TextDeltas = append(out.TextDeltas, string(payload))
		case 2:
			out.ReasoningDeltas = append(out.ReasoningDeltas, string(payload))
		}
	}
}

func parseNestedToolCall(data []byte, out *parsedEvent) {
	d := decoder{data: data}
	toolID := ""
	toolName := ""
	toolInput := ""
	for !d.eof() {
		field, wire, err := d.readKey()
		if err != nil {
			return
		}
		switch field {
		case 1:
			if wire != 2 {
				_ = d.skip(wire)
				continue
			}
			payload, err := d.readBytes()
			if err != nil {
				return
			}
			toolID = string(payload)
		case 12:
			if wire != 2 {
				_ = d.skip(wire)
				continue
			}
			payload, err := d.readBytes()
			if err != nil {
				return
			}
			name, input := parseCallMCPTool(payload)
			if name != "" {
				toolName = name
				toolInput = input
			}
		case 5:
			if wire != 2 {
				_ = d.skip(wire)
				continue
			}
			payload, err := d.readBytes()
			if err != nil {
				return
			}
			resolvedName, resolvedInput := parseFallbackToolInput("read_files", payload)
			if resolvedInput != "" && resolvedInput != "{}" {
				toolName = resolvedName
				toolInput = resolvedInput
			}
		default:
			if toolName == "" && wire == 2 {
				payload, err := d.readBytes()
				if err != nil {
					return
				}
				fallbackName := fallbackToolName(field)
				resolvedName, resolvedInput := parseFallbackToolInput(fallbackName, payload)
				if strings.TrimSpace(resolvedName) == "" {
					resolvedName = fallbackName
				}
				toolName = resolvedName
				toolInput = resolvedInput
				if toolInput == "" {
					toolInput = "{}"
				}
				continue
			}
			_ = d.skip(wire)
		}
	}
	if toolName == "" {
		return
	}
	toolName = orchids.NormalizeToolNameFallback(toolName)
	toolInput = normalizeToolInputForToolName(toolName, toolInput)
	if isIncompleteToolCall(toolName, toolInput) {
		return
	}
	if toolID == "" {
		toolID = nestedFallbackToolCallID(toolName, toolInput)
	}
	out.ToolCalls = append(out.ToolCalls, toolCall{ID: toolID, Name: toolName, Input: toolInput})
}

func sniffNonProtobufResponse(reader *bufio.Reader) (preview string, kind string, ok bool) {
	const maxPeek = 256
	peeked, err := reader.Peek(maxPeek)
	if err != nil && len(peeked) == 0 {
		return "", "", false
	}

	text := sanitizeResponsePreview(peeked)
	if text == "" {
		return "", "", false
	}
	lower := strings.ToLower(text)

	switch {
	case strings.HasPrefix(lower, "<!doctype"), strings.HasPrefix(lower, "<html"), strings.HasPrefix(lower, "<?xml"), strings.HasPrefix(lower, "<head"), strings.HasPrefix(lower, "<body"):
		return text, "html", true
	case strings.HasPrefix(lower, "data:"), strings.HasPrefix(lower, "event:"), strings.HasPrefix(lower, ":"):
		return text, "sse", true
	case strings.HasPrefix(lower, "{"), strings.HasPrefix(lower, "["):
		return text, "json", true
	case looksLikeDisplayStringBytes(peeked):
		return text, "text", true
	default:
		return "", "", false
	}
}

func sanitizeResponsePreview(data []byte) string {
	if len(data) == 0 {
		return ""
	}
	text := strings.ToValidUTF8(string(data), "")
	text = strings.TrimSpace(text)
	if text == "" {
		return ""
	}
	text = strings.NewReplacer(
		"\r", "\\r",
		"\n", "\\n",
		"\t", "\\t",
	).Replace(text)
	if len(text) > 160 {
		text = text[:160] + "..."
	}
	return text
}



func looksLikeDisplayStringBytes(data []byte) bool {
	if len(data) == 0 || !utf8.Valid(data) {
		return false
	}
	printable := 0
	total := 0
	for len(data) > 0 {
		r, size := utf8.DecodeRune(data)
		data = data[size:]
		total++
		switch {
		case r == utf8.RuneError && size == 1:
			return false
		case r == 0:
			return false
		case r == '\n' || r == '\r' || r == '\t':
			printable++
		case r >= 0x20:
			printable++
		}
	}
	return total > 0 && printable*100/total >= 85
}

func parseRawProtobuf(data []byte) map[uint32][]interface{} {
	result := make(map[uint32][]interface{})
	for len(data) > 0 {
		num, typ, n := protowire.ConsumeTag(data)
		if n < 0 {
			return result
		}
		data = data[n:]

		switch typ {
		case protowire.VarintType:
			v, n := protowire.ConsumeVarint(data)
			if n < 0 {
				return result
			}
			result[uint32(num)] = append(result[uint32(num)], v)
			data = data[n:]
		case protowire.Fixed32Type:
			v, n := protowire.ConsumeFixed32(data)
			if n < 0 {
				return result
			}
			result[uint32(num)] = append(result[uint32(num)], v)
			data = data[n:]
		case protowire.Fixed64Type:
			v, n := protowire.ConsumeFixed64(data)
			if n < 0 {
				return result
			}
			result[uint32(num)] = append(result[uint32(num)], v)
			data = data[n:]
		case protowire.BytesType:
			v, n := protowire.ConsumeBytes(data)
			if n < 0 {
				return result
			}
			result[uint32(num)] = append(result[uint32(num)], v)
			data = data[n:]
		default:
			return result
		}
	}
	return result
}

func extractStringValue(v interface{}) string {
	switch t := v.(type) {
	case []byte:
		if !looksLikeDisplayStringBytes(t) {
			return ""
		}
		return strings.TrimSpace(strings.ToValidUTF8(string(t), ""))
	case string:
		if !looksLikeDisplayStringBytes([]byte(t)) {
			return ""
		}
		return strings.TrimSpace(strings.ToValidUTF8(t, ""))
	default:
		return ""
	}
}

func firstNonEmptyString(values []interface{}) string {
	for _, value := range values {
		if text := extractStringValue(value); text != "" {
			return text
		}
	}
	return ""
}

func isDone(values []interface{}) bool {
	for _, value := range values {
		if flag, ok := value.(uint64); ok && flag == 1 {
			return true
		}
	}
	return false
}

func parseUsage(values []interface{}) *finishInfo {
	for _, value := range values {
		payload, ok := value.([]byte)
		if !ok || len(payload) == 0 {
			continue
		}
		fields := parseRawProtobuf(payload)
		usage := &finishInfo{}
		if len(fields[1]) > 0 {
			if n, ok := fields[1][0].(uint64); ok {
				usage.InputTokens = int(n)
			}
		}
		if len(fields[2]) > 0 {
			if n, ok := fields[2][0].(uint64); ok {
				usage.OutputTokens = int(n)
			}
		}
		return usage
	}
	return nil
}

func parseToolCalls(fields map[uint32][]interface{}, index int) []toolCall {
	name := firstNonEmptyString(fields[1])
	args := firstNonEmptyString(fields[2])
	callID := firstNonEmptyString(fields[3])
	if name == "" {
		return nil
	}
	return mapWarpToolCalls(name, args, callID, index)
}

func mapWarpToolCalls(name, rawArgs, callID string, index int) []toolCall {
	args := map[string]interface{}{}
	if strings.TrimSpace(rawArgs) != "" {
		if err := json.Unmarshal([]byte(rawArgs), &args); err != nil {
			args["raw"] = strings.TrimSpace(rawArgs)
		}
	}

	baseName, transformed := transformWarpToolCall(name, args)
	if strings.EqualFold(name, "read_files") {
		paths := extractReadPaths(args)
		if len(paths) > 1 {
			calls := make([]toolCall, 0, len(paths))
			for i, path := range paths {
				input := map[string]interface{}{"file_path": path}
				if start, ok := args["start"]; ok {
					input["offset"] = start
				}
				if end, ok := args["end"]; ok {
					input["limit"] = end
				}
				calls = append(calls, toolCall{
					ID:    derivedCallID(callID, name, index+i),
					Name:  fmt.Sprintf("%s_%d", baseName, i),
					Input: marshalToolInput(input),
				})
			}
			return calls
		}
	}

	return []toolCall{{
		ID:    derivedCallID(callID, name, index),
		Name:  baseName,
		Input: marshalToolInput(transformed),
	}}
}

func transformWarpToolCall(name string, args map[string]interface{}) (string, map[string]interface{}) {
	baseName := name
	if mapped, ok := warpToClientToolMap[name]; ok {
		baseName = mapped
	}
	baseName = orchids.NormalizeToolNameFallback(baseName)

	out := map[string]interface{}{}
	switch name {
	case "grep":
		copyIfPresent(out, "pattern", args, "pattern")
		copyIfPresent(out, "path", args, "path")
		copyIfPresent(out, "glob", args, "include")
	case "file_glob":
		copyIfPresent(out, "pattern", args, "pattern")
		copyIfPresent(out, "path", args, "path")
	case "read_files":
		if paths := extractReadPaths(args); len(paths) > 0 {
			out["file_path"] = paths[0]
		}
		copyIfPresent(out, "offset", args, "start")
		copyIfPresent(out, "limit", args, "end")
	case "edit_file":
		copyStringIfPresent(out, "file_path", args, "path", sanitizeFileName)
		copyStringIfPresent(out, "old_string", args, "old_string", stripLineNumberPrefixes)
		copyStringIfPresent(out, "new_string", args, "new_string", stripLineNumberPrefixes)
	case "write_file", "create_file":
		copyStringIfPresent(out, "file_path", args, "path", sanitizeFileName)
		copyStringIfPresent(out, "content", args, "content", stripLineNumberPrefixes)
	case "run_command":
		copyIfPresent(out, "command", args, "command")
		copyIfPresent(out, "timeout", args, "timeout")
	case "list_directory":
		copyIfPresent(out, "path", args, "path")
	case "subagent":
		for k, v := range args {
			out[k] = v
		}
	default:
		for k, v := range args {
			out[k] = v
		}
	}

	if len(out) == 0 {
		out = map[string]interface{}{}
	}
	return baseName, out
}

func copyIfPresent(dst map[string]interface{}, dstKey string, src map[string]interface{}, srcKey string) {
	if value, ok := src[srcKey]; ok {
		dst[dstKey] = value
	}
}

func copyStringIfPresent(dst map[string]interface{}, dstKey string, src map[string]interface{}, srcKey string, transform func(string) string) {
	value, ok := src[srcKey]
	if !ok {
		return
	}

	text := fmt.Sprintf("%v", value)
	if transform != nil {
		text = transform(text)
	}
	dst[dstKey] = text
}

func extractReadPaths(args map[string]interface{}) []string {
	var out []string
	appendPath := func(value interface{}) {
		if path := extractSinglePath(value); path != "" {
			out = append(out, path)
		}
	}

	if value, ok := args["paths"]; ok {
		switch paths := value.(type) {
		case []interface{}:
			for _, path := range paths {
				appendPath(path)
			}
		default:
			appendPath(paths)
		}
	}
	if len(out) == 0 {
		appendPath(args["path"])
		appendPath(args["file_path"])
	}

	seen := map[string]struct{}{}
	uniq := out[:0]
	for _, path := range out {
		if _, ok := seen[path]; ok {
			continue
		}
		seen[path] = struct{}{}
		uniq = append(uniq, path)
	}
	return uniq
}

func extractSinglePath(value interface{}) string {
	switch v := value.(type) {
	case string:
		return strings.TrimSpace(v)
	default:
		return ""
	}
}

func derivedCallID(callID, toolName string, index int) string {
	callID = strings.TrimSpace(callID)
	if callID != "" {
		if index <= 0 {
			return callID
		}
		return fmt.Sprintf("%s_%d", callID, index)
	}
	name := strings.ToLower(strings.ReplaceAll(strings.TrimSpace(toolName), " ", "_"))
	if name == "" {
		name = "tool"
	}
	return fmt.Sprintf("call_%s_%d", name, index)
}

func marshalToolInput(input map[string]interface{}) string {
	if len(input) == 0 {
		return "{}"
	}
	data, err := json.Marshal(input)
	if err != nil {
		return "{}"
	}
	return string(data)
}

func sanitizeFileName(name string) string {
	name = strings.ReplaceAll(name, "\x00", "")
	name = filepath.Clean(name)
	name = strings.TrimSpace(name)
	return name
}

var lineNumberPrefixRe = regexp.MustCompile(`(?m)^\s*\d+\t`)

func stripLineNumberPrefixes(text string) string {
	lines := strings.Split(text, "\n")
	if len(lines) == 0 {
		return text
	}

	matchCount := 0
	nonEmpty := 0
	for _, line := range lines {
		if line == "" {
			continue
		}
		nonEmpty++
		if lineNumberPrefixRe.MatchString(line) {
			matchCount++
		}
	}

	if nonEmpty > 0 && float64(matchCount)/float64(nonEmpty) > 0.5 {
		return lineNumberPrefixRe.ReplaceAllString(text, "")
	}
	return text
}

func nestedFallbackToolCallID(toolName, toolInput string) string {
	name := strings.ToLower(strings.TrimSpace(toolName))
	input := strings.TrimSpace(toolInput)
	if input == "" {
		input = "{}"
	}
	h := fnv.New64a()
	_, _ = h.Write([]byte(name))
	_, _ = h.Write([]byte{0})
	_, _ = h.Write([]byte(input))
	return fmt.Sprintf("warp_anon_%x", h.Sum64())
}

func isIncompleteToolCall(toolName, toolInput string) bool {
	switch strings.ToLower(strings.TrimSpace(toolName)) {
	case "run_shell_command", "write_to_long_running_shell_command", "bash":
		input := strings.TrimSpace(toolInput)
		if input == "" || input == "{}" {
			return true
		}
		var payload map[string]interface{}
		if err := json.Unmarshal([]byte(input), &payload); err != nil {
			return false
		}
		command, _ := payload["command"].(string)
		return strings.TrimSpace(command) == ""
	case "write":
		var payload map[string]interface{}
		if err := json.Unmarshal([]byte(toolInput), &payload); err != nil {
			return false
		}
		path, _ := payload["file_path"].(string)
		return strings.TrimSpace(path) == ""
	case "edit":
		var payload map[string]interface{}
		if err := json.Unmarshal([]byte(toolInput), &payload); err != nil {
			return false
		}
		path, _ := payload["file_path"].(string)
		if strings.TrimSpace(path) == "" {
			return true
		}
		_, hasOld := payload["old_string"]
		_, hasNew := payload["new_string"]
		return !hasOld || !hasNew
	default:
		return false
	}
}

func normalizeToolInputForToolName(toolName, toolInput string) string {
	input := strings.TrimSpace(toolInput)
	if input == "" {
		return "{}"
	}

	switch strings.ToLower(strings.TrimSpace(toolName)) {
	case "bash":
		var payload map[string]interface{}
		if err := json.Unmarshal([]byte(input), &payload); err != nil {
			return input
		}
		command, _ := payload["command"].(string)
		if strings.TrimSpace(command) == "" {
			return "{}"
		}
		minimal := map[string]string{"command": command}
		b, err := json.Marshal(minimal)
		if err != nil {
			return "{}"
		}
		return string(b)
	case "read":
		normalized := normalizeWarpReadToolInput(input)
		if normalized != "" {
			return normalized
		}
		return input
	default:
		return input
	}
}

func normalizeWarpReadToolInput(input string) string {
	var payload map[string]interface{}
	if err := json.Unmarshal([]byte(input), &payload); err != nil {
		return ""
	}
	path := extractWarpReadPath(payload)
	if strings.TrimSpace(path) == "" {
		return ""
	}
	minimal := map[string]string{"file_path": path}
	b, err := json.Marshal(minimal)
	if err != nil {
		return ""
	}
	return string(b)
}

func extractWarpReadPath(payload map[string]interface{}) string {
	for _, key := range []string{"file_path", "path"} {
		if raw, ok := payload[key]; ok {
			if path := extractWarpPathLikeString(raw); path != "" {
				return path
			}
		}
	}
	for _, key := range []string{"files", "file_paths", "paths"} {
		if raw, ok := payload[key]; ok {
			if paths := extractWarpPathList(raw); len(paths) > 0 {
				return pickWarpPreferredReadPath(paths)
			}
		}
	}
	for _, raw := range payload {
		if path := extractWarpPathLikeString(raw); path != "" {
			return path
		}
	}
	return ""
}

func extractWarpPathList(raw interface{}) []string {
	switch v := raw.(type) {
	case []interface{}:
		out := make([]string, 0, len(v))
		for _, item := range v {
			if path := extractWarpPathLikeString(item); path != "" {
				out = append(out, path)
			}
		}
		return out
	default:
		if path := extractWarpPathLikeString(raw); path != "" {
			return []string{path}
		}
		return nil
	}
}

func pickWarpPreferredReadPath(paths []string) string {
	if len(paths) == 0 {
		return ""
	}
	score := func(path string) int {
		base := strings.ToLower(strings.TrimSpace(path))
		switch {
		case strings.HasSuffix(base, "/readme.md"), base == "readme.md":
			return 0
		case strings.HasSuffix(base, "/pyproject.toml"), base == "pyproject.toml":
			return 1
		case strings.HasSuffix(base, "/requirements.txt"), base == "requirements.txt":
			return 2
		case strings.HasSuffix(base, "/package.json"), base == "package.json":
			return 3
		case strings.HasSuffix(base, "/go.mod"), base == "go.mod":
			return 4
		case strings.HasSuffix(base, "/api.py"), base == "api.py":
			return 5
		case strings.HasSuffix(base, "/main.py"), base == "main.py":
			return 6
		case strings.HasSuffix(base, "/app.py"), base == "app.py":
			return 7
		case strings.HasSuffix(base, "/main.go"), base == "main.go":
			return 8
		default:
			return 100
		}
	}
	best := strings.TrimSpace(paths[0])
	bestScore := score(best)
	for _, path := range paths[1:] {
		path = strings.TrimSpace(path)
		if path == "" {
			continue
		}
		if s := score(path); s < bestScore {
			best = path
			bestScore = s
		}
	}
	return best
}

func extractWarpPathLikeString(raw interface{}) string {
	switch v := raw.(type) {
	case string:
		return findWarpPathInString(v)
	case []interface{}:
		for _, item := range v {
			if path := extractWarpPathLikeString(item); path != "" {
				return path
			}
		}
	case map[string]interface{}:
		for _, item := range v {
			if path := extractWarpPathLikeString(item); path != "" {
				return path
			}
		}
	}
	return ""
}

func findWarpPathInString(s string) string {
	trimmed := strings.TrimSpace(s)
	if trimmed == "" {
		return ""
	}
	lines := strings.FieldsFunc(trimmed, func(r rune) bool {
		return r == '\n' || r == '\r'
	})
	if len(lines) == 0 {
		lines = []string{trimmed}
	}
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if path := trimWarpPathCandidate(line); path != "" {
			return path
		}
		if idx := strings.Index(line, "/"); idx >= 0 {
			if path := trimWarpPathCandidate(line[idx:]); path != "" {
				return path
			}
		}
		if idx := findWarpWindowsPathStart(line); idx >= 0 {
			if path := trimWarpPathCandidate(line[idx:]); path != "" {
				return path
			}
		}
	}
	return ""
}

func trimWarpPathCandidate(s string) string {
	s = strings.TrimSpace(s)
	s = strings.Trim(s, "\"'")
	s = strings.TrimRight(s, ",")
	s = strings.TrimRight(s, "]})")
	if looksLikeWarpPathCandidate(s) {
		return s
	}
	return ""
}

func looksLikeWarpPathCandidate(s string) bool {
	if s == "" {
		return false
	}
	if strings.HasPrefix(s, "/") || strings.HasPrefix(s, "./") || strings.HasPrefix(s, "../") {
		return true
	}
	return findWarpWindowsPathStart(s) == 0
}

func findWarpWindowsPathStart(s string) int {
	for i := 0; i+2 < len(s); i++ {
		ch := s[i]
		if ((ch >= 'A' && ch <= 'Z') || (ch >= 'a' && ch <= 'z')) && s[i+1] == ':' && (s[i+2] == '\\' || s[i+2] == '/') {
			return i
		}
	}
	return -1
}

func parseCallMCPTool(data []byte) (string, string) {
	d := decoder{data: data}
	name := ""
	input := "{}"
	for !d.eof() {
		field, wire, err := d.readKey()
		if err != nil {
			return name, input
		}
		if wire != 2 {
			_ = d.skip(wire)
			continue
		}
		payload, err := d.readBytes()
		if err != nil {
			return name, input
		}
		switch field {
		case 1:
			name = string(payload)
		case 2:
			var st structpb.Struct
			if err := proto.Unmarshal(payload, &st); err == nil {
				if m := st.AsMap(); len(m) > 0 {
					if b, err := json.Marshal(m); err == nil {
						input = string(b)
					}
				}
			}
		}
	}
	return name, input
}

func parseFallbackToolInput(toolName string, payload []byte) (string, string) {
	switch toolName {
	case "run_shell_command", "write_to_long_running_shell_command":
		input := map[string]interface{}{}
		d := decoder{data: payload}
		for !d.eof() {
			field, wire, err := d.readKey()
			if err != nil {
				break
			}
			switch field {
			case 1:
				if wire != 2 {
					_ = d.skip(wire)
					continue
				}
				b, err := d.readBytes()
				if err != nil {
					break
				}
				if cmd := string(b); cmd != "" {
					input["command"] = cmd
				}
			case 2:
				if wire != 0 {
					_ = d.skip(wire)
					continue
				}
				v, err := d.readVarint()
				if err != nil {
					break
				}
				input["is_read_only"] = v != 0
			case 3:
				if wire != 0 {
					_ = d.skip(wire)
					continue
				}
				v, err := d.readVarint()
				if err != nil {
					break
				}
				input["uses_pager"] = v != 0
			case 5:
				if wire != 0 {
					_ = d.skip(wire)
					continue
				}
				v, err := d.readVarint()
				if err != nil {
					break
				}
				input["is_risky"] = v != 0
			case 6:
				if wire != 0 {
					_ = d.skip(wire)
					continue
				}
				v, err := d.readVarint()
				if err != nil {
					break
				}
				input["wait_until_complete"] = v != 0
			default:
				_ = d.skip(wire)
			}
		}
		if len(input) == 0 {
			return toolName, "{}"
		}
		b, err := json.Marshal(input)
		if err != nil {
			return toolName, "{}"
		}
		return toolName, string(b)
	case "apply_file_diffs":
		return parseApplyFileDiffsPayload(payload)
	case "read_files":
		var files []string
		d := decoder{data: payload}
		for !d.eof() {
			field, wire, err := d.readKey()
			if err != nil {
				break
			}
			if field == 1 && wire == 2 {
				b, err := d.readBytes()
				if err != nil {
					break
				}
				if path := strings.TrimSpace(string(b)); path != "" {
					files = append(files, path)
				}
			} else {
				_ = d.skip(wire)
			}
		}
		if len(files) == 0 {
			return "Read", "{}"
		}
		path := pickWarpPreferredReadPath(files)
		if path == "" {
			path = files[0]
		}
		input := map[string]string{"file_path": path}
		b, err := json.Marshal(input)
		if err != nil {
			return "Read", "{}"
		}
		return "Read", string(b)
	default:
		input := map[string]interface{}{}
		d := decoder{data: payload}
		for !d.eof() {
			field, wire, err := d.readKey()
			if err != nil {
				break
			}
			switch wire {
			case 0:
				v, err := d.readVarint()
				if err != nil {
					break
				}
				input[fmt.Sprintf("field%d", field)] = v
			case 2:
				b, err := d.readBytes()
				if err != nil {
					break
				}
				if utf8.Valid(b) {
					input[fmt.Sprintf("field%d", field)] = string(b)
				}
			default:
				_ = d.skip(wire)
			}
		}
		if len(input) == 0 {
			return toolName, "{}"
		}
		b, err := json.Marshal(input)
		if err != nil {
			return toolName, "{}"
		}
		return toolName, string(b)
	}
}

func parseApplyFileDiffsPayload(payload []byte) (string, string) {
	d := decoder{data: payload}
	writePath := ""
	writeContent := ""
	editPath := ""
	editOld := ""
	editNew := ""

	for !d.eof() {
		field, wire, err := d.readKey()
		if err != nil {
			break
		}
		if wire != 2 {
			_ = d.skip(wire)
			continue
		}
		b, err := d.readBytes()
		if err != nil {
			break
		}
		switch field {
		case 2:
			if editPath == "" {
				p, oldStr, newStr := parseApplyFileDiffItem(b)
				if strings.TrimSpace(p) != "" {
					editPath = strings.TrimSpace(p)
					editOld = oldStr
					editNew = newStr
				}
			}
		case 3:
			if writePath == "" {
				p, c := parseApplyFileDiffNewFile(b)
				if strings.TrimSpace(p) != "" {
					writePath = strings.TrimSpace(p)
					writeContent = c
				}
			}
		}
	}

	if writePath != "" {
		input := map[string]interface{}{
			"file_path": writePath,
			"content":   writeContent,
		}
		b, err := json.Marshal(input)
		if err != nil {
			return "Write", "{}"
		}
		return "Write", string(b)
	}

	if editPath != "" {
		input := map[string]interface{}{
			"file_path":  editPath,
			"old_string": editOld,
			"new_string": editNew,
		}
		b, err := json.Marshal(input)
		if err != nil {
			return "Edit", "{}"
		}
		return "Edit", string(b)
	}

	return "apply_file_diffs", "{}"
}

func parseApplyFileDiffNewFile(payload []byte) (string, string) {
	d := decoder{data: payload}
	path := ""
	content := ""
	for !d.eof() {
		field, wire, err := d.readKey()
		if err != nil {
			break
		}
		if wire != 2 {
			_ = d.skip(wire)
			continue
		}
		b, err := d.readBytes()
		if err != nil {
			break
		}
		switch field {
		case 1:
			path = string(b)
		case 2:
			content = string(b)
		}
	}
	return path, content
}

func parseApplyFileDiffItem(payload []byte) (string, string, string) {
	d := decoder{data: payload}
	path := ""
	oldStr := ""
	newStr := ""
	for !d.eof() {
		field, wire, err := d.readKey()
		if err != nil {
			break
		}
		switch field {
		case 1:
			if wire != 2 {
				_ = d.skip(wire)
				continue
			}
			b, err := d.readBytes()
			if err != nil {
				break
			}
			path = string(b)
		case 3:
			if wire != 2 {
				_ = d.skip(wire)
				continue
			}
			b, err := d.readBytes()
			if err != nil {
				break
			}
			if oldStr == "" && newStr == "" {
				o, n := parseApplyFileDiffReplacement(b)
				oldStr = o
				newStr = n
			}
		default:
			_ = d.skip(wire)
		}
	}
	return path, oldStr, newStr
}

func parseApplyFileDiffReplacement(payload []byte) (string, string) {
	d := decoder{data: payload}
	oldStr := ""
	newStr := ""
	for !d.eof() {
		field, wire, err := d.readKey()
		if err != nil {
			break
		}
		if wire != 2 {
			_ = d.skip(wire)
			continue
		}
		b, err := d.readBytes()
		if err != nil {
			break
		}
		switch field {
		case 1:
			oldStr = string(b)
		case 2:
			newStr = string(b)
		}
	}
	return oldStr, newStr
}

func fallbackToolName(field int) string {
	switch field {
	case 2:
		return "run_shell_command"
	case 3:
		return "search_codebase"
	case 4:
		return "server"
	case 5:
		return "read_files"
	case 6:
		return "apply_file_diffs"
	case 7:
		return "suggest_plan"
	case 8:
		return "suggest_create_plan"
	case 9:
		return "grep"
	case 10:
		return "file_glob"
	case 11:
		return "read_mcp_resource"
	case 13:
		return "write_to_long_running_shell_command"
	case 14:
		return "suggest_new_conversation"
	case 15:
		return "file_glob_v2"
	default:
		return "unknown_tool"
	}
}

func parseNestedStreamFinished(data []byte, out *parsedEvent) {
	d := decoder{data: data}
	inputTokens := 0
	outputTokens := 0
	for !d.eof() {
		field, wire, err := d.readKey()
		if err != nil {
			break
		}
		if field == 8 && wire == 2 {
			payload, err := d.readBytes()
			if err != nil {
				break
			}
			in, outTok := parseNestedTokenUsage(payload)
			inputTokens += in
			outputTokens += outTok
			continue
		}
		_ = d.skip(wire)
	}
	out.Finish = &finishInfo{InputTokens: inputTokens, OutputTokens: outputTokens}
}

func parseNestedTokenUsage(data []byte) (int, int) {
	d := decoder{data: data}
	inputTokens := 0
	outputTokens := 0
	for !d.eof() {
		field, wire, err := d.readKey()
		if err != nil {
			break
		}
		switch field {
		case 2:
			if wire != 0 {
				_ = d.skip(wire)
				continue
			}
			v, err := d.readVarint()
			if err != nil {
				break
			}
			inputTokens = int(v)
		case 3:
			if wire != 0 {
				_ = d.skip(wire)
				continue
			}
			v, err := d.readVarint()
			if err != nil {
				break
			}
			outputTokens = int(v)
		default:
			_ = d.skip(wire)
		}
	}
	return inputTokens, outputTokens
}

func parseNestedStreamError(data []byte) string {
	d := decoder{data: data}
	errMsg := ""
	errCode := ""
	for !d.eof() {
		field, wire, err := d.readKey()
		if err != nil {
			break
		}
		if wire == 2 {
			payload, err := d.readBytes()
			if err != nil {
				break
			}
			switch field {
			case 1:
				errMsg = string(payload)
			case 2:
				errCode = string(payload)
			}
			continue
		}
		_ = d.skip(wire)
	}
	if errMsg == "" && errCode == "" {
		return string(data)
	}
	if errCode != "" && errMsg != "" {
		return errCode + ": " + errMsg
	}
	if errMsg != "" {
		return errMsg
	}
	return errCode
}
