package warp

import (
	"bytes"
	"context"
	"encoding/binary"
	"strings"
	"testing"

	warpapi "github.com/warpdotdev/warp-proto-apis/apis/multi_agent/v1/gen/go"
	"google.golang.org/protobuf/proto"
	"orchids-api/internal/debug"
)

func TestWarpDiagnosticsContainDecodedProtobuf(t *testing.T) {
	ctx, capture := debug.WithCapture(context.Background(), "warp")
	logger := debug.NewForContext(ctx, false, false)
	logger.LogUpstreamRequest("https://warp.invalid", nil, map[string]string{"input": "hello"})
	event := warpapi.ResponseEvent_builder{Init: warpapi.ResponseEvent_StreamInit_builder{ConversationId: stringPtr("conversation-evidence"), RequestId: stringPtr("request-evidence")}.Build()}.Build()
	finish := warpapi.ResponseEvent_builder{Finished: warpapi.ResponseEvent_StreamFinished_builder{Done: warpapi.ResponseEvent_StreamFinished_Done_builder{}.Build()}.Build()}.Build()
	var stream bytes.Buffer
	for _, event := range []*warpapi.ResponseEvent{event, finish} {
		raw, err := proto.Marshal(event)
		if err != nil {
			t.Fatal(err)
		}
		binary.Write(&stream, binary.BigEndian, uint32(len(raw)))
		stream.Write(raw)
	}
	if err := processStreamBody(ctx, &stream, nil, logger); err != nil {
		t.Fatal(err)
	}
	joined := ""
	for _, s := range capture.Bundle().Sections {
		if strings.HasSuffix(s.Name, "response.txt") {
			joined += s.Payload
		}
	}
	if !strings.Contains(joined, "conversation-evidence") || !strings.Contains(joined, "request-evidence") || !strings.Contains(joined, "finished") {
		t.Fatal("protobuf content missing", joined)
	}
}
