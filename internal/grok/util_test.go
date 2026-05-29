package grok

import (
	"errors"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"orchids-api/internal/store"
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

	text, attachments, err := extractMessageAndAttachmentsWithTools(messages, false, nil, nil, true)
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

func TestValidateChatMessages_AcceptsCaseInsensitiveRoleAndType(t *testing.T) {
	messages := []ChatMessage{
		{
			Role: "User",
			Content: []interface{}{
				map[string]interface{}{"type": "Text", "text": "hello"},
				map[string]interface{}{"type": "Image_URL", "image_url": map[string]interface{}{"url": "https://a/b.png"}},
			},
		},
		{
			Role: "ASSISTANT",
			Content: []interface{}{
				map[string]interface{}{"type": "TEXT", "text": "ok"},
			},
		},
	}

	if err := validateChatMessages(messages); err != nil {
		t.Fatalf("validateChatMessages() error = %v", err)
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

func TestParseUpstreamLines_CollectAndSkip(t *testing.T) {
	raw := strings.Join([]string{
		`{"result":{"response":{"token":"hello"}}}`,
		`{"result":{"other":1}}`,
		`{"other":2}`,
		`{"result":{"response":{"token":"world"}}}`,
	}, "")

	got := make([]string, 0, 2)
	err := parseUpstreamLines(strings.NewReader(raw), func(line map[string]interface{}) error {
		token, _ := line["token"].(string)
		got = append(got, token)
		return nil
	})
	if err != nil {
		t.Fatalf("parseUpstreamLines() error: %v", err)
	}
	if len(got) != 2 || got[0] != "hello" || got[1] != "world" {
		t.Fatalf("unexpected tokens: %v", got)
	}
}

func TestParseUpstreamLines_CallbackError(t *testing.T) {
	expectErr := errors.New("stop")
	raw := `{"result":{"response":{"token":"hello"}}}`
	err := parseUpstreamLines(strings.NewReader(raw), func(line map[string]interface{}) error {
		return expectErr
	})
	if !errors.Is(err, expectErr) {
		t.Fatalf("parseUpstreamLines error=%v want=%v", err, expectErr)
	}
}

func TestParseUpstreamLines_InvalidJSON(t *testing.T) {
	raw := `{"result":{"response":{"token":"hello"}}`
	err := parseUpstreamLines(strings.NewReader(raw), func(line map[string]interface{}) error {
		return nil
	})
	if err == nil {
		t.Fatalf("expected invalid json error")
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
	if !info.HasLimit || !info.HasRemaining {
		t.Fatalf("expected complete quota pair, got %#v", info)
	}
	if info.Unit != "requests" {
		t.Fatalf("unit=%q want=requests", info.Unit)
	}
}

func TestApplyQuotaInfo_InfersLiteSubscription(t *testing.T) {
	acc := &store.Account{Subscription: "basic"}
	changed := ApplyQuotaInfo(acc, &RateLimitInfo{
		Limit:        70,
		HasLimit:     true,
		Remaining:    63,
		HasRemaining: true,
		Unit:         "requests",
	})
	if !changed {
		t.Fatal("ApplyQuotaInfo changed=false, want true")
	}
	if acc.Subscription != "lite" {
		t.Fatalf("subscription=%q want lite", acc.Subscription)
	}
	if acc.UsageLimit != 70 || acc.UsageCurrent != 63 {
		t.Fatalf("unexpected quota: limit=%v current=%v", acc.UsageLimit, acc.UsageCurrent)
	}
}

func TestApplyQuotaInfo_InfersBasicFromFreeAutoWindow(t *testing.T) {
	acc := &store.Account{}
	changed := ApplyQuotaInfo(acc, &RateLimitInfo{
		Limit:        7,
		HasLimit:     true,
		Remaining:    7,
		HasRemaining: true,
		Unit:         "requests",
	})
	if !changed {
		t.Fatal("ApplyQuotaInfo changed=false, want true")
	}
	if acc.Subscription != "basic" {
		t.Fatalf("subscription=%q want basic", acc.Subscription)
	}
	if acc.UsageLimit != 7 || acc.UsageCurrent != 7 {
		t.Fatalf("unexpected quota: limit=%v current=%v", acc.UsageLimit, acc.UsageCurrent)
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

func TestParseRateLimitPayload_CollectsNestedResetAndNumbers(t *testing.T) {
	rawReset := "2026-03-05T19:00:00Z"
	wantReset, _ := time.Parse(time.RFC3339, rawReset)
	payload := map[string]interface{}{
		"quota_limit": "not-a-number",
		"meta": map[string]interface{}{
			"limits": map[string]interface{}{
				"maxQueries":       140,
				"remainingQueries": "23",
			},
			"resetAt": rawReset,
		},
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
	if !info.ResetAt.Equal(wantReset) {
		t.Fatalf("reset=%v want=%v", info.ResetAt, wantReset)
	}
}

func TestParseRateLimitPayload_PrefersTokenPairOverRequestPair(t *testing.T) {
	payload := map[string]interface{}{
		"limits": map[string]interface{}{
			"maxTokens":        500000,
			"remainingTokens":  499000,
			"maxQueries":       140,
			"remainingQueries": 23,
		},
	}

	info := parseRateLimitPayload(payload)
	if info == nil {
		t.Fatalf("parseRateLimitPayload returned nil")
	}
	if info.Limit != 500000 || info.Remaining != 499000 {
		t.Fatalf("unexpected info: %#v", info)
	}
	if info.Unit != "tokens" {
		t.Fatalf("unit=%q want=tokens", info.Unit)
	}
}

func TestParseRateLimitPayload_PrefersTokensWhenQueriesAreZero(t *testing.T) {
	payload := map[string]interface{}{
		"windowSizeSeconds": 72000,
		"remainingQueries":  0,
		"totalQueries":      0,
		"remainingTokens":   80,
		"totalTokens":       80,
		"lowEffortRateLimits": map[string]interface{}{
			"cost":             1,
			"remainingQueries": 80,
		},
	}

	info := parseRateLimitPayload(payload)
	if info == nil {
		t.Fatalf("parseRateLimitPayload returned nil")
	}
	if info.Limit != 80 || info.Remaining != 80 {
		t.Fatalf("unexpected info: %#v", info)
	}
	if info.Unit != "tokens" {
		t.Fatalf("unit=%q want=tokens", info.Unit)
	}
}

func TestParseRateLimitPayload_IncompletePairKeepsPresenceFlags(t *testing.T) {
	payload := map[string]interface{}{
		"maxQueries": 80,
	}

	info := parseRateLimitPayload(payload)
	if info == nil {
		t.Fatalf("parseRateLimitPayload returned nil")
	}
	if !info.HasLimit || info.HasRemaining {
		t.Fatalf("expected incomplete info with limit only, got %#v", info)
	}
	if info.Limit != 80 || info.Remaining != 0 {
		t.Fatalf("unexpected values: %#v", info)
	}
	if info.Unit != "requests" {
		t.Fatalf("unit=%q want=requests", info.Unit)
	}
}

func TestParseRateLimitPayload_SkipsInvalidResetAndFindsNested(t *testing.T) {
	const resetMS int64 = 1730000000000
	wantReset := time.UnixMilli(resetMS)
	payload := map[string]interface{}{
		"reset": "invalid-time",
		"meta": map[string]interface{}{
			"reset_at_ms": resetMS,
		},
	}

	info := parseRateLimitPayload(payload)
	if info == nil {
		t.Fatalf("parseRateLimitPayload returned nil")
	}
	if !info.ResetAt.Equal(wantReset) {
		t.Fatalf("reset=%v want=%v", info.ResetAt, wantReset)
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

func TestExtractToolUsageCardText_PrefixesRolloutID(t *testing.T) {
	raw := `<rolloutId>abc123</rolloutId><xai:tool_usage_card><xai:tool_name>web_search</xai:tool_name><xai:tool_args>{"query":"hello"}</xai:tool_args></xai:tool_usage_card>`
	got := extractToolUsageCardText(raw)
	if got != "[abc123][WebSearch] hello" {
		t.Fatalf("extractToolUsageCardText()=%q want=%q", got, "[abc123][WebSearch] hello")
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

func BenchmarkParseRateLimitPayload_Nested(b *testing.B) {
	payload := map[string]interface{}{
		"quota": map[string]interface{}{
			"kind": "daily",
		},
		"limits": map[string]interface{}{
			"maxQueries":       140,
			"remainingQueries": 23,
			"resetAt":          "2026-03-05T19:00:00Z",
		},
		"meta": []interface{}{
			map[string]interface{}{"k": "v"},
			map[string]interface{}{"unused": 1},
		},
	}
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_ = parseRateLimitPayload(payload)
	}
}

func BenchmarkParseUpstreamLines(b *testing.B) {
	raw := strings.Join([]string{
		`{"result":{"response":{"token":"hello","progress":10}}}`,
		`{"result":{"response":{"token":"world","progress":90}}}`,
		`{"result":{"other":1}}`,
		`{"other":2}`,
	}, "")
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		if err := parseUpstreamLines(strings.NewReader(raw), func(line map[string]interface{}) error {
			_, _ = line["token"].(string)
			return nil
		}); err != nil {
			b.Fatalf("parseUpstreamLines error: %v", err)
		}
	}
}

func BenchmarkParseRateLimitValue_CompoundHeader(b *testing.B) {
	raw := "100;w=3600"
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_, _ = parseRateLimitValue(raw)
	}
}

func TestEncodeJSONBytesDoesNotEscapeHTML(t *testing.T) {
	payload := map[string]interface{}{
		"type": "chunk",
		"data": map[string]interface{}{
			"text": "hello <world>",
			"n":    1,
		},
	}
	got := string(encodeJSONBytes(payload))
	if !strings.Contains(got, "hello <world>") {
		t.Fatalf("got=%q want unescaped html", got)
	}
}

func TestWriteSSEBytesWritesEventFrame(t *testing.T) {
	bytesRec := httptest.NewRecorder()
	writeSSEBytes(bytesRec, "demo", []byte(`{"ok":true}`))

	got := bytesRec.Body.String()
	if !strings.Contains(got, "event: demo\n") || !strings.Contains(got, `data: {"ok":true}`) {
		t.Fatalf("unexpected sse frame: %q", got)
	}
}

func BenchmarkEncodeJSON_Bytes(b *testing.B) {
	payload := map[string]interface{}{
		"id": "msg_1",
		"choices": []map[string]interface{}{{
			"index": 0,
			"delta": map[string]interface{}{"content": "hello world"},
		}},
	}
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_ = encodeJSONBytes(payload)
	}
}

func BenchmarkWriteSSE_Bytes(b *testing.B) {
	writer := httptest.NewRecorder()
	data := []byte(`{"ok":true}`)
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		writer.Body.Reset()
		writeSSEBytes(writer, "demo", data)
	}
}
