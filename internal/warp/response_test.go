package warp

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/goccy/go-json"
	warpapi "github.com/warpdotdev/warp-proto-apis/apis/multi_agent/v1/gen/go"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/fieldmaskpb"

	"orchids-api/internal/upstream"
)

func warpSSEEventFrame(t *testing.T, event *warpapi.ResponseEvent) string {
	t.Helper()
	raw, err := proto.Marshal(event)
	if err != nil {
		t.Fatalf("marshal Warp response event: %v", err)
	}
	return "data: " + base64.RawURLEncoding.EncodeToString(raw) + "\n\n"
}

func warpSSEActionFrame(t *testing.T, action *warpapi.ClientAction) string {
	t.Helper()
	return warpSSEEventFrame(t, warpapi.ResponseEvent_builder{
		ClientActions: warpapi.ResponseEvent_ClientActions_builder{
			Actions: []*warpapi.ClientAction{action},
		}.Build(),
	}.Build())
}

func warpSSEFinishFrame(t *testing.T) string {
	t.Helper()
	return warpSSEEventFrame(t, warpapi.ResponseEvent_builder{
		Finished: warpapi.ResponseEvent_StreamFinished_builder{
			Done: warpapi.ResponseEvent_StreamFinished_Done_builder{}.Build(),
		}.Build(),
	}.Build())
}

func warpSSETextFrame(t *testing.T, text string) string {
	t.Helper()
	return warpSSEActionFrame(t, warpapi.ClientAction_builder{
		AddMessagesToTask: warpapi.ClientAction_AddMessagesToTask_builder{
			TaskId:   stringPtr("task-1"),
			Messages: []*warpapi.Message{warpAgentOutputMessage("message-1", text)},
		}.Build(),
	}.Build())
}

func warpAgentOutputMessage(id, text string) *warpapi.Message {
	return warpapi.Message_builder{
		Id: stringPtr(id),
		AgentOutput: warpapi.Message_AgentOutput_builder{
			Text: stringPtr(text),
		}.Build(),
	}.Build()
}

func TestProcessStreamBody_DeduplicatesMessageSnapshotsAndIgnoresMetadataActions(t *testing.T) {
	messageID := "message-1"
	taskID := "task-1"
	stream := warpSSEActionFrame(t, warpapi.ClientAction_builder{
		AddMessagesToTask: warpapi.ClientAction_AddMessagesToTask_builder{
			TaskId:   stringPtr(taskID),
			Messages: []*warpapi.Message{warpAgentOutputMessage(messageID, "CONTINUATION")},
		}.Build(),
	}.Build())
	for _, delta := range []string{" ", "OK"} {
		stream += warpSSEActionFrame(t, warpapi.ClientAction_builder{
			AppendToMessageContent: warpapi.ClientAction_AppendToMessageContent_builder{
				TaskId:  stringPtr(taskID),
				Message: warpAgentOutputMessage(messageID, delta),
				Mask:    &fieldmaskpb.FieldMask{Paths: []string{"agent_output.text"}},
			}.Build(),
		}.Build())
	}
	stream += warpSSEActionFrame(t, warpapi.ClientAction_builder{
		UpdateTaskMessage: warpapi.ClientAction_UpdateTaskMessage_builder{
			TaskId:  stringPtr(taskID),
			Message: warpAgentOutputMessage(messageID, "CONTINUATION OK"),
			Mask:    &fieldmaskpb.FieldMask{Paths: []string{"agent_output.text"}},
		}.Build(),
	}.Build())
	stream += warpSSEActionFrame(t, warpapi.ClientAction_builder{
		UpdateTaskDescription: warpapi.ClientAction_UpdateTaskDescription_builder{
			TaskId:      stringPtr(taskID),
			Description: stringPtr("Acknowledge command with CONTINUATION OK"),
		}.Build(),
	}.Build())
	stream += warpSSEFinishFrame(t)

	var text strings.Builder
	var reasoning strings.Builder
	if err := processStreamBody(context.Background(), strings.NewReader(stream), func(message upstream.SSEMessage) {
		delta, _ := message.Event["delta"].(string)
		switch message.Type {
		case "model.text-delta":
			text.WriteString(delta)
		case "model.reasoning-delta":
			reasoning.WriteString(delta)
		}
	}, nil); err != nil {
		t.Fatalf("processStreamBody error: %v", err)
	}

	if got := text.String(); got != "CONTINUATION OK" {
		t.Fatalf("text=%q want one complete, whitespace-preserving response", got)
	}
	if got := reasoning.String(); got != "" {
		t.Fatalf("reasoning=%q want metadata action to stay hidden", got)
	}
}

func TestProcessStreamBody_DetectsNonProtobufHTML(t *testing.T) {
	err := processStreamBody(context.Background(), strings.NewReader("<!doctype html><html><body>challenge</body></html>"), func(upstream.SSEMessage) {}, nil)
	if err == nil {
		t.Fatal("expected error")
	}

	var nonProtoErr *nonProtobufStreamError
	if !errors.As(err, &nonProtoErr) {
		t.Fatalf("expected nonProtobufStreamError, got %T (%v)", err, err)
	}
	if nonProtoErr.Kind != "html" {
		t.Fatalf("kind=%q want html", nonProtoErr.Kind)
	}
	if !strings.Contains(nonProtoErr.Preview, "<!doctype html>") {
		t.Fatalf("preview=%q missing html prefix", nonProtoErr.Preview)
	}
}

func TestProcessStreamBody_ParsesNestedWarpFrames(t *testing.T) {
	var events []upstream.SSEMessage

	textFrame := wrapFrame(appendBytesField(2,
		appendBytesField(1,
			appendBytesField(1,
				appendBytesField(1,
					appendBytesField(5,
						appendBytesField(3,
							appendBytesField(1, []byte("hi")),
						),
					),
				),
			),
		),
	))
	finishFrame := wrapFrame(appendBytesField(3, appendBytesField(8, appendVarintField(2, 3))))

	err := processStreamBody(context.Background(), bytes.NewReader(append(textFrame, finishFrame...)), func(msg upstream.SSEMessage) {
		events = append(events, msg)
	}, nil)
	if err != nil {
		t.Fatalf("processStreamBody error: %v", err)
	}

	if len(events) != 2 {
		t.Fatalf("len(events)=%d want 2", len(events))
	}
	if events[0].Type != "model.text-delta" || events[0].Event["delta"] != "hi" {
		t.Fatalf("first event=%#v want text delta hi", events[0])
	}
	if events[1].Type != "model.finish" {
		t.Fatalf("second event=%#v want finish", events[1])
	}
	usage, _ := events[1].Event["usage"].(map[string]interface{})
	if usage["inputTokens"] != 3 || usage["outputTokens"] != 0 {
		t.Fatalf("usage=%#v want inputTokens=3 outputTokens=0", usage)
	}
}

func TestProcessStreamBody_HandlesDoneFinishReason(t *testing.T) {
	var events []upstream.SSEMessage

	finishPayload := appendBytesField(2, nil)
	finishPayload = append(finishPayload, appendBytesField(8, appendVarintField(2, 3))...)
	finishPayload = append(finishPayload, appendBytesField(8, appendVarintField(3, 5))...)
	finishFrame := wrapFrame(appendBytesField(3, finishPayload))

	err := processStreamBody(context.Background(), bytes.NewReader(finishFrame), func(msg upstream.SSEMessage) {
		events = append(events, msg)
	}, nil)
	if err != nil {
		t.Fatalf("processStreamBody error: %v", err)
	}
	if len(events) != 1 || events[0].Type != "model.finish" {
		t.Fatalf("events=%#v want one finish event", events)
	}
	if got, _ := events[0].Event["finishReason"].(string); got != "end_turn" {
		t.Fatalf("finishReason=%q want end_turn", got)
	}
	usage, _ := events[0].Event["usage"].(map[string]interface{})
	if usage["inputTokens"] != 3 || usage["outputTokens"] != 5 {
		t.Fatalf("usage=%#v want inputTokens=3 outputTokens=5", usage)
	}
}

func TestProcessStreamBody_MapsMaxTokenLimitToNormalStop(t *testing.T) {
	finishFrame := wrapFrame(appendBytesField(3, appendBytesField(3, nil)))
	var events []upstream.SSEMessage
	err := processStreamBody(context.Background(), bytes.NewReader(finishFrame), func(message upstream.SSEMessage) {
		events = append(events, message)
	}, nil)
	if err != nil {
		t.Fatalf("processStreamBody error: %v", err)
	}
	if len(events) != 1 || events[0].Type != "model.finish" || events[0].Event["finishReason"] != "max_tokens" {
		t.Fatalf("events=%#v want max_tokens finish", events)
	}
}

func TestParseStreamFinished_UsesCurrentChargeMetadata(t *testing.T) {
	input, output, cacheRead, cacheWrite := uint64(100), uint64(20), uint64(30), uint64(4)
	inputCost, outputCost, cacheReadCost, cacheWriteCost := float32(0.1), float32(0.2), float32(0.03), float32(0.04)
	searches, searchCost, platformCost := uint32(2), float32(0.5), float32(0.6)
	exactCredits, platformCredits := float32(1.25), float32(0.75)
	totalInput, conversationCredits, conversationPlatform, contextUsage := uint32(777), float32(8.5), float32(2.5), float32(0.42)

	count := warpapi.TokenCount_builder{
		Input: &input, Output: &output, InputCacheRead: &cacheRead, InputCacheWrite: &cacheWrite,
	}.Build()
	cost := warpapi.TokenCost_builder{
		InputCostInCents: &inputCost, OutputCostInCents: &outputCost,
		InputCacheReadCostInCents: &cacheReadCost, InputCacheWriteCostInCents: &cacheWriteCost,
	}.Build()
	inference := warpapi.InferenceUsage_builder{
		TokenCount: count, TokenCost: cost, WebSearchCount: &searches, WebSearchCostInCents: &searchCost,
	}.Build()
	charged := warpapi.ChargedUsage_builder{
		DirectApiInferenceUsage: map[string]*warpapi.InferenceUsage{"model": inference},
		PlatformUsageInCents:    &platformCost,
	}.Build()
	charges := warpapi.RequestCharges_builder{
		UsageByCategory: map[string]*warpapi.ChargedUsage{"primary_agent": charged},
	}.Build()
	requestCost := warpapi.ResponseEvent_StreamFinished_RequestCost_builder{Exact: &exactCredits, PlatformCredits: &platformCredits}.Build()
	conversation := warpapi.ResponseEvent_StreamFinished_ConversationUsageMetadata_builder{
		TotalInputTokens: &totalInput, CreditsSpent: &conversationCredits,
		PlatformCreditsSpent: &conversationPlatform, ContextWindowUsage: &contextUsage,
	}.Build()
	finished := warpapi.ResponseEvent_StreamFinished_builder{
		Done:           warpapi.ResponseEvent_StreamFinished_Done_builder{}.Build(),
		RequestCharges: charges, RequestCost: requestCost, ConversationUsageMetadata: conversation,
	}.Build()

	parsed := parseStreamFinished(finished)
	if parsed.InputTokens != 100 || parsed.OutputTokens != 20 || parsed.CacheReadTokens != 30 || parsed.CacheWriteTokens != 4 {
		t.Fatalf("unexpected token charges: %+v", parsed)
	}
	if parsed.WebSearchCount != 2 || parsed.RequestCredits != 1.25 || parsed.RequestPlatformCredits != 0.75 {
		t.Fatalf("unexpected request charges: %+v", parsed)
	}
	if parsed.ConversationTotalInput != 777 || parsed.ConversationCredits != 8.5 || parsed.ConversationPlatform != 2.5 {
		t.Fatalf("unexpected conversation usage: %+v", parsed)
	}
}

func TestProcessStreamBody_ExposesShouldRefreshModelConfig(t *testing.T) {
	var events []upstream.SSEMessage

	finishPayload := appendBytesField(2, nil)
	finishPayload = append(finishPayload, appendVarintField(9, 1)...)
	finishFrame := wrapFrame(appendBytesField(3, finishPayload))

	err := processStreamBody(context.Background(), bytes.NewReader(finishFrame), func(msg upstream.SSEMessage) {
		events = append(events, msg)
	}, nil)
	if err != nil {
		t.Fatalf("processStreamBody error: %v", err)
	}
	if len(events) != 1 || events[0].Type != "model.finish" {
		t.Fatalf("events=%#v want one finish event", events)
	}
	if got, _ := events[0].Event["shouldRefreshModelConfig"].(bool); !got {
		t.Fatalf("shouldRefreshModelConfig=%v want true", events[0].Event["shouldRefreshModelConfig"])
	}
}

func TestProcessStreamBody_ReturnsQuotaLimitFinishReason(t *testing.T) {
	finishPayload := appendBytesField(4, nil)
	finishPayload = append(finishPayload, appendBytesField(8, appendVarintField(2, 12))...)
	finishPayload = append(finishPayload, appendBytesField(8, appendVarintField(3, 34))...)
	finishFrame := wrapFrame(appendBytesField(3, finishPayload))

	var events []upstream.SSEMessage
	err := processStreamBody(context.Background(), bytes.NewReader(finishFrame), func(message upstream.SSEMessage) {
		events = append(events, message)
	}, nil)
	if err == nil {
		t.Fatal("expected quota limit error")
	}
	if got := err.Error(); !strings.Contains(got, "quota_limit") || !strings.Contains(got, "no remaining quota") {
		t.Fatalf("error=%q want quota_limit with no remaining quota", got)
	}
	if len(events) != 1 || events[0].Type != "model.tokens-used" || events[0].Event["inputTokens"] != 12 || events[0].Event["outputTokens"] != 34 {
		t.Fatalf("events=%#v want terminal usage before quota error", events)
	}
}

func TestProcessStreamBody_ReturnsContextWindowFinishReason(t *testing.T) {
	finishFrame := wrapFrame(appendBytesField(3, appendBytesField(5, nil)))

	err := processStreamBody(context.Background(), bytes.NewReader(finishFrame), func(upstream.SSEMessage) {}, nil)
	if err == nil {
		t.Fatal("expected context window error")
	}
	if got := err.Error(); !strings.Contains(got, "context_window_exceeded") || !strings.Contains(got, "input is too long") {
		t.Fatalf("error=%q want context_window_exceeded with input too long", got)
	}
}

func TestProcessStreamBody_ReturnsInternalErrorFinishMessage(t *testing.T) {
	finishFrame := wrapFrame(appendBytesField(3,
		appendBytesField(7, appendBytesField(1, []byte("upstream blew up"))),
	))

	err := processStreamBody(context.Background(), bytes.NewReader(finishFrame), func(upstream.SSEMessage) {}, nil)
	if err == nil {
		t.Fatal("expected internal error")
	}
	if got := err.Error(); !strings.Contains(got, "internal_error") || !strings.Contains(got, "upstream blew up") {
		t.Fatalf("error=%q want internal_error message", got)
	}
}

func TestProcessStreamBody_ReturnsInvalidAPIKeyFinishMessage(t *testing.T) {
	payload := appendVarintField(1, 2)
	payload = append(payload, appendBytesField(2, []byte("gpt-test"))...)
	finishFrame := wrapFrame(appendBytesField(3, appendBytesField(12, payload)))

	err := processStreamBody(context.Background(), bytes.NewReader(finishFrame), func(upstream.SSEMessage) {}, nil)
	if err == nil {
		t.Fatal("expected invalid api key error")
	}
	if got := err.Error(); !strings.Contains(got, "invalid_api_key") ||
		!strings.Contains(got, "provider=openai") ||
		!strings.Contains(got, "model=gpt-test") {
		t.Fatalf("error=%q want invalid_api_key provider/model", got)
	}
}

func TestProcessStreamBody_ErrorsWhenFramesHaveNoParsedEvents(t *testing.T) {
	err := processStreamBody(context.Background(), bytes.NewReader(wrapFrame(appendBytesField(99, nil))), func(upstream.SSEMessage) {}, nil)
	if err == nil {
		t.Fatal("expected no parsed events error")
	}
	if got := err.Error(); !strings.Contains(got, "without parsed response events") {
		t.Fatalf("error=%q want without parsed response events", got)
	}
}

func TestProcessStreamBody_AcceptsOfficialSSETransport(t *testing.T) {
	var events []upstream.SSEMessage
	stream := warpSSETextFrame(t, "hi") + warpSSEFinishFrame(t)

	err := processStreamBody(context.Background(), strings.NewReader(stream), func(msg upstream.SSEMessage) {
		events = append(events, msg)
	}, nil)
	if err != nil {
		t.Fatalf("processStreamBody error: %v", err)
	}

	if len(events) != 2 {
		t.Fatalf("len(events)=%d want 2", len(events))
	}
	if events[0].Type != "model.text-delta" || events[0].Event["delta"] != "hi" {
		t.Fatalf("first event=%#v want text delta hi", events[0])
	}
	if events[1].Type != "model.finish" {
		t.Fatalf("second event=%#v want finish", events[1])
	}
}

func TestProcessStreamBody_RejectsMissingStreamFinished(t *testing.T) {
	err := processStreamBody(context.Background(), strings.NewReader(warpSSETextFrame(t, "hi")), func(upstream.SSEMessage) {}, nil)
	if err == nil || !strings.Contains(err.Error(), "without StreamFinished") {
		t.Fatalf("error=%v want missing StreamFinished error", err)
	}
}

func TestProcessStreamBody_RejectsInvalidSSEPayload(t *testing.T) {
	err := processStreamBody(context.Background(), strings.NewReader("data: %%%\n\n"), func(upstream.SSEMessage) {}, nil)
	if err == nil || !strings.Contains(err.Error(), "decode Warp SSE payload") {
		t.Fatalf("error=%v want SSE decode error", err)
	}
}

func TestProcessStreamBody_AllowsNilOnMessage(t *testing.T) {
	stream := warpSSETextFrame(t, "hi") + warpSSEFinishFrame(t)

	if err := processStreamBody(context.Background(), strings.NewReader(stream), nil, nil); err != nil {
		t.Fatalf("processStreamBody(nil onMessage) error: %v", err)
	}
}

func TestParseResponseEvent_CapturesUpstreamRequestID(t *testing.T) {
	event := warpapi.ResponseEvent_builder{
		Init: warpapi.ResponseEvent_StreamInit_builder{
			ConversationId: stringPtr("conversation-1"),
			RequestId:      stringPtr("request-1"),
			RunId:          stringPtr("run-1"),
		}.Build(),
	}.Build()
	raw, err := proto.Marshal(event)
	if err != nil {
		t.Fatalf("marshal init event: %v", err)
	}
	parsed, err := parseResponseEvent(raw)
	if err != nil {
		t.Fatalf("parseResponseEvent() error = %v", err)
	}
	if parsed.RequestID != "request-1" || parsed.RunID != "run-1" || parsed.ConversationID != "conversation-1" {
		t.Fatalf("unexpected init metadata: %+v", parsed)
	}
}

func TestHandleStreamResponse_AllowsNilOnMessageWithConversationID(t *testing.T) {
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{},
		Body:       io.NopCloser(strings.NewReader(warpSSETextFrame(t, "hi") + warpSSEFinishFrame(t))),
	}
	client := &Client{}
	req := upstream.UpstreamRequest{ChatSessionID: "conv_123"}

	if err := client.handleStreamResponse(context.Background(), req, resp, nil, nil); err != nil {
		t.Fatalf("handleStreamResponse(nil onMessage) error: %v", err)
	}
}

func TestProcessStreamBody_SuppressesUnsupportedServerTool(t *testing.T) {
	var events []upstream.SSEMessage

	event := warpapi.ResponseEvent_builder{
		ClientActions: warpapi.ResponseEvent_ClientActions_builder{
			Actions: []*warpapi.ClientAction{
				warpapi.ClientAction_builder{
					CreateTask: warpapi.ClientAction_CreateTask_builder{
						Task: warpapi.Task_builder{
							Messages: []*warpapi.Message{
								warpAgentOutputMessage("text", "4"),
								warpapi.Message_builder{
									Id: stringPtr("server-tool"),
									ToolCall: warpapi.Message_ToolCall_builder{
										ToolCallId: stringPtr("call-server"),
										Server: warpapi.Message_ToolCall_Server_builder{
											Payload: stringPtr("bogus"),
										}.Build(),
									}.Build(),
								}.Build(),
							},
						}.Build(),
					}.Build(),
				}.Build(),
			},
		}.Build(),
	}.Build()
	rawEvent, err := proto.Marshal(event)
	if err != nil {
		t.Fatalf("marshal response event: %v", err)
	}
	textAndToolFrame := wrapFrame(rawEvent)
	finishFrame := wrapFrame(appendBytesField(3, appendBytesField(8, appendVarintField(2, 1))))

	err = processStreamBody(context.Background(), bytes.NewReader(append(textAndToolFrame, finishFrame...)), func(msg upstream.SSEMessage) {
		events = append(events, msg)
	}, nil)
	if err != nil {
		t.Fatalf("processStreamBody error: %v", err)
	}

	if len(events) != 2 {
		t.Fatalf("len(events)=%d want 2", len(events))
	}
	if events[0].Type != "model.text-delta" || events[0].Event["delta"] != "4" {
		t.Fatalf("first event=%#v want text delta 4", events[0])
	}
	if events[1].Type != "model.finish" {
		t.Fatalf("second event=%#v want finish", events[1])
	}
	if got, _ := events[1].Event["finishReason"].(string); got != "end_turn" {
		t.Fatalf("finishReason=%q want end_turn", got)
	}
}

func TestProcessStreamBody_ParsesRunShellCommand(t *testing.T) {
	var events []upstream.SSEMessage

	messagePayload := appendBytesField(4, appendBytesField(2, appendBytesField(1, []byte("pwd"))))
	toolFrame := wrapFrame(appendBytesField(2,
		appendBytesField(1,
			appendBytesField(1,
				appendBytesField(1,
					appendBytesField(5, messagePayload),
				),
			),
		),
	))
	finishFrame := wrapFrame(appendBytesField(3, appendBytesField(8, appendVarintField(2, 1))))

	err := processStreamBody(context.Background(), bytes.NewReader(append(toolFrame, finishFrame...)), func(msg upstream.SSEMessage) {
		events = append(events, msg)
	}, nil)
	if err != nil {
		t.Fatalf("processStreamBody error: %v", err)
	}

	if len(events) != 2 {
		t.Fatalf("len(events)=%d want 2", len(events))
	}
	if events[0].Type != "model.tool-call" {
		t.Fatalf("first event=%#v want tool call", events[0])
	}
	if got, _ := events[0].Event["toolName"].(string); got != "Bash" {
		t.Fatalf("toolName=%q want Bash", got)
	}
	if got, _ := events[0].Event["input"].(string); !strings.Contains(got, `"command":"pwd"`) {
		t.Fatalf("input=%q want command pwd", got)
	}
	if events[1].Type != "model.finish" {
		t.Fatalf("second event=%#v want finish", events[1])
	}
	if got, _ := events[1].Event["finishReason"].(string); got != "tool_use" {
		t.Fatalf("finishReason=%q want tool_use", got)
	}
}

func TestProcessStreamBody_ParsesReadShellCommandOutput(t *testing.T) {
	var events []upstream.SSEMessage

	readOutputPayload := appendBytesField(1, []byte("cmd_123"))
	readOutputPayload = append(readOutputPayload, appendBytesField(2,
		appendVarintField(1, 2),
	)...)
	messagePayload := appendBytesField(4, appendBytesField(23, readOutputPayload))
	toolFrame := wrapFrame(appendBytesField(2,
		appendBytesField(1,
			appendBytesField(1,
				appendBytesField(1,
					appendBytesField(5, messagePayload),
				),
			),
		),
	))
	finishFrame := wrapFrame(appendBytesField(3, appendBytesField(8, appendVarintField(2, 1))))

	err := processStreamBody(context.Background(), bytes.NewReader(append(toolFrame, finishFrame...)), func(msg upstream.SSEMessage) {
		events = append(events, msg)
	}, nil)
	if err != nil {
		t.Fatalf("processStreamBody error: %v", err)
	}

	if len(events) != 2 {
		t.Fatalf("len(events)=%d want 2", len(events))
	}
	if events[0].Type != "model.tool-call" {
		t.Fatalf("first event=%#v want tool call", events[0])
	}
	if got, _ := events[0].Event["toolName"].(string); got != "read_shell_command_output" {
		t.Fatalf("toolName=%q want read_shell_command_output", got)
	}
	var input map[string]interface{}
	rawInput, _ := events[0].Event["input"].(string)
	if err := json.Unmarshal([]byte(rawInput), &input); err != nil {
		t.Fatalf("tool input json: %v", err)
	}
	if got, _ := input["command_id"].(string); got != "cmd_123" {
		t.Fatalf("command_id=%q want cmd_123", got)
	}
	duration, _ := input["duration"].(map[string]interface{})
	if got := duration["seconds"]; got != float64(2) {
		t.Fatalf("duration.seconds=%v want 2", got)
	}
	if got, _ := events[1].Event["finishReason"].(string); got != "tool_use" {
		t.Fatalf("finishReason=%q want tool_use", got)
	}
}

func TestParseApplyFileDiffsPayload_UsesOfficialSchema(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		call      *warpapi.Message_ToolCall_ApplyFileDiffs
		wantTool  string
		wantInput map[string]string
	}{
		{
			name: "new file",
			call: warpapi.Message_ToolCall_ApplyFileDiffs_builder{
				NewFiles: []*warpapi.Message_ToolCall_ApplyFileDiffs_NewFile{
					warpapi.Message_ToolCall_ApplyFileDiffs_NewFile_builder{
						FilePath: stringPtr("/tmp/generated.txt"),
						Content:  stringPtr("generated content"),
					}.Build(),
				},
			}.Build(),
			wantTool:  "Write",
			wantInput: map[string]string{"file_path": "/tmp/generated.txt", "content": "generated content"},
		},
		{
			name: "file edit",
			call: warpapi.Message_ToolCall_ApplyFileDiffs_builder{
				Diffs: []*warpapi.Message_ToolCall_ApplyFileDiffs_FileDiff{
					warpapi.Message_ToolCall_ApplyFileDiffs_FileDiff_builder{
						FilePath: stringPtr("/tmp/existing.txt"),
						Search:   stringPtr("before"),
						Replace:  stringPtr("after"),
					}.Build(),
				},
			}.Build(),
			wantTool:  "Edit",
			wantInput: map[string]string{"file_path": "/tmp/existing.txt", "old_string": "before", "new_string": "after"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			payload, err := proto.Marshal(tt.call)
			if err != nil {
				t.Fatalf("marshal tool call: %v", err)
			}
			toolName, rawInput := parseApplyFileDiffsPayload(payload)
			if toolName != tt.wantTool {
				t.Fatalf("tool=%q want %q", toolName, tt.wantTool)
			}
			var input map[string]string
			if err := json.Unmarshal([]byte(rawInput), &input); err != nil {
				t.Fatalf("decode input: %v", err)
			}
			for key, want := range tt.wantInput {
				if got := input[key]; got != want {
					t.Fatalf("input[%q]=%q want %q", key, got, want)
				}
			}
		})
	}
}

func TestParseWarpToolInput_RejectsLossyBatches(t *testing.T) {
	t.Parallel()
	read := warpapi.Message_ToolCall_ReadFiles_builder{Files: []*warpapi.Message_ToolCall_ReadFiles_File{
		warpapi.Message_ToolCall_ReadFiles_File_builder{Name: stringPtr("a.go")}.Build(),
		warpapi.Message_ToolCall_ReadFiles_File_builder{Name: stringPtr("b.go")}.Build(),
	}}.Build()
	readPayload, err := proto.Marshal(read)
	if err != nil {
		t.Fatalf("marshal read batch: %v", err)
	}
	if name, input := parseWarpToolInput("read_files", readPayload); name != "Read" || input != "{}" {
		t.Fatalf("lossy read batch was emitted as %q %q", name, input)
	}

	diffs := warpapi.Message_ToolCall_ApplyFileDiffs_builder{Diffs: []*warpapi.Message_ToolCall_ApplyFileDiffs_FileDiff{
		warpapi.Message_ToolCall_ApplyFileDiffs_FileDiff_builder{FilePath: stringPtr("a.go"), Search: stringPtr("a"), Replace: stringPtr("b")}.Build(),
		warpapi.Message_ToolCall_ApplyFileDiffs_FileDiff_builder{FilePath: stringPtr("b.go"), Search: stringPtr("a"), Replace: stringPtr("b")}.Build(),
	}}.Build()
	diffPayload, err := proto.Marshal(diffs)
	if err != nil {
		t.Fatalf("marshal diff batch: %v", err)
	}
	if name, input := parseApplyFileDiffsPayload(diffPayload); name != "apply_file_diffs" || input != "{}" {
		t.Fatalf("lossy diff batch was emitted as %q %q", name, input)
	}
}

func TestParseWarpToolInput_UsesOfficialSearchSchemas(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		toolType    string
		message     proto.Message
		wantTool    string
		wantPattern string
		wantPath    string
	}{
		{
			name:     "grep",
			toolType: "grep",
			message: warpapi.Message_ToolCall_Grep_builder{
				Queries: []string{"TODO"},
				Path:    stringPtr("/workspace"),
			}.Build(),
			wantTool:    "Grep",
			wantPattern: "TODO",
			wantPath:    "/workspace",
		},
		{
			name:     "glob v2",
			toolType: "file_glob_v2",
			message: warpapi.Message_ToolCall_FileGlobV2_builder{
				Patterns:  []string{"**/*.go"},
				SearchDir: stringPtr("/workspace"),
			}.Build(),
			wantTool:    "Glob",
			wantPattern: "**/*.go",
			wantPath:    "/workspace",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			payload, err := proto.Marshal(tt.message)
			if err != nil {
				t.Fatalf("marshal tool input: %v", err)
			}
			toolName, rawInput := parseWarpToolInput(tt.toolType, payload)
			if toolName != tt.wantTool {
				t.Fatalf("tool=%q want %q", toolName, tt.wantTool)
			}
			var input map[string]string
			if err := json.Unmarshal([]byte(rawInput), &input); err != nil {
				t.Fatalf("decode input: %v", err)
			}
			if input["pattern"] != tt.wantPattern || input["path"] != tt.wantPath {
				t.Fatalf("input=%#v want pattern=%q path=%q", input, tt.wantPattern, tt.wantPath)
			}
		})
	}
}

func TestDecodeWarpPayload_UsesMatchingBase64Alphabet(t *testing.T) {
	payload := []byte{0xfb, 0xff, 0x00, 0x41, 0x42}
	encodings := []*base64.Encoding{
		base64.RawURLEncoding,
		base64.URLEncoding,
		base64.StdEncoding,
	}
	for _, encoding := range encodings {
		encoded := encoding.EncodeToString(payload)
		decoded, err := decodeWarpPayload(encoded)
		if err != nil {
			t.Fatalf("decode %q: %v", encoded, err)
		}
		if !bytes.Equal(decoded, payload) {
			t.Fatalf("decode %q=%x want %x", encoded, decoded, payload)
		}
	}
}

func BenchmarkWarpStreamStateAppendContent(b *testing.B) {
	const chunks = 1000
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		state := newWarpStreamState()
		for j := 0; j < chunks; j++ {
			state.applyContentUpdate(warpContentUpdate{MessageID: "message", Text: "x"})
		}
		if got := state.textByMessage["message"].Len(); got != chunks {
			b.Fatalf("content length=%d want %d", got, chunks)
		}
	}
}

func appendBytesField(fieldNum int, payload []byte) []byte {
	var buf []byte
	buf = appendTestVarint(buf, uint64(fieldNum<<3|2))
	buf = appendTestVarint(buf, uint64(len(payload)))
	buf = append(buf, payload...)
	return buf
}

func appendVarintField(fieldNum int, value uint64) []byte {
	var buf []byte
	buf = appendTestVarint(buf, uint64(fieldNum<<3))
	buf = appendTestVarint(buf, value)
	return buf
}

func appendTestVarint(buf []byte, v uint64) []byte {
	for v >= 0x80 {
		buf = append(buf, byte(v)|0x80)
		v >>= 7
	}
	buf = append(buf, byte(v))
	return buf
}

func wrapFrame(payload []byte) []byte {
	out := make([]byte, 4+len(payload))
	binary.BigEndian.PutUint32(out[:4], uint32(len(payload)))
	copy(out[4:], payload)
	return out
}
