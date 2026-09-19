package grok

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/goccy/go-json"

	"orchids-api/internal/modelpolicy"
)

var buildToolAliasInvalid = regexp.MustCompile(`[^A-Za-z0-9_-]+`)

const buildCompatibilityWarningsKey = "__orchids_build_compatibility_warnings"

type buildToolNormalizationState struct {
	seen     map[string]int
	aliases  map[string]string
	warnings []string
	warning  map[string]struct{}
}

func newBuildToolNormalizationState() *buildToolNormalizationState {
	return &buildToolNormalizationState{seen: map[string]int{}, aliases: map[string]string{}, warning: map[string]struct{}{}}
}

func (s *buildToolNormalizationState) addWarning(value string) {
	if s == nil || value == "" {
		return
	}
	if _, exists := s.warning[value]; exists {
		return
	}
	s.warning[value] = struct{}{}
	s.warnings = append(s.warnings, value)
}

func (s *buildToolNormalizationState) alias(namespace, name string) string {
	key := strings.TrimSpace(namespace) + "\x00" + strings.TrimSpace(name)
	if alias := s.aliases[key]; alias != "" {
		return alias
	}
	base := buildToolAlias(namespace, name)
	alias := base
	for index := 2; s.seen[alias] > 0; index++ {
		suffix := fmt.Sprintf("_%d", index)
		limit := 128 - len(suffix)
		prefix := base
		if limit < len(base) {
			prefix = base[:limit]
		}
		alias = strings.TrimSuffix(prefix, "_") + suffix
		s.addWarning("function_name_collision_renamed")
	}
	s.seen[alias] = 1
	s.aliases[key] = alias
	return alias
}

func takeBuildCompatibilityWarnings(payload map[string]interface{}) string {
	if payload == nil {
		return ""
	}
	raw := payload[buildCompatibilityWarningsKey]
	delete(payload, buildCompatibilityWarningsKey)
	values, _ := raw.([]string)
	return strings.Join(values, ",")
}

type buildToolAliasIdentity struct {
	Kind        string
	Namespace   string
	Name        string
	Declaration map[string]interface{}
}

func collectBuildToolAliases(payload map[string]interface{}) map[string]buildToolAliasIdentity {
	aliases := map[string]buildToolAliasIdentity{}
	state := newBuildToolNormalizationState()
	var collect func([]map[string]interface{}, string)
	collect = func(tools []map[string]interface{}, namespace string) {
		for _, tool := range tools {
			kind := strings.ToLower(strings.TrimSpace(fmt.Sprint(tool["type"])))
			switch kind {
			case "namespace":
				collect(interfaceMaps(tool["tools"]), strings.TrimSpace(fmt.Sprint(tool["name"])))
			case "function":
				name := strings.TrimSpace(fmt.Sprint(tool["name"]))
				if nested, ok := tool["function"].(map[string]interface{}); ok {
					name = strings.TrimSpace(fmt.Sprint(nested["name"]))
				}
				if name != "" && name != "<nil>" {
					aliases[state.alias(namespace, name)] = buildToolAliasIdentity{Kind: "function", Namespace: namespace, Name: name, Declaration: cloneStringInterfaceMap(tool)}
				}
			case "tool_search":
				if strings.EqualFold(strings.TrimSpace(fmt.Sprint(tool["execution"])), "client") {
					aliases["tool_search"] = buildToolAliasIdentity{Kind: "tool_search", Name: "tool_search", Declaration: cloneStringInterfaceMap(tool)}
				}
			case "apply_patch":
				aliases["apply_patch"] = buildToolAliasIdentity{Kind: "apply_patch", Name: "apply_patch", Declaration: cloneStringInterfaceMap(tool)}
			case "custom":
				name := strings.TrimSpace(fmt.Sprint(tool["name"]))
				if name != "" && name != "<nil>" {
					aliases[buildToolAlias(namespace, name)] = buildToolAliasIdentity{Kind: "custom", Namespace: namespace, Name: name, Declaration: cloneStringInterfaceMap(tool)}
				}
			}
		}
	}
	collect(interfaceMaps(payload["tools"]), "")
	return aliases
}

// responsesPayloadFromChat converts Chat/Messages compatibility input into
// the native Responses wire shape used by both Build and Console.
func (h *Handler) responsesPayloadFromChat(spec ModelSpec, req *ChatCompletionsRequest, build bool) (map[string]interface{}, error) {
	if build {
		spec.Upstream, spec.ConsoleModel = UpstreamCLI, ""
	}
	if err := validateNativeChatContent(req.Messages); err != nil {
		return nil, err
	}
	input, instructions := responsesInputFromChatMessages(req.Messages)
	if len(req.ResponsesInput) > 0 {
		input = append([]interface{}(nil), req.ResponsesInput...)
	}
	if req.ReasoningReplay && strings.TrimSpace(req.PromptCacheKey) != "" {
		if items := h.loadReasoningReplayItems(req.Model, req.PromptCacheKey); len(items) > 0 {
			// Filtering drops anything the caller already sent, so a client that
			// resends its own history is never duplicated.
			if filtered := filterReplayItemsForInput(input, items); len(filtered) > 0 {
				input = insertReplayItems(input, filtered)
			}
		}
	}
	if len(input) == 0 && instructions == "" {
		return nil, fmt.Errorf("empty message")
	}
	model := spec.ConsoleModel
	if build {
		model = spec.UpstreamModel
	}
	payload := map[string]interface{}{"model": model, "input": input}
	if instructions != "" {
		payload["instructions"] = instructions
	}
	if req.Stream {
		payload["stream"] = true
	}
	if req.Temperature != nil {
		payload["temperature"] = *req.Temperature
	}
	if req.TopP != nil {
		payload["top_p"] = *req.TopP
	}
	if req.MaxTokens != nil && *req.MaxTokens > 0 {
		payload["max_output_tokens"] = *req.MaxTokens
	}
	// Chat Completions has no native reasoning object; rebuild the Responses
	// shape from the relay's reasoning_effort / reasoning_summary extensions.
	if reasoning := chatReasoningControls(req); len(reasoning) > 0 {
		payload["reasoning"] = reasoning
	}
	if len(req.Stop) > 0 {
		payload["stop"] = append([]string(nil), req.Stop...)
	}
	if value := strings.TrimSpace(req.SafetyIdentifier); value != "" {
		payload["safety_identifier"] = value
	}
	if len(req.ResponseText) > 0 {
		payload["text"] = cloneStringInterfaceMap(req.ResponseText)
	} else if len(req.ResponseFormat) > 0 {
		payload["text"] = map[string]interface{}{"format": normalizeChatResponseFormat(req.ResponseFormat)}
	}
	// Both planes return opaque reasoning only when it is explicitly requested.
	// Without it the replay cache is never populated, so a default (auto) turn
	// would silently lose multi-turn reasoning continuity.
	include := uniqueStrings(append([]string(nil), req.Include...))
	include = uniqueStrings(append(include, "reasoning.encrypted_content"))
	if len(include) > 0 {
		payload["include"] = include
	}
	tools := append([]map[string]interface{}(nil), req.ResponsesTools...)
	tools = append(tools, consoleToolsFromOpenAI(req.Tools)...)
	// OpenAI's web_search_options has no function form: it means "run the
	// hosted search tool". Lower it to the native tool the Responses planes
	// understand (grok2api does the same) when the caller did not already
	// declare a search tool.
	if len(req.WebSearchOptions) > 0 && !hasNativeSearchTool(tools) {
		tools = append(tools, map[string]interface{}{"type": "web_search"})
	}
	if len(tools) > 0 {
		payload["tools"] = tools
		if choice := consoleToolChoiceFromOpenAI(req.ToolChoice); choice != nil {
			payload["tool_choice"] = choice
		}
	}
	// These are forwarded unconditionally, exactly like grok2api: a caller that
	// asks for a service tier or attaches metadata must not have it dropped
	// just because the request declared no tools.
	if req.ParallelToolCalls != nil {
		payload["parallel_tool_calls"] = *req.ParallelToolCalls
	}
	if len(req.Metadata) > 0 {
		payload["metadata"] = cloneStringInterfaceMap(req.Metadata)
	}
	if tier := strings.TrimSpace(req.ServiceTier); tier != "" {
		payload["service_tier"] = tier
	}
	if err := validatePayloadReasoning(payload); err != nil {
		return nil, err
	}
	if build {
		// The official Grok Build client always asks its Responses backend for a
		// reasoning summary, even at the model's default effort. Chat Completions
		// has no summary parameter, so make that Build-specific default explicit —
		// without overriding a summary the caller already asked for, and without
		// imposing it on native Responses or Anthropic requests.
		if req.sourceOperation == "" && (req.ReasoningEffort == nil || *req.ReasoningEffort != "none") {
			reasoning, _ := payload["reasoning"].(map[string]interface{})
			if reasoning == nil {
				reasoning = map[string]interface{}{}
			}
			if _, exists := reasoning["summary"]; !exists {
				reasoning["summary"] = "concise"
			}
			payload["reasoning"] = reasoning
		}
		if strings.TrimSpace(req.PromptCacheKey) != "" {
			payload["prompt_cache_key"] = strings.TrimSpace(req.PromptCacheKey)
		}
		if err := normalizeBuildResponsesPayload(payload); err != nil {
			return nil, err
		}
		normalizeBuildReasoningEffort(payload, model)
		return payload, nil
	}
	// Console is stateless and rejects these client-side state hints.
	payload["store"] = false
	delete(payload, "prompt_cache_key")
	normalizeConsoleReasoningEffort(payload, model)
	if len(interfaceMaps(payload["tools"])) == 0 {
		delete(payload, "tools")
		delete(payload, "tool_choice")
	}
	return payload, nil
}

func responsesInputFromChatMessages(messages []ChatMessage) ([]interface{}, string) {
	items := make([]interface{}, 0, len(messages))
	var instructions strings.Builder
	for _, message := range messages {
		role := strings.ToLower(strings.TrimSpace(message.Role))
		if role == "system" || role == "developer" {
			text := strings.TrimSpace(chatMessageContentText(message.Content))
			if text != "" {
				if instructions.Len() > 0 {
					instructions.WriteString("\n\n")
				}
				instructions.WriteString(text)
			}
			continue
		}
		if role == "tool" {
			// Only tool_call_id may name the call it answers. Falling back to
			// the function name produced a function_call_output for a call id
			// that does not exist, and the upstream then rejected the whole
			// turn with a message that named neither field.
			callID := strings.TrimSpace(message.ToolCallID)
			if callID == "" {
				// A tool message without an id cannot be paired with its call;
				// skipping it keeps the rest of the turn valid instead of
				// emitting a function_call_output for an id that does not exist.
				continue
			}
			items = append(items, map[string]interface{}{"type": "function_call_output", "call_id": callID, "output": responsesToolOutput(message.Content)})
			continue
		}
		if role == "assistant" && (strings.TrimSpace(message.ReasoningContent) != "" || strings.TrimSpace(message.ReasoningEncryptedContent) != "") {
			reasoning := map[string]interface{}{"type": "reasoning", "summary": []interface{}{}}
			if text := strings.TrimSpace(message.ReasoningContent); text != "" {
				reasoning["summary"] = []interface{}{map[string]interface{}{"type": "summary_text", "text": text}}
			}
			if encrypted := strings.TrimSpace(message.ReasoningEncryptedContent); encrypted != "" {
				reasoning["encrypted_content"] = encrypted
			}
			items = append(items, reasoning)
		}
		if role == "assistant" {
			for _, call := range message.ToolCalls {
				name := strings.TrimSpace(fmt.Sprint(call.Function["name"]))
				if name == "" {
					continue
				}
				items = append(items, map[string]interface{}{
					"type": "function_call", "call_id": firstNonEmpty(strings.TrimSpace(call.ID), "call_"+randomHex(12)),
					"name": name, "arguments": stringifyToolArguments(call.Function["arguments"]),
				})
			}
		}
		parts := responsesMessageParts(message.Content, role == "assistant")
		if len(parts) == 0 {
			continue
		}
		if role != "assistant" {
			role = "user"
		}
		items = append(items, map[string]interface{}{"type": "message", "role": role, "content": parts})
	}
	return items, strings.TrimSpace(instructions.String())
}

func validateNativeChatContent(messages []ChatMessage) error {
	for _, message := range messages {
		for _, part := range interfaceMaps(message.Content) {
			switch parseLooseStringAny(part["type"]) {
			case "text", "input_text", "output_text", "image_url", "input_image":
			default:
				return fmt.Errorf("Build/Console Chat does not support content.type=%q", part["type"])
			}
		}
	}
	return nil
}

func responsesMessageParts(content interface{}, assistant bool) []interface{} {
	textType := "input_text"
	if assistant {
		textType = "output_text"
	}
	switch value := content.(type) {
	case string:
		if strings.TrimSpace(value) == "" {
			return nil
		}
		return []interface{}{map[string]interface{}{"type": textType, "text": value}}
	case []interface{}:
		parts := make([]interface{}, 0, len(value))
		for _, raw := range value {
			block, _ := raw.(map[string]interface{})
			kind := strings.ToLower(strings.TrimSpace(fmt.Sprint(block["type"])))
			switch kind {
			case "text", "input_text", "output_text":
				if text := fmt.Sprint(block["text"]); text != "" && text != "<nil>" {
					parts = append(parts, map[string]interface{}{"type": textType, "text": text})
				}
			case "image_url", "input_image", "image":
				if url := responseImageURL(block); url != "" {
					part := map[string]interface{}{"type": "input_image", "image_url": url}
					detail := parseLooseStringAny(block["detail"])
					if nested, ok := block["image_url"].(map[string]interface{}); ok && detail == "" {
						detail = parseLooseStringAny(nested["detail"])
					}
					if detail == "" {
						// The upstream treats an absent detail as its own default,
						// which is not "auto"; stating it makes the request
						// deterministic and matches what grok2api sends.
						detail = "auto"
					}
					part["detail"] = detail
					parts = append(parts, part)
				}
			case "file_url", "input_file":
				part := map[string]interface{}{"type": "input_file"}
				for _, key := range []string{"file_url", "file_data", "file_id", "filename"} {
					if v, ok := block[key]; ok {
						if nested, nestedOK := v.(map[string]interface{}); nestedOK {
							v = nested["url"]
						}
						if text := strings.TrimSpace(fmt.Sprint(v)); text != "" && text != "<nil>" {
							part[key] = v
						}
					}
				}
				if len(part) > 1 {
					parts = append(parts, part)
				}
			}
		}
		return parts
	default:
		return nil
	}
}

func responsesToolOutput(content interface{}) interface{} {
	if text, ok := content.(string); ok {
		return text
	}
	if parts := responsesMessageParts(content, false); len(parts) > 0 {
		return parts
	}
	return chatMessageContentText(content)
}

func responseImageURL(block map[string]interface{}) string {
	for _, key := range []string{"image_url", "url"} {
		value := block[key]
		if nested, ok := value.(map[string]interface{}); ok {
			value = nested["url"]
		}
		if text := strings.TrimSpace(fmt.Sprint(value)); text != "" && text != "<nil>" {
			return text
		}
	}
	return ""
}

func stringifyToolArguments(value interface{}) string {
	if text, ok := value.(string); ok && strings.TrimSpace(text) != "" {
		return strings.TrimSpace(text)
	}
	if value == nil {
		return "{}"
	}
	if raw, err := json.Marshal(value); err == nil {
		return string(raw)
	}
	return "{}"
}

func normalizeChatResponseFormat(format map[string]interface{}) map[string]interface{} {
	copy := cloneStringInterfaceMap(format)
	if strings.EqualFold(strings.TrimSpace(fmt.Sprint(copy["type"])), "json_schema") {
		if nested, ok := copy["json_schema"].(map[string]interface{}); ok {
			flattened := map[string]interface{}{"type": "json_schema"}
			for key, value := range nested {
				flattened[key] = value
			}
			copy = flattened
		}
	}
	return copy
}

func interfaceMaps(value interface{}) []map[string]interface{} {
	switch values := value.(type) {
	case []map[string]interface{}:
		return values
	case []interface{}:
		out := make([]map[string]interface{}, 0, len(values))
		for _, value := range values {
			if item, ok := value.(map[string]interface{}); ok {
				out = append(out, item)
			}
		}
		return out
	default:
		return nil
	}
}

func normalizeBuildResponsesPayload(payload map[string]interface{}) error {
	state := newBuildToolNormalizationState()
	if err := normalizeBuildInputHistory(payload, state); err != nil {
		return err
	}
	// NOTE: the native Build relay is intentionally byte-transparent (see
	// relay_policy_test.go: a client payload must reach the upstream unchanged,
	// and only prompt_cache_key is rewritten). grok2api instead injects
	// `store:false` and `reasoning.encrypted_content` here; doing that in this
	// gateway would break the documented transparency contract, so the two
	// defaults stay a deliberate deviation. The chat->Responses bridge, which
	// builds its own payload, does request encrypted reasoning.
	if raw, ok := payload["response_format"].(map[string]interface{}); ok {
		delete(payload, "response_format")
		if _, exists := payload["text"]; !exists {
			payload["text"] = map[string]interface{}{"format": normalizeChatResponseFormat(raw)}
		}
	}
	tools := interfaceMaps(payload["tools"])
	if len(tools) == 0 {
		delete(payload, "tools")
		delete(payload, "tool_choice")
		return nil
	}
	clientSearch := false
	serverSearch := false
	for _, tool := range tools {
		if strings.EqualFold(strings.TrimSpace(fmt.Sprint(tool["type"])), "tool_search") {
			execution := strings.ToLower(strings.TrimSpace(fmt.Sprint(tool["execution"])))
			if execution == "client" {
				clientSearch = true
			} else {
				serverSearch = true
			}
		}
	}
	if clientSearch && serverSearch {
		return fmt.Errorf("tools cannot mix client and server tool_search")
	}
	normalized := make([]map[string]interface{}, 0, len(tools))
	for index, tool := range tools {
		items, err := normalizeBuildTool(tool, "", clientSearch, serverSearch, fmt.Sprintf("tools.%d", index), state)
		if err != nil {
			return err
		}
		normalized = append(normalized, items...)
	}
	if clientSearch {
		normalized = append(normalized, map[string]interface{}{
			"type": "function", "name": "tool_search", "description": "Search for tools needed to continue the task.",
			"parameters": map[string]interface{}{"type": "object", "properties": map[string]interface{}{}, "additionalProperties": true},
		})
		if parallel, exists := payload["parallel_tool_calls"]; !exists || parallel != false {
			state.addWarning("client_tool_search_forced_serial")
		}
		state.addWarning("client_tool_search_emulated")
		payload["parallel_tool_calls"] = false
	} else if serverSearch {
		state.addWarning("server_tool_search_eager_loaded")
	}
	if len(normalized) == 0 {
		delete(payload, "tools")
		delete(payload, "tool_choice")
		return nil
	}
	payload["tools"] = normalized
	normalizeBuildToolChoice(payload, state)
	for _, item := range interfaceMaps(payload["input"]) {
		// A client that echoes back the calls this layer emulates (custom_tool_call,
		// apply_patch_call and their outputs) is lowered onto the emulated function
		// shape, otherwise the upstream rejects an unknown item type.
		switch strings.ToLower(strings.TrimSpace(parseLooseStringAny(item["type"]))) {
		case "custom_tool_call", "apply_patch_call":
			lowerEmulatedCallItem(item, state)
			continue
		case "custom_tool_call_output", "apply_patch_call_output":
			item["type"] = "function_call_output"
			continue
		}
		if parseLooseStringAny(item["type"]) != "function_call" {
			continue
		}
		key := strings.TrimSpace(parseLooseStringAny(item["namespace"])) + "\x00" + strings.TrimSpace(parseLooseStringAny(item["name"]))
		if alias := state.aliases[key]; alias != "" {
			item["name"] = alias
			delete(item, "namespace")
		}
	}
	if len(state.warnings) > 0 {
		payload[buildCompatibilityWarningsKey] = append([]string(nil), state.warnings...)
	}
	return nil
}

func normalizeBuildTool(tool map[string]interface{}, namespace string, clientSearch, serverSearch bool, param string, state *buildToolNormalizationState) ([]map[string]interface{}, error) {
	kind := strings.ToLower(strings.TrimSpace(fmt.Sprint(tool["type"])))
	if kind == "function" {
		if nested, ok := tool["function"].(map[string]interface{}); ok {
			flattened := cloneStringInterfaceMap(nested)
			flattened["type"] = "function"
			tool = flattened
		}
		name := strings.TrimSpace(fmt.Sprint(tool["name"]))
		if name == "" || name == "<nil>" {
			return nil, fmt.Errorf("%s.name is required", param)
		}
		if deferred, _ := tool["defer_loading"].(bool); deferred && clientSearch && !serverSearch {
			return nil, nil
		}
		if deferred, _ := tool["defer_loading"].(bool); deferred && !clientSearch && !serverSearch {
			state.addWarning("orphan_deferred_tool_loaded")
		}
		out := cloneStringInterfaceMap(tool)
		delete(out, "defer_loading")
		out["name"] = state.alias(namespace, name)
		if schema, ok := out["parameters"].(map[string]interface{}); ok {
			normalized := normalizeBuildFunctionRoot(schema)
			if !mapsEqualJSON(schema, normalized) {
				state.addWarning("function_parameters_nullable_root_normalized")
			}
			out["parameters"] = normalized
		}
		return []map[string]interface{}{out}, nil
	}
	if kind == "namespace" {
		name := strings.TrimSpace(fmt.Sprint(tool["name"]))
		children := interfaceMaps(tool["tools"])
		if name == "" || name == "<nil>" || len(children) == 0 {
			return nil, fmt.Errorf("%s namespace requires name and function tools", param)
		}
		out := make([]map[string]interface{}, 0, len(children))
		for index, child := range children {
			if !strings.EqualFold(strings.TrimSpace(fmt.Sprint(child["type"])), "function") {
				return nil, fmt.Errorf("%s.tools.%d must be a function", param, index)
			}
			items, err := normalizeBuildTool(child, name, clientSearch, serverSearch, fmt.Sprintf("%s.tools.%d", param, index), state)
			if err != nil {
				return nil, err
			}
			out = append(out, items...)
		}
		return out, nil
	}
	switch kind {
	case "tool_search":
		return nil, nil
	case "apply_patch":
		// The emulated function mirrors the upstream-compatible contract used
		// by grok2api: one structured V4A operation, so the response side can
		// restore `operation` on the apply_patch_call without parsing a patch.
		state.addWarning("apply_patch_emulated")
		return []map[string]interface{}{{
			"type": "function", "name": "apply_patch",
			"description": "Create, update, or delete one file using a structured V4A patch operation. " +
				"create_file and update_file require path and diff; delete_file requires path.",
			"parameters": map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"operation": map[string]interface{}{
						"type": "object",
						"properties": map[string]interface{}{
							"type": map[string]interface{}{"type": "string", "enum": []interface{}{"create_file", "update_file", "delete_file"}},
							"path": map[string]interface{}{"type": "string", "minLength": 1},
							"diff": map[string]interface{}{"type": "string"},
						},
						"required": []interface{}{"type", "path"}, "additionalProperties": false,
					},
				},
				"required": []interface{}{"operation"}, "additionalProperties": false,
			},
			"strict": true,
		}}, nil
	case "local_shell":
		state.addWarning("local_shell_normalized")
		out := cloneStringInterfaceMap(tool)
		out["type"] = "shell"
		return []map[string]interface{}{out}, nil
	case "web_search_preview", "web_search_preview_2025_03_11", "web_search_2025_08_26":
		state.addWarning("web_search_controls_downgraded")
		out := cloneStringInterfaceMap(tool)
		out["type"] = "web_search"
		return []map[string]interface{}{out}, nil
	case "custom":
		// A freeform/grammar tool has no upstream equivalent. Emulate it as a
		// function that takes the raw input string (grok2api does the same) and
		// restore custom_tool_call on the way back.
		name := strings.TrimSpace(fmt.Sprint(tool["name"]))
		if name == "" || name == "<nil>" {
			return nil, fmt.Errorf("%s.name is required", param)
		}
		if _, exists := tool["format"]; exists {
			state.addWarning("custom_tool_format_downgraded")
		}
		state.addWarning("custom_tool_emulated")
		description := strings.TrimSpace(parseLooseStringAny(tool["description"]))
		if description != "" {
			description += "\n"
		}
		description += "Provide the custom tool input in the input string field."
		return []map[string]interface{}{{
			"type": "function", "name": buildToolAlias(namespace, name), "description": description,
			"parameters": map[string]interface{}{
				"type":                 "object",
				"properties":           map[string]interface{}{"input": map[string]interface{}{"type": "string"}},
				"required":             []interface{}{"input"},
				"additionalProperties": false,
			},
		}}, nil
	case "x_search", "web_search":
		out := cloneStringInterfaceMap(tool)
		if stripWebSearchControlFields(out) {
			state.addWarning("web_search_controls_downgraded")
		}
		return []map[string]interface{}{out}, nil
	case "mcp", "shell", "image_generation", "collections_search", "file_search", "code_execution", "code_interpreter":
		return []map[string]interface{}{cloneStringInterfaceMap(tool)}, nil
	case "computer_use_preview":
		return nil, fmt.Errorf("%s.type computer_use_preview is not supported by Grok Build", param)
	default:
		return nil, fmt.Errorf("%s.type %q is not supported by Grok Build", param, kind)
	}
}

func normalizeBuildFunctionRoot(schema map[string]interface{}) map[string]interface{} {
	out := cloneStringInterfaceMap(schema)
	if types, ok := out["type"].([]interface{}); ok {
		filtered := make([]interface{}, 0, len(types))
		for _, value := range types {
			if fmt.Sprint(value) != "null" {
				filtered = append(filtered, value)
			}
		}
		if len(filtered) == 1 && fmt.Sprint(filtered[0]) == "object" {
			out["type"] = "object"
		}
	}
	for _, keyword := range []string{"anyOf", "oneOf"} {
		branches, ok := out[keyword].([]interface{})
		if !ok {
			continue
		}
		kept := make([]interface{}, 0, len(branches))
		for _, raw := range branches {
			branch, _ := raw.(map[string]interface{})
			if strings.EqualFold(strings.TrimSpace(fmt.Sprint(branch["type"])), "null") {
				continue
			}
			kept = append(kept, raw)
		}
		if len(kept) == 1 {
			if branch, ok := kept[0].(map[string]interface{}); ok && (branch["type"] == "object" || branch["properties"] != nil) {
				delete(out, keyword)
				for key, value := range branch {
					out[key] = value
				}
				out["type"] = "object"
			}
		}
	}
	return out
}

func buildToolAlias(namespace, name string) string {
	value := name
	if strings.TrimSpace(namespace) != "" {
		value = namespace + "__" + name
	}
	value = strings.Trim(buildToolAliasInvalid.ReplaceAllString(value, "_"), "_")
	if value == "" {
		value = "tool"
	}
	if len(value) > 128 {
		value = value[:128]
	}
	return value
}

func normalizeBuildToolChoice(payload map[string]interface{}, state *buildToolNormalizationState) {
	choice, ok := payload["tool_choice"].(map[string]interface{})
	if !ok {
		return
	}
	kind := strings.ToLower(strings.TrimSpace(fmt.Sprint(choice["type"])))
	if kind == "tool_search" {
		payload["tool_choice"] = map[string]interface{}{"type": "function", "name": "tool_search"}
		state.addWarning("server_tool_search_choice_downgraded")
		return
	}
	if kind != "function" && kind != "apply_patch" {
		return
	}
	if kind == "apply_patch" {
		payload["tool_choice"] = map[string]interface{}{"type": "function", "name": "apply_patch"}
		return
	}
	name := strings.TrimSpace(parseLooseStringAny(choice["name"]))
	namespace := strings.TrimSpace(parseLooseStringAny(choice["namespace"]))
	if nested, ok := choice["function"].(map[string]interface{}); ok {
		name = strings.TrimSpace(fmt.Sprint(nested["name"]))
		namespace = strings.TrimSpace(parseLooseStringAny(nested["namespace"]))
	}
	if name != "" && name != "<nil>" {
		payload["tool_choice"] = map[string]interface{}{"type": "function", "name": state.alias(namespace, name)}
	}
}

func mapsEqualJSON(left, right map[string]interface{}) bool {
	a, errA := json.Marshal(left)
	b, errB := json.Marshal(right)
	return errA == nil && errB == nil && string(a) == string(b)
}

// Chat Completions has no native reasoning object. Accept the relay's
// reasoning_effort / reasoning_summary extensions and rebuild the Responses
// shape from them. A control the caller omitted stays omitted rather than being
// guessed here; plane-specific defaults are applied later.
func chatReasoningControls(req *ChatCompletionsRequest) map[string]interface{} {
	if req == nil {
		return nil
	}
	reasoning := map[string]interface{}{}
	if req.ReasoningEffort != nil {
		if effort := strings.TrimSpace(*req.ReasoningEffort); effort != "" {
			reasoning["effort"] = effort
		}
	}
	if req.ReasoningSummary != nil {
		if summary := strings.TrimSpace(*req.ReasoningSummary); summary != "" {
			reasoning["summary"] = summary
		}
	}
	return reasoning
}

// normalizeBuildReasoningEffort maps client effort aliases onto levels the
// selected model actually accepts. Grok 4.5 and other models without an xhigh
// wire contract take the proven defensive xhigh/max -> high mapping; models
// that do advertise xhigh keep it. Composer never receives an effort at all,
// but keeps its other reasoning controls such as summary.
func normalizeBuildReasoningEffort(payload map[string]interface{}, model string) {
	reasoning, _ := payload["reasoning"].(map[string]interface{})
	if reasoning == nil {
		return
	}
	effort := strings.ToLower(strings.TrimSpace(interfaceString(reasoning["effort"])))
	if effort == "" {
		return
	}
	if modelpolicy.IsGrokComposerModel(model) {
		delete(reasoning, "effort")
		if len(reasoning) == 0 {
			delete(payload, "reasoning")
		}
		return
	}
	var normalized string
	switch effort {
	case "minimal":
		normalized = "low"
	case "xhigh", "max":
		if modelpolicy.SupportsReasoningEffort(model, "xhigh") {
			normalized = "xhigh"
		} else {
			normalized = "high"
		}
	default:
		return
	}
	reasoning["effort"] = normalized
}

// normalizeConsoleReasoningEffort applies the Console wire aliases: minimal
// collapses to low, and both xhigh and the client-only max alias become xhigh.
// Any other value is forwarded unchanged rather than silently downgraded.
// consoleModelSemantics is the per-model Console contract grok2api drives from
// its catalog (console/catalog.go): whether a model reasons at all, whether it
// accepts an effort level, the effort to use when none is sent, and the output
// ceiling to inject when the caller did not set one.
//
// Without it a relay forwards `reasoning` to models that reject it, forwards
// `effort` to models with a fixed reasoning budget, and lets the upstream pick
// its own (shorter) output limit.
type consoleModelSemantics struct {
	SupportsReasoning       bool
	SupportsReasoningEffort bool
	DefaultReasoningEffort  string
	MaxOutputTokens         int
}

// consoleModelSemanticsFor resolves the Console semantics for a model id. The
// id may carry the "console/" route prefix this gateway's catalog uses.
func consoleModelSemanticsFor(model string) (consoleModelSemantics, bool) {
	id := strings.ToLower(strings.TrimSpace(model))
	id = strings.TrimPrefix(id, "console/")
	switch id {
	case "grok-4.3", "grok-4.5", "grok-4.20-multi-agent-0309":
		return consoleModelSemantics{SupportsReasoning: true, SupportsReasoningEffort: true, DefaultReasoningEffort: "medium", MaxOutputTokens: 1_000_000}, true
	case "grok-4.20-0309-reasoning":
		// Fixed reasoning budget: an effort is not accepted.
		return consoleModelSemantics{SupportsReasoning: true, MaxOutputTokens: 1_000_000}, true
	case "grok-4.20-0309-non-reasoning", "grok-build-0.1":
		return consoleModelSemantics{MaxOutputTokens: 256_000}, true
	}
	return consoleModelSemantics{}, false
}

// normalizeConsoleReasoningEffort applies the Console wire aliases and the
// per-model Contract Console actually enforces.
func normalizeConsoleReasoningEffort(payload map[string]interface{}, model string) {
	// The output ceiling is injected for any Console model that declares one,
	// independent of reasoning, so it runs before the reasoning switch.
	if spec, ok := consoleModelSemanticsFor(model); ok && spec.MaxOutputTokens > 0 {
		if _, exists := payload["max_output_tokens"]; !exists {
			payload["max_output_tokens"] = spec.MaxOutputTokens
		}
	}
	spec, known := consoleModelSemanticsFor(model)
	reasoning, _ := payload["reasoning"].(map[string]interface{})
	if known && !spec.SupportsReasoning {
		// The model rejects the reasoning object outright.
		delete(payload, "reasoning")
		return
	}
	if reasoning == nil {
		if !known || spec.DefaultReasoningEffort == "" {
			return
		}
		reasoning = map[string]interface{}{}
	}
	if known && !spec.SupportsReasoningEffort {
		// Fixed reasoning budget: keep any other reasoning control, drop effort.
		delete(reasoning, "effort")
		if len(reasoning) == 0 {
			delete(payload, "reasoning")
		} else {
			payload["reasoning"] = reasoning
		}
		return
	}
	switch strings.ToLower(strings.TrimSpace(interfaceString(reasoning["effort"]))) {
	case "minimal", "low":
		reasoning["effort"] = "low"
	case "medium":
		reasoning["effort"] = "medium"
	case "high":
		reasoning["effort"] = "high"
	case "xhigh", "max":
		reasoning["effort"] = "xhigh"
	default:
		if known && spec.DefaultReasoningEffort != "" {
			reasoning["effort"] = spec.DefaultReasoningEffort
		}
	}
	payload["reasoning"] = reasoning
}

// Validate structure only. Upstreams, not the relay's model catalog, decide
// which reasoning effort values are supported. Never downgrade client values.
func validatePayloadReasoning(payload map[string]interface{}) error {
	if raw, exists := payload["reasoning"]; exists && raw != nil {
		reasoning, ok := raw.(map[string]interface{})
		if !ok {
			return fmt.Errorf("reasoning must be an object")
		}
		if raw, exists := reasoning["effort"]; exists {
			if effort, ok := raw.(string); !ok || strings.TrimSpace(effort) == "" {
				return fmt.Errorf("reasoning.effort must be a non-empty string")
			}
		}
		if raw, exists := reasoning["summary"]; exists {
			if summary, ok := raw.(string); !ok || strings.TrimSpace(summary) == "" {
				return fmt.Errorf("reasoning.summary must be a non-empty string")
			}
		}
	}
	return nil
}

// hasNativeSearchTool reports whether the tool list already declares a hosted
// search tool, so web_search_options does not duplicate it.
func hasNativeSearchTool(tools []map[string]interface{}) bool {
	for _, tool := range tools {
		switch strings.ToLower(strings.TrimSpace(fmt.Sprint(tool["type"]))) {
		case "web_search", "x_search":
			return true
		}
	}
	return false
}

// webSearchCompatibilityFields are newer OpenAI/Codex controls that the Grok
// Build wire contract rejects. grok2api drops them (keeping only the native
// minimal search tool) instead of letting the whole request fail; the
// operator's intent — a web search — is preserved either way.
var webSearchCompatibilityFields = []string{
	"external_web_access",
	"indexed_web_access",
	"search_content_types",
	"search_context_size",
	"user_location",
	"max_search_results",
	"safe_search",
}

// stripWebSearchControlFields removes the controls above from a hosted search
// tool, returning true when anything was removed.
func stripWebSearchControlFields(tool map[string]interface{}) bool {
	changed := false
	for _, field := range webSearchCompatibilityFields {
		if _, exists := tool[field]; exists {
			delete(tool, field)
			changed = true
		}
	}
	return changed
}

// lowerEmulatedCallItem rewrites a client-side custom_tool_call / apply_patch_call
// history item into the emulated function_call the Build plane accepts, so a
// multi-turn agent loop keeps working after the tool declaration was emulated.
func lowerEmulatedCallItem(item map[string]interface{}, state *buildToolNormalizationState) {
	switch strings.ToLower(strings.TrimSpace(parseLooseStringAny(item["type"]))) {
	case "custom_tool_call":
		name := strings.TrimSpace(parseLooseStringAny(item["name"]))
		if name == "" {
			return
		}
		arguments, err := json.Marshal(map[string]interface{}{"input": parseLooseStringAny(item["input"])})
		if err != nil {
			return
		}
		item["type"] = "function_call"
		item["name"] = buildToolAlias("", name)
		item["arguments"] = string(arguments)
		delete(item, "input")
	case "apply_patch_call":
		operation, ok := item["operation"].(map[string]interface{})
		if !ok {
			return
		}
		arguments, err := json.Marshal(map[string]interface{}{"operation": operation})
		if err != nil {
			return
		}
		item["type"] = "function_call"
		item["name"] = "apply_patch"
		item["arguments"] = string(arguments)
		delete(item, "operation")
	}
}
