package grok

import (
	"context"
	"encoding/base64"
	"testing"

	"github.com/goccy/go-json"

	"orchids-api/internal/store"
)

// testReplayCipher produces a distinct but equally valid opaque cipher per seed,
// so dedup tests never collide by accident.
func testReplayCipher(seed byte) string {
	data := make([]byte, 128)
	for i := range data {
		data[i] = byte(i) ^ seed
	}
	return base64.RawStdEncoding.EncodeToString(data)
}

func TestNormalizeReplayItemsRequiresAnAnchor(t *testing.T) {
	reasoning := map[string]interface{}{"type": "reasoning", "encrypted_content": testReplayCipher(1)}
	message := map[string]interface{}{"type": "message", "role": "assistant", "content": []interface{}{map[string]interface{}{"type": "output_text", "text": "answer"}}}

	items, ok := normalizeReplayItems([]interface{}{reasoning, message})
	if !ok || len(items) != 2 {
		t.Fatalf("items=%v ok=%v", items, ok)
	}
	// Reasoning items must carry only the portable shape; extra keys 400 upstream.
	normalized, _ := items[0].(map[string]interface{})
	if len(normalized) != 3 {
		t.Fatalf("reasoning item not reduced to its portable shape: %v", normalized)
	}
	// A message alone is not replayable.
	if _, ok := normalizeReplayItems([]interface{}{message}); ok {
		t.Fatal("message-only replay should not be anchorable")
	}
	// An undecodable cipher is dropped, so it cannot act as the anchor either.
	bad := map[string]interface{}{"type": "reasoning", "encrypted_content": "gAAAA-not-a-cipher"}
	if items, ok := normalizeReplayItems([]interface{}{bad, message}); ok || len(items) != 1 {
		t.Fatalf("invalid cipher items=%v ok=%v", items, ok)
	}
}

func TestFilterReplayItemsDropsWhatTheRequestAlreadyHas(t *testing.T) {
	cipher := testReplayCipher(2)
	items := []interface{}{
		map[string]interface{}{"type": "reasoning", "summary": []interface{}{}, "encrypted_content": cipher},
		map[string]interface{}{"type": "function_call", "call_id": "call_1", "name": "exec", "arguments": "{}"},
	}
	// The request already carries this cipher, so nothing is replayed.
	duplicate := []interface{}{
		map[string]interface{}{"type": "reasoning", "encrypted_content": cipher},
		map[string]interface{}{"role": "user", "content": "next"},
	}
	if got := filterReplayItemsForInput(duplicate, items); len(got) != 0 {
		t.Fatalf("duplicate replay=%v", got)
	}
	// A tool call whose output the request does not carry is not replayable.
	plain := []interface{}{map[string]interface{}{"role": "user", "content": "next"}}
	got := filterReplayItemsForInput(plain, items)
	if len(got) != 1 || interfaceString(got[0].(map[string]interface{})["type"]) != "reasoning" {
		t.Fatalf("expected only the reasoning item, got %v", got)
	}
}

func TestFilterReplayItemsMatchesPrefixedToolCallIDs(t *testing.T) {
	items := []interface{}{
		map[string]interface{}{"type": "function_call", "call_id": "call_1", "name": "exec", "arguments": "{}"},
	}
	// The client's own output uses the Anthropic-style prefix for the same call.
	input := []interface{}{
		map[string]interface{}{"type": "function_call_output", "call_id": "toolu_call_1", "output": "done"},
		map[string]interface{}{"role": "user", "content": "next"},
	}
	got := filterReplayItemsForInput(input, items)
	if len(got) != 1 {
		t.Fatalf("prefixed call not matched: %v", got)
	}
	// The replayed call is rebound to the spelling the client already uses.
	if callID := interfaceString(got[0].(map[string]interface{})["call_id"]); callID != "toolu_call_1" {
		t.Fatalf("call_id=%q want toolu_call_1", callID)
	}
}

func TestInsertReplayItemsPlacesBeforeMatchingToolOutput(t *testing.T) {
	items := []interface{}{
		map[string]interface{}{"type": "reasoning", "summary": []interface{}{}, "encrypted_content": testReplayCipher(3)},
		map[string]interface{}{"type": "function_call", "call_id": "call_9", "name": "exec", "arguments": "{}"},
	}
	input := []interface{}{
		map[string]interface{}{"role": "system", "content": "sys"},
		map[string]interface{}{"role": "user", "content": "go"},
		map[string]interface{}{"type": "function_call_output", "call_id": "call_9", "output": "done"},
	}
	got := insertReplayItems(input, items)
	if len(got) != 5 {
		t.Fatalf("inserted=%v", got)
	}
	// The replay must land ahead of its own tool output, never after it.
	for index, want := range []string{"", "", "reasoning", "function_call", "function_call_output"} {
		if want == "" {
			continue
		}
		if typeName := interfaceString(got[index].(map[string]interface{})["type"]); typeName != want {
			t.Fatalf("index %d type=%q want %q (%v)", index, typeName, want, got)
		}
	}
}

func TestReplayItemsFromLegacyCipher(t *testing.T) {
	cipher := validTestReplayCipher()
	items := replayItemsFromStored(&store.StoredReasoningReplay{EncryptedContent: cipher})
	if len(items) != 1 {
		t.Fatalf("legacy items=%v", items)
	}
	if got := interfaceString(items[0].(map[string]interface{})["encrypted_content"]); got != cipher {
		t.Fatalf("legacy cipher=%q", got)
	}
	// A normalized item list takes precedence when both forms are present.
	encoded, err := json.Marshal(map[string]interface{}{"type": "reasoning", "summary": []interface{}{}, "encrypted_content": testReplayCipher(4)})
	if err != nil {
		t.Fatal(err)
	}
	items = replayItemsFromStored(&store.StoredReasoningReplay{EncryptedContent: cipher, Items: []json.RawMessage{encoded}})
	if len(items) != 1 || interfaceString(items[0].(map[string]interface{})["encrypted_content"]) == cipher {
		t.Fatalf("items should win over the legacy cipher: %v", items)
	}
}

func TestCaptureReasoningReplayClearsStateWithoutAnchor(t *testing.T) {
	h := &Handler{affinity: map[string]sessionAffinityEntry{}, replay: map[string]reasoningReplayEntry{}}
	h.storeReasoningReplay("grok-4.6", "session", validTestReplayCipher())
	// The completed response carries only a message, so nothing is anchorable.
	h.captureReasoningReplayFromMap(context.Background(), "grok-4.6", "session", map[string]interface{}{
		"output": []interface{}{
			map[string]interface{}{"type": "message", "role": "assistant", "content": []interface{}{map[string]interface{}{"type": "output_text", "text": "hi"}}},
		},
	})
	if items := h.loadReasoningReplayItems("grok-4.6", "session"); len(items) != 0 {
		t.Fatalf("stale state kept: %v", items)
	}
}

func TestCaptureReasoningReplayFromStream(t *testing.T) {
	h := &Handler{affinity: map[string]sessionAffinityEntry{}, replay: map[string]reasoningReplayEntry{}}
	cipher := testReplayCipher(5)
	stream := "data: {\"type\":\"response.output_item.done\",\"item\":{\"type\":\"reasoning\",\"encrypted_content\":\"" + cipher + "\"}}\n\n" +
		"data: {\"type\":\"response.completed\",\"response\":{\"output\":[{\"type\":\"reasoning\",\"encrypted_content\":\"" + cipher + "\"}]}}\n\n"
	h.captureReasoningReplay(context.Background(), "grok-4.6", "session", []byte(stream))
	items := h.loadReasoningReplayItems("grok-4.6", "session")
	if len(items) != 1 {
		t.Fatalf("stream capture items=%v", items)
	}
	if got := interfaceString(items[0].(map[string]interface{})["encrypted_content"]); got != cipher {
		t.Fatalf("stream capture cipher=%q want %q", got, cipher)
	}
}
