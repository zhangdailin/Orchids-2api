package warp

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"time"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/structpb"
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
				_ = payload
				toolName = fallbackToolName(field)
				toolInput = "{}"
				continue
			}
			_ = d.skip(wire)
		}
	}
	if toolName == "" {
		return
	}
	if toolID == "" {
		toolID = fmt.Sprintf("warp_%d", time.Now().UnixNano())
	}
	out.ToolCalls = append(out.ToolCalls, toolCall{ID: toolID, Name: toolName, Input: toolInput})
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
