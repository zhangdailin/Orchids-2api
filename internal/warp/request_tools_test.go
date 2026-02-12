package warp

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestConvertTools_DedupsAndCompacts(t *testing.T) {
	t.Parallel()

	longDesc := strings.Repeat("x", 3000)
	hugeSchema := map[string]interface{}{
		"type": "object",
		"properties": map[string]interface{}{
			"command": map[string]interface{}{
				"type":        "string",
				"description": strings.Repeat("y", 12000),
			},
		},
	}

	tools := []interface{}{
		map[string]interface{}{
			"name":        "Read",
			"description": longDesc,
			"input_schema": map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"file_path": map[string]interface{}{"type": "string"},
				},
			},
		},
		map[string]interface{}{
			"name":        "read_file",
			"description": "duplicate alias",
			"input_schema": map[string]interface{}{
				"type": "object",
			},
		},
		map[string]interface{}{
			"type": "function",
			"function": map[string]interface{}{
				"name":        "Bash",
				"description": longDesc,
				"parameters":  hugeSchema,
			},
		},
		map[string]interface{}{
			"name": "WebSearch",
		},
	}

	defs := convertTools(tools)
	if len(defs) != 2 {
		t.Fatalf("expected 2 tools after dedup/block, got %d", len(defs))
	}

	if defs[0].Name != "Read" {
		t.Fatalf("first tool name = %q, want %q", defs[0].Name, "Read")
	}
	if defs[1].Name != "Bash" {
		t.Fatalf("second tool name = %q, want %q", defs[1].Name, "Bash")
	}

	for _, def := range defs {
		if len([]rune(def.Description)) > maxWarpToolDescLen+20 {
			t.Fatalf("description not compacted for %s", def.Name)
		}
		raw, err := json.Marshal(def.Schema)
		if err != nil {
			t.Fatalf("marshal schema: %v", err)
		}
		if len(raw) > maxWarpToolSchemaJSONLen+256 {
			t.Fatalf("schema too large for %s: %d bytes", def.Name, len(raw))
		}
	}
}
