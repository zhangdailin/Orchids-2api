package grok

import (
	"bytes"
	"io"
	"strings"
	"testing"
)

func TestReadResponseSSEBytesPreservesPayloads(t *testing.T) {
	stream := "\ufeffevent: message\r\ndata: {\"text\":\"hello\"}\r\ndata: \r\n\r\n" +
		"data: [DONE]\n\n"
	var events []string
	var payloads [][]byte
	if err := readResponseSSEBytes(strings.NewReader(stream), func(event string, data []byte) error {
		events = append(events, event)
		payloads = append(payloads, bytes.Clone(data))
		return nil
	}); err != nil {
		t.Fatalf("readResponseSSEBytes: %v", err)
	}
	if len(payloads) != 2 {
		t.Fatalf("got %d payloads, want 2", len(payloads))
	}
	if events[0] != "message" || string(payloads[0]) != "{\"text\":\"hello\"}\n" {
		t.Fatalf("first event = %q %q", events[0], payloads[0])
	}
	if events[1] != "" || string(payloads[1]) != "[DONE]" {
		t.Fatalf("terminal event = %q %q", events[1], payloads[1])
	}
}

func BenchmarkReadResponseSSEBytesJSONPayload(b *testing.B) {
	payload := `{"id":"chatcmpl-1","choices":[{"index":0,"delta":{"content":"` + strings.Repeat("response text ", 32) + `"}}]}`
	stream := []byte("event: message\ndata: " + payload + "\n\ndata: [DONE]\n\n")
	b.ReportAllocs()
	b.SetBytes(int64(len(stream)))
	for b.Loop() {
		if err := readResponseSSEBytes(bytes.NewReader(stream), func(_ string, data []byte) error {
			_, _ = io.Discard.Write(data)
			return nil
		}); err != nil {
			b.Fatal(err)
		}
	}
}
