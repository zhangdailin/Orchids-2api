package grok

import (
	"bytes"
	"context"
	"github.com/goccy/go-json"
	"image"
	"image/color"
	"image/png"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"orchids-api/internal/config"
)

func testPNGBytes(t *testing.T) []byte {
	t.Helper()
	var buf bytes.Buffer
	img := image.NewRGBA(image.Rect(0, 0, 2, 2))
	img.Set(0, 0, color.RGBA{R: 255, A: 255})
	if err := png.Encode(&buf, img); err != nil {
		t.Fatalf("encode png: %v", err)
	}
	return buf.Bytes()
}

func testGrokImageHandler(t *testing.T) *Handler {
	t.Helper()
	raw := testPNGBytes(t)
	return &Handler{
		client: &Client{
			assetClient: &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
				return &http.Response{
					StatusCode: http.StatusOK,
					Header:     http.Header{"Content-Type": []string{"image/png"}},
					Body:       io.NopCloser(bytes.NewReader(raw)),
					Request:    req,
				}, nil
			})},
		},
	}
}

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
	oldBase := cacheBaseDir
	cacheBaseDir = t.TempDir()
	t.Cleanup(func() { cacheBaseDir = oldBase })

	h := testGrokImageHandler(t)
	rec := httptest.NewRecorder()

	h.streamImageGeneration(rec, strings.NewReader(`{"result":{"response":{"modelResponse":{"generatedImageUrls":["https://assets.grok.com/users/u-1/generated/a1/image.png"]}}}}`), "", "test prompt", "url", 1, "")

	raw := rec.Body.String()
	if !strings.Contains(raw, "image_generation.completed") {
		t.Fatalf("expected completed image event, raw=%q", raw)
	}
	if !strings.Contains(raw, "/grok/v1/files/image/") {
		t.Fatalf("expected local cached image url, raw=%q", raw)
	}
	if !strings.Contains(raw, "[DONE]") {
		t.Fatalf("expected DONE after success, raw=%q", raw)
	}
}

func TestImageOutputValue_CachesGrokAssetURL(t *testing.T) {
	oldBase := cacheBaseDir
	cacheBaseDir = t.TempDir()
	t.Cleanup(func() { cacheBaseDir = oldBase })

	h := testGrokImageHandler(t)

	got, err := h.imageOutputValue(context.Background(), "token", "https://assets.grok.com/users/u/generated/a/image.png", "url")
	if err != nil {
		t.Fatalf("imageOutputValue error: %v", err)
	}
	if !strings.HasPrefix(got, "/grok/v1/files/image/") {
		t.Fatalf("url=%q want local file url", got)
	}
	_, fileName, ok := parseFilesPath(got)
	if !ok {
		t.Fatalf("parse local file url failed: %q", got)
	}
	if _, err := os.Stat(filepath.Join(cacheBaseDir, "image", fileName)); err != nil {
		t.Fatalf("cached file missing: %v", err)
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
	oldBase := cacheBaseDir
	cacheBaseDir = t.TempDir()
	t.Cleanup(func() { cacheBaseDir = oldBase })

	h := testGrokImageHandler(t)
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

func TestAppChatImagePayload_MatchesGrokImagineAgentShape(t *testing.T) {
	c := New(nil)
	spec := ModelSpec{
		ID:            "grok-imagine-image-lite",
		UpstreamModel: "grok-imagine-image-lite",
		ModelMode:     "MODEL_MODE_FAST",
		Tier:          grokTierBasic,
		IsImage:       true,
	}

	payload := c.appChatImagePayload(spec, "apple", "1024x1792", 1)

	for _, key := range []string{
		"preferBest",
		"modelConfigOverride",
		"parentResponseId",
		"skipCancelCurrentInflightRequests",
	} {
		if _, ok := payload[key]; ok {
			t.Fatalf("%s should not be present: %#v", key, payload)
		}
	}
	if got, _ := payload["modelName"].(string); got != "grok-imagine-image-lite" {
		t.Fatalf("modelName=%q want grok-imagine-image-lite", got)
	}
	if got, _ := payload["modelMode"].(string); got != "MODEL_MODE_FAST" {
		t.Fatalf("modelMode=%q want MODEL_MODE_FAST", got)
	}
	if got, _ := payload["modelTier"].(string); got != "basic" {
		t.Fatalf("modelTier=%q want basic", got)
	}
	if got, _ := payload["modeId"].(string); got != "fast" {
		t.Fatalf("modeId=%q want fast", got)
	}
	if got, _ := payload["message"].(string); got != "Drawing: apple" {
		t.Fatalf("message=%q want Drawing: apple", got)
	}
	if got, _ := payload["enableImageGeneration"].(bool); !got {
		t.Fatalf("enableImageGeneration=%v want true", got)
	}
	if got, _ := payload["enableImageStreaming"].(bool); !got {
		t.Fatalf("enableImageStreaming=%v want true", got)
	}
	if got, _ := payload["sendFinalMetadata"].(bool); !got {
		t.Fatalf("sendFinalMetadata=%v want true", got)
	}
	if got, _ := payload["disableMemory"].(bool); got {
		t.Fatalf("disableMemory=%v want false", got)
	}
	if got, _ := payload["disableSearch"].(bool); got {
		t.Fatalf("disableSearch=%v want false", got)
	}
	if got, _ := payload["disableTextFollowUps"].(bool); got {
		t.Fatalf("disableTextFollowUps=%v want false", got)
	}
	if got, _ := payload["enableSideBySide"].(bool); !got {
		t.Fatalf("enableSideBySide=%v want true", got)
	}
	if got, _ := payload["imageGenerationCount"].(int); got != 2 {
		t.Fatalf("imageGenerationCount=%v want 2", payload["imageGenerationCount"])
	}
	if got, ok := payload["imageAttachments"].([]string); !ok || len(got) != 0 {
		t.Fatalf("imageAttachments=%#v want empty string slice", payload["imageAttachments"])
	}
	if got, ok := payload["fileAttachments"].([]string); !ok || len(got) != 0 {
		t.Fatalf("fileAttachments=%#v want empty string slice", payload["fileAttachments"])
	}
	if _, ok := payload["responseMetadata"].(map[string]interface{}); !ok {
		t.Fatalf("responseMetadata missing: %#v", payload["responseMetadata"])
	}
	if _, ok := payload["deviceEnvInfo"].(map[string]interface{}); !ok {
		t.Fatalf("deviceEnvInfo missing: %#v", payload["deviceEnvInfo"])
	}
}

func TestCollectAppChatImageURLs_LiteUsesNewConversationEndpoint(t *testing.T) {
	var upstreamPaths []string
	var upstreamPayload map[string]interface{}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamPaths = append(upstreamPaths, r.URL.Path)
		if r.URL.Path != defaultChatPath {
			http.NotFound(w, r)
			return
		}
		if err := json.NewDecoder(r.Body).Decode(&upstreamPayload); err != nil {
			t.Fatalf("decode upstream payload: %v", err)
		}
		resp := map[string]interface{}{
			"result": map[string]interface{}{
				"response": map[string]interface{}{
					"cardAttachment": map[string]interface{}{
						"jsonData": `{"id":"card-1","image_chunk":{"progress":100,"imageUuid":"img-1","imageUrl":"users/u/generated/a/image.png"}}`,
					},
				},
			},
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer upstream.Close()

	h := NewHandler(&config.Config{GrokAPIBaseURL: upstream.URL}, nil)
	sess := &chatAccountSession{token: "basic-token"}
	spec := ModelSpec{
		ID:            "grok-imagine-image-lite",
		UpstreamModel: "grok-imagine-image-lite",
		ModelMode:     "MODEL_MODE_FAST",
		Tier:          grokTierBasic,
		IsImage:       true,
	}
	req := ImagesGenerationsRequest{
		Model:          "grok-imagine-image-lite",
		Prompt:         "apple",
		Size:           "1024x1024",
		N:              1,
		ResponseFormat: "url",
	}

	urls, err := h.collectAppChatImageURLs(context.Background(), sess, spec, req, nil, false)
	if err != nil {
		t.Fatalf("collectAppChatImageURLs error: %v", err)
	}
	if len(urls) != 1 || urls[0] != "https://assets.grok.com/users/u/generated/a/image.png" {
		t.Fatalf("urls=%#v", urls)
	}
	if len(upstreamPaths) != 1 || upstreamPaths[0] != defaultChatPath {
		t.Fatalf("upstream paths=%#v want only %s", upstreamPaths, defaultChatPath)
	}
	if got, _ := upstreamPayload["message"].(string); got != "Drawing: apple" {
		t.Fatalf("message=%q want Drawing: apple", got)
	}
	if got, _ := upstreamPayload["modeId"].(string); got != "fast" {
		t.Fatalf("modeId=%q want fast", got)
	}
}

func TestIsAppChatImageLimitResponse(t *testing.T) {
	resp := map[string]interface{}{
		"result": map[string]interface{}{
			"response": map[string]interface{}{
				"modelResponse": map[string]interface{}{
					"streamErrors": []interface{}{
						map[string]interface{}{
							"message": "Unable to render: You've reached your image generation limit. Please try again later.",
							"renderToolRateLimited": map[string]interface{}{
								"rateLimitedCount": 1,
							},
						},
					},
				},
			},
		},
	}
	if !isAppChatImageLimitResponse(resp) {
		t.Fatalf("limit response was not detected")
	}
}

func TestEnsureImageNSFW_DoesNotCreateModelOverride(t *testing.T) {
	payload := map[string]interface{}{}
	nsfw := true

	ensureImageNSFW(payload, "grok-imagine-image-lite", &nsfw)

	if _, ok := payload["modelConfigOverride"]; ok {
		t.Fatalf("modelConfigOverride should not be created by NSFW cleanup: %#v", payload["modelConfigOverride"])
	}
}

func TestGrokAppChatImagePrompt_PrefersDrawTrigger(t *testing.T) {
	if got := grokAppChatImagePrompt("a red apple"); got != "a red apple" {
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
	if got[0] != "美女图片" {
		t.Fatalf("first=%q", got[0])
	}
	if !strings.Contains(got[1], "safe-for-work portrait photo of an adult woman") {
		t.Fatalf("fallback=%q", got[1])
	}
}
