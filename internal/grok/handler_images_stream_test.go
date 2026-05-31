package grok

import (
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
)

func TestStreamImageGeneration_ParseErrorUsesSSEErrorEvent(t *testing.T) {
	h := &Handler{}
	rec := httptest.NewRecorder()

	h.streamImageGeneration(rec, strings.NewReader(`{"result":{"response":{"token":"bad"}}`), "", "test prompt", "url", 1, "")

	raw := rec.Body.String()
	if !strings.Contains(raw, "event: error") {
		t.Fatalf("expected SSE error event, raw=%q", raw)
	}
	if !strings.Contains(raw, `"code":"stream_error"`) {
		t.Fatalf("expected stream_error code, raw=%q", raw)
	}
	if !strings.Contains(raw, "[DONE]") {
		t.Fatalf("expected DONE after stream error, raw=%q", raw)
	}
}

func TestStreamImageGeneration_SuccessEndsWithDone(t *testing.T) {
	h := &Handler{client: New(nil)}
	rec := httptest.NewRecorder()

	h.streamImageGeneration(rec, strings.NewReader(`{"result":{"response":{"modelResponse":{"generatedImageUrls":["https://assets.grok.com/users/u-1/generated/a1/image.png"]}}}}`), "", "test prompt", "url", 1, "")

	raw := rec.Body.String()
	if !strings.Contains(raw, "image_generation.completed") {
		t.Fatalf("expected completed image event, raw=%q", raw)
	}
	if !strings.Contains(raw, "[DONE]") {
		t.Fatalf("expected DONE after success, raw=%q", raw)
	}
}

func TestStreamImageGeneration_NoImageUsesSSEErrorEvent(t *testing.T) {
	h := &Handler{client: New(nil)}
	rec := httptest.NewRecorder()

	h.streamImageGeneration(rec, strings.NewReader(`{"result":{"response":{"token":"still working"}}}`), "", "test prompt", "url", 1, "")

	raw := rec.Body.String()
	if !strings.Contains(raw, "event: error") {
		t.Fatalf("expected SSE error event, raw=%q", raw)
	}
	if !strings.Contains(raw, `"code":"no_image_generated"`) {
		t.Fatalf("expected no_image_generated code, raw=%q", raw)
	}
	if !strings.Contains(raw, "[DONE]") {
		t.Fatalf("expected DONE after no image error, raw=%q", raw)
	}
}

func TestStreamImageGeneration_AcceptsAlternateProgressShape(t *testing.T) {
	h := &Handler{client: New(nil)}
	rec := httptest.NewRecorder()

	h.streamImageGeneration(rec, strings.NewReader(
		`{"result":{"response":{"streaming_image_generation_response":{"image_index":1,"percentage":55}}}}`+
			`{"result":{"response":{"model_response":{"generatedImageUrls":["https://assets.grok.com/users/u-1/generated/a1/image.png"]}}}}`,
	), "", "alt prompt", "url", 2, "")

	raw := rec.Body.String()
	if !strings.Contains(raw, `"progress":55`) {
		t.Fatalf("expected alternate progress shape to be parsed, raw=%q", raw)
	}
	if !strings.Contains(raw, `"prompt_tokens":`) {
		t.Fatalf("expected estimated usage to be included, raw=%q", raw)
	}
}

func TestAppendImageResultURLs_AcceptsCardAttachmentImageChunk(t *testing.T) {
	resp := map[string]interface{}{
		"modelResponse": map[string]interface{}{
			"cardAttachment": map[string]interface{}{
				"jsonData": `{"image_chunk":{"imageUrl":"/users/u-1/generated/a1/image.jpg","progress":100}}`,
			},
		},
	}

	urls := appendImageResultURLs(nil, resp)
	if len(urls) != 1 {
		t.Fatalf("urls len=%d want 1: %#v", len(urls), urls)
	}
	if want := "https://assets.grok.com/users/u-1/generated/a1/image.jpg"; urls[0] != want {
		t.Fatalf("url=%q want %q", urls[0], want)
	}
}

func TestAppendImageResultURLs_AcceptsUserResponseCardAttachmentsJSON(t *testing.T) {
	resp := map[string]interface{}{
		"userResponse": map[string]interface{}{
			"cardAttachmentsJson": `["{\"jsonData\":\"{\\\"image_chunk\\\":{\\\"imageUrl\\\":\\\"users/u-1/generated/a2/image.png\\\",\\\"progress\\\":100}}\"}"]`,
		},
	}

	urls := appendImageResultURLs(nil, resp)
	if len(urls) != 1 {
		t.Fatalf("urls len=%d want 1: %#v", len(urls), urls)
	}
	if want := "https://assets.grok.com/users/u-1/generated/a2/image.png"; urls[0] != want {
		t.Fatalf("url=%q want %q", urls[0], want)
	}
}

func TestExtractAppChatImageURLs_AcceptsImageChunkOnly(t *testing.T) {
	resp := map[string]interface{}{
		"userResponse": map[string]interface{}{
			"cardAttachment": map[string]interface{}{
				"jsonData": `{"image_chunk":{"imageUrl":"/generated/apple.png","progress":100}}`,
			},
			"fileAttachments": []interface{}{"not-image-generation"},
		},
	}

	got := extractAppChatImageURLs(resp)
	if len(got) != 1 {
		t.Fatalf("len=%d want 1: %#v", len(got), got)
	}
	if got[0] != "https://assets.grok.com/generated/apple.png" {
		t.Fatalf("url=%q want generated apple", got[0])
	}
}

func TestExtractAppChatImageURLs_IgnoresPlainFileAttachments(t *testing.T) {
	resp := map[string]interface{}{
		"userResponse": map[string]interface{}{
			"fileAttachments": []interface{}{"svg-from-text-response"},
		},
	}

	if got := extractAppChatImageURLs(resp); len(got) != 0 {
		t.Fatalf("got=%#v want no image urls", got)
	}
}

func TestPrepareAppChatImageGenerationPayload_MatchesLiteImageShape(t *testing.T) {
	payload := map[string]interface{}{
		"responseMetadata": map[string]interface{}{
			"requestModelDetails": map[string]interface{}{"modelId": "grok-imagine-image-lite"},
		},
		"toolOverrides": map[string]interface{}{"webSearch": true},
	}

	prepareAppChatImageGenerationPayload(payload, 1)

	if !reflect.DeepEqual(payload["responseMetadata"], map[string]interface{}{}) {
		t.Fatalf("responseMetadata=%#v want empty object", payload["responseMetadata"])
	}
	if got, _ := payload["imageGenerationCount"].(int); got != 1 {
		t.Fatalf("imageGenerationCount=%d want 1", got)
	}
	if got, _ := payload["disableMemory"].(bool); !got {
		t.Fatalf("disableMemory=%v want true", got)
	}
	for _, key := range []string{"modelName", "modelMode", "isReasoning"} {
		if _, ok := payload[key]; ok {
			t.Fatalf("%s should be removed for app-chat image payload", key)
		}
	}
	toolOverrides := payload["toolOverrides"].(map[string]interface{})
	if got, _ := toolOverrides["webSearch"].(bool); got {
		t.Fatalf("webSearch=%v want false", got)
	}
}

func TestEnsureImageConfig_UsesTopLevelModelOverride(t *testing.T) {
	payload := map[string]interface{}{}
	nsfw := true

	ensureImageAspectRatio(payload, "grok-imagine-image-lite", "3:2")
	ensureImageNSFW(payload, "grok-imagine-image-lite", &nsfw)

	override, ok := payload["modelConfigOverride"].(map[string]interface{})
	if !ok {
		t.Fatalf("modelConfigOverride missing: %#v", payload)
	}
	modelMap, ok := override["modelMap"].(map[string]interface{})
	if !ok {
		t.Fatalf("modelMap missing: %#v", override)
	}
	if got := modelMap["imageGenModel"]; got != "grok-imagine-image-lite" {
		t.Fatalf("imageGenModel=%v want grok-imagine-image-lite", got)
	}
	cfg, ok := modelMap["imageGenModelConfig"].(map[string]interface{})
	if !ok {
		t.Fatalf("imageGenModelConfig missing: %#v", modelMap)
	}
	if got := cfg["aspectRatio"]; got != "3:2" {
		t.Fatalf("aspectRatio=%v want 3:2", got)
	}
	if got := cfg["enableNsfw"]; got != true {
		t.Fatalf("enableNsfw=%v want true", got)
	}
	if _, ok := payload["responseMetadata"]; ok {
		t.Fatalf("responseMetadata should not be created for image config: %#v", payload["responseMetadata"])
	}
}

func TestGrokAppChatImagePrompt_PrefersDrawTrigger(t *testing.T) {
	if got := grokAppChatImagePrompt("a red apple"); got != "Draw a red apple" {
		t.Fatalf("prompt=%q", got)
	}
	if got := grokAppChatImagePrompt("Draw a red apple"); got != "Draw a red apple" {
		t.Fatalf("prompt=%q", got)
	}
}

func TestGrokAppChatImagePrompts_AddsSafePortraitFallbackForShortChinesePrompt(t *testing.T) {
	got := grokAppChatImagePrompts("美女图片")
	if len(got) != 2 {
		t.Fatalf("len=%d want 2: %#v", len(got), got)
	}
	if got[0] != "Draw 美女图片" {
		t.Fatalf("first=%q", got[0])
	}
	if !strings.Contains(got[1], "safe-for-work portrait photo of an adult woman") {
		t.Fatalf("fallback=%q", got[1])
	}
}
