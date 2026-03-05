package grok

import (
	"strings"
	"testing"
	"time"
)

func TestNormalizeSSOToken(t *testing.T) {
	tests := []struct {
		in   string
		want string
	}{
		{in: "abc", want: "abc"},
		{in: "sso=abc123", want: "abc123"},
		{in: "foo=1; sso=abc123; bar=2", want: "abc123"},
		{in: "notsso=abc123", want: "notsso=abc123"},
	}
	for _, tt := range tests {
		got := NormalizeSSOToken(tt.in)
		if got != tt.want {
			t.Fatalf("NormalizeSSOToken(%q)=%q want=%q", tt.in, got, tt.want)
		}
	}
}

func TestParseDataURI(t *testing.T) {
	name, content, mime, err := parseDataURI("data:image/png;base64,QUJD")
	if err != nil {
		t.Fatalf("parseDataURI error: %v", err)
	}
	if name != "file.png" {
		t.Fatalf("name=%q want=file.png", name)
	}
	if content != "QUJD" {
		t.Fatalf("content=%q want=QUJD", content)
	}
	if mime != "image/png" {
		t.Fatalf("mime=%q want=image/png", mime)
	}
}

func TestExtractMessageAndAttachments(t *testing.T) {
	messages := []ChatMessage{
		{
			Role: "user",
			Content: []interface{}{
				map[string]interface{}{"type": "text", "text": "hello"},
				map[string]interface{}{"type": "image_url", "image_url": map[string]interface{}{"url": "https://a/b.png"}},
			},
		},
	}

	text, attachments, err := extractMessageAndAttachments(messages, false)
	if err != nil {
		t.Fatalf("extractMessageAndAttachments error: %v", err)
	}
	if text != "hello" {
		t.Fatalf("text=%q want=hello", text)
	}
	if len(attachments) != 1 {
		t.Fatalf("attachments=%d want=1", len(attachments))
	}
	if attachments[0].Data != "https://a/b.png" {
		t.Fatalf("attachment=%q want=https://a/b.png", attachments[0].Data)
	}
}

func TestResolveAspectRatio(t *testing.T) {
	if got := resolveAspectRatio("1024x1024"); got != "1:1" {
		t.Fatalf("resolveAspectRatio(1024x1024)=%q want=1:1", got)
	}
	if got := resolveAspectRatio("unknown"); got != "2:3" {
		t.Fatalf("resolveAspectRatio(unknown)=%q want=2:3", got)
	}
}

func TestExtractLastUserText(t *testing.T) {
	messages := []ChatMessage{
		{Role: "system", Content: "sys"},
		{Role: "user", Content: "第一轮问题"},
		{Role: "assistant", Content: "第一轮回答"},
		{
			Role: "user",
			Content: []interface{}{
				map[string]interface{}{"type": "text", "text": "不要图片"},
				map[string]interface{}{"type": "text", "text": "只回答文字"},
				map[string]interface{}{"type": "image_url", "image_url": map[string]interface{}{"url": "https://a/b.png"}},
			},
		},
	}

	got := extractLastUserText(messages)
	want := "不要图片\n只回答文字"
	if got != want {
		t.Fatalf("extractLastUserText()=%q want=%q", got, want)
	}
}

func TestParseRateLimitPayload_AcceptsQueriesFields(t *testing.T) {
	payload := map[string]interface{}{
		"maxQueries":       140,
		"remainingQueries": 23,
	}
	info := parseRateLimitPayload(payload)
	if info == nil {
		t.Fatalf("parseRateLimitPayload returned nil")
	}
	if info.Limit != 140 {
		t.Fatalf("limit=%d want=140", info.Limit)
	}
	if info.Remaining != 23 {
		t.Fatalf("remaining=%d want=23", info.Remaining)
	}
}

func TestParseRateLimitPayload_SkipsNonNumericMatchedField(t *testing.T) {
	payload := map[string]interface{}{
		"quota": map[string]interface{}{
			"kind": "daily",
		},
		"limits": map[string]interface{}{
			"maxQueries":       140,
			"remainingQueries": 23,
		},
	}

	for i := 0; i < 200; i++ {
		info := parseRateLimitPayload(payload)
		if info == nil {
			t.Fatalf("parseRateLimitPayload returned nil at iter=%d", i)
		}
		if info.Limit != 140 {
			t.Fatalf("limit=%d want=140 iter=%d", info.Limit, i)
		}
		if info.Remaining != 23 {
			t.Fatalf("remaining=%d want=23 iter=%d", info.Remaining, i)
		}
	}
}

func TestParseRateLimitValue_ComplexFormats(t *testing.T) {
	tests := []struct {
		in   string
		want int64
	}{
		{in: "100;w=3600", want: 100},
		{in: "23/50", want: 23},
		{in: "remaining=42", want: 42},
		{in: "  7.9 requests", want: 7},
	}

	for _, tt := range tests {
		got, ok := parseRateLimitValue(tt.in)
		if !ok {
			t.Fatalf("parseRateLimitValue(%q) not parsed", tt.in)
		}
		if got != tt.want {
			t.Fatalf("parseRateLimitValue(%q)=%d want=%d", tt.in, got, tt.want)
		}
	}
}

func TestParseRateLimitReset_RFC3339(t *testing.T) {
	raw := "2026-03-05T19:00:00Z"
	got := parseRateLimitReset(raw)
	want, _ := time.Parse(time.RFC3339, raw)
	if !got.Equal(want) {
		t.Fatalf("parseRateLimitReset(%q)=%v want=%v", raw, got, want)
	}
}

func TestStripToolAndRenderMarkup_ExtractsToolCardText(t *testing.T) {
	in := strings.Join([]string{
		`<xai:tool_usage_card><xai:tool_name>web_search</xai:tool_name><xai:tool_args>{"query":"特朗普头像"}</xai:tool_args></xai:tool_usage_card>`,
		`<grok:render card_id="x">ignore</grok:render>`,
		`结论`,
	}, "\n")
	out := stripToolAndRenderMarkup(in)
	if !strings.Contains(out, "[WebSearch] 特朗普头像") {
		t.Fatalf("tool card text missing, got=%q", out)
	}
	if strings.Contains(strings.ToLower(out), "grok:render") {
		t.Fatalf("render tag should be removed, got=%q", out)
	}
	if !strings.Contains(out, "结论") {
		t.Fatalf("final content missing, got=%q", out)
	}
}

func BenchmarkExtractToolUsageCardText(b *testing.B) {
	raw := `<xai:tool_usage_card><xai:tool_name>web_search</xai:tool_name><xai:tool_args>{"query":"特朗普头像","q":"特朗普头像"}</xai:tool_args></xai:tool_usage_card>`
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_ = extractToolUsageCardText(raw)
	}
}

func BenchmarkStripToolAndRenderMarkup(b *testing.B) {
	in := strings.Join([]string{
		`<xai:tool_usage_card><xai:tool_name>web_search</xai:tool_name><xai:tool_args>{"query":"特朗普头像"}</xai:tool_args></xai:tool_usage_card>`,
		`<xai:tool_usage_card><xai:tool_name>chatroom_send</xai:tool_name><xai:tool_args><![CDATA[{"message":"分析结果"}]]></xai:tool_args></xai:tool_usage_card>`,
		`<grok:render card_id="x">ignore</grok:render>`,
		`结论`,
	}, "\n")
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_ = stripToolAndRenderMarkup(in)
	}
}
