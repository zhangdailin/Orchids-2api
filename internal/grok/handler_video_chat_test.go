package grok

import (
	"bytes"
	"github.com/goccy/go-json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestVideoSegmentLengthsMatchGrok2API(t *testing.T) {
	cases := map[int][]int{
		6:  {6},
		10: {10},
		12: {6, 6},
		16: {10, 6},
		20: {10, 10},
	}
	for seconds, want := range cases {
		got, err := videoSegmentLengths(seconds)
		if err != nil {
			t.Fatalf("videoSegmentLengths(%d) error=%v", seconds, err)
		}
		if len(got) != len(want) {
			t.Fatalf("videoSegmentLengths(%d)=%v want=%v", seconds, got, want)
		}
		for i := range got {
			if got[i] != want[i] {
				t.Fatalf("videoSegmentLengths(%d)=%v want=%v", seconds, got, want)
			}
		}
	}
}

func TestBuildVideoExtendPayload_MatchesGrok2APIFields(t *testing.T) {
	h := &Handler{}
	spec := ModelSpec{ID: "grok-imagine-video", UpstreamModel: "imagine-video-gen", ModelMode: "MODEL_MODE_AUTO", IsVideo: true}
	payload := h.buildVideoExtendPayload(spec, "make a clip", "parent-1", "extend-1", &VideoConfig{
		AspectRatio:    "16:9",
		VideoLength:    6,
		ResolutionName: "720p",
		Preset:         "normal",
	}, 6, 10)

	if got := payload["modelName"]; got != "imagine-video-gen" {
		t.Fatalf("modelName=%#v want imagine-video-gen", got)
	}
	respMeta := payload["responseMetadata"].(map[string]interface{})
	modelCfg := respMeta["modelConfigOverride"].(map[string]interface{})
	modelMap := modelCfg["modelMap"].(map[string]interface{})
	videoCfg := modelMap["videoGenModelConfig"].(map[string]interface{})
	if got := videoCfg["isVideoExtension"]; got != true {
		t.Fatalf("isVideoExtension=%#v want true", got)
	}
	if got := videoCfg["extendPostId"]; got != "extend-1" {
		t.Fatalf("extendPostId=%#v want extend-1", got)
	}
	if got := videoCfg["originalRefType"]; got != "ORIGINAL_REF_TYPE_VIDEO_EXTENSION" {
		t.Fatalf("originalRefType=%#v", got)
	}
	if got := videoCfg["videoLength"]; got != 6 {
		t.Fatalf("videoLength=%#v want 6", got)
	}
	if got := videoCfg["videoExtensionStartTime"].(float64); got != 10.041667 {
		t.Fatalf("videoExtensionStartTime=%#v want 10.041667", got)
	}
}

func TestVideoExtensionStartTime_RoundsLikeGrok2API(t *testing.T) {
	if got := videoExtensionStartTime(6); got != 6.041667 {
		t.Fatalf("videoExtensionStartTime(6)=%#v want 6.041667", got)
	}
}

func TestCollectVideoSegmentFromBody_AssetFallback(t *testing.T) {
	h := &Handler{}
	body := strings.NewReader(`{"result":{"response":{"streamingVideoGenerationResponse":{"progress":100,"assetId":"asset-video-1","videoPostId":"post-1"}}}}`)
	got, err := h.collectVideoSegmentFromBody(body, nil, nil)
	if err != nil {
		t.Fatalf("collectVideoSegmentFromBody error=%v", err)
	}
	if got.URL != "https://assets.grok.com/asset-video-1/content" {
		t.Fatalf("URL=%q", got.URL)
	}
	if got.VideoPostID != "post-1" {
		t.Fatalf("VideoPostID=%q", got.VideoPostID)
	}
}

func TestHandleVideosRetrieveAndContent(t *testing.T) {
	h := &Handler{}
	name, err := h.cacheMediaBytes("unit-video", "video", bytes.Repeat([]byte{1, 2, 3, 4}, 1024), "video/mp4")
	if err != nil {
		t.Fatalf("cacheMediaBytes error=%v", err)
	}
	job := &videoJob{
		ID:          "video_unit",
		Model:       "grok-imagine-video",
		Prompt:      "hello",
		Seconds:     6,
		Size:        "720x1280",
		Quality:     "standard",
		CreatedAt:   time.Now().Unix(),
		Status:      "completed",
		Progress:    100,
		CompletedAt: 456,
		ContentPath: "data/tmp/video/" + name,
	}
	putVideoJob(job)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/videos/video_unit", nil)
	h.HandleVideosRetrieve(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("retrieve status=%d body=%s", rec.Code, rec.Body.String())
	}

	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/v1/videos/video_unit/content", nil)
	h.HandleVideosContent(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("content status=%d body=%s", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "video/mp4") {
		t.Fatalf("content-type=%q", ct)
	}
}

func TestHandleVideosRetrieveIncludesStableContentURL(t *testing.T) {
	oldBase := cacheBaseDir
	cacheBaseDir = t.TempDir()
	t.Cleanup(func() { cacheBaseDir = oldBase })

	h := &Handler{}
	name, err := h.cacheMediaBytes("unit-video", "video", bytes.Repeat([]byte{1, 2, 3, 4}, 1024), "video/mp4")
	if err != nil {
		t.Fatalf("cacheMediaBytes error=%v", err)
	}
	job := &videoJob{
		ID:          "video_stable",
		Model:       "grok-imagine-video",
		Prompt:      "hello",
		Seconds:     6,
		Size:        "720x1280",
		Quality:     "standard",
		CreatedAt:   time.Now().Unix(),
		Status:      "completed",
		Progress:    100,
		CompletedAt: 456,
		VideoURL:    "https://assets.grok.com/video/content",
		ContentPath: "data/tmp/video/" + name,
	}
	putVideoJob(job)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/videos/video_stable", nil)
	h.HandleVideosRetrieve(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("retrieve status=%d body=%s", rec.Code, rec.Body.String())
	}
	var out map[string]interface{}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("json decode error=%v", err)
	}
	if got := out["content_url"]; got != "/grok/v1/videos/video_stable/content" {
		t.Fatalf("content_url=%#v", got)
	}
	if got := out["video_url"]; got != "https://assets.grok.com/video/content" {
		t.Fatalf("video_url=%#v", got)
	}
}

func TestValidateVideoConfig_AcceptsSecondsSizeShape(t *testing.T) {
	cfg, err := validateVideoConfig(&VideoConfig{VideoLength: 12, Size: "1280x720"})
	if err != nil {
		t.Fatalf("validateVideoConfig error=%v", err)
	}
	if cfg.AspectRatio != "16:9" {
		t.Fatalf("AspectRatio=%q want 16:9", cfg.AspectRatio)
	}
	if cfg.ResolutionName != "720p" {
		t.Fatalf("ResolutionName=%q want 720p", cfg.ResolutionName)
	}
}

func TestExtractVideoPromptAndAttachments_MatchesGrok2API(t *testing.T) {
	prompt, refs, err := extractVideoPromptAndAttachments([]ChatMessage{
		{Role: "user", Content: []interface{}{
			map[string]interface{}{"type": "text", "text": "old prompt"},
			map[string]interface{}{"type": "image_url", "image_url": map[string]interface{}{"url": "https://example.com/old.png"}},
		}},
		{Role: "assistant", Content: "ignored assistant"},
		{Role: "user", Content: []interface{}{
			map[string]interface{}{"type": "text", "text": "new"},
			map[string]interface{}{"type": "text", "text": "prompt"},
			map[string]interface{}{"type": "image_url", "image_url": map[string]interface{}{"url": "https://example.com/new-a.png"}},
			map[string]interface{}{"type": "image_url", "image_url": "https://example.com/new-b.png"},
		}},
	})
	if err != nil {
		t.Fatalf("extractVideoPromptAndAttachments error=%v", err)
	}
	if prompt != "new prompt" {
		t.Fatalf("prompt=%q want new prompt", prompt)
	}
	if len(refs) != 2 || refs[0].Data != "https://example.com/new-a.png" || refs[1].Data != "https://example.com/new-b.png" {
		t.Fatalf("refs=%#v", refs)
	}
}

func TestVideosRequestUnmarshal_AcceptsGrok2APIShape(t *testing.T) {
	var req VideosRequest
	if err := json.Unmarshal([]byte(`{
		"model":"grok-imagine-video",
		"prompt":"hello",
		"seconds":"12",
		"size":"1280x720",
		"resolution_name":"720p",
		"preset":"normal",
		"input_references":[{"image_url":"data:image/png;base64,abc"}]
	}`), &req); err != nil {
		t.Fatalf("Unmarshal error=%v", err)
	}
	if req.Seconds != 12 || req.Size != "1280x720" {
		t.Fatalf("request=%+v", req)
	}
	if len(req.InputReferences) != 1 || req.InputReferences[0] != "data:image/png;base64,abc" {
		t.Fatalf("InputReferences=%#v", req.InputReferences)
	}
}

func TestParseVideosRequest_FormURLEncodedShape(t *testing.T) {
	body := strings.NewReader("model=grok-imagine-video&prompt=hello&seconds=12&size=1280x720&input_reference=https%3A%2F%2Fexample.com%2Fa.png")
	req := httptest.NewRequest(http.MethodPost, "/v1/videos", body)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	got, err := parseVideosRequest(req)
	if err != nil {
		t.Fatalf("parseVideosRequest error=%v", err)
	}
	if got.Prompt != "hello" || got.Seconds != 12 || got.Size != "1280x720" {
		t.Fatalf("request=%+v", got)
	}
	if len(got.InputReferences) != 1 || got.InputReferences[0] != "https://example.com/a.png" {
		t.Fatalf("InputReferences=%#v", got.InputReferences)
	}
}
