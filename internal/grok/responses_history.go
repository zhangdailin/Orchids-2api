package grok

import (
	"fmt"
	"strings"

	"github.com/goccy/go-json"
)

// normalizeBuildInputHistory lowers only client extension items that Grok Build
// cannot deserialize. Native Build items are left untouched, including unknown
// fields, so already-valid relay payloads retain their structure.
func normalizeBuildInputHistory(payload map[string]interface{}, state *buildToolNormalizationState) error {
	items, ok := payload["input"].([]interface{})
	if !ok {
		return nil
	}
	for index, raw := range items {
		item, ok := raw.(map[string]interface{})
		if !ok {
			continue
		}
		kind := strings.ToLower(strings.TrimSpace(interfaceString(item["type"])))
		var converted map[string]interface{}
		var err error
		switch kind {
		case "agent_message":
			converted = normalizeBuildAgentMessage(item)
		case "local_shell_call":
			converted, err = normalizeBuildLocalShellCall(item)
		case "local_shell_call_output":
			converted, err = normalizeBuildLocalShellOutput(item)
		case "mcp_tool_call_output":
			converted, err = normalizeBuildMCPToolOutput(item)
		default:
			continue
		}
		if err != nil {
			return fmt.Errorf("input[%d]: %w", index, err)
		}
		items[index] = converted
		state.addWarning(kind + "_normalized")
	}
	payload["input"] = items
	return nil
}

func normalizeBuildAgentMessage(item map[string]interface{}) map[string]interface{} {
	content, visible := buildHistoryText(item["content"])
	if !visible {
		return buildCompatibilityBoundary("An encrypted inter-agent message occurred here but is not portable to the Grok Build account.")
	}
	author := firstNonEmpty(strings.TrimSpace(interfaceString(item["author"])), "agent")
	recipient := firstNonEmpty(strings.TrimSpace(interfaceString(item["recipient"])), "recipient")
	return buildHistoryMessage("developer", "Agent message ("+author+" -> "+recipient+"):\n"+content)
}

func normalizeBuildLocalShellCall(item map[string]interface{}) (map[string]interface{}, error) {
	callID := strings.TrimSpace(interfaceString(item["call_id"]))
	if callID == "" {
		return nil, fmt.Errorf("local_shell_call.call_id is required")
	}
	action, ok := item["action"].(map[string]interface{})
	if !ok {
		return nil, fmt.Errorf("local_shell_call.action must be an object")
	}
	command := strings.TrimSpace(interfaceString(action["command"]))
	if command == "" {
		parts := make([]string, 0)
		for _, raw := range interfaceSlice(action["command"]) {
			part, ok := raw.(string)
			if !ok {
				return nil, fmt.Errorf("local_shell_call.action.command must contain strings")
			}
			parts = append(parts, part)
		}
		command = strings.Join(parts, " ")
	}
	if command == "" {
		return nil, fmt.Errorf("local_shell_call.action.command is required")
	}
	converted := map[string]interface{}{
		"type": "shell_call", "call_id": callID,
		"action": map[string]interface{}{"type": "exec", "commands": []interface{}{command}},
	}
	for _, key := range []string{"id", "status", "timeout_ms", "max_output_length"} {
		if value, exists := item[key]; exists && value != nil {
			converted[key] = value
		}
	}
	return converted, nil
}

func normalizeBuildLocalShellOutput(item map[string]interface{}) (map[string]interface{}, error) {
	callID := strings.TrimSpace(interfaceString(item["call_id"]))
	if callID == "" {
		return nil, fmt.Errorf("local_shell_call_output.call_id is required")
	}
	var output interface{}
	switch value := item["output"].(type) {
	case []interface{}:
		output = value
	case string:
		exitCode := 0
		if strings.EqualFold(interfaceString(item["status"]), "failed") {
			exitCode = 1
		}
		output = []interface{}{map[string]interface{}{
			"stdout": value, "stderr": "",
			"outcome": map[string]interface{}{"type": "exit", "exit_code": exitCode},
		}}
	default:
		return nil, fmt.Errorf("local_shell_call_output.output must be a string or array")
	}
	converted := map[string]interface{}{"type": "shell_call_output", "call_id": callID, "output": output}
	if value, exists := item["max_output_length"]; exists && value != nil {
		converted["max_output_length"] = value
	}
	return converted, nil
}

func normalizeBuildMCPToolOutput(item map[string]interface{}) (map[string]interface{}, error) {
	encoded, err := json.Marshal(item["output"])
	if err != nil {
		return nil, fmt.Errorf("mcp_tool_call_output.output cannot be encoded")
	}
	callID := firstNonEmpty(strings.TrimSpace(interfaceString(item["call_id"])), "unknown")
	return buildHistoryMessage("developer", "MCP tool output for call "+callID+": "+string(encoded)), nil
}

func buildHistoryText(raw interface{}) (string, bool) {
	if text, ok := raw.(string); ok {
		return text, true
	}
	parts := make([]string, 0)
	values, ok := raw.([]interface{})
	if !ok {
		return "", false
	}
	for _, rawPart := range values {
		part, ok := rawPart.(map[string]interface{})
		if !ok {
			return "", false
		}
		switch interfaceString(part["type"]) {
		case "text", "input_text", "output_text":
			parts = append(parts, interfaceString(part["text"]))
		default:
			return "", false
		}
	}
	return strings.Join(parts, "\n"), true
}

func buildHistoryMessage(role, text string) map[string]interface{} {
	partType := "input_text"
	if role == "assistant" {
		partType = "output_text"
	}
	return map[string]interface{}{"type": "message", "role": role, "content": []interface{}{map[string]interface{}{"type": partType, "text": text}}}
}

func buildCompatibilityBoundary(text string) map[string]interface{} {
	return buildHistoryMessage("developer", text)
}
