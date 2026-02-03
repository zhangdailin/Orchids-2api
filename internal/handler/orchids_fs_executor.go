package handler

import (
	"encoding/json"
	"fmt"
	"strings"
)

func (h *Handler) orchidsFSExecutor(op map[string]interface{}, workdir string) (bool, interface{}, string) {
	if op == nil {
		return false, nil, "invalid operation payload"
	}
	operation, _ := op["operation"].(string)
	operation = strings.ToLower(strings.TrimSpace(operation))
	if operation == "" {
		return false, nil, "missing operation"
	}

	call := toolCall{}
	if id, _ := op["id"].(string); id != "" {
		call.id = id
	}
	input := map[string]interface{}{}

	switch operation {
	case "read":
		path, _ := op["path"].(string)
		if strings.TrimSpace(path) == "" {
			return false, nil, "path is required for read"
		}
		call.name = "Read"
		input["file_path"] = path

	case "write":
		path, _ := op["path"].(string)
		if strings.TrimSpace(path) == "" {
			return false, nil, "path is required for write"
		}
		content, ok := op["content"]
		if !ok {
			return false, nil, "content is required for write"
		}
		call.name = "Write"
		input["file_path"] = path
		input["content"] = content

	case "edit":
		path, _ := op["path"].(string)
		if strings.TrimSpace(path) == "" {
			return false, nil, "path is required for edit"
		}
		content, ok := op["content"]
		if !ok {
			return false, nil, "content is required for edit"
		}
		switch payload := content.(type) {
		case string:
			call.name = "Write"
			input["file_path"] = path
			input["content"] = payload
		case []interface{}:
			call.name = "Edit"
			input["file_path"] = path
			input["edits"] = payload
		case map[string]interface{}:
			call.name = "Edit"
			input["file_path"] = path
			for k, v := range payload {
				input[k] = v
			}
		default:
			return false, nil, "unsupported edit payload"
		}

	case "delete":
		path, _ := op["path"].(string)
		if strings.TrimSpace(path) == "" {
			return false, nil, "path is required for delete"
		}
		call.name = "Delete"
		input["file_path"] = path

	case "list":
		target, _ := op["path"].(string)
		if strings.TrimSpace(target) == "" {
			target = "."
		}
		call.name = "LS"
		input["path"] = target

	case "glob":
		params, _ := op["globParameters"].(map[string]interface{})
		pattern, _ := op["pattern"].(string)
		if strings.TrimSpace(pattern) == "" && params != nil {
			if v, ok := params["pattern"].(string); ok {
				pattern = v
			}
		}
		if strings.TrimSpace(pattern) == "" {
			pattern = "*"
		}
		root := ""
		if params != nil {
			if v, ok := params["path"].(string); ok {
				root = v
			}
		}
		if strings.TrimSpace(root) == "" {
			root = "."
		}
		call.name = "Glob"
		input["pattern"] = pattern
		input["path"] = root
		if params != nil {
			if v, ok := asInt(params["maxResults"]); ok {
				input["max_results"] = v
			}
		}

	case "ripgrep", "grep", "search":
		params, _ := op["ripgrepParameters"].(map[string]interface{})
		pattern, _ := op["pattern"].(string)
		if strings.TrimSpace(pattern) == "" && params != nil {
			if v, ok := params["pattern"].(string); ok {
				pattern = v
			}
		}
		if strings.TrimSpace(pattern) == "" {
			return false, nil, "pattern is required for grep"
		}
		root := ""
		if params != nil {
			if v, ok := params["path"].(string); ok {
				root = v
			}
		}
		if strings.TrimSpace(root) == "" {
			root = "."
		}
		call.name = "Grep"
		input["pattern"] = pattern
		input["path"] = root
		if params != nil {
			if v, ok := asInt(params["maxResults"]); ok {
				input["max_results"] = v
			}
		}

	case "run_command":
		command, _ := op["command"].(string)
		if strings.TrimSpace(command) == "" {
			return false, nil, "command is required for run_command"
		}
		call.name = "Bash"
		input["command"] = command

	case "get_background_output":
		return false, nil, "background commands are not supported"
	case "kill_background_process":
		return false, nil, "background commands are not supported"
	case "get_terminal_logs", "get_browser_logs", "update_startup_commands":
		return true, "", ""

	default:
		return false, nil, fmt.Sprintf("unknown operation: %s", operation)
	}

	inputJSON, err := json.Marshal(input)
	if err != nil {
		return false, nil, "invalid tool input"
	}
	call.input = string(inputJSON)

	result := executeToolCallWithBaseDir(call, h.config, workdir)
	if result.isError {
		return false, nil, result.output
	}
	return true, result.output, ""
}
