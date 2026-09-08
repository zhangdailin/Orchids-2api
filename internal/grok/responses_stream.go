package grok

import (
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/goccy/go-json"
)

// One state per output item; late signatures and snapshots update that item,
// never a single global reasoning buffer. Public IDs remain stable throughout.
type chatResponseItem struct {
	index     int
	value     map[string]interface{}
	text      strings.Builder
	signature string
	closed    bool
}

func writeResponsesStreamFromChatReaderRequest(w http.ResponseWriter, request ResponsesCreateRequest, reader io.Reader) {
	streamResponseHeaders(w)
	writer := &checkedStreamWriter{target: w}
	id := "resp_" + randomHex(12)
	created := time.Now().Unix()
	items := []*chatResponseItem{}
	thoughts := map[string]*chatResponseItem{}
	tools := map[int]*chatResponseItem{}
	callIDs := map[string]bool{}
	searches := map[string]*chatResponseItem{}
	var message *chatResponseItem
	textIndex, refusalIndex := -1, -1
	var text, refusal strings.Builder
	annotations := []interface{}{}
	annotationKeys := map[string]bool{}
	activeThought := ""
	var usage map[string]interface{}
	finish := ""
	sawDone := false
	meaningful := false
	emit := func(kind string, payload map[string]interface{}) {
		payload["type"] = kind
		data, err := json.Marshal(payload)
		if err != nil {
			writer.err = err
			return
		}
		_, _ = fmt.Fprintf(writer, "event: %s\ndata: %s\n\n", kind, data)
		writer.Flush()
	}
	response := func(status string) map[string]interface{} {
		out := make([]interface{}, 0, len(items))
		for _, item := range items {
			out = append(out, item.value)
		}
		v := map[string]interface{}{"id": id, "object": "response", "created_at": created, "model": request.Model, "status": status, "output": out}
		if usage != nil {
			v["usage"] = usage
		}
		if len(request.Metadata) > 0 {
			v["metadata"] = request.Metadata
		}
		if request.Truncation != "" {
			v["truncation"] = request.Truncation
		}
		return v
	}
	add := func(value map[string]interface{}) *chatResponseItem {
		item := &chatResponseItem{index: len(items), value: value}
		items = append(items, item)
		emit("response.output_item.added", map[string]interface{}{"output_index": item.index, "item": value})
		return item
	}
	ensureMessage := func() {
		if message == nil {
			message = add(map[string]interface{}{"id": "msg_" + randomHex(12), "type": "message", "role": "assistant", "status": "in_progress", "content": []interface{}{}})
		}
	}
	emit("response.created", map[string]interface{}{"response": response("in_progress")})
	if writer.err != nil {
		return
	}
	err := readResponseSSE(reader, func(event, data string) error {
		if writer.err != nil {
			return writer.err
		}
		if data == "[DONE]" {
			sawDone = true
			return io.EOF
		}
		var chunk map[string]interface{}
		if err := json.Unmarshal([]byte(data), &chunk); err != nil {
			return fmt.Errorf("invalid chat SSE: %w", err)
		}
		if chunk == nil {
			return fmt.Errorf("chat SSE must be an object")
		}
		if chunk["error"] != nil || event == "error" {
			return responseFailure(chunk)
		}
		if u, ok := chunk["usage"].(map[string]interface{}); ok {
			usage = responsesUsageFromChat(u)
		}
		for _, raw := range interfaceSlice(chunk["choices"]) {
			choice, ok := raw.(map[string]interface{})
			if !ok {
				return fmt.Errorf("invalid chat choice")
			}
			if index, exists := choice["index"]; exists && interfaceToInt(index) != 0 {
				return fmt.Errorf("multiple chat choices are not supported by Responses")
			}
			delta, _ := choice["delta"].(map[string]interface{})
			if finish != "" && len(delta) > 0 {
				return fmt.Errorf("chat output after finish_reason")
			}
			thinking := streamString(firstNonNil(delta["reasoning_content"], delta["reasoning"]))
			signature := streamString(delta["reasoning_encrypted_content"])
			key := streamString(delta["reasoning_item_id"])
			if thinking != "" || signature != "" {
				if key == "" {
					key = activeThought
					if key == "" {
						key = "anonymous_" + randomHex(8)
					}
				}
				activeThought = key
				state := thoughts[key]
				if state == nil {
					state = add(map[string]interface{}{"id": "rs_" + randomHex(12), "type": "reasoning", "status": "in_progress", "summary": []interface{}{}})
					thoughts[key] = state
					emit("response.reasoning_summary_part.added", map[string]interface{}{"item_id": state.value["id"], "output_index": state.index, "summary_index": 0, "part": map[string]interface{}{"type": "summary_text", "text": ""}})
				}
				if thinking != "" {
					if state.closed {
						return fmt.Errorf("reasoning delta after item completion")
					}
					state.text.WriteString(thinking)
					emit("response.reasoning_summary_text.delta", map[string]interface{}{"item_id": state.value["id"], "output_index": state.index, "summary_index": 0, "delta": thinking})
				}
				if signature != "" {
					state.signature = signature
				}
				meaningful = true
			}
			if done, _ := delta["reasoning_done"].(bool); done {
				if key == "" {
					key = activeThought
				}
				if state := thoughts[key]; state != nil {
					state.closed = true
				}
				activeThought = ""
			}
			if search, ok := delta["x_grok_search"].(map[string]interface{}); ok {
				key := searchIdentity(search)
				state := searches[key]
				if state == nil {
					value := cloneStringInterfaceMap(search)
					if interfaceString(value["id"]) == "" {
						value["id"] = "ws_" + randomHex(12)
					}
					value["status"] = "in_progress"
					state = add(value)
					searches[key] = state
				}
				for k, v := range search {
					if k != "id" && k != "status" {
						state.value[k] = v
					}
				}
				if done, _ := delta["x_grok_search_done"].(bool); done {
					state.closed = true
					state.value["status"] = firstNonEmpty(interfaceString(search["status"]), "completed")
				}
				meaningful = true
				activeThought = ""
			}
			for _, kind := range []string{"content", "refusal"} {
				value := streamString(delta[kind])
				if value == "" {
					continue
				}
				meaningful = true
				activeThought = ""
				ensureMessage()
				partIndex := &textIndex
				partType := "output_text"
				builder := &text
				if kind == "refusal" {
					partIndex = &refusalIndex
					partType = "refusal"
					builder = &refusal
				}
				if *partIndex < 0 {
					parts := interfaceSlice(message.value["content"])
					*partIndex = len(parts)
					part := map[string]interface{}{"type": partType}
					if kind == "content" {
						part["text"] = ""
						part["annotations"] = []interface{}{}
					} else {
						part["refusal"] = ""
					}
					message.value["content"] = append(parts, part)
					emit("response.content_part.added", map[string]interface{}{"item_id": message.value["id"], "output_index": message.index, "content_index": *partIndex, "part": part})
				}
				builder.WriteString(value)
				emit("response."+partType+".delta", map[string]interface{}{"item_id": message.value["id"], "output_index": message.index, "content_index": *partIndex, "delta": value})
			}
			for _, ann := range responseAnnotations(delta["annotations"]) {
				b, _ := json.Marshal(ann)
				key := string(b)
				if !annotationKeys[key] {
					annotationKeys[key] = true
					annotations = append(annotations, ann)
				}
			}
			for _, raw := range interfaceSlice(delta["tool_calls"]) {
				call, ok := raw.(map[string]interface{})
				if !ok {
					return fmt.Errorf("invalid tool call")
				}
				fn, _ := call["function"].(map[string]interface{})
				index := interfaceToInt(call["index"])
				if index < 0 {
					return fmt.Errorf("invalid tool index")
				}
				callID, name := streamString(call["id"]), streamString(fn["name"])
				state := tools[index]
				if state == nil {
					if strings.TrimSpace(callID) == "" || callID == "<nil>" || strings.TrimSpace(name) == "" || name == "<nil>" || callIDs[callID] {
						return fmt.Errorf("invalid or duplicate tool identity")
					}
					callIDs[callID] = true
					state = add(map[string]interface{}{"id": "fc_" + randomHex(12), "type": "function_call", "call_id": callID, "name": name, "arguments": "", "status": "in_progress"})
					tools[index] = state
				} else if (callID != "" && callID != state.value["call_id"]) || (name != "" && name != state.value["name"]) {
					return fmt.Errorf("tool identity changed")
				}
				if fragment, exists := fn["arguments"]; exists {
					value, ok := fragment.(string)
					if !ok {
						return fmt.Errorf("tool arguments must be a string")
					}
					state.text.WriteString(value)
					if value != "" {
						emit("response.function_call_arguments.delta", map[string]interface{}{"item_id": state.value["id"], "output_index": state.index, "delta": value})
					}
				}
				meaningful = true
				activeThought = ""
			}
			if end := streamString(choice["finish_reason"]); end != "" {
				switch end {
				case "stop", "tool_calls", "length", "content_filter":
					finish = end
				default:
					return fmt.Errorf("unsupported finish_reason %q", end)
				}
			}
		}
		return writer.err
	})
	if err == io.EOF {
		err = nil
	}
	if err == nil && (!sawDone || finish == "") {
		err = fmt.Errorf("chat stream ended without finish_reason and [DONE]")
	}
	if err == nil && !meaningful && finish != "length" && finish != "content_filter" {
		err = fmt.Errorf("chat stream completed without output")
	}
	status, details := responseStatusFromFinish(finish)
	for _, item := range items {
		if item.value["type"] == "function_call" {
			args := item.text.String()
			if args == "" {
				args = "{}"
			}
			if status == "completed" && !json.Valid([]byte(args)) && err == nil {
				err = fmt.Errorf("invalid completed tool arguments")
			}
			item.value["arguments"] = args
		}
	}
	if writer.err != nil {
		return
	}
	if err != nil {
		v := response("failed")
		v["error"] = map[string]interface{}{"code": "upstream_stream_error", "message": err.Error()}
		emit("response.failed", map[string]interface{}{"response": v})
		_, _ = io.WriteString(writer, "data: [DONE]\n\n")
		writer.Flush()
		return
	}
	for _, item := range items {
		kind := streamString(item.value["type"])
		itemStatus := status
		if kind == "web_search_call" && item.closed {
			itemStatus = interfaceString(item.value["status"])
		}
		item.value["status"] = itemStatus
		switch kind {
		case "reasoning":
			part := map[string]interface{}{"type": "summary_text", "text": item.text.String()}
			item.value["summary"] = []interface{}{part}
			if item.signature != "" {
				item.value["encrypted_content"] = item.signature
			}
			emit("response.reasoning_summary_text.done", map[string]interface{}{"item_id": item.value["id"], "output_index": item.index, "summary_index": 0, "text": item.text.String()})
			emit("response.reasoning_summary_part.done", map[string]interface{}{"item_id": item.value["id"], "output_index": item.index, "summary_index": 0, "part": part})
		case "function_call":
			emit("response.function_call_arguments.done", map[string]interface{}{"item_id": item.value["id"], "output_index": item.index, "arguments": item.value["arguments"]})
		case "message":
			parts := interfaceSlice(item.value["content"])
			for i, raw := range parts {
				part := raw.(map[string]interface{})
				partType := streamString(part["type"])
				fields := map[string]interface{}{"item_id": item.value["id"], "output_index": item.index, "content_index": i}
				if partType == "output_text" {
					part["text"] = text.String()
					part["annotations"] = annotations
					fields["text"] = text.String()
					for n, ann := range annotations {
						emit("response.output_text.annotation.added", map[string]interface{}{"item_id": item.value["id"], "output_index": item.index, "content_index": i, "annotation_index": n, "annotation": ann})
					}
				} else {
					part["refusal"] = refusal.String()
					fields["refusal"] = refusal.String()
				}
				emit("response."+partType+".done", fields)
				emit("response.content_part.done", map[string]interface{}{"item_id": item.value["id"], "output_index": item.index, "content_index": i, "part": part})
			}
		}
		emit("response.output_item.done", map[string]interface{}{"output_index": item.index, "item": item.value})
	}
	if usage == nil {
		usage = responsesUsageFromChat(nil)
	}
	v := response(status)
	if details != nil {
		v["incomplete_details"] = details
	}
	emit("response."+status, map[string]interface{}{"response": v})
	_, _ = io.WriteString(writer, "data: [DONE]\n\n")
	writer.Flush()
}
