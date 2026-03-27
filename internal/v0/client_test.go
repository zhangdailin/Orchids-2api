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

func TestBuildSendRequestBody_ForExistingChat(t *testing.T) {
	raw, referer, path, err := buildSendRequestBody("hi", "v0-auto", "-chat123", 3)
	if err != nil {
		t.Fatalf("buildSendRequestBody() error = %v", err)
	}
	if referer != "https://v0.app/chat/-chat123" {
		t.Fatalf("referer=%q want https://v0.app/chat/-chat123", referer)
	}
	if path != "/chat/-chat123" {
		t.Fatalf("path=%q want /chat/-chat123", path)
	}

	var payload map[string]any
	if err := json.Unmarshal(raw, &payload); err != nil {
		t.Fatalf("unmarshal payload: %v", err)
	}
	if payload["action"] != "SubmitNewUserMessage" {
		t.Fatalf("action=%v want SubmitNewUserMessage", payload["action"])
	}
	if payload["chat_id"] != "-chat123" {
		t.Fatalf("chat_id=%v want -chat123", payload["chat_id"])
	}
	if payload["generic_route"] != "/[id]" {
		t.Fatalf("generic_route=%v want /[id]", payload["generic_route"])
	}
	meta, _ := payload["meta"].(string)
	if !strings.Contains(meta, `"modelId":"v0-auto"`) {
		t.Fatalf("meta=%q missing modelId", meta)
	}
	if !strings.Contains(meta, `"index":3`) {
		t.Fatalf("meta=%q missing index", meta)
	}
}

func TestExtractSendResponseTextPrefersAssistantText(t *testing.T) {
	raw := []byte(`{"ok":true,"events":[{"role":"user","text":"hi"},{"role":"assistant","content":"hello from v0"}]}`)
	if got := extractSendResponseText(raw, "hi"); got != "hello from v0" {
		t.Fatalf("extractSendResponseText()=%q want hello from v0", got)
	}
}

func TestExtractSendResponseText_ParsesNestedJSONString(t *testing.T) {
	raw := []byte(`{"result":"{\"messages\":[{\"role\":\"assistant\",\"content\":\"hello from nested json\"}]}"}`)
	if got := extractSendResponseText(raw, "hi"); got != "hello from nested json" {
		t.Fatalf("extractSendResponseText()=%q want hello from nested json", got)
	}
}

func TestExtractSendResponseText_FallsBackToDataLine(t *testing.T) {
	raw := []byte("data: hello from data line\n\nevent: done\n")
	if got := extractSendResponseText(raw, "hi"); got != "hello from data line" {
		t.Fatalf("extractSendResponseText()=%q want hello from data line", got)
	}
}

func TestExtractSendResponseText_DoesNotTreatStatusWordAsReply(t *testing.T) {
	raw := []byte(`{"ok":true}`)
	if got := extractSendResponseText(raw, "hi"); got != "" {
		t.Fatalf("extractSendResponseText()=%q want empty", got)
	}
}

func TestExtractSendResponseChatID(t *testing.T) {
	raw := []byte(`{"referer":"https://v0.app/chat/-abc123","message":"ok"}`)
	if got := extractSendResponseChatID(raw); got != "-abc123" {
		t.Fatalf("extractSendResponseChatID()=%q want -abc123", got)
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
