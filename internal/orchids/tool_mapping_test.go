package orchids

import (
	"testing"
)

func TestMapToolNameToClientPrefersOriginalToolDefinition(t *testing.T) {
	t.Parallel()

	clientTools := []interface{}{
		map[string]interface{}{
			"name": "read_file",
			"aliases": []interface{}{"Read"},
		},
		map[string]interface{}{
			"name": "str_replace_editor",
			"aliases": []interface{}{"Edit"},
		},
	}
	toolMapper := buildClientToolMapper(clientTools)

	if got := MapToolNameToClient("Read", clientTools, toolMapper); got != "read_file" {
		t.Fatalf("MapToolNameToClient(Read) = %q want read_file", got)
	}
	if got := MapToolNameToClient("Edit", clientTools, toolMapper); got != "str_replace_editor" {
		t.Fatalf("MapToolNameToClient(Edit) = %q want str_replace_editor", got)
	}
}

func TestMapToolNameToClientMatchesSnakeCaseAlias(t *testing.T) {
	t.Parallel()

	clientTools := []interface{}{
		map[string]interface{}{
			"name": "run_command",
			"aliases": []interface{}{"Bash"},
		},
	}
	toolMapper := buildClientToolMapper(clientTools)

	if got := MapToolNameToClient("Bash", clientTools, toolMapper); got != "run_command" {
		t.Fatalf("MapToolNameToClient(Bash) = %q want run_command", got)
	}
}

func TestMapToolNameToClientSupportsFunctionWrappedTools(t *testing.T) {
	t.Parallel()

	clientTools := []interface{}{
		map[string]interface{}{
			"type": "function",
			"function": map[string]interface{}{
				"name": "read_file",
			},
			"aliases": []interface{}{"Read"},
		},
	}
	toolMapper := buildClientToolMapper(clientTools)

	if got := MapToolNameToClient("Read", clientTools, toolMapper); got != "read_file" {
		t.Fatalf("MapToolNameToClient(Read) = %q want read_file", got)
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


