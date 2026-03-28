package handler

import (
	"strings"

	"github.com/goccy/go-json"

	"orchids-api/internal/bolt"
	"orchids-api/internal/orchids"
	"orchids-api/internal/tiktoken"
)

const (
	maxCompactToolCount         = 24
	maxCompactToolSchemaJSONLen = 4096
	maxIncomingToolDescLen      = 128
)

var incomingToolPropertyAllowlist = map[string]map[string]struct{}{
	"bash": {
		"command":                   {},
		"description":               {},
		"dangerouslyDisableSandbox": {},
		"run_in_background":         {},
		"timeout":                   {},
	},
	"glob": {
		"path":    {},
		"pattern": {},
	},
	"grep": {
		"-A":          {},
		"-B":          {},
		"-C":          {},
		"-i":          {},
		"-n":          {},
		"context":     {},
		"glob":        {},
		"head_limit":  {},
		"multiline":   {},
		"offset":      {},
		"output_mode": {},
		"path":        {},
		"pattern":     {},
		"type":        {},
	},
	"read": {
		"file_path": {},
		"limit":     {},
		"offset":    {},
		"pages":     {},
	},
	"edit": {
		"file_path":   {},
		"new_string":  {},
		"old_string":  {},
		"replace_all": {},
	},
	"write": {
		"content":   {},
		"file_path": {},
	},
}

func supportedToolNames(tools []interface{}) []string {
	return bolt.FilterSupportedToolNames(collectIncomingToolNames(tools))
}

func collectIncomingToolNames(tools []interface{}) []string {
	if len(tools) == 0 {
		return nil
	}

	rawNames := make([]string, 0, len(tools))
	for _, tool := range tools {
		name, _, _ := extractIncomingToolSpecFields(tool)
		if name == "" {
			continue
		}
		rawNames = append(rawNames, name)
	}
	return rawNames
}

func declaredToolNames(tools []interface{}) []string {
	if len(tools) == 0 {
		return nil
	}

	seen := make(map[string]struct{}, len(tools)*2)
	out := make([]string, 0, len(tools)*2)
	add := func(name string) {
		name = strings.TrimSpace(name)
		if name == "" {
			return
		}
		key := strings.ToLower(name)
		if _, ok := seen[key]; ok {
			return
		}
		seen[key] = struct{}{}
		out = append(out, name)
	}

	for _, tool := range tools {
		name, _, _ := extractIncomingToolSpecFields(tool)
		if name == "" {
			continue
		}
		add(name)
		mappedName := orchids.NormalizeToolNameFallback(name)
		if !strings.EqualFold(strings.TrimSpace(name), strings.TrimSpace(mappedName)) {
			add(mappedName)
		}
	}

	if len(out) == 0 {
		return nil
	}
	return out
}

func passthroughAllowedToolNames(tools []interface{}, supportedOnly bool) []string {
	if supportedOnly {
		return supportedToolNames(tools)
	}
	return declaredToolNames(tools)
}

func validationAllowedToolNames(effectiveTools []interface{}, originalTools []interface{}, supportedOnly bool) []string {
	if supportedOnly && len(originalTools) > 0 {
		if declared := declaredToolNames(originalTools); len(declared) > 0 {
			return declared
		}
	}
	return passthroughAllowedToolNames(effectiveTools, supportedOnly)
}

func estimateCompactedToolsTokens(tools []interface{}) int {
	if len(tools) == 0 {
		return 0
	}
	compacted := compactIncomingTools(tools)
	if len(compacted) == 0 {
		return 0
	}
	raw, err := json.Marshal(compacted)
	if err != nil {
		return 0
	}
	var estimator tiktoken.Estimator
	estimator.AddBytes(raw)
	return estimator.Count()
}

func compactIncomingTools(tools []interface{}) []interface{} {
	if len(tools) == 0 {
		return nil
	}

	out := make([]interface{}, 0, len(tools))
	seen := make(map[string]struct{})

	for _, raw := range tools {
		rawMap, ok := raw.(map[string]interface{})
		if !ok {
			continue
		}

		name, description, schema := extractIncomingToolSpecFields(rawMap)
		if name == "" {
			continue
		}

		mappedName := orchids.NormalizeToolNameFallback(name)
		if !bolt.IsCoreTool(mappedName) {
			continue
		}

		key := strings.ToLower(strings.TrimSpace(mappedName))
		if key == "" {
			continue
		}
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}

		description = compactIncomingToolDescription(description)
		schema = compactIncomingToolSchema(mappedName, schema)

		rebuilt := map[string]interface{}{}
		if _, ok := rawMap["function"].(map[string]interface{}); ok {
			rebuilt["type"] = "function"
			function := map[string]interface{}{
				"name": strings.TrimSpace(name),
			}
			if description != "" {
				function["description"] = description
			}
			if len(schema) > 0 {
				function["parameters"] = schema
			}
			rebuilt["function"] = function
		} else {
			rebuilt["name"] = strings.TrimSpace(name)
			if description != "" {
				rebuilt["description"] = description
			}
			if len(schema) > 0 {
				rebuilt["input_schema"] = schema
			}
		}

		out = append(out, rebuilt)
		if len(out) >= maxCompactToolCount {
			break
		}
	}
	return out
}

func compactIncomingToolDescription(description string) string {
	description = strings.TrimSpace(description)
	if description == "" {
		return ""
	}
	runes := []rune(description)
	if len(runes) <= maxIncomingToolDescLen {
		return description
	}
	return string(runes[:maxIncomingToolDescLen]) + "...[truncated]"
}

func compactIncomingToolSchema(toolName string, schema map[string]interface{}) map[string]interface{} {
	if schema == nil {
		return nil
	}
	cleaned := cleanJSONSchemaProperties(schema)
	if cleaned == nil {
		return nil
	}
	stripped := stripSchemaDescriptions(cleaned)
	filtered := filterIncomingToolSchema(toolName, stripped)
	if schemaJSONLen(filtered) <= maxCompactToolSchemaJSONLen {
		return filtered
	}
	return map[string]interface{}{
		"type":       "object",
		"properties": map[string]interface{}{},
	}
}

func filterIncomingToolSchema(toolName string, schema map[string]interface{}) map[string]interface{} {
	if schema == nil {
		return nil
	}
	allowlist, ok := incomingToolPropertyAllowlist[strings.ToLower(strings.TrimSpace(toolName))]
	if !ok || len(allowlist) == 0 {
		return schema
	}

	filtered := make(map[string]interface{}, len(schema))
	for key, value := range schema {
		switch key {
		case "properties":
			props, _ := value.(map[string]interface{})
			if len(props) == 0 {
				continue
			}
			nextProps := make(map[string]interface{}, len(props))
			for propName, propValue := range props {
				if _, keep := allowlist[propName]; !keep {
					continue
				}
				nextProps[propName] = propValue
			}
			if len(nextProps) > 0 {
				filtered["properties"] = nextProps
			}
		case "required":
			switch required := value.(type) {
			case []interface{}:
				if len(required) == 0 {
					continue
				}
				nextRequired := make([]interface{}, 0, len(required))
				for _, item := range required {
					name, _ := item.(string)
					if _, keep := allowlist[name]; keep {
						nextRequired = append(nextRequired, item)
					}
				}
				if len(nextRequired) > 0 {
					filtered["required"] = nextRequired
				}
			case []string:
				if len(required) == 0 {
					continue
				}
				nextRequired := make([]string, 0, len(required))
				for _, name := range required {
					if _, keep := allowlist[name]; keep {
						nextRequired = append(nextRequired, name)
					}
				}
				if len(nextRequired) > 0 {
					filtered["required"] = nextRequired
				}
			}
		default:
			filtered[key] = value
		}
	}
	return filtered
}

func stripSchemaDescriptions(schema map[string]interface{}) map[string]interface{} {
	if schema == nil {
		return nil
	}
	out := make(map[string]interface{}, len(schema))
	for k, v := range schema {
		if strings.EqualFold(k, "description") || strings.EqualFold(k, "title") {
			continue
		}
		if strings.EqualFold(k, "properties") {
			if props, ok := v.(map[string]interface{}); ok {
				cleanProps := make(map[string]interface{}, len(props))
				for name, prop := range props {
					cleanProps[name] = stripSchemaDescriptionsValue(prop)
				}
				out[k] = cleanProps
				continue
			}
		}
		out[k] = stripSchemaDescriptionsValue(v)
	}
	return out
}

func stripSchemaDescriptionsValue(value interface{}) interface{} {
	switch v := value.(type) {
	case map[string]interface{}:
		return stripSchemaDescriptions(v)
	case []interface{}:
		out := make([]interface{}, 0, len(v))
		for _, item := range v {
			out = append(out, stripSchemaDescriptionsValue(item))
		}
		return out
	default:
		return value
	}
}

func schemaJSONLen(schema map[string]interface{}) int {
	if schema == nil {
		return 0
	}
	raw, err := json.Marshal(schema)
	if err != nil {
		return 0
	}
	return len(raw)
}

func extractIncomingToolSpecFields(tool interface{}) (string, string, map[string]interface{}) {
	tm, ok := tool.(map[string]interface{})
	if !ok {
		return "", "", nil
	}

	var name string
	var description string
	var schema map[string]interface{}

	if fn, ok := tm["function"].(map[string]interface{}); ok {
		if v, ok := fn["name"].(string); ok {
			name = strings.TrimSpace(v)
		}
		if v, ok := fn["description"].(string); ok {
			description = v
		}
		schema = extractIncomingToolSchemaMap(fn, "parameters", "input_schema", "inputSchema")
	}
	if name == "" {
		if v, ok := tm["name"].(string); ok {
			name = strings.TrimSpace(v)
		}
	}
	if description == "" {
		if v, ok := tm["description"].(string); ok {
			description = v
		}
	}
	if schema == nil {
		schema = extractIncomingToolSchemaMap(tm, "input_schema", "inputSchema", "parameters")
	}
	return name, description, schema
}

func extractIncomingToolSchemaMap(tm map[string]interface{}, keys ...string) map[string]interface{} {
	if tm == nil {
		return nil
	}
	for _, key := range keys {
		if v, ok := tm[key]; ok {
			if schema, ok := v.(map[string]interface{}); ok {
				return schema
			}
		}
	}
	return nil
}

func cleanJSONSchemaProperties(schema map[string]interface{}) map[string]interface{} {
	if schema == nil {
		return nil
	}
	sanitized := map[string]interface{}{}
	for _, key := range []string{"type", "description", "properties", "required", "enum", "items"} {
		if v, ok := schema[key]; ok {
			sanitized[key] = v
		}
	}
	if props, ok := sanitized["properties"].(map[string]interface{}); ok {
		cleanProps := map[string]interface{}{}
		for name, prop := range props {
			cleanProps[name] = cleanJSONSchemaValue(prop)
		}
		sanitized["properties"] = cleanProps
	}
	if items, ok := sanitized["items"]; ok {
		sanitized["items"] = cleanJSONSchemaValue(items)
	}
	return sanitized
}

func cleanJSONSchemaValue(value interface{}) interface{} {
	if value == nil {
		return value
	}
	if m, ok := value.(map[string]interface{}); ok {
		return cleanJSONSchemaProperties(m)
	}
	if arr, ok := value.([]interface{}); ok {
		out := make([]interface{}, 0, len(arr))
		for _, item := range arr {
			out = append(out, cleanJSONSchemaValue(item))
		}
		return out
	}
	return value
}
