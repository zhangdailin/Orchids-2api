package warp

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"strings"
	"testing"

	"github.com/goccy/go-json"

	"orchids-api/internal/upstream"
)

func TestMapWarpToolCalls_SplitsReadFiles(t *testing.T) {
	args := `{"paths":["/tmp/a.go","/tmp/b.go"],"start":10,"end":20}`
	calls := mapWarpToolCalls("read_files", args, "call_read", 0)
	if len(calls) != 2 {
		t.Fatalf("len(calls)=%d want 2", len(calls))
	}
	if calls[0].Name != "Read_0" || calls[1].Name != "Read_1" {
		t.Fatalf("unexpected tool names: %#v", calls)
	}

	var input map[string]interface{}
	if err := json.Unmarshal([]byte(calls[0].Input), &input); err != nil {
		t.Fatalf("unmarshal first input: %v", err)
	}
	if got := input["file_path"]; got != "/tmp/a.go" {
		t.Fatalf("file_path=%v want /tmp/a.go", got)
	}
	if got := input["offset"]; got != float64(10) {
		t.Fatalf("offset=%v want 10", got)
	}
	if got := input["limit"]; got != float64(20) {
		t.Fatalf("limit=%v want 20", got)
	}
}

func TestTransformWarpToolCall_MapsRunCommandToBash(t *testing.T) {
	name, input := transformWarpToolCall("run_command", map[string]interface{}{
		"command": "ls -la",
		"timeout": 30,
	})
	if name != "Bash" {
		t.Fatalf("name=%q want Bash", name)
	}
	if input["command"] != "ls -la" {
		t.Fatalf("command=%v want ls -la", input["command"])
	}
}

func TestTransformWarpToolCall_SanitizesEditPayload(t *testing.T) {
	name, input := transformWarpToolCall("edit_file", map[string]interface{}{
		"path":       " ./tmp/../tmp/demo.txt\x00 ",
		"old_string": "  1\tbefore\n  2\tafter",
		"new_string": "  1\treplaced\n  2\tvalue",
	})
	if name != "Edit" {
		t.Fatalf("name=%q want Edit", name)
	}
	if input["file_path"] != "./tmp/demo.txt" {
		t.Fatalf("file_path=%v want ./tmp/demo.txt", input["file_path"])
	}
	if input["old_string"] != "before\nafter" {
		t.Fatalf("old_string=%q want stripped line numbers", input["old_string"])
	}
	if input["new_string"] != "replaced\nvalue" {
		t.Fatalf("new_string=%q want stripped line numbers", input["new_string"])
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

func TestProcessStreamBody_FallsBackToLegacySSE(t *testing.T) {
	var events []upstream.SSEMessage

	payload := appendBytesField(2,
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
	)
	encoded := base64.RawURLEncoding.EncodeToString(payload)
	stream := "data: " + encoded + "\n\n"

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

func TestExtractStringValue_IgnoresBinaryPayload(t *testing.T) {
	if got := extractStringValue([]byte{0x02, 0x52, 0x00}); got != "" {
		t.Fatalf("extractStringValue(binary)=%q want empty", got)
	}
	if got := extractStringValue([]byte("hello")); got != "hello" {
		t.Fatalf("extractStringValue(text)=%q want hello", got)
	}
}

func appendBytesField(fieldNum int, payload []byte) []byte {
	var buf []byte
	buf = appendVarint(buf, uint64(fieldNum<<3|2))
	buf = appendVarint(buf, uint64(len(payload)))
	buf = append(buf, payload...)
	return buf
}

func appendVarintField(fieldNum int, value uint64) []byte {
	var buf []byte
	buf = appendVarint(buf, uint64(fieldNum<<3))
	buf = appendVarint(buf, value)
	return buf
}

func wrapFrame(payload []byte) []byte {
	out := make([]byte, 4+len(payload))
	binary.BigEndian.PutUint32(out[:4], uint32(len(payload)))
	copy(out[4:], payload)
	return out
}
