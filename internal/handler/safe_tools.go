package handler

import (
	"encoding/json"
	"strings"
)

const (
	safeToolTimeout       = 0
	safeToolMaxOutputSize = 0
	safeToolMaxLines      = 0
	safeToolMaxFindDepth  = -1
)

type safeToolResult struct {
	call    toolCall
	input   interface{}
	output  string
	isError bool
}

func executeSafeTool(call toolCall) safeToolResult {
	// Delegate to the robust tool execution logic in tool_exec.go
	// This ensures consistent behavior and supports both "command" and "path" input styles.
	return executeToolCall(call, nil)
}

func parseToolInputValue(inputJSON string) interface{} {
	if strings.TrimSpace(inputJSON) == "" {
		return map[string]interface{}{}
	}
	fixed := fixToolInput(inputJSON)
	var value interface{}
	if err := json.Unmarshal([]byte(fixed), &value); err != nil {
		return map[string]interface{}{}
	}
	return value
}
