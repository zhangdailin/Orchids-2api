package warp

import (
	"strings"
	"testing"

	"orchids-api/internal/prompt"
	"orchids-api/internal/upstream"
)

// benchmarkWarpConversation mirrors the shape a coding harness re-sends every
// turn: alternating user tool_result and assistant tool_use turns, each carrying
// a text block of the requested size.
func benchmarkWarpConversation(turns, blockChars int) upstream.UpstreamRequest {
	text := strings.Repeat("x", blockChars)
	msgs := make([]prompt.Message, 0, turns*2)
	for i := 0; i < turns; i++ {
		msgs = append(msgs,
			prompt.Message{
				Role: "user",
				Content: prompt.MessageContent{Blocks: []prompt.ContentBlock{
					{Type: "text", Text: text},
					{Type: "tool_result", ToolUseID: "toolu_1", Content: "ok"},
				}},
			},
			prompt.Message{
				Role: "assistant",
				Content: prompt.MessageContent{Blocks: []prompt.ContentBlock{
					{Type: "text", Text: "inspecting"},
					{Type: "tool_use", ID: "toolu_1", Name: "Read", Input: map[string]interface{}{"file_path": "/tmp/a.txt"}},
				}},
			},
		)
	}
	return upstream.UpstreamRequest{
		Model:         "claude-4-5-sonnet",
		ChatSessionID: "warp_conv_1",
		Messages:      msgs,
	}
}

func BenchmarkBuildWarpFileGlobV2Result(b *testing.B) {
	payload := strings.Repeat("/repo/file.go\n", 128)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = buildWarpFileGlobV2Result(payload, false)
	}
}

// One upstream payload build per request: this is what turns the re-sent
// conversation into the protobuf Warp receives, so it carries the whole request
// body's worth of work on the channel DeepSeek Harness drives.
func BenchmarkBuildRequestBytes(b *testing.B) {
	req := benchmarkWarpConversation(20, 800)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, payload, err := buildRequestBytes(req)
		if err != nil {
			b.Fatal(err)
		}
		_ = payload
	}
}
