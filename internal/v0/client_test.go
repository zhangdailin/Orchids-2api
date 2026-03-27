package v0

import (
	"strings"
	"testing"

	"github.com/goccy/go-json"

	"orchids-api/internal/prompt"
)

func TestExtractUserSessionFromCookieHeader(t *testing.T) {
	raw := "foo=bar; user_session=abc123; hello=world"
	if got := extractUserSession(raw); got != "abc123" {
		t.Fatalf("extractUserSession() = %q, want %q", got, "abc123")
	}
}

func TestNormalizeWebModelUsesV0Max(t *testing.T) {
	for _, model := range []string{"", "v0", "v0-max", "v0-1.5-md"} {
		if got := normalizeWebModel(model); got != "v0-max" {
			t.Fatalf("normalizeWebModel(%q) = %q, want %q", model, got, "v0-max")
		}
	}
}

func TestNormalizeWebModelPreservesKnownV0Variants(t *testing.T) {
	tests := map[string]string{
		"v0-auto":     "v0-auto",
		"v0 mini":     "v0-mini",
		"v0-pro":      "v0-pro",
		"v0 max fast": "v0-max-fast",
		"v0-custom":   "v0-custom",
	}
	for in, want := range tests {
		if got := normalizeWebModel(in); got != want {
			t.Fatalf("normalizeWebModel(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestBuildSeedModelChoicesReturnsAllKnownV0Models(t *testing.T) {
	got := buildSeedModelChoices()
	want := []string{"v0-auto", "v0-mini", "v0-pro", "v0-max", "v0-max-fast"}
	if len(got) != len(want) {
		t.Fatalf("len(buildSeedModelChoices()) = %d, want %d", len(got), len(want))
	}
	for i, modelID := range want {
		if got[i].ID != modelID {
			t.Fatalf("buildSeedModelChoices()[%d].ID = %q, want %q", i, got[i].ID, modelID)
		}
		if got[i].Name == "" {
			t.Fatalf("buildSeedModelChoices()[%d].Name is empty", i)
		}
	}
}

func TestBuildChatRequestBodyForNewChat(t *testing.T) {
	raw, referer, err := buildChatRequestBody("你好", "v0-max", "eex3OukD0X7", "team-a", "msg-1", "", true)
	if err != nil {
		t.Fatalf("buildChatRequestBody() error = %v", err)
	}
	if referer != "https://v0.app/chat/eex3OukD0X7" {
		t.Fatalf("referer=%q want https://v0.app/chat/eex3OukD0X7", referer)
	}

	var payload map[string]any
	if err := json.Unmarshal(raw, &payload); err != nil {
		t.Fatalf("unmarshal payload: %v", err)
	}
	if payload["chatId"] != "eex3OukD0X7" {
		t.Fatalf("chatId=%v want eex3OukD0X7", payload["chatId"])
	}
	if payload["team"] != "team-a" {
		t.Fatalf("team=%v want team-a", payload["team"])
	}
	if payload["isNew"] != true {
		t.Fatalf("isNew=%v want true", payload["isNew"])
	}
	if _, ok := payload["parentId"]; ok {
		t.Fatalf("parentId should be omitted for new chat")
	}
}

func TestBuildChatRequestBodyForExistingChatIncludesParent(t *testing.T) {
	raw, _, err := buildChatRequestBody("hi", "v0-auto", "chat123", "team-a", "msg-1", "parent-1", false)
	if err != nil {
		t.Fatalf("buildChatRequestBody() error = %v", err)
	}

	var payload map[string]any
	if err := json.Unmarshal(raw, &payload); err != nil {
		t.Fatalf("unmarshal payload: %v", err)
	}
	if payload["parentId"] != "parent-1" {
		t.Fatalf("parentId=%v want parent-1", payload["parentId"])
	}
	modelConfig, _ := payload["modelConfiguration"].(map[string]any)
	if modelConfig["modelId"] != "v0-auto" {
		t.Fatalf("modelConfiguration.modelId=%v want v0-auto", modelConfig["modelId"])
	}
}

func TestExtractAssistantReplyFromLatest(t *testing.T) {
	latest := &v0LatestResponse{
		OK: true,
		Value: v0LatestValue{
			ResumeUserMessageMap: map[string]v0ResumeUserMessage{
				"user-msg": {ResponseMessageID: "assistant-msg"},
			},
			NewMessages: []v0ChatMessage{
				{ID: "user-msg", Role: "user"},
				{
					ID:   "assistant-msg",
					Role: "assistant",
					Content: v0MessageContent{
						Type: "message-binary-format",
						Value: []interface{}{
							[]interface{}{
								float64(0),
								[]interface{}{
									[]interface{}{"AssistantMessageContentPart", map[string]interface{}{"part": map[string]interface{}{"type": "task-thinking-v1"}}},
									[]interface{}{"p", map[string]interface{}{}, []interface{}{"text", map[string]interface{}{}, "你好！我是 "}, []interface{}{"strong", map[string]interface{}{}, []interface{}{"text", map[string]interface{}{}, "v0"}}},
									[]interface{}{"p", map[string]interface{}{}, []interface{}{"text", map[string]interface{}{}, "我可以帮助你构建页面。"}},
								},
							},
						},
					},
				},
			},
		},
	}

	reply, assistantID := extractAssistantReplyFromLatest(latest, "user-msg")
	if assistantID != "assistant-msg" {
		t.Fatalf("assistantID=%q want assistant-msg", assistantID)
	}
	want := "你好！我是 v0\n\n我可以帮助你构建页面。"
	if reply != want {
		t.Fatalf("reply=%q want %q", reply, want)
	}
}

func TestRenderV0MessageContentIgnoresNonMessageBinaryFormat(t *testing.T) {
	got := renderV0MessageContent(v0MessageContent{Type: "plain", Value: "hello"})
	if got != "" {
		t.Fatalf("renderV0MessageContent()=%q want empty", got)
	}
}

func TestNormalizeChatIDRemovesLeadingDash(t *testing.T) {
	if got := normalizeChatID("-abc123"); got != "abc123" {
		t.Fatalf("normalizeChatID()=%q want abc123", got)
	}
}

func TestEstimateMessageIndexCountsUserTurns(t *testing.T) {
	messages := []prompt.Message{
		{Role: "user", Content: prompt.MessageContent{Text: "hello"}},
		{Role: "assistant", Content: prompt.MessageContent{Text: "world"}},
		{Role: "user", Content: prompt.MessageContent{Text: "again"}},
	}
	if got := estimateMessageIndex(messages); got != 3 {
		t.Fatalf("estimateMessageIndex()=%d want 3", got)
	}
}

func TestCleanupRenderedV0TextCollapsesBlankLines(t *testing.T) {
	got := cleanupRenderedV0Text([]string{"hello", "\n\n", "\n", "world", "\n\n"})
	if got != "hello\n\nworld" {
		t.Fatalf("cleanupRenderedV0Text()=%q want hello\\n\\nworld", got)
	}
}

func TestGenerateRandomIDLength(t *testing.T) {
	got, err := generateRandomID(16)
	if err != nil {
		t.Fatalf("generateRandomID() error = %v", err)
	}
	if len(got) != 16 {
		t.Fatalf("len(generateRandomID())=%d want 16", len(got))
	}
	if strings.TrimSpace(got) == "" {
		t.Fatalf("generateRandomID() returned empty string")
	}
}
