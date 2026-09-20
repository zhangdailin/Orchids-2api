package warp

import (
	"fmt"
	"strings"
	"testing"
)

// Tool definitions are the client's contract with the model. Rewriting them
// removes capability the model was told it had, so the conversion must forward
// what the client declared.
func TestConvertToolsPreservesBuiltinSchemaProperties(t *testing.T) {
	t.Parallel()

	tools := []interface{}{
		map[string]interface{}{
			"name":        "Bash",
			"description": "run a shell command",
			"input_schema": map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"command":                   map[string]interface{}{"type": "string"},
					"dangerouslyDisableSandbox": map[string]interface{}{"type": "boolean"},
					"client_only_option":        map[string]interface{}{"type": "string"},
				},
				"required": []interface{}{"command", "dangerouslyDisableSandbox"},
			},
		},
	}

	got := convertTools(tools)
	if len(got) != 1 {
		t.Fatalf("convertTools len=%d want=1", len(got))
	}
	props, ok := got[0].Schema["properties"].(map[string]interface{})
	if !ok {
		t.Fatalf("properties type=%T", got[0].Schema["properties"])
	}
	// The per-builtin allowlist used to delete every property it did not list,
	// which removed arguments the client had declared and the model was told about.
	for _, want := range []string{"command", "dangerouslyDisableSandbox", "client_only_option"} {
		if _, ok := props[want]; !ok {
			t.Fatalf("Bash schema lost the %q property it declared: %#v", want, props)
		}
	}
	required, ok := got[0].Schema["required"].([]interface{})
	if !ok || len(required) != 2 {
		t.Fatalf("required = %#v, want both declared names", got[0].Schema["required"])
	}
}

// Schema keywords outside {type,description,properties,required,enum,items} used
// to be dropped. A strict schema is exactly where those keywords carry meaning.
func TestConvertToolsPreservesSchemaKeywords(t *testing.T) {
	t.Parallel()

	tools := []interface{}{
		map[string]interface{}{
			"name": "custom_tool",
			"input_schema": map[string]interface{}{
				"type":                 "object",
				"additionalProperties": false,
				"$schema":              "https://json-schema.org/draft/2020-12/schema",
				"properties": map[string]interface{}{
					"mode": map[string]interface{}{
						"enum":    []interface{}{"fast", "slow"},
						"default": "fast",
						"pattern": "^[a-z]+$",
					},
				},
				"oneOf": []interface{}{
					map[string]interface{}{"required": []interface{}{"mode"}},
				},
			},
		},
	}

	got := convertTools(tools)
	if len(got) != 1 {
		t.Fatalf("convertTools len=%d want=1", len(got))
	}
	for _, key := range []string{"additionalProperties", "$schema", "oneOf"} {
		if _, ok := got[0].Schema[key]; !ok {
			t.Fatalf("schema lost the %q keyword: %#v", key, got[0].Schema)
		}
	}
	props := got[0].Schema["properties"].(map[string]interface{})
	mode := props["mode"].(map[string]interface{})
	for _, key := range []string{"enum", "default", "pattern"} {
		if _, ok := mode[key]; !ok {
			t.Fatalf("property schema lost %q: %#v", key, mode)
		}
	}
}

// A client that declares MCP server tools reaches 32 easily. Dropping the rest
// left the model unaware of tools the client believed it could call.
func TestConvertToolsKeepsToolsBeyondTheOldCap(t *testing.T) {
	t.Parallel()

	tools := make([]interface{}, 0, 60)
	for i := 0; i < 60; i++ {
		tools = append(tools, map[string]interface{}{
			"name":        fmt.Sprintf("mcp_tool_%02d", i),
			"description": fmt.Sprintf("tool number %d", i),
			"input_schema": map[string]interface{}{
				"type":       "object",
				"properties": map[string]interface{}{"arg": map[string]interface{}{"type": "string"}},
			},
		})
	}

	got := convertTools(tools)
	if len(got) != len(tools) {
		t.Fatalf("convertTools kept %d of %d declared tools", len(got), len(tools))
	}
	last := got[len(got)-1].Name
	if last != "mcp_tool_59" {
		t.Fatalf("last tool = %q, want the final declared tool", last)
	}
}

func TestWarpToolDescriptionIsForwardedWhole(t *testing.T) {
	t.Parallel()

	// Longer than the old 512-character cut, far below the transport ceiling.
	description := strings.TrimSpace(strings.Repeat("explain when this tool applies ", 100))
	got := warpToolDescription(description)
	if got != description {
		t.Fatalf("description was rewritten: %d runes in, %d out", len([]rune(description)), len([]rune(got)))
	}
}

// The ceiling exists only so a malformed request cannot build an unbounded
// frame; reaching it is pathological and is reported rather than hidden.
func TestWarpToolDescriptionCeilingStillBoundsAPathologicalInput(t *testing.T) {
	t.Parallel()

	description := strings.Repeat("x", warpToolDescriptionCeiling+1000)
	got := warpToolDescription(description)
	if len([]rune(got)) > warpToolDescriptionCeiling {
		t.Fatalf("description = %d runes, want at most %d", len([]rune(got)), warpToolDescriptionCeiling)
	}
	if !strings.HasSuffix(got, "...[truncated]") {
		t.Fatalf("an over-ceiling description must be marked, got %q", got[len(got)-32:])
	}
}
