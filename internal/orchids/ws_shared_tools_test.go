package orchids

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestConvertOrchidsTools_DedupsAndCompacts(t *testing.T) {
	t.Parallel()

	longDesc := strings.Repeat("d", 2000)
	hugeSchema := map[string]interface{}{
		"type": "object",
		"properties": map[string]interface{}{
			"content": map[string]interface{}{
				"type":        "string",
				"description": strings.Repeat("x", 12000),
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
			"description": "duplicate alias should be deduped",
			"input_schema": map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"path": map[string]interface{}{"type": "string"},
				},
			},
		},
		map[string]interface{}{
			"name":         "Write",
			"description":  longDesc,
			"input_schema": hugeSchema,
		},
		map[string]interface{}{
			"name":        "WebSearch",
			"description": "blocked",
		},
	}

	converted := convertOrchidsTools(tools)
	if len(converted) != 2 {
		t.Fatalf("expected 2 compacted tools, got %d", len(converted))
	}

	if converted[0].ToolSpecification.Name != "Read" {
		t.Fatalf("first tool name = %q, want %q", converted[0].ToolSpecification.Name, "Read")
	}
	if converted[1].ToolSpecification.Name != "Write" {
		t.Fatalf("second tool name = %q, want %q", converted[1].ToolSpecification.Name, "Write")
	}

	for _, spec := range converted {
		if len([]rune(spec.ToolSpecification.Description)) > maxCompactToolDescLen+20 {
			t.Fatalf("description not compacted for %s", spec.ToolSpecification.Name)
		}
		raw, err := json.Marshal(spec.ToolSpecification.InputSchema)
		if err != nil {
			t.Fatalf("marshal schema: %v", err)
		}
		if len(raw) > maxCompactToolSchemaJSONLen+256 {
			t.Fatalf("schema still too large for %s: %d bytes", spec.ToolSpecification.Name, len(raw))
		}
	}
}

func TestCompactIncomingTools_PreservesFunctionShapeAndDedups(t *testing.T) {
	t.Parallel()

	tools := []interface{}{
		map[string]interface{}{
			"type": "function",
			"function": map[string]interface{}{
				"name":        "Read",
				"description": strings.Repeat("a", 1500),
				"parameters": map[string]interface{}{
					"type": "object",
					"properties": map[string]interface{}{
						"file_path": map[string]interface{}{
							"type":        "string",
							"description": strings.Repeat("b", 3000),
						},
					},
				},
			},
		},
		map[string]interface{}{
			"name":        "read_file",
			"description": "duplicate alias should be removed",
			"input_schema": map[string]interface{}{
				"type": "object",
			},
		},
	}

	compacted := compactIncomingTools(tools)
	if len(compacted) != 1 {
		t.Fatalf("expected 1 compacted tool after dedup, got %d", len(compacted))
	}

	first, ok := compacted[0].(map[string]interface{})
	if !ok {
		t.Fatalf("unexpected compacted tool type %T", compacted[0])
	}
	if first["type"] != "function" {
		t.Fatalf("expected function shape to be preserved")
	}

	fn, ok := first["function"].(map[string]interface{})
	if !ok {
		t.Fatalf("missing function object")
	}
	desc, _ := fn["description"].(string)
	if len([]rune(desc)) > maxCompactToolDescLen+20 {
		t.Fatalf("function description not compacted")
	}
}
