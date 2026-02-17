package grok

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"orchids-api/internal/config"
)

func TestResolveModel_AcceptsGorkAlias(t *testing.T) {
	spec, ok := ResolveModel("gork-3")
	if !ok {
		t.Fatalf("ResolveModel(gork-3) should succeed")
	}
	if spec.ID != "grok-3" {
		t.Fatalf("spec.ID=%q want grok-3", spec.ID)
	}
}

func TestChatCompletionsRequestValidate_CompatFields(t *testing.T) {
	temp := 0.7
	topP := 0.9
	effort := "high"
	req := ChatCompletionsRequest{
		Model: "grok-3",
		Messages: []ChatMessage{{
			Role:    "user",
			Content: "hello",
		}},
		Temperature:     &temp,
		TopP:            &topP,
		ReasoningEffort: &effort,
		ImageConfig: &ImageConfig{
			N:              2,
			Size:           "1280x720",
			ResponseFormat: "url",
		},
	}
	if err := req.Validate(); err != nil {
		t.Fatalf("Validate() error: %v", err)
	}
}

func TestChatCompletionsRequestValidate_DefaultSampling(t *testing.T) {
	req := ChatCompletionsRequest{
		Model: "grok-3",
		Messages: []ChatMessage{{
			Role:    "user",
			Content: "hello",
		}},
	}
	if err := req.Validate(); err != nil {
		t.Fatalf("Validate() error: %v", err)
	}
	if req.Temperature == nil || *req.Temperature != 0.8 {
		t.Fatalf("temperature default mismatch: got=%v", req.Temperature)
	}
	if req.TopP == nil || *req.TopP != 0.95 {
		t.Fatalf("top_p default mismatch: got=%v", req.TopP)
	}
}

func TestNormalizeImageSize(t *testing.T) {
	if got, err := normalizeImageSize(""); err != nil || got != "1024x1024" {
		t.Fatalf("normalizeImageSize(empty)=(%q,%v) want (1024x1024,nil)", got, err)
	}
	if got, err := normalizeImageSize("1792x1024"); err != nil || got != "1792x1024" {
		t.Fatalf("normalizeImageSize valid failed: got=%q err=%v", got, err)
	}
	if _, err := normalizeImageSize("2000x2000"); err == nil {
		t.Fatalf("normalizeImageSize should reject unsupported size")
	}
}

func TestBuildChatPayload_InjectsSamplingOverrides(t *testing.T) {
	h := &Handler{client: New(nil)}
	temp := 1.1
	topP := 0.25
	effort := "medium"
	req := &ChatCompletionsRequest{
		Temperature:     &temp,
		TopP:            &topP,
		ReasoningEffort: &effort,
	}
	spec := ModelSpec{ID: "grok-3", UpstreamModel: "grok-3", ModelMode: "MODEL_MODE_GROK_3"}

	payload, err := h.buildChatPayload(context.Background(), "", spec, "hello", nil, nil, nil, req)
	if err != nil {
		t.Fatalf("buildChatPayload error: %v", err)
	}

	respMeta, ok := payload["responseMetadata"].(map[string]interface{})
	if !ok {
		t.Fatalf("responseMetadata missing")
	}
	override, ok := respMeta["modelConfigOverride"].(map[string]interface{})
	if !ok {
		t.Fatalf("modelConfigOverride missing")
	}
	if got, _ := override["temperature"].(float64); got != temp {
		t.Fatalf("temperature=%v want=%v", got, temp)
	}
	if got, _ := override["topP"].(float64); got != topP {
		t.Fatalf("topP=%v want=%v", got, topP)
	}
	if got, _ := override["reasoningEffort"].(string); got != "medium" {
		t.Fatalf("reasoningEffort=%q want=medium", got)
	}
}

func TestChatCompletionsRequest_UnmarshalLooseTypes(t *testing.T) {
	raw := []byte(`{
		"model":"grok-3",
		"messages":[{"role":"user","content":"hello"}],
		"stream":"true",
		"temperature":"1.2",
		"top_p":"0.6",
		"video_config":{"aspect_ratio":"1280x720","video_length":"10","resolution_name":"720p","preset":"normal"},
		"image_config":{"n":"2","size":"1024x1024","response_format":"base64"}
	}`)
	var req ChatCompletionsRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		t.Fatalf("unmarshal failed: %v", err)
	}
	if req.Stream != true {
		t.Fatalf("stream=%v want=true", req.Stream)
	}
	if req.Temperature == nil || *req.Temperature != 1.2 {
		t.Fatalf("temperature=%v want=1.2", req.Temperature)
	}
	if req.TopP == nil || *req.TopP != 0.6 {
		t.Fatalf("top_p=%v want=0.6", req.TopP)
	}
	if req.VideoConfig == nil || req.VideoConfig.VideoLength != 10 {
		t.Fatalf("video_config=%+v want video_length=10", req.VideoConfig)
	}
	if req.ImageConfig == nil || req.ImageConfig.N != 2 {
		t.Fatalf("image_config=%+v want n=2", req.ImageConfig)
	}
}

func TestChatCompletionsRequest_UnmarshalInvalidStream(t *testing.T) {
	raw := []byte(`{
		"model":"grok-3",
		"messages":[{"role":"user","content":"hello"}],
		"stream":"maybe"
	}`)
	var req ChatCompletionsRequest
	if err := json.Unmarshal(raw, &req); err == nil {
		t.Fatalf("expected stream parse error")
	}
}

func TestChatCompletionsRequest_StreamProvidedFlagAndDefault(t *testing.T) {
	rawNoStream := []byte(`{
		"model":"grok-3",
		"messages":[{"role":"user","content":"hello"}]
	}`)
	var req ChatCompletionsRequest
	if err := json.Unmarshal(rawNoStream, &req); err != nil {
		t.Fatalf("unmarshal failed: %v", err)
	}
	if req.StreamProvided {
		t.Fatalf("stream should be marked as not provided")
	}

	cfg := &config.Config{}
	config.ApplyDefaults(cfg)
	h := &Handler{cfg: cfg}
	h.applyDefaultChatStream(&req)
	if req.Stream != true {
		t.Fatalf("default stream=%v want=true", req.Stream)
	}

	rawWithStream := []byte(`{
		"model":"grok-3",
		"messages":[{"role":"user","content":"hello"}],
		"stream":false
	}`)
	var req2 ChatCompletionsRequest
	if err := json.Unmarshal(rawWithStream, &req2); err != nil {
		t.Fatalf("unmarshal failed: %v", err)
	}
	if !req2.StreamProvided {
		t.Fatalf("stream should be marked as provided")
	}
	h.applyDefaultChatStream(&req2)
	if req2.Stream != false {
		t.Fatalf("explicit stream should not be overridden, got=%v", req2.Stream)
	}

	rawWithNullStream := []byte(`{
		"model":"grok-3",
		"messages":[{"role":"user","content":"hello"}],
		"stream":null
	}`)
	var req3 ChatCompletionsRequest
	if err := json.Unmarshal(rawWithNullStream, &req3); err != nil {
		t.Fatalf("unmarshal failed: %v", err)
	}
	if req3.StreamProvided {
		t.Fatalf("stream=null should be treated as not provided")
	}
	h.applyDefaultChatStream(&req3)
	if req3.Stream != true {
		t.Fatalf("null stream should fallback to default true, got=%v", req3.Stream)
	}
}

func TestApplyDefaultChatStream_AppStreamOverridesStream(t *testing.T) {
	stream := true
	appStream := false
	h := &Handler{
		cfg: &config.Config{
			Stream:    &stream,
			AppStream: &appStream,
		},
	}
	req := ChatCompletionsRequest{
		StreamProvided: false,
	}
	h.applyDefaultChatStream(&req)
	if req.Stream != false {
		t.Fatalf("stream=%v want=false (app_stream override)", req.Stream)
	}
}

func TestImagesGenerationsRequest_UnmarshalLooseTypes(t *testing.T) {
	raw := []byte(`{
		"model":"grok-imagine-1.0",
		"prompt":"hello",
		"n":"2",
		"size":"1024x1024",
		"stream":"false",
		"nsfw":"true",
		"response_format":"url"
	}`)
	var req ImagesGenerationsRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		t.Fatalf("unmarshal failed: %v", err)
	}
	if req.N != 2 {
		t.Fatalf("n=%d want=2", req.N)
	}
	if req.Stream != false {
		t.Fatalf("stream=%v want=false", req.Stream)
	}
	if req.NSFW == nil || *req.NSFW != true {
		t.Fatalf("nsfw=%v want=true", req.NSFW)
	}
}

func TestHandlePublicVideoStart_DefaultPresetNormal(t *testing.T) {
	h := &Handler{}
	req := httptest.NewRequest(http.MethodPost, "/v1/public/video/start", strings.NewReader(`{"prompt":"hello"}`))
	rec := httptest.NewRecorder()
	h.HandlePublicVideoStart(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d want=200 body=%s", rec.Code, rec.Body.String())
	}

	var out map[string]interface{}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	taskID, _ := out["task_id"].(string)
	taskID = strings.TrimSpace(taskID)
	if taskID == "" {
		t.Fatalf("missing task_id")
	}
	session, ok := getPublicVideoSession(taskID)
	if !ok {
		t.Fatalf("session not found")
	}
	if session.Preset != "normal" {
		t.Fatalf("preset=%q want=normal", session.Preset)
	}
}
