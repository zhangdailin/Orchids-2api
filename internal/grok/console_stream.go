package grok

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/goccy/go-json"
)

type chatOutcome struct {
	Usage      map[string]interface{}
	Finish     string
	FirstToken time.Time
	Err        error
}

// stopFilter keeps only a suffix that could still become a stop sequence.
// It operates on the original deltas: trimming whitespace corrupts tool JSON
// and text, and a stop sequence can span multiple upstream events.
type stopFilter struct {
	sequences []string
	pending   string
	matched   string
}

func (f *stopFilter) push(delta string, flush bool) string {
	if f.matched != "" {
		return ""
	}
	f.pending += delta
	cut, match := len(f.pending), ""
	for _, stop := range f.sequences {
		if stop == "" {
			continue
		}
		if i := strings.Index(f.pending, stop); i >= 0 && (match == "" || i < cut) {
			cut, match = i, stop
		}
	}
	if match != "" {
		out := f.pending[:cut]
		f.pending = ""
		f.matched = match
		return out
	}
	keep := 0
	if !flush {
		for _, stop := range f.sequences {
			for n := 1; n < len(stop) && n <= len(f.pending); n++ {
				if strings.HasSuffix(f.pending, stop[:n]) && n > keep {
					keep = n
				}
			}
		}
	}
	cut = len(f.pending) - keep
	for cut > 0 && !utf8.ValidString(f.pending[:cut]) {
		cut--
	}
	out := f.pending[:cut]
	f.pending = f.pending[cut:]
	return out
}

type responseToolState struct {
	itemID, callID, name, arguments string
	index                           int
}
type responseReasoningState struct{ source, text, signature, key string }

// readResponseSSE consumes whole SSE frames, including multi-line data. Event
// names are reset between frames; JSON type wins when a server supplies both.
func readResponseSSE(reader io.Reader, consume func(string, string) error) error {
	return consumeCompatibleSSE(reader, func(event compatibleSSEEvent) error {
		if !event.HasData() {
			return nil
		}
		return consume(event.Event, string(event.Data()))
	})
}

func responseFailure(ev map[string]interface{}) error {
	value := ev["error"]
	if response, ok := ev["response"].(map[string]interface{}); ok {
		value = response["error"]
	}
	if detail, ok := value.(map[string]interface{}); ok {
		if message := interfaceString(detail["message"]); message != "" {
			return fmt.Errorf("%s", message)
		}
	}
	if message := interfaceString(value); message != "" {
		return fmt.Errorf("%s", message)
	}
	if message := streamString(ev["message"]); message != "" {
		return fmt.Errorf("%s", message)
	}
	return fmt.Errorf("upstream response failed")
}

func (h *Handler) streamConsoleChat(w http.ResponseWriter, req *ChatCompletionsRequest, body io.Reader) (outcome chatOutcome) {
	flusher := streamResponseHeaders(w)
	id, created := "chatcmpl_"+randomHex(8), time.Now().Unix()
	var text, reasoning, refusal strings.Builder
	tools := map[string]*responseToolState{}
	byCall := map[string]*responseToolState{}
	byIndex := map[int]*responseToolState{}
	var ordered []*responseToolState
	thoughts := map[string]*responseReasoningState{}
	var annotations []map[string]interface{}
	filter := stopFilter{sequences: req.Stop}
	terminal := false
	sawText := false
	lastSignature := ""
	var replayItems []interface{}
	activeReasoning := "anonymous"
	searchDone := map[string]bool{}
	finish := "stop"
	emit := func(delta map[string]interface{}, done string, usage map[string]interface{}) error {
		chunk := map[string]interface{}{"id": id, "object": "chat.completion.chunk", "created": created, "model": req.Model,
			"choices": []map[string]interface{}{{"index": 0, "delta": delta, "finish_reason": nil}}}
		if done != "" {
			chunk["choices"].([]map[string]interface{})[0]["finish_reason"] = done
		}
		if usage != nil {
			chunk["usage"] = usage
		}
		if outcome.FirstToken.IsZero() && (delta["content"] != nil || delta["refusal"] != nil || delta["reasoning_content"] != nil || delta["tool_calls"] != nil) {
			outcome.FirstToken = time.Now()
		}
		raw, err := json.Marshal(chunk)
		if err != nil {
			return err
		}
		if _, err = fmt.Fprintf(w, "data: %s\n\n", raw); err != nil {
			return err
		}
		flusher.Flush()
		return nil
	}
	fail := func(err error) {
		outcome.Err = err
		outcome.Finish = "error"
		writeSSEStreamError(w, flusher, nil, err.Error())
	}
	if err := emit(map[string]interface{}{"role": "assistant"}, "", nil); err != nil {
		outcome.Err = err
		return
	}
	emitText := func(value string) error {
		if value == "" {
			return nil
		}
		text.WriteString(value)
		return emit(map[string]interface{}{"content": value}, "", nil)
	}
	emitArgs := func(tc *responseToolState, value string, snapshot bool) error {
		if snapshot {
			if value == "" {
				value = "{}"
			}
			if !strings.HasPrefix(value, tc.arguments) {
				return fmt.Errorf("tool %s arguments snapshot conflicts with streamed arguments", tc.callID)
			}
			value = strings.TrimPrefix(value, tc.arguments)
		}
		if value == "" {
			return nil
		}
		tc.arguments += value
		return emit(map[string]interface{}{"tool_calls": []map[string]interface{}{{"index": tc.index, "function": map[string]interface{}{"arguments": value}}}}, "", nil)
	}
	toolItem := func(item, ev map[string]interface{}) error {
		itemID, callID, name := strings.TrimSpace(streamString(item["id"])), strings.TrimSpace(streamString(item["call_id"])), strings.TrimSpace(streamString(item["name"]))
		if callID == "" {
			callID = itemID
		}
		if itemID == "" {
			itemID = callID
		}
		if name == "" || name == "<nil>" || callID == "" || callID == "<nil>" {
			return fmt.Errorf("upstream function_call missing valid name or call_id")
		}
		tc := tools[itemID]
		if tc == nil {
			if byCall[callID] != nil {
				return fmt.Errorf("duplicate upstream call_id %q", callID)
			}
			tc = &responseToolState{itemID: itemID, callID: callID, name: name, index: len(ordered)}
			tools[itemID], byCall[callID] = tc, tc
			ordered = append(ordered, tc)
			if index, exists := ev["output_index"]; exists {
				if prior := byIndex[interfaceToInt(index)]; prior != nil && prior != tc {
					return fmt.Errorf("upstream output_index reused by different tools")
				}
				byIndex[interfaceToInt(index)] = tc
			}
			if err := emit(map[string]interface{}{"tool_calls": []map[string]interface{}{{"index": tc.index, "id": callID, "type": "function", "function": map[string]interface{}{"name": name, "arguments": ""}}}}, "", nil); err != nil {
				return err
			}
		} else if tc.callID != callID || tc.name != name {
			return fmt.Errorf("upstream tool identity changed for %s", itemID)
		}
		if args, ok := item["arguments"]; ok && args != nil {
			value, ok := args.(string)
			if !ok {
				return fmt.Errorf("tool arguments must be a JSON string")
			}
			if value != "" {
				return emitArgs(tc, value, true)
			}
		}
		return nil
	}
	thought := func(key string) *responseReasoningState {
		if key == "" {
			key = activeReasoning
		}
		if thoughts[key] == nil {
			thoughts[key] = &responseReasoningState{key: key}
		}
		return thoughts[key]
	}
	emitThought := func(key, source, value string) error {
		state := thought(key)
		if state.source == "" {
			state.source = source
		}
		if state.source != source || value == "" {
			return nil
		}
		state.text += value
		reasoning.WriteString(value)
		return emit(map[string]interface{}{"reasoning_content": value, "reasoning_item_id": state.key}, "", nil)
	}
	err := readResponseSSE(body, func(event, data string) error {
		if data == "[DONE]" {
			if !terminal {
				return fmt.Errorf("upstream stream ended without a response terminal event")
			}
			return io.EOF
		}
		var ev map[string]interface{}
		if err := json.Unmarshal([]byte(data), &ev); err != nil {
			return fmt.Errorf("console stream parse error: %w", err)
		}
		kind := firstNonEmpty(interfaceString(ev["type"]), event)
		if kind == "error" || kind == "response.failed" {
			return responseFailure(ev)
		}
		if terminal {
			return nil
		}
		annotations = appendUniqueConsoleAnnotations(annotations, consoleFlatAnnotations(ev))
		if usage := consoleUsageFromStreamEvent(ev); len(usage) > 0 {
			outcome.Usage = usage
		}
		item, _ := ev["item"].(map[string]interface{})
		if kind == "response.output_item.done" && item != nil {
			// Accumulate the portable items so the next turn can replay the whole
			// completed turn, not just its opaque reasoning cipher.
			switch interfaceString(item["type"]) {
			case "reasoning", "message", "function_call", "custom_tool_call":
				replayItems = append(replayItems, item)
			}
		}
		if kind == "response.output_item.added" || kind == "response.output_item.done" {
			switch interfaceString(item["type"]) {
			case "function_call":
				if filter.matched == "" {
					return toolItem(item, ev)
				}
			case "web_search_call", "x_search_call":
				if filter.matched != "" {
					return nil
				}
				searchDone[searchIdentity(item)] = kind == "response.output_item.done"
				return emit(map[string]interface{}{"x_grok_search": item, "x_grok_search_done": kind == "response.output_item.done"}, "", nil)
			case "reasoning":
				key := interfaceString(item["id"])
				if filter.matched != "" {
					return nil
				}
				if key != "" && kind == "response.output_item.done" && thoughts[key] == nil && activeReasoning == "anonymous" && thoughts["anonymous"] != nil {
					thoughts[key] = thoughts["anonymous"]
				}
				if key != "" {
					activeReasoning = key
				}
				state := thought(key)
				if kind == "response.output_item.done" {
					if state.text == "" {
						value := consoleExtractReasoningText(map[string]interface{}{"output": []interface{}{item}})
						if err := emitThought(key, "snapshot", value); err != nil {
							return err
						}
					}
				}
				if signature := streamString(item["encrypted_content"]); signature != "" && signature != state.signature {
					state.signature = signature
					lastSignature = signature
					if err := emit(map[string]interface{}{"reasoning_item_id": state.key, "reasoning_encrypted_content": signature}, "", nil); err != nil {
						return err
					}
				}
				if kind == "response.output_item.done" {
					return emit(map[string]interface{}{"reasoning_item_id": state.key, "reasoning_done": true}, "", nil)
				}
			}
			return nil
		}
		if strings.HasPrefix(kind, "response.function_call_arguments.") {
			if filter.matched != "" {
				return nil
			}
			itemID := interfaceString(ev["item_id"])
			tc := tools[itemID]
			if tc == nil && itemID == "" {
				if index, exists := ev["output_index"]; exists {
					tc = byIndex[interfaceToInt(index)]
				}
			}
			if tc == nil {
				return fmt.Errorf("arguments event references an unknown tool item")
			}
			if strings.HasSuffix(kind, ".delta") {
				value, _ := ev["delta"].(string)
				return emitArgs(tc, value, false)
			}
			if strings.HasSuffix(kind, ".done") {
				value, _ := ev["arguments"].(string)
				return emitArgs(tc, value, true)
			}
			return nil
		}
		if value := consoleReasoningDelta(kind, ev); value != "" && filter.matched == "" {
			source := "raw"
			if strings.Contains(kind, "summary") {
				source = "summary"
			}
			return emitThought(interfaceString(ev["item_id"]), source, value)
		}
		if kind == "response.output_text.delta" && filter.matched == "" {
			sawText = true
			value, _ := ev["delta"].(string)
			return emitText(filter.push(value, false))
		}
		if kind == "response.refusal.delta" && filter.matched == "" {
			value := streamString(ev["delta"])
			refusal.WriteString(value)
			return emit(map[string]interface{}{"refusal": value}, "", nil)
		}
		if kind == "response.completed" || kind == "response.incomplete" {
			response, _ := ev["response"].(map[string]interface{})
			terminalFinish, err := responseTerminalFinish(kind, response)
			if err != nil {
				return err
			}
			finish = terminalFinish
			for _, raw := range interfaceSlice(response["output"]) {
				entry, _ := raw.(map[string]interface{})
				if interfaceString(entry["type"]) == "reasoning" && filter.matched == "" {
					key := interfaceString(entry["id"])
					state := thought(key)
					if signature := interfaceString(entry["encrypted_content"]); signature != "" {
						lastSignature = signature
					}
					if state.text == "" {
						if err := emitThought(key, "snapshot", consoleExtractReasoningText(map[string]interface{}{"output": []interface{}{entry}})); err != nil {
							return err
						}
					}
					if signature := interfaceString(entry["encrypted_content"]); signature != "" && signature != state.signature {
						state.signature = signature
						if err := emit(map[string]interface{}{"reasoning_item_id": state.key, "reasoning_encrypted_content": signature}, "", nil); err != nil {
							return err
						}
					}
					if err := emit(map[string]interface{}{"reasoning_item_id": state.key, "reasoning_done": true}, "", nil); err != nil {
						return err
					}
				}
				if (interfaceString(entry["type"]) == "web_search_call" || interfaceString(entry["type"]) == "x_search_call") && !searchDone[searchIdentity(entry)] && filter.matched == "" {
					searchDone[searchIdentity(entry)] = true
					if err := emit(map[string]interface{}{"x_grok_search": entry, "x_grok_search_done": true}, "", nil); err != nil {
						return err
					}
				}
				if interfaceString(entry["type"]) == "function_call" && filter.matched == "" {
					if err := toolItem(entry, nil); err != nil {
						return err
					}
				}
			}
			// Some compatible servers only provide a final output snapshot.
			if refusal.Len() == 0 && filter.matched == "" {
				if value := consoleExtractRefusal(response); value != "" {
					refusal.WriteString(value)
					if err := emit(map[string]interface{}{"refusal": value}, "", nil); err != nil {
						return err
					}
				}
			}
			if !sawText && filter.matched == "" {
				if err := emitText(filter.push(consoleExtractMessageText(response), false)); err != nil {
					return err
				}
			}
			terminal = true
			return io.EOF
		}
		return nil
	})
	if err != nil && err != io.EOF {
		fail(fmt.Errorf("console stream read error: %w", err))
		return
	}
	if !terminal {
		fail(fmt.Errorf("upstream stream closed before response completion"))
		return
	}
	if err := emitText(filter.push("", true)); err != nil {
		fail(err)
		return
	}
	var calls []map[string]interface{}
	for _, tc := range ordered {
		if tc.arguments == "" {
			if err := emitArgs(tc, "{}", false); err != nil {
				fail(err)
				return
			}
		}
		if !json.Valid([]byte(tc.arguments)) && finish != "length" {
			fail(fmt.Errorf("invalid JSON arguments for tool %s", tc.callID))
			return
		}
		calls = append(calls, map[string]interface{}{"id": tc.callID, "type": "function", "function": map[string]interface{}{"name": tc.name, "arguments": tc.arguments}})
	}
	if finish != "length" && len(calls) > 0 {
		finish = "tool_calls"
	}
	if filter.matched != "" {
		finish = "stop"
	}
	if text.Len() == 0 && refusal.Len() == 0 && len(calls) == 0 && finish != "length" && filter.matched == "" {
		fail(fmt.Errorf("upstream completed response with no content or tool calls"))
		return
	}
	if outcome.Usage == nil {
		outcome.Usage = addReasoningUsage(buildChatUsagePayload(req, text.String()+refusal.String(), calls), reasoning.String())
	}
	delta := map[string]interface{}{}
	if len(annotations) > 0 {
		delta["annotations"] = consoleChatAnnotations(annotations)
	}
	if filter.matched != "" {
		delta["stop_sequence"] = filter.matched
	}
	if err := emit(delta, finish, outcome.Usage); err != nil {
		outcome.Err = err
		return
	}
	if _, err := io.WriteString(w, "data: [DONE]\n\n"); err != nil {
		outcome.Err = err
		return
	}
	flusher.Flush()
	outcome.Finish = finish
	if req.ReasoningReplay {
		if len(replayItems) > 0 {
			h.captureReasoningReplayItems(context.Background(), req.Model, req.PromptCacheKey, replayItems)
		} else if lastSignature != "" {
			h.storeReasoningReplay(req.Model, req.PromptCacheKey, lastSignature)
		}
	}
	return
}
