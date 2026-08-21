package puter

import (
	"strings"
	"testing"

	"orchids-api/internal/errors"
)

func TestNormalizeStreamToolInput(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want string
	}{
		{name: "empty", raw: "", want: "{}"},
		{name: "null", raw: "null", want: "{}"},
		{name: "object", raw: `{"query":"SpaceX latest news"}`, want: `{"query":"SpaceX latest news"}`},
		{name: "object-native-tool", raw: `{"file_path":"README.md"}`, want: `{"file_path":"README.md"}`},
		{name: "json-string", raw: `"{\"file_path\":\"README.md\"}"`, want: `{"file_path":"README.md"}`},
		{name: "openai-wrapper", raw: `{"arguments":"{\"file_path\":\"README.md\"}"}`, want: `{"file_path":"README.md"}`},
		// 上游把 arguments 又包了一层字面引号（日志中观察到的形态）。
		{name: "openai-wrapper-quoted", raw: `{"arguments":"\"{\\\"file_path\\\":\\\"README.md\\\"}\""}`, want: `{"file_path":"README.md"}`},
		// 再深一层：整个 input 是 JSON 字符串，内容又是 {"arguments":"..."} 包装。
		{name: "string-wrapped-openai", raw: `"{\"arguments\":\"{\\\"file_path\\\":\\\"README.md\\\"}\"}"`, want: `{"file_path":"README.md"}`},
		// arguments 不是合法 JSON 时保持原样，不误伤本地工具。
		{name: "plain-arguments-kept", raw: `{"arguments":"just a plain string"}`, want: `{"arguments":"just a plain string"}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := normalizeStreamToolInput([]byte(tt.raw))
			if got != tt.want {
				t.Fatalf("normalizeStreamToolInput(%q) = %q, want %q", tt.raw, got, tt.want)
			}
		})
	}
}

func TestSplitLeadingHTTPStatus(t *testing.T) {
	tests := []struct{ in, code, rest string }{
		{in: "400 {\"type\":\"error\"}", code: "400", rest: `{"type":"error"}`},
		{in: "429 Too many requests", code: "429", rest: "Too many requests"},
		{in: "500", code: "500", rest: ""},
		{in: "403: forbidden", code: "403", rest: "forbidden"},
		{in: "12345", code: "", rest: "12345"},
		{in: "no status here", code: "", rest: "no status here"},
		{in: "12x short", code: "", rest: "12x short"},
		{in: "", code: "", rest: ""},
	}
	for _, tt := range tests {
		code, rest := splitLeadingHTTPStatus(tt.in)
		if code != tt.code || rest != tt.rest {
			t.Fatalf("splitLeadingHTTPStatus(%q) = (%q, %q), want (%q, %q)", tt.in, code, rest, tt.code, tt.rest)
		}
	}
}

func TestPuterStreamErrorNormalizesLeadingStatus(t *testing.T) {
	cases := []struct {
		chunk   string
		line    string
		wantSub string
	}{
		// puter 实际形态:message 以 "400" 开头,后接整段 JSON。
		{chunk: `400 {"type":"error","error":{"type":"invalid_request_error","message":"messages: at least one message is required"}}`,
			wantSub: "status=400"},
		// 无前导状态码:保持原样,不带 status。
		{chunk: `{"type":"error","error":{"type":"server_error","message":"boom"}}`,
			wantSub: "boom"},
		// chunk.Message 为空时退回整行。
		{chunk: "", line: `{"type":"error","error":"raw line"}`,
			wantSub: "raw line"},
	}
	for _, tc := range cases {
		err := puterStreamError(tc.chunk, tc.line)
		if err == nil || !strings.Contains(err.Error(), tc.wantSub) {
			t.Fatalf("puterStreamError(%q, %q) = %v, want containing %q", tc.chunk, tc.line, err, tc.wantSub)
		}
	}
	// 归一化后的错误必须能被上游分类器识别为不可重试的 client 错误,而不是 unknown。
	err := puterStreamError(`400 {"type":"error","error":{"type":"invalid_request_error","message":"bad request"}}`, "")
	if got := errors.ClassifyUpstreamError(err.Error()); got.Category != "client" || got.Retryable {
		t.Fatalf("ClassifyUpstreamError(%q) = %#v, want client/non-retryable", err.Error(), got)
	}
}
