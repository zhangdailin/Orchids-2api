package warp

import (
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
)

func TestParseFallbackToolInput_RunShellCommand(t *testing.T) {
	t.Parallel()

	// payload shape observed in Warp stream:
	// field1 command(string), field2 is_read_only(varint), field3 uses_pager(varint),
	// field5 is_risky(varint), field6 wait_until_complete(varint)
	command := "ls -la"
	payload := []byte{0x0a, 0x06, 'l', 's', ' ', '-', 'l', 'a', 0x10, 0x01, 0x18, 0x00, 0x28, 0x00, 0x30, 0x00}

	toolName, rawInput := parseFallbackToolInput("run_shell_command", payload)
	if toolName != "run_shell_command" {
		t.Fatalf("expected tool name run_shell_command, got %q", toolName)
	}
	if strings.TrimSpace(rawInput) == "" || rawInput == "{}" {
		t.Fatalf("expected non-empty input, got %q", rawInput)
	}

	var parsed map[string]interface{}
	if err := json.Unmarshal([]byte(rawInput), &parsed); err != nil {
		t.Fatalf("unmarshal input: %v", err)
	}
	if got, _ := parsed["command"].(string); got != command {
		t.Fatalf("expected command %q, got %q", command, got)
	}
}

func TestParseToolCall_ApplyFileDiffsNewFileBecomesWrite(t *testing.T) {
	t.Parallel()

	newFile := testEncodeMessage(
		testEncodeStringField(1, "/Users/dailin/Documents/GitHub/TEST/index.html"),
		testEncodeStringField(2, "<!doctype html><html></html>"),
	)
	applyFileDiffs := testEncodeMessage(
		testEncodeBytesField(3, newFile), // new_files
	)

	payload := testEncodeMessage(
		testEncodeStringField(1, "tool_id_write_1"),
		testEncodeBytesField(6, applyFileDiffs), // field 6 => apply_file_diffs
	)

	out := &parsedEvent{}
	parseToolCall(payload, out)

	if len(out.ToolCalls) != 1 {
		t.Fatalf("expected one tool call, got %d", len(out.ToolCalls))
	}
	if out.ToolCalls[0].Name != "Write" {
		t.Fatalf("expected Write, got %q", out.ToolCalls[0].Name)
	}

	var input map[string]interface{}
	if err := json.Unmarshal([]byte(out.ToolCalls[0].Input), &input); err != nil {
		t.Fatalf("unmarshal input: %v", err)
	}
	if got, _ := input["file_path"].(string); got != "/Users/dailin/Documents/GitHub/TEST/index.html" {
		t.Fatalf("expected file_path, got %q", got)
	}
	if got, _ := input["content"].(string); got != "<!doctype html><html></html>" {
		t.Fatalf("expected content, got %q", got)
	}
}

func TestParseToolCall_ApplyFileDiffsReplacementBecomesEdit(t *testing.T) {
	t.Parallel()

	replacement := testEncodeMessage(
		testEncodeStringField(1, "old"),
		testEncodeStringField(2, "new"),
	)
	fileDiff := testEncodeMessage(
		testEncodeStringField(1, "/tmp/a.txt"),
		testEncodeBytesField(3, replacement), // replacements
	)
	applyFileDiffs := testEncodeMessage(
		testEncodeBytesField(2, fileDiff), // file_diffs
	)
	payload := testEncodeMessage(
		testEncodeStringField(1, "tool_id_edit_1"),
		testEncodeBytesField(6, applyFileDiffs),
	)

	out := &parsedEvent{}
	parseToolCall(payload, out)

	if len(out.ToolCalls) != 1 {
		t.Fatalf("expected one tool call, got %d", len(out.ToolCalls))
	}
	if out.ToolCalls[0].Name != "Edit" {
		t.Fatalf("expected Edit, got %q", out.ToolCalls[0].Name)
	}

	var input map[string]interface{}
	if err := json.Unmarshal([]byte(out.ToolCalls[0].Input), &input); err != nil {
		t.Fatalf("unmarshal input: %v", err)
	}
	if got, _ := input["file_path"].(string); got != "/tmp/a.txt" {
		t.Fatalf("expected file_path, got %q", got)
	}
	if got, _ := input["old_string"].(string); got != "old" {
		t.Fatalf("expected old_string, got %q", got)
	}
	if got, _ := input["new_string"].(string); got != "new" {
		t.Fatalf("expected new_string, got %q", got)
	}
}

func testEncodeMessage(fields ...[]byte) []byte {
	var out []byte
	for _, f := range fields {
		out = append(out, f...)
	}
	return out
}

func testEncodeStringField(field int, value string) []byte {
	return testEncodeBytesField(field, []byte(value))
}

func testEncodeBytesField(field int, value []byte) []byte {
	var out []byte
	out = append(out, testEncodeKey(field, 2)...)
	out = append(out, testEncodeVarint(uint64(len(value)))...)
	out = append(out, value...)
	return out
}

func testEncodeKey(field int, wire int) []byte {
	return testEncodeVarint(uint64((field << 3) | wire))
}

func testEncodeVarint(v uint64) []byte {
	buf := make([]byte, 0, 10)
	for v >= 0x80 {
		buf = append(buf, byte(v)|0x80)
		v >>= 7
	}
	buf = append(buf, byte(v))
	return buf
}

func TestIsIncompleteToolCall_WriteAndEditValidation(t *testing.T) {
	t.Parallel()

	if !isIncompleteToolCall("Write", `{}`) {
		t.Fatalf("expected empty write to be incomplete")
	}
	if isIncompleteToolCall("Write", `{"file_path":"/tmp/a.txt","content":"x"}`) {
		t.Fatalf("expected valid write to be complete")
	}
	if !isIncompleteToolCall("Edit", `{"file_path":"/tmp/a.txt","old_string":"a"}`) {
		t.Fatalf("expected edit missing new_string to be incomplete")
	}
	if isIncompleteToolCall("Edit", `{"file_path":"/tmp/a.txt","old_string":"a","new_string":"b"}`) {
		t.Fatalf("expected valid edit to be complete")
	}

}

func TestParseResponseEvent_FallbackToolCallHasCommand(t *testing.T) {
	t.Parallel()

	// This base64 payload is from debug log 2026-02-09_09-34-32.410_717e where
	// Warp emits a fallback tool_call for run_shell_command with command "ls -la".
	const b64 = "EogGCoUGIoIGCvMDCiQ5NDRhOTc3Yy00NzNkLTRlZmUtYjg4MC1mNTgxYjUwMjI3ZWE68AJDaVE1WkRZeVlqazROQzB6WmpNM0xUUTRNemN0T1dZd1pTMDNZVEJsWVRneFptSXpPR1VxNmdFS0dPYWZwZWVjaS1XOWstV0pqZWVicnVXOWxlZTdrLWFlaEJBQUdza0JWR2hsSUc5eVkyaGxjM1J5WVhSdmNpQmxlR1ZqZFhSbFpDQjBhR2x6SUdOdmJXMWhibVFnWm05eUlIUm9aU0J5WldGemIyNGdaMmwyWlc0Z1ltVnNiM2N1SUU5aWMyVnlkbVVnZEdobElHTnZiVzFoYm1RbmN5QnZkWFJ3ZFhRZ1lXNWtJR1Z1YzNWeVpTQjBhR1VnWTI5dGJXRnVaQ0JvWVhNZ1pYaHBkR1ZrSUdKbFptOXlaU0J5WlhCdmNuUnBibWNnZEdobElISmxiR1YyWVc1MElHOTFkSEIxZEM0S0NsSmxZWE52YmpvSzVwLWw1NXlMNWIyVDVZbU41NXV1NWIyVjU3dVQ1cDZFSWdBPVokODA0ODllNDctZTllOC00MTVkLWJhNzItODQ1MTM1OGM5NDkzIjIKHnRvb2x1XzAxQXpzZzRDem5YdjZzcFFYZHJ2RlBteRIQCgZscyAtbGEQARgAKAAwABLjAQojdG9vbF9jYWxsLnJ1bl9zaGVsbF9jb21tYW5kLmNvbW1hbmQKKHRvb2xfY2FsbC5ydW5fc2hlbGxfY29tbWFuZC5pc19yZWFkX29ubHkKJnRvb2xfY2FsbC5ydW5fc2hlbGxfY29tbWFuZC51c2VzX3BhZ2VyCiR0b29sX2NhbGwucnVuX3NoZWxsX2NvbW1hbmQuaXNfcmlza3kKE3NlcnZlcl9tZXNzYWdlX2RhdGEKL3Rvb2xfY2FsbC5ydW5fc2hlbGxfY29tbWFuZC53YWl0X3VudGlsX2NvbXBsZXRlGiQ4MDQ4OWU0Ny1lOWU4LTQxNWQtYmE3Mi04NDUxMzU4Yzk0OTM="

	raw, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		t.Fatalf("decode base64: %v", err)
	}

	parsed, err := parseResponseEvent(raw)
	if err != nil {
		t.Fatalf("parseResponseEvent: %v", err)
	}
	if len(parsed.ToolCalls) == 0 {
		t.Fatalf("expected at least one tool call")
	}

	var found bool
	for _, tc := range parsed.ToolCalls {
		if tc.Name != "Bash" {
			continue
		}
		found = true
		if !strings.Contains(tc.Input, "ls -la") {
			t.Fatalf("expected command in input, got %q", tc.Input)
		}
	}
	if !found {
		t.Fatalf("Bash tool call not found")
	}
}

func TestParseToolCall_DropsIncompleteRunShellCommand(t *testing.T) {
	t.Parallel()

	// field1 tool_call_id, field2 run_shell_command payload(without command)
	payload := []byte{
		0x0a, 0x08, 't', 'o', 'o', 'l', '_', 'i', 'd', '1',
		0x12, 0x06, 0x10, 0x00, 0x18, 0x00, 0x28, 0x00,
	}
	out := &parsedEvent{}
	parseToolCall(payload, out)
	if len(out.ToolCalls) != 0 {
		t.Fatalf("expected no tool calls, got %d", len(out.ToolCalls))
	}
}

func TestParseToolCall_NormalizesRunShellToBash(t *testing.T) {
	t.Parallel()

	// field1 tool_call_id, field2 run_shell_command payload(with command)
	payload := []byte{
		0x0a, 0x08, 't', 'o', 'o', 'l', '_', 'i', 'd', '2',
		0x12, 0x10, 0x0a, 0x06, 'l', 's', ' ', '-', 'l', 'a', 0x10, 0x01, 0x18, 0x00, 0x28, 0x00, 0x30, 0x00,
	}
	out := &parsedEvent{}
	parseToolCall(payload, out)
	if len(out.ToolCalls) != 1 {
		t.Fatalf("expected one tool call, got %d", len(out.ToolCalls))
	}
	if out.ToolCalls[0].Name != "Bash" {
		t.Fatalf("expected Bash, got %q", out.ToolCalls[0].Name)
	}
	var input map[string]interface{}
	if err := json.Unmarshal([]byte(out.ToolCalls[0].Input), &input); err != nil {
		t.Fatalf("unmarshal input: %v", err)
	}
	if got, _ := input["command"].(string); got != "ls -la" {
		t.Fatalf("expected command ls -la, got %q", got)
	}
	if _, ok := input["is_read_only"]; ok {
		t.Fatalf("unexpected is_read_only in bash input: %s", out.ToolCalls[0].Input)
	}
	if _, ok := input["is_risky"]; ok {
		t.Fatalf("unexpected is_risky in bash input: %s", out.ToolCalls[0].Input)
	}
}
