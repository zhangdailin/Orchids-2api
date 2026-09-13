package util

import (
	"encoding/json"
	"fmt"
)

// CompactToolInput renders a tool-call argument value as compact JSON.
func CompactToolInput(value interface{}) string {
	if value == nil {
		return "{}"
	}
	raw, err := json.Marshal(value)
	if err != nil || len(raw) == 0 || string(raw) == "null" {
		return "{}"
	}
	return string(raw)
}

// StringValue renders an arbitrary value as a string, preferring a plain string.
func StringValue(value interface{}) string {
	if value == nil {
		return ""
	}
	if text, ok := value.(string); ok {
		return text
	}
	return fmt.Sprint(value)
}

// SchemaJSONLen reports the marshalled byte length of a JSON schema map.
// It is used to size-compare schemas during compaction.
func SchemaJSONLen(schema map[string]interface{}) int {
	if schema == nil {
		return 0
	}
	raw, err := json.Marshal(schema)
	if err != nil {
		return 0
	}
	return len(raw)
}
