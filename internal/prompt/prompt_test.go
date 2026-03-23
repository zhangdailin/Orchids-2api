package prompt

import (
	"testing"

	"github.com/goccy/go-json"
)

func TestMessageUnmarshalJSON_OpenAIToolCallsWithNullContent(t *testing.T) {
	raw := []byte(`{
		"role":"assistant",
		"content":null,
		"tool_calls":[
			{
				"id":"call_write_1",
				"type":"function",
				"function":{
					"name":"Write",
					"arguments":"{\"file_path\":\"note.txt\",\"content\":\"hello world\"}"
				}
			}
		]
	}`)

	var msg Message
	if err := json.Unmarshal(raw, &msg); err != nil {
		t.Fatalf("unmarshal message: %v", err)
	}

	if msg.Role != "assistant" {
		t.Fatalf("role = %q, want assistant", msg.Role)
	}
	if msg.Content.IsString() {
		t.Fatal("expected assistant tool call message to normalize into content blocks")
	}

	blocks := msg.Content.GetBlocks()
	if len(blocks) != 1 {
		t.Fatalf("blocks len = %d, want 1", len(blocks))
	}
	if blocks[0].Type != "tool_use" {
		t.Fatalf("block type = %q, want tool_use", blocks[0].Type)
	}
	if blocks[0].ID != "call_write_1" {
		t.Fatalf("block id = %q, want call_write_1", blocks[0].ID)
	}
	if blocks[0].Name != "Write" {
		t.Fatalf("block name = %q, want Write", blocks[0].Name)
	}
	input, ok := blocks[0].Input.(map[string]interface{})
	if !ok {
		t.Fatalf("block input type = %T, want map[string]interface{}", blocks[0].Input)
	}
	if input["file_path"] != "note.txt" || input["content"] != "hello world" {
		t.Fatalf("block input = %#v", input)
	}
}

func TestMessageUnmarshalJSON_OpenAIToolResultMessage(t *testing.T) {
	raw := []byte(`{
		"role":"tool",
		"tool_call_id":"call_write_1",
		"content":"Write succeeded: note.txt created with hello world"
	}`)

	var msg Message
	if err := json.Unmarshal(raw, &msg); err != nil {
		t.Fatalf("unmarshal message: %v", err)
	}

	if msg.Role != "user" {
		t.Fatalf("role = %q, want user", msg.Role)
	}
	if msg.Content.IsString() {
		t.Fatal("expected tool message to normalize into content blocks")
	}

	blocks := msg.Content.GetBlocks()
	if len(blocks) != 1 {
		t.Fatalf("blocks len = %d, want 1", len(blocks))
	}
	if blocks[0].Type != "tool_result" {
		t.Fatalf("block type = %q, want tool_result", blocks[0].Type)
	}
	if blocks[0].ToolUseID != "call_write_1" {
		t.Fatalf("tool_use_id = %q, want call_write_1", blocks[0].ToolUseID)
	}
	if got, ok := blocks[0].Content.(string); !ok || got != "Write succeeded: note.txt created with hello world" {
		t.Fatalf("tool_result content = %#v", blocks[0].Content)
	}
}
