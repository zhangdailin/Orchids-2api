package warp

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"strings"
	"time"
	"unicode/utf8"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/structpb"
	"orchids-api/internal/orchids"
)

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

type toolCall struct {
	ID    string
	Name  string
	Input string
}

type finishInfo struct {
	InputTokens  int
	OutputTokens int
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
		case 1: // init
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
		case 2: // client_actions
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
		case 3: // finished
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
			parseStreamFinished(payload, out)
		case 4: // error
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
			out.Error = parseStreamError(payload)
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
		case 1: // create_task
			if wire != 2 {
				_ = d.skip(wire)
				continue
			}
			payload, err := d.readBytes()
			if err != nil {
				return
			}
			parseCreateTask(payload, out)
		case 3: // add_messages_to_task
			if wire != 2 {
				_ = d.skip(wire)
				continue
			}
			payload, err := d.readBytes()
			if err != nil {
				return
			}
			parseAddMessages(payload, out)
		case 4: // update_task_message
			if wire != 2 {
				_ = d.skip(wire)
				continue
			}
			payload, err := d.readBytes()
			if err != nil {
				return
			}
			parseUpdateTaskMessage(payload, out)
		case 5: // append_to_message_content
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
		case 3: // agent_output
			if wire != 2 {
				_ = d.skip(wire)
				continue
			}
			payload, err := d.readBytes()
			if err != nil {
				return
			}
			parseAgentOutput(payload, out)
		case 4: // tool_call
			if wire != 2 {
				_ = d.skip(wire)
				continue
			}
			payload, err := d.readBytes()
			if err != nil {
				return
			}
			parseToolCall(payload, out)
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
		case 1: // text
			out.TextDeltas = append(out.TextDeltas, string(payload))
		case 2: // reasoning
			out.ReasoningDeltas = append(out.ReasoningDeltas, string(payload))
		}
	}
}

func parseToolCall(data []byte, out *parsedEvent) {
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
		case 1: // tool_call_id
			if wire != 2 {
				_ = d.skip(wire)
				continue
			}
			payload, err := d.readBytes()
			if err != nil {
				return
			}
			toolID = string(payload)
		case 12: // call_mcp_tool
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
	if isIncompleteToolCall(toolName, toolInput) {
		return
	}
	toolName = orchids.NormalizeToolName(toolName)
	toolInput = normalizeToolInputForToolName(toolName, toolInput)
	if orchids.DefaultToolMapper.IsBlocked(toolName) {
		return
	}
	if toolID == "" {
		toolID = fmt.Sprintf("warp_%d", time.Now().UnixNano())
	}
	out.ToolCalls = append(out.ToolCalls, toolCall{ID: toolID, Name: toolName, Input: toolInput})
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
	default:
		return input
	}
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
			case 1: // command
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
			case 2: // is_read_only
				if wire != 0 {
					_ = d.skip(wire)
					continue
				}
				v, err := d.readVarint()
				if err != nil {
					break
				}
				input["is_read_only"] = v != 0
			case 3: // uses_pager
				if wire != 0 {
					_ = d.skip(wire)
					continue
				}
				v, err := d.readVarint()
				if err != nil {
					break
				}
				input["uses_pager"] = v != 0
			case 5: // is_risky
				if wire != 0 {
					_ = d.skip(wire)
					continue
				}
				v, err := d.readVarint()
				if err != nil {
					break
				}
				input["is_risky"] = v != 0
			case 6: // wait_until_complete
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
	default:
		// Generic protobuf extraction: pull all string fields so non-shell
		// tools don't receive empty input.
		input := map[string]interface{}{}
		d := decoder{data: payload}
		for !d.eof() {
			field, wire, err := d.readKey()
			if err != nil {
				break
			}
			switch wire {
			case 0: // varint
				v, err := d.readVarint()
				if err != nil {
					break
				}
				input[fmt.Sprintf("field%d", field)] = v
			case 2: // length-delimited (string/bytes)
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
		case 2: // file_diffs
			if editPath == "" {
				p, oldStr, newStr := parseApplyFileDiffItem(b)
				if strings.TrimSpace(p) != "" {
					editPath = strings.TrimSpace(p)
					editOld = oldStr
					editNew = newStr
				}
			}
		case 3: // new_files
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
		case 1: // file_path
			path = string(b)
		case 2: // content
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
		case 1: // file_path
			if wire != 2 {
				_ = d.skip(wire)
				continue
			}
			b, err := d.readBytes()
			if err != nil {
				break
			}
			path = string(b)
		case 3: // replacements
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

func parseStreamFinished(data []byte, out *parsedEvent) {
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
			in, outTok := parseTokenUsage(payload)
			inputTokens += in
			outputTokens += outTok
			continue
		}
		_ = d.skip(wire)
	}
	out.Finish = &finishInfo{InputTokens: inputTokens, OutputTokens: outputTokens}
}

func parseTokenUsage(data []byte) (int, int) {
	d := decoder{data: data}
	inputTokens := 0
	outputTokens := 0
	for !d.eof() {
		field, wire, err := d.readKey()
		if err != nil {
			break
		}
		switch field {
		case 2: // total_input
			if wire != 0 {
				_ = d.skip(wire)
				continue
			}
			v, err := d.readVarint()
			if err != nil {
				break
			}
			inputTokens = int(v)
		case 3: // output
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

// parseStreamError extracts an error message from a protobuf error field.
// It tries to read string fields (field 1 = message, field 2 = code).
func parseStreamError(data []byte) string {
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
