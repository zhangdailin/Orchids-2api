package orchids

import (
	"strings"
	"testing"
)

func TestMapToolNameToClientPrefersOriginalToolDefinition(t *testing.T) {
	t.Parallel()

	clientTools := []interface{}{
		map[string]interface{}{
			"name": "read_file",
		},
		map[string]interface{}{
			"name": "str_replace_editor",
		},
	}

	if got := MapToolNameToClient("Read", clientTools); got != "read_file" {
		t.Fatalf("MapToolNameToClient(Read) = %q want read_file", got)
	}
	if got := MapToolNameToClient("Edit", clientTools); got != "str_replace_editor" {
		t.Fatalf("MapToolNameToClient(Edit) = %q want str_replace_editor", got)
	}
}

func TestMapToolNameToClientMatchesSnakeCaseAlias(t *testing.T) {
	t.Parallel()

	clientTools := []interface{}{
		map[string]interface{}{
			"name": "run_command",
		},
	}

	if got := MapToolNameToClient("Bash", clientTools); got != "run_command" {
		t.Fatalf("MapToolNameToClient(Bash) = %q want run_command", got)
	}
}

func TestTransformToolInputNormalizesReadAliases(t *testing.T) {
	t.Parallel()

	transformed := TransformToolInput("Read", "read_file", map[string]interface{}{
		"path": "/tmp/demo.txt",
	})

	if got := mapStringValue(transformed, "file_path"); got != "/tmp/demo.txt" {
		t.Fatalf("file_path=%q want /tmp/demo.txt", got)
	}
	if _, ok := transformed["path"]; ok {
		t.Fatalf("expected path alias to be removed, got %#v", transformed)
	}
}

func TestExtractToolCallsFromResponseMapsAndTransformsInput(t *testing.T) {
	t.Parallel()

	msg := map[string]interface{}{
		"response": map[string]interface{}{
			"output": []interface{}{
				map[string]interface{}{
					"type":      "function_call",
					"name":      "Read",
					"arguments": `{"path":"/tmp/demo.txt"}`,
				},
			},
		},
	}
	clientTools := []interface{}{
		map[string]interface{}{
			"name": "read_file",
		},
	}

	calls := extractToolCallsFromResponse(msg, clientTools)
	if len(calls) != 1 {
		t.Fatalf("len(calls)=%d want 1", len(calls))
	}
	if calls[0].name != "read_file" {
		t.Fatalf("tool name=%q want read_file", calls[0].name)
	}
	if !strings.Contains(calls[0].input, `"file_path":"/tmp/demo.txt"`) {
		t.Fatalf("input=%q want normalized file_path", calls[0].input)
	}
}

func TestExtractToolCallsFromFastResponseMapsAndTransformsInput(t *testing.T) {
	t.Parallel()

	var msg orchidsFastResponseDone
	msg.Response.Output = []orchidsFastToolOutput{
		{
			Type:  "tool_use",
			Name:  "Read",
			Input: map[string]interface{}{"path": "/tmp/demo.txt"},
		},
	}
	clientTools := []interface{}{
		map[string]interface{}{
			"name": "read_file",
		},
	}

	calls := extractToolCallsFromFastResponse(msg, clientTools)
	if len(calls) != 1 {
		t.Fatalf("len(calls)=%d want 1", len(calls))
	}
	if calls[0].name != "read_file" {
		t.Fatalf("tool name=%q want read_file", calls[0].name)
	}
	if !strings.Contains(calls[0].input, `"file_path":"/tmp/demo.txt"`) {
		t.Fatalf("input=%q want normalized file_path", calls[0].input)
	}
	if calls[0].id == "" {
		t.Fatal("expected fallback tool call id to be populated")
	}
}
