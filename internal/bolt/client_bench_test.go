package bolt

import (
	"strings"
	"testing"

	"orchids-api/internal/prompt"
	"orchids-api/internal/upstream"
)

func benchmarkBoltRequest() upstream.UpstreamRequest {
	return upstream.UpstreamRequest{
		Model:   "claude-opus-4-6",
		Workdir: "d:\\Code\\Orchids-2api",
		Tools: []interface{}{
			map[string]interface{}{"name": "Read"},
			map[string]interface{}{"name": "Write"},
			map[string]interface{}{"name": "Edit"},
			map[string]interface{}{"name": "Bash"},
			map[string]interface{}{"name": "Glob"},
			map[string]interface{}{"name": "Grep"},
		},
		System: []prompt.SystemItem{
			{Type: "text", Text: "keep this custom instruction"},
			{Type: "text", Text: "# MCP Server\n- filesystem\n# VSCode Extension Context\nworkspace ready"},
		},
		Messages: []prompt.Message{
			{Role: "user", Content: prompt.MessageContent{Text: "inspect this repository and summarize the architecture"}},
			{Role: "assistant", Content: prompt.MessageContent{Text: "I will inspect the repository."}},
			{
				Role: "user",
				Content: prompt.MessageContent{
					Blocks: []prompt.ContentBlock{
						{
							Type:      "tool_result",
							ToolUseID: "tool_read",
							Content:   "# Orchids-2api\nA Go multi-provider proxy.",
						},
						{
							Type: "text",
							Text: "continue and inspect the handler package too",
						},
					},
				},
			},
		},
	}
}

func BenchmarkPrepareRequest(b *testing.B) {
	req := benchmarkBoltRequest()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		boltReq, estimate := prepareRequest(req, "sb1-demo")
		if boltReq == nil || estimate.Total == 0 {
			b.Fatal("unexpected empty prepared request")
		}
	}
}

func BenchmarkOutboundConverterProcessStream_Text(b *testing.B) {
	payload := strings.Join([]string{
		`0:"hello "`,
		`0:"world"`,
		"0:\"```json\\n{\\\"not\\\":\\\"tool\\\"}\\n```\"",
		`e:{"finishReason":"stop","usage":{"promptTokens":12,"completionTokens":7}}`,
	}, "\n") + "\n"

	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		converter := newOutboundConverter("claude-opus-4-6", 123)
		if err := converter.ProcessStream(strings.NewReader(payload), func(msg upstream.SSEMessage) error {
			return nil
		}); err != nil {
			b.Fatalf("ProcessStream() error = %v", err)
		}
	}
}

func BenchmarkOutboundConverterProcessStream_StructuredToolCall(b *testing.B) {
	payload := strings.Join([]string{
		`9:{"tool_calls":[{"function":"Read","parameters":{"file_path":"README.md"}}]}`,
		`e:{"finishReason":"stop","usage":{"promptTokens":12,"completionTokens":7}}`,
	}, "\n") + "\n"

	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		converter := newOutboundConverter("claude-opus-4-6", 123)
		if err := converter.ProcessStream(strings.NewReader(payload), func(msg upstream.SSEMessage) error {
			return nil
		}); err != nil {
			b.Fatalf("ProcessStream() error = %v", err)
		}
	}
}
