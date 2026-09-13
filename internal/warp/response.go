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
	"strings"
	"unicode/utf8"

	"github.com/goccy/go-json"
	warpapi "github.com/warpdotdev/warp-proto-apis/apis/multi_agent/v1/gen/go"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	"orchids-api/internal/debug"
	"orchids-api/internal/toolname"
	"orchids-api/internal/upstream"
)

type toolCall struct {
	ID    string
	Name  string
	Input string
	Type  string
}

type finishInfo struct {
	InputTokens              int
	OutputTokens             int
	CacheReadTokens          int
	CacheWriteTokens         int
	WebSearchCount           int
	RequestCredits           float64
	RequestPlatformCredits   float64
	RequestProviderCostCents float64
	RequestPlatformCostCents float64
	ConversationCredits      float64
	ConversationPlatform     float64
	ConversationTotalInput   int
	ContextWindowUsage       float64
	Reason                   string
	Message                  string
	ShouldRefreshModelConfig bool
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

type parsedEvent struct {
	Recognized     bool
	ConversationID string
	RequestID      string
	RunID          string
	ContentUpdates []warpContentUpdate
	ToolCalls      []toolCall
	Finish         *finishInfo
}

type warpContentUpdate struct {
	MessageID string
	Text      string
	Reasoning bool
	Snapshot  bool
}

type warpStreamState struct {
	sawToolCall        bool
	textByMessage      map[string]*strings.Builder
	reasoningByMessage map[string]*strings.Builder
	seenToolCalls      map[string]struct{}
}

func newWarpStreamState() *warpStreamState {
	return &warpStreamState{
		textByMessage:      make(map[string]*strings.Builder),
		reasoningByMessage: make(map[string]*strings.Builder),
	}
}

func (s *warpStreamState) applyContentUpdate(update warpContentUpdate) string {
	if update.Text == "" {
		return ""
	}
	key := strings.TrimSpace(update.MessageID)
	if key == "" {
		key = "primary"
	}
	values := s.textByMessage
	if update.Reasoning {
		values = s.reasoningByMessage
	}
	buffer := values[key]
	if buffer == nil {
		buffer = &strings.Builder{}
		values[key] = buffer
	}

	current := buffer.String()
	if !update.Snapshot {
		buffer.WriteString(update.Text)
		return update.Text
	}

	// Add/update actions contain the complete message value, while append
	// actions contain only a delta. Warp commonly sends a final update after
	// all append events; emit only the unseen suffix instead of duplicating the
	// full response.
	switch {
	case current == "":
		buffer.WriteString(update.Text)
		return update.Text
	case update.Text == current, strings.HasPrefix(current, update.Text):
		return ""
	case strings.HasPrefix(update.Text, current):
		buffer.WriteString(update.Text[len(current):])
		return update.Text[len(current):]
	default:
		// Streaming APIs cannot retract already-emitted text. Keep the latest
		// snapshot for subsequent comparisons without appending a conflicting
		// replacement as duplicate output.
		buffer.Reset()
		buffer.WriteString(update.Text)
		return ""
	}
}

func (s *warpStreamState) acceptToolCall(call toolCall) bool {
	if strings.TrimSpace(call.ID) == "" {
		return true
	}
	if s.seenToolCalls == nil {
		s.seenToolCalls = make(map[string]struct{})
	}
	if _, exists := s.seenToolCalls[call.ID]; exists {
		return false
	}
	s.seenToolCalls[call.ID] = struct{}{}
	return true
}

func (s *warpStreamState) finishReason() string {
	if s.sawToolCall {
		return "tool_use"
	}
	return "end_turn"
}

func processStreamBody(ctx context.Context, reader io.Reader, onMessage func(upstream.SSEMessage), logger *debug.Logger) error {
	if onMessage == nil {
		onMessage = func(upstream.SSEMessage) {}
	}
	if closer, ok := reader.(io.Closer); ok {
		stopClose := context.AfterFunc(ctx, func() { _ = closer.Close() })
		defer stopClose()
	}

	br := bufio.NewReaderSize(reader, 64*1024)
	sawFrame := false
	handledFrame := false
	state := newWarpStreamState()

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
		if logger.SSEEnabled() {
			var event warpapi.ResponseEvent
			if decodeErr := proto.Unmarshal(frame, &event); decodeErr != nil {
				logger.LogUpstreamSSE("warp_decode_error", fmt.Sprintf("bytes=%d error=%v", len(frame), decodeErr))
			} else if raw, marshalErr := protojson.Marshal(&event); marshalErr == nil {
				logger.LogUpstreamSSE("warp_protobuf_decoded", string(raw))
			}
		}
		handled, done, err := emitWarpPayload(frame, onMessage, state)
		if err != nil {
			return err
		}
		if handled {
			handledFrame = true
		}
		if done {
			return nil
		}
	}

	if !sawFrame {
		return fmt.Errorf("warp stream ended without protobuf frames")
	}
	if !handledFrame {
		return fmt.Errorf("warp stream ended without parsed response events")
	}
	return fmt.Errorf("warp stream ended without StreamFinished event")
}

func processSSEStreamBody(ctx context.Context, reader *bufio.Reader, onMessage func(upstream.SSEMessage), logger *debug.Logger) error {
	var dataBuilder strings.Builder
	dataEventCount := 0
	parsedEventCount := 0
	state := newWarpStreamState()
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
			return fmt.Errorf("decode Warp SSE payload: %w", err)
		}

		handled, done, err := emitWarpPayload(payloadBytes, onMessage, state)
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
		return fmt.Errorf("warp SSE stream ended without StreamFinished event")
	}
	return nil
}

func decodeWarpPayload(data string) ([]byte, error) {
	if data == "" {
		return nil, fmt.Errorf("empty payload")
	}
	encoding := base64.RawURLEncoding
	if strings.ContainsAny(data, "+/") {
		encoding = base64.StdEncoding
	} else if strings.HasSuffix(data, "=") {
		encoding = base64.URLEncoding
	}
	return encoding.DecodeString(data)
}

func emitWarpPayload(frame []byte, onMessage func(upstream.SSEMessage), state *warpStreamState) (bool, bool, error) {
	parsed, err := parseResponseEvent(frame)
	if err != nil {
		return false, false, fmt.Errorf("decode Warp response event: %w", err)
	}
	if !parsed.Recognized {
		return false, false, nil
	}
	if parsed.ConversationID != "" {
		onMessage(upstream.SSEMessage{
			Type:  "model.conversation_id",
			Event: map[string]interface{}{"id": parsed.ConversationID},
		})
	}
	if parsed.RequestID != "" {
		onMessage(upstream.SSEMessage{
			Type:  "model.request_id",
			Event: map[string]interface{}{"id": parsed.RequestID, "runId": parsed.RunID},
		})
	}
	for _, update := range parsed.ContentUpdates {
		delta := state.applyContentUpdate(update)
		if delta == "" {
			continue
		}
		eventType := "model.text-delta"
		if update.Reasoning {
			eventType = "model.reasoning-delta"
		}
		onMessage(upstream.SSEMessage{
			Type:  eventType,
			Event: map[string]interface{}{"delta": delta},
		})
	}
	for _, call := range parsed.ToolCalls {
		if !state.acceptToolCall(call) {
			continue
		}
		state.sawToolCall = true
		onMessage(upstream.SSEMessage{
			Type: "model.tool-call",
			Event: map[string]interface{}{
				"toolCallId":   call.ID,
				"toolName":     call.Name,
				"input":        call.Input,
				"warpToolType": call.Type,
			},
		})
	}
	if parsed.Finish == nil {
		return true, false, nil
	}
	if usageMetadata := parsed.Finish.usageMetadata(); usageMetadata != nil {
		onMessage(upstream.SSEMessage{Type: "model.usage-metadata", Event: usageMetadata})
	}
	if err := parsed.Finish.terminalError(); err != nil {
		if parsed.Finish.InputTokens > 0 || parsed.Finish.OutputTokens > 0 {
			onMessage(upstream.SSEMessage{
				Type: "model.tokens-used",
				Event: map[string]interface{}{
					"inputTokens":  parsed.Finish.InputTokens,
					"outputTokens": parsed.Finish.OutputTokens,
				},
			})
		}
		return true, false, err
	}
	finishReason := state.finishReason()
	if parsed.Finish.Reason == "max_token_limit" {
		finishReason = "max_tokens"
	}
	finish := map[string]interface{}{"finishReason": finishReason}
	if parsed.Finish.InputTokens > 0 || parsed.Finish.OutputTokens > 0 {
		finish["usage"] = map[string]interface{}{
			"inputTokens":  parsed.Finish.InputTokens,
			"outputTokens": parsed.Finish.OutputTokens,
		}
	}
	if parsed.Finish.ShouldRefreshModelConfig {
		finish["shouldRefreshModelConfig"] = true
	}
	onMessage(upstream.SSEMessage{Type: "model.finish", Event: finish})
	return true, true, nil
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
	var event warpapi.ResponseEvent
	if err := proto.Unmarshal(data, &event); err != nil {
		return nil, err
	}
	out := &parsedEvent{Recognized: event.HasType()}
	switch event.WhichType() {
	case warpapi.ResponseEvent_Init_case:
		init := event.GetInit()
		out.ConversationID = init.GetConversationId()
		out.RequestID = init.GetRequestId()
		out.RunID = init.GetRunId()
	case warpapi.ResponseEvent_ClientActions_case:
		for _, action := range event.GetClientActions().GetActions() {
			appendWarpClientAction(out, action)
		}
	case warpapi.ResponseEvent_Finished_case:
		out.Finish = parseStreamFinished(event.GetFinished())
	}
	return out, nil
}

func appendWarpClientAction(out *parsedEvent, action *warpapi.ClientAction) {
	if action == nil {
		return
	}
	switch action.WhichAction() {
	case warpapi.ClientAction_CreateTask_case:
		appendWarpMessages(out, action.GetCreateTask().GetTask().GetMessages(), true)
	case warpapi.ClientAction_AddMessagesToTask_case:
		appendWarpMessages(out, action.GetAddMessagesToTask().GetMessages(), true)
	case warpapi.ClientAction_UpdateTaskMessage_case:
		appendWarpMessage(out, action.GetUpdateTaskMessage().GetMessage(), true)
	case warpapi.ClientAction_AppendToMessageContent_case:
		appendWarpMessage(out, action.GetAppendToMessageContent().GetMessage(), false)
	}
}

func appendWarpMessages(out *parsedEvent, messages []*warpapi.Message, snapshot bool) {
	for _, message := range messages {
		appendWarpMessage(out, message, snapshot)
	}
}

func appendWarpMessage(out *parsedEvent, message *warpapi.Message, snapshot bool) {
	if message == nil {
		return
	}
	switch message.WhichMessage() {
	case warpapi.Message_AgentOutput_case:
		out.ContentUpdates = append(out.ContentUpdates, warpContentUpdate{
			MessageID: message.GetId(),
			Text:      message.GetAgentOutput().GetText(),
			Snapshot:  snapshot,
		})
	case warpapi.Message_AgentReasoning_case:
		out.ContentUpdates = append(out.ContentUpdates, warpContentUpdate{
			MessageID: message.GetId(),
			Text:      message.GetAgentReasoning().GetReasoning(),
			Reasoning: true,
			Snapshot:  snapshot,
		})
	case warpapi.Message_ToolCall_case:
		if call, ok := parseWarpToolCall(message.GetToolCall()); ok {
			out.ToolCalls = append(out.ToolCalls, call)
		}
	}
}

func parseWarpToolCall(call *warpapi.Message_ToolCall) (toolCall, bool) {
	if call == nil || !call.HasTool() {
		return toolCall{}, false
	}

	toolName := ""
	toolInput := "{}"
	toolType := ""
	if mcpCall := call.GetCallMcpTool(); mcpCall != nil {
		toolName = mcpCall.GetName()
		toolType = "call_mcp_tool"
		if mcpCall.GetArgs() != nil {
			toolInput = marshalToolInput(mcpCall.GetArgs().AsMap())
		}
	} else {
		message := call.ProtoReflect()
		field := message.WhichOneof(message.Descriptor().Oneofs().ByName("tool"))
		if field == nil {
			return toolCall{}, false
		}
		toolType = string(field.Name())
		if !shouldEmitWarpToolName(toolType) {
			return toolCall{}, false
		}
		payload, err := proto.Marshal(message.Get(field).Message().Interface())
		if err != nil {
			return toolCall{}, false
		}
		toolName, toolInput = parseWarpToolInput(toolType, payload)
	}

	toolName = normalizeWarpToolName(toolName)
	if toolName == "" || isIncompleteToolCall(toolName, toolInput) {
		return toolCall{}, false
	}
	toolID := call.GetToolCallId()
	if toolID == "" {
		toolID = derivedWarpToolCallID(toolName, toolInput)
	}
	return toolCall{ID: toolID, Name: toolName, Input: toolInput, Type: toolType}, true
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

func normalizeWarpToolName(name string) string {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "write_to_long_running_shell_command":
		return "Bash"
	default:
		return toolname.NormalizeToolNameFallback(name)
	}
}

func shouldEmitWarpToolName(name string) bool {
	switch strings.ToLower(strings.TrimSpace(normalizeWarpToolName(name))) {
	case "bash", "grep", "glob", "read", "edit", "write", "read_shell_command_output":
		return true
	default:
		return false
	}
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

func derivedWarpToolCallID(toolName, toolInput string) string {
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
	case "read_shell_command_output":
		input := strings.TrimSpace(toolInput)
		if input == "" || input == "{}" {
			return true
		}
		var payload map[string]interface{}
		if err := json.Unmarshal([]byte(input), &payload); err != nil {
			return false
		}
		commandID, _ := payload["command_id"].(string)
		return strings.TrimSpace(commandID) == ""
	case "write":
		var payload map[string]interface{}
		if err := json.Unmarshal([]byte(toolInput), &payload); err != nil {
			return false
		}
		path, _ := payload["file_path"].(string)
		return strings.TrimSpace(path) == ""
	case "read":
		var payload map[string]interface{}
		if err := json.Unmarshal([]byte(toolInput), &payload); err != nil {
			return false
		}
		path, _ := payload["file_path"].(string)
		return strings.TrimSpace(path) == ""
	case "grep", "glob":
		var payload map[string]interface{}
		if err := json.Unmarshal([]byte(toolInput), &payload); err != nil {
			return false
		}
		pattern, _ := payload["pattern"].(string)
		return strings.TrimSpace(pattern) == ""
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

func parseWarpToolInput(toolName string, payload []byte) (string, string) {
	switch toolName {
	case "run_shell_command":
		var call warpapi.Message_ToolCall_RunShellCommand
		if err := proto.Unmarshal(payload, &call); err != nil {
			return toolName, "{}"
		}
		return toolName, marshalToolInput(map[string]interface{}{"command": call.GetCommand()})
	case "write_to_long_running_shell_command":
		var call warpapi.Message_ToolCall_WriteToLongRunningShellCommand
		if err := proto.Unmarshal(payload, &call); err != nil {
			return toolName, "{}"
		}
		return toolName, marshalToolInput(map[string]interface{}{"command": string(call.GetInput())})
	case "read_shell_command_output":
		var call warpapi.Message_ToolCall_ReadShellCommandOutput
		if err := proto.Unmarshal(payload, &call); err != nil {
			return toolName, "{}"
		}
		input := map[string]interface{}{"command_id": call.GetCommandId()}
		if duration := call.GetDuration(); duration != nil {
			input["duration"] = map[string]interface{}{"seconds": duration.GetSeconds(), "nanos": duration.GetNanos()}
		}
		if call.GetOnCompletion() != nil {
			input["on_completion"] = true
		}
		return toolName, marshalToolInput(input)
	case "apply_file_diffs":
		return parseApplyFileDiffsPayload(payload)
	case "read_files":
		var call warpapi.Message_ToolCall_ReadFiles
		if err := proto.Unmarshal(payload, &call); err != nil {
			return "Read", "{}"
		}
		files := make([]string, 0, len(call.GetFiles()))
		for _, file := range call.GetFiles() {
			if name := strings.TrimSpace(file.GetName()); name != "" {
				files = append(files, name)
			}
		}
		// A single Warp action can ask for many files, but an OpenAI tool call
		// has one result ID. Emitting one arbitrary file here loses operations
		// and makes the following continuation invalid, so reject batches until
		// they can be represented as a lossless bridge.
		if len(files) != 1 {
			return "Read", "{}"
		}
		return "Read", marshalToolInput(map[string]interface{}{"file_path": files[0]})
	case "grep":
		var call warpapi.Message_ToolCall_Grep
		if err := proto.Unmarshal(payload, &call); err != nil || len(call.GetQueries()) != 1 {
			return "Grep", "{}"
		}
		return "Grep", marshalToolInput(map[string]interface{}{
			"pattern": call.GetQueries()[0],
			"path":    call.GetPath(),
		})
	case "file_glob":
		var call warpapi.Message_ToolCall_FileGlob
		if err := proto.Unmarshal(payload, &call); err != nil || len(call.GetPatterns()) != 1 {
			return "Glob", "{}"
		}
		return "Glob", marshalToolInput(map[string]interface{}{
			"pattern": call.GetPatterns()[0],
			"path":    call.GetPath(),
		})
	case "file_glob_v2":
		var call warpapi.Message_ToolCall_FileGlobV2
		if err := proto.Unmarshal(payload, &call); err != nil || len(call.GetPatterns()) != 1 {
			return "Glob", "{}"
		}
		return "Glob", marshalToolInput(map[string]interface{}{
			"pattern": call.GetPatterns()[0],
			"path":    call.GetSearchDir(),
		})
	default:
		return toolName, "{}"
	}
}

func parseApplyFileDiffsPayload(payload []byte) (string, string) {
	var call warpapi.Message_ToolCall_ApplyFileDiffs
	if err := proto.Unmarshal(payload, &call); err == nil {
		operationCount := len(call.GetNewFiles()) + len(call.GetDiffs())
		for _, update := range call.GetV4AUpdates() {
			operationCount += len(update.GetHunks())
		}
		if operationCount != 1 {
			return "apply_file_diffs", "{}"
		}
		if len(call.GetNewFiles()) == 1 {
			file := call.GetNewFiles()[0]
			if path := strings.TrimSpace(file.GetFilePath()); path != "" {
				return "Write", marshalToolInput(map[string]interface{}{"file_path": path, "content": file.GetContent()})
			}
		}
		if len(call.GetDiffs()) == 1 {
			diff := call.GetDiffs()[0]
			if path := strings.TrimSpace(diff.GetFilePath()); path != "" {
				return "Edit", marshalToolInput(map[string]interface{}{"file_path": path, "old_string": diff.GetSearch(), "new_string": diff.GetReplace()})
			}
		}
		for _, update := range call.GetV4AUpdates() {
			if len(update.GetHunks()) != 1 || strings.TrimSpace(update.GetFilePath()) == "" {
				continue
			}
			hunk := update.GetHunks()[0]
			return "Edit", marshalToolInput(map[string]interface{}{"file_path": update.GetFilePath(), "old_string": hunk.GetOld(), "new_string": hunk.GetNew()})
		}
	}

	return "apply_file_diffs", "{}"
}

func parseStreamFinished(event *warpapi.ResponseEvent_StreamFinished) *finishInfo {
	finish := &finishInfo{}
	if event == nil {
		finish.Reason = "invalid_finished_event"
		return finish
	}

	finish.ShouldRefreshModelConfig = event.GetShouldRefreshModelConfig()
	for _, usage := range event.GetTokenUsage() {
		finish.InputTokens += int(usage.GetTotalInput())
		finish.OutputTokens += int(usage.GetOutput())
	}
	if charges := event.GetRequestCharges(); charges != nil {
		input, output, cacheRead, cacheWrite, searches, providerCents, platformCents := summarizeRequestCharges(charges)
		if input+output+cacheRead+cacheWrite > 0 {
			finish.InputTokens = input
			finish.OutputTokens = output
		}
		finish.CacheReadTokens = cacheRead
		finish.CacheWriteTokens = cacheWrite
		finish.WebSearchCount = searches
		finish.RequestProviderCostCents = providerCents
		finish.RequestPlatformCostCents = platformCents
	}
	if cost := event.GetRequestCost(); cost != nil {
		finish.RequestCredits = float64(cost.GetExact())
		finish.RequestPlatformCredits = float64(cost.GetPlatformCredits())
	}
	if conversation := event.GetConversationUsageMetadata(); conversation != nil {
		finish.ConversationCredits = float64(conversation.GetCreditsSpent())
		finish.ConversationPlatform = float64(conversation.GetPlatformCreditsSpent())
		finish.ConversationTotalInput = int(conversation.GetTotalInputTokens())
		finish.ContextWindowUsage = float64(conversation.GetContextWindowUsage())
	}
	switch event.WhichReason() {
	case warpapi.ResponseEvent_StreamFinished_Other_case:
		finish.Reason = "other"
	case warpapi.ResponseEvent_StreamFinished_Done_case:
		finish.Reason = "done"
	case warpapi.ResponseEvent_StreamFinished_MaxTokenLimit_case:
		finish.Reason = "max_token_limit"
	case warpapi.ResponseEvent_StreamFinished_QuotaLimit_case:
		finish.Reason = "quota_limit"
	case warpapi.ResponseEvent_StreamFinished_ContextWindowExceeded_case:
		finish.Reason = "context_window_exceeded"
	case warpapi.ResponseEvent_StreamFinished_LlmUnavailable_case:
		finish.Reason = "llm_unavailable"
	case warpapi.ResponseEvent_StreamFinished_InternalError_case:
		finish.Reason = "internal_error"
		finish.Message = event.GetInternalError().GetMessage()
	case warpapi.ResponseEvent_StreamFinished_InvalidApiKey_case:
		finish.Reason = "invalid_api_key"
		invalidKey := event.GetInvalidApiKey()
		provider := strings.TrimPrefix(strings.ToLower(invalidKey.GetProvider().String()), "llm_provider_")
		model := invalidKey.GetModelName()
		switch {
		case provider != "" && provider != "unknown" && model != "":
			finish.Message = fmt.Sprintf("provider=%s model=%s", provider, model)
		case provider != "" && provider != "unknown":
			finish.Message = "provider=" + provider
		case model != "":
			finish.Message = "model=" + model
		}
	}
	return finish
}

func summarizeRequestCharges(charges *warpapi.ResponseEvent_StreamFinished_RequestCharges) (input, output, cacheRead, cacheWrite, searches int, providerCents, platformCents float64) {
	if charges == nil {
		return
	}
	for _, charged := range charges.GetUsageByCategory() {
		if charged == nil {
			continue
		}
		platformCents += float64(charged.GetPlatformUsageInCents())
		usageSets := []map[string]*warpapi.ResponseEvent_StreamFinished_InferenceUsage{
			charged.GetDirectApiInferenceUsage(),
			charged.GetByokInferenceUsage(),
			charged.GetCustomEndpointInferenceUsage(),
		}
		for _, usages := range usageSets {
			for _, usage := range usages {
				if usage == nil {
					continue
				}
				if count := usage.GetTokenCount(); count != nil {
					input += int(count.GetInput())
					output += int(count.GetOutput())
					cacheRead += int(count.GetInputCacheRead())
					cacheWrite += int(count.GetInputCacheWrite())
				}
				if cost := usage.GetTokenCost(); cost != nil {
					providerCents += float64(cost.GetInputCostInCents() + cost.GetOutputCostInCents() + cost.GetInputCacheReadCostInCents() + cost.GetInputCacheWriteCostInCents())
				}
				searches += int(usage.GetWebSearchCount())
				providerCents += float64(usage.GetWebSearchCostInCents())
			}
		}
	}
	return
}

func (f *finishInfo) usageMetadata() map[string]interface{} {
	if f == nil {
		return nil
	}
	if f.CacheReadTokens == 0 && f.CacheWriteTokens == 0 && f.WebSearchCount == 0 &&
		f.RequestCredits == 0 && f.RequestPlatformCredits == 0 &&
		f.RequestProviderCostCents == 0 && f.RequestPlatformCostCents == 0 &&
		f.ConversationCredits == 0 && f.ConversationPlatform == 0 &&
		f.ConversationTotalInput == 0 && f.ContextWindowUsage == 0 {
		return nil
	}
	return map[string]interface{}{
		"cacheReadTokens":          f.CacheReadTokens,
		"cacheWriteTokens":         f.CacheWriteTokens,
		"webSearchCount":           f.WebSearchCount,
		"requestCredits":           f.RequestCredits,
		"requestPlatformCredits":   f.RequestPlatformCredits,
		"requestProviderCostCents": f.RequestProviderCostCents,
		"requestPlatformCostCents": f.RequestPlatformCostCents,
		"conversationCredits":      f.ConversationCredits,
		"conversationPlatform":     f.ConversationPlatform,
		"conversationTotalInput":   f.ConversationTotalInput,
		"contextWindowUsage":       f.ContextWindowUsage,
	}
}

func (f *finishInfo) terminalError() error {
	if f == nil {
		return nil
	}
	reason := strings.TrimSpace(f.Reason)
	switch reason {
	case "", "done", "other":
		return nil
	case "max_token_limit":
		return nil
	case "quota_limit":
		return fmt.Errorf("warp stream finished with quota_limit: no remaining quota")
	case "context_window_exceeded":
		return fmt.Errorf("warp stream finished with context_window_exceeded: input is too long")
	case "llm_unavailable":
		return fmt.Errorf("warp stream finished with llm_unavailable: model unavailable")
	case "internal_error":
		if f.Message != "" {
			return fmt.Errorf("warp stream finished with internal_error: %s", f.Message)
		}
		return fmt.Errorf("warp stream finished with internal_error")
	case "invalid_api_key":
		if f.Message != "" {
			return fmt.Errorf("warp stream finished with invalid_api_key: %s", f.Message)
		}
		return fmt.Errorf("warp stream finished with invalid_api_key")
	default:
		return fmt.Errorf("warp stream finished with %s", reason)
	}
}
