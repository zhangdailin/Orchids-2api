package grok

import (
	"context"
	"github.com/goccy/go-json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"orchids-api/internal/config"
	"orchids-api/internal/modelpolicy"
)

func TestResolveModel_RejectsRemovedAliases(t *testing.T) {
	for _, id := range []string{"gork-4.20-0309", "grok-4-1-thinking", "grok-imagine-1.0"} {
		if _, ok := ResolveModel(id); ok {
			t.Fatalf("ResolveModel(%s) should fail", id)
		}
		if _, ok := ResolveModelOrDynamic(id); ok {
			t.Fatalf("ResolveModelOrDynamic(%s) should fail", id)
		}
	}
}

func TestResolveModelOrDynamic_Grok42Rejected(t *testing.T) {
	if _, ok := ResolveModel("grok-4.2"); ok {
		t.Fatalf("ResolveModel(grok-4.2) should fail")
	}
	if _, ok := ResolveModelOrDynamic("grok-4.2"); ok {
		t.Fatalf("ResolveModelOrDynamic(grok-4.2) should fail")
	}
	if _, ok := ResolveModelOrDynamic("grok-4-2"); ok {
		t.Fatalf("ResolveModelOrDynamic(grok-4-2) should fail")
	}
}

func TestResolveModel_Grok420Rejected(t *testing.T) {
	if _, ok := ResolveModel("grok-420"); ok {
		t.Fatalf("ResolveModel(grok-420) should fail")
	}
	if _, ok := ResolveModelOrDynamic("grok-420"); ok {
		t.Fatalf("ResolveModelOrDynamic(grok-420) should fail")
	}
}

func TestResolveModel_AliasBaseMappingsMatchGrok2API(t *testing.T) {
	cases := []struct {
		modelID       string
		wantUpstream  string
		wantModelMode string
		wantModeID    string
	}{
		{modelID: "grok-4.20-0309-non-reasoning", wantUpstream: "grok-4.20-0309-non-reasoning", wantModelMode: "MODEL_MODE_FAST", wantModeID: "fast"},
		{modelID: "grok-4.20-0309", wantUpstream: "grok-4.20-0309", wantModelMode: "MODEL_MODE_AUTO", wantModeID: "auto"},
		{modelID: "grok-4.20-0309-reasoning", wantUpstream: "grok-4.20-0309-reasoning", wantModelMode: "MODEL_MODE_EXPERT", wantModeID: "expert"},
		{modelID: "grok-4.20-0309-non-reasoning-super", wantUpstream: "grok-4.20-0309-non-reasoning-super", wantModelMode: "MODEL_MODE_FAST", wantModeID: "fast"},
		{modelID: "grok-4.20-0309-super", wantUpstream: "grok-4.20-0309-super", wantModelMode: "MODEL_MODE_AUTO", wantModeID: "auto"},
		{modelID: "grok-4.20-0309-reasoning-super", wantUpstream: "grok-4.20-0309-reasoning-super", wantModelMode: "MODEL_MODE_EXPERT", wantModeID: "expert"},
		{modelID: "grok-4.20-0309-non-reasoning-heavy", wantUpstream: "grok-4.20-0309-non-reasoning-heavy", wantModelMode: "MODEL_MODE_FAST", wantModeID: "fast"},
		{modelID: "grok-4.20-0309-heavy", wantUpstream: "grok-4.20-0309-heavy", wantModelMode: "MODEL_MODE_AUTO", wantModeID: "auto"},
		{modelID: "grok-4.20-0309-reasoning-heavy", wantUpstream: "grok-4.20-0309-reasoning-heavy", wantModelMode: "MODEL_MODE_EXPERT", wantModeID: "expert"},
		{modelID: "grok-4.20-multi-agent-0309", wantUpstream: "grok-4.20-multi-agent-0309", wantModelMode: "MODEL_MODE_HEAVY", wantModeID: "heavy"},
		{modelID: "grok-4.20-fast", wantUpstream: "grok-4.20-fast", wantModelMode: "MODEL_MODE_FAST", wantModeID: "fast"},
		{modelID: "grok-4.20-auto", wantUpstream: "grok-4.20-auto", wantModelMode: "MODEL_MODE_AUTO", wantModeID: "auto"},
		{modelID: "grok-4.20-expert", wantUpstream: "grok-4.20-expert", wantModelMode: "MODEL_MODE_EXPERT", wantModeID: "expert"},
		{modelID: "grok-4.20-heavy", wantUpstream: "grok-4.20-heavy", wantModelMode: "MODEL_MODE_HEAVY", wantModeID: "heavy"},
		// grok-4.3 models are console-only — skip mode/modeID checks
		{modelID: "grok-4.3-beta", wantUpstream: "grok-4.3-beta", wantModelMode: "", wantModeID: "grok-420-computer-use-sa"},
	}
	for _, tc := range cases {
		spec, ok := ResolveModel(tc.modelID)
		if !ok {
			t.Fatalf("ResolveModel(%s) should succeed", tc.modelID)
		}
		if spec.UpstreamModel != tc.wantUpstream {
			t.Fatalf("%s upstream=%q want=%q", tc.modelID, spec.UpstreamModel, tc.wantUpstream)
		}
		if spec.ModelMode != tc.wantModelMode {
			t.Fatalf("%s mode=%q want=%q", tc.modelID, spec.ModelMode, tc.wantModelMode)
		}
		if spec.ModeID != tc.wantModeID {
			t.Fatalf("%s modeID=%q want=%q", tc.modelID, spec.ModeID, tc.wantModeID)
		}
	}
}

func TestPublicGrokModelsAllResolveToAppChatSpecs(t *testing.T) {
	for _, id := range modelpolicy.PublicGrokModelIDs() {
		spec, ok := ResolveModel(id)
		if !ok {
			t.Fatalf("public Grok model %q is missing from SupportedModels", id)
		}
		if spec.ConsoleModel != "" {
			t.Logf("public Grok model %q uses Console API only", id); continue
		}
		if spec.UpstreamModel == "" {
			t.Fatalf("public Grok model %q has empty UpstreamModel", id)
		}
		if IsDeprecatedModelID(id) {
			t.Fatalf("public Grok model %q should not be deprecated", id)
		}
	}
}

func TestResolveModel_ImagineMappingsMatchGrok2API(t *testing.T) {
	cases := []struct {
		modelID       string
		wantUpstream  string
		wantModelMode string
	}{
		{modelID: "grok-imagine-image-lite", wantUpstream: "grok-imagine-image-lite", wantModelMode: "MODEL_MODE_FAST"},
		{modelID: "grok-imagine-image", wantUpstream: "grok-imagine-image", wantModelMode: "MODEL_MODE_AUTO"},
		{modelID: "grok-imagine-image-pro", wantUpstream: "grok-imagine-image-pro", wantModelMode: "MODEL_MODE_AUTO"},
		{modelID: "grok-imagine-image-edit", wantUpstream: "imagine-image-edit", wantModelMode: "MODEL_MODE_AUTO"},
		{modelID: "grok-imagine-video", wantUpstream: "imagine-video-gen", wantModelMode: "MODEL_MODE_AUTO"},
	}

	for _, tc := range cases {
		spec, ok := ResolveModel(tc.modelID)
		if !ok {
			t.Fatalf("ResolveModel(%s) should succeed", tc.modelID)
		}
		if spec.UpstreamModel != tc.wantUpstream {
			t.Fatalf("%s upstream=%q want=%q", tc.modelID, spec.UpstreamModel, tc.wantUpstream)
		}
		if spec.ModelMode != tc.wantModelMode {
			t.Fatalf("%s mode=%q want=%q", tc.modelID, spec.ModelMode, tc.wantModelMode)
		}
	}
}

func TestResolveModel_Grok420BetaRejected(t *testing.T) {
	if _, ok := ResolveModel("grok-4.20-beta"); ok {
		t.Fatalf("ResolveModel(grok-4.20-beta) should fail")
	}
}

func TestResolveModel_Grok2APIAppChatModelsAccepted(t *testing.T) {
	for _, id := range []string{
		"grok-4.20-0309-non-reasoning",
		"grok-4.20-0309",
		"grok-4.20-0309-reasoning",
		"grok-4.20-0309-non-reasoning-super",
		"grok-4.20-0309-super",
		"grok-4.20-0309-reasoning-super",
		"grok-4.20-0309-non-reasoning-heavy",
		"grok-4.20-0309-heavy",
		"grok-4.20-0309-reasoning-heavy",
		"grok-4.20-multi-agent-0309",
		"grok-4.20-fast",
		"grok-4.20-auto",
		"grok-4.20-expert",
		"grok-4.20-heavy",
		"grok-4.3-beta",
	} {
		if _, ok := ResolveModel(id); !ok {
			t.Fatalf("ResolveModel(%s) should succeed", id)
		}
		if IsDeprecatedModelID(id) {
			t.Fatalf("%s should not be deprecated", id)
		}
	}
}

func TestResolveModel_AcceptsGrok43(t *testing.T) {
	for _, id := range []string{
		"grok-4.3",
		"grok-4.3-beta",
		"grok-build-0.1",
	} {
		if _, ok := ResolveModel(id); !ok {
			t.Fatalf("ResolveModel(%s) should succeed", id)
		}
	}
}

func TestResolveModel_Grok420BetaHyphenAliasRejected(t *testing.T) {
	if _, ok := ResolveModel("grok-4-20-beta"); ok {
		t.Fatalf("ResolveModel(grok-4-20-beta) should fail")
	}
}

func TestResolveModelOrDynamic_RejectsUnknownImagineModel(t *testing.T) {
	if _, ok := ResolveModelOrDynamic("grok-imagine-2.0"); ok {
		t.Fatalf("ResolveModelOrDynamic(grok-imagine-2.0) should fail")
	}
}

func TestChatCompletionsRequestValidate_CompatFields(t *testing.T) {
	temp := 0.7
	topP := 0.9
	effort := "high"
	req := ChatCompletionsRequest{
		Model: "grok-4.20-0309",
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
		Model: "grok-4.20-0309",
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

func TestChatCompletionsRequestValidate_ToolChoice(t *testing.T) {
	req := ChatCompletionsRequest{
		Model: "grok-4.20-0309",
		Messages: []ChatMessage{{
			Role:    "user",
			Content: "hello",
		}},
		Tools: []ToolDef{{
			Type: "function",
			Function: map[string]interface{}{
				"name": "weather",
			},
		}},
		ToolChoice: "required",
	}
	if err := req.Validate(); err != nil {
		t.Fatalf("Validate() error: %v", err)
	}

	req.ToolChoice = "bad-choice"
	if err := req.Validate(); err == nil {
		t.Fatalf("expected invalid tool_choice error")
	}

	req.ToolChoice = map[string]interface{}{
		"type": "function",
		"function": map[string]interface{}{
			"name": "weather",
		},
	}
	if err := req.Validate(); err != nil {
		t.Fatalf("Validate() object tool_choice error: %v", err)
	}

	req.ToolChoice = map[string]interface{}{
		"type": "function",
		"function": map[string]interface{}{
			"name": "unknown_tool",
		},
	}
	if err := req.Validate(); err == nil {
		t.Fatalf("expected tool_choice function reference error")
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

func TestNormalizeImageEditSize_MatchesGrok2API(t *testing.T) {
	if got, err := normalizeImageEditSize(""); err != nil || got != "1024x1024" {
		t.Fatalf("normalizeImageEditSize(empty)=(%q,%v) want (1024x1024,nil)", got, err)
	}
	if got, err := normalizeImageEditSize("1024x1024"); err != nil || got != "1024x1024" {
		t.Fatalf("normalizeImageEditSize valid failed: got=%q err=%v", got, err)
	}
	if _, err := normalizeImageEditSize("1792x1024"); err == nil {
		t.Fatalf("normalizeImageEditSize should reject non-square edit size")
	}
}

func TestReplaceImageEditPlaceholders_MatchesGrok2API(t *testing.T) {
	got := replaceImageEditPlaceholders("blend @IMAGE1 with @image2 and keep @IMAGE3", []imageEditReference{
		{fileID: "file-a", contentURL: "https://assets.grok.com/a/content"},
		{fileID: "file-b", contentURL: "https://assets.grok.com/b/content"},
	})
	want := "blend @file-a with @file-b and keep @IMAGE3"
	if got != want {
		t.Fatalf("replaceImageEditPlaceholders=%q want %q", got, want)
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
	spec := ModelSpec{ID: "grok-4.20-0309", UpstreamModel: "grok-4.20-0309", ModelMode: "MODEL_MODE_AUTO"}

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
	if _, ok := payload["modelName"]; ok {
		t.Fatalf("text app-chat payload should not include modelName: %#v", payload["modelName"])
	}
	if got, _ := payload["modeId"].(string); got != "auto" {
		t.Fatalf("modeId=%q want auto", got)
	}
}

func TestBuildChatPayload_UsesGrokConfigFlags(t *testing.T) {
	temporary := false
	disableMemory := false
	cfg := &config.Config{
		GrokTemporary:         &temporary,
		GrokDisableMemory:     &disableMemory,
		GrokCustomInstruction: "be concise",
	}
	h := &Handler{client: New(cfg), cfg: cfg}
	spec := ModelSpec{ID: "grok-4.20-0309", UpstreamModel: "grok-4.20-0309", ModelMode: "MODEL_MODE_AUTO"}

	payload, err := h.buildChatPayload(context.Background(), "", spec, "hello", nil, nil, nil, &ChatCompletionsRequest{})
	if err != nil {
		t.Fatalf("buildChatPayload error: %v", err)
	}
	if got, _ := payload["temporary"].(bool); got {
		t.Fatalf("temporary=%v want=false", got)
	}
	if got, _ := payload["disableMemory"].(bool); got {
		t.Fatalf("disableMemory=%v want=false", got)
	}
	if got, _ := payload["customPersonality"].(string); got != "be concise" {
		t.Fatalf("customPersonality=%q want=%q", got, "be concise")
	}
}

func TestBuildImageEditPayload_UsesGrokConfigFlags(t *testing.T) {
	temporary := false
	disableMemory := true
	cfg := &config.Config{
		GrokTemporary:         &temporary,
		GrokDisableMemory:     &disableMemory,
		GrokCustomInstruction: "image mode",
	}
	h := &Handler{cfg: cfg}
	spec := ModelSpec{ID: "grok-imagine-image-edit", UpstreamModel: "imagine-image-edit"}

	payload := h.buildImageEditPayload(spec, "edit this", []string{"https://assets.grok.com/demo.png"}, "post-1")
	if got, _ := payload["temporary"].(bool); got {
		t.Fatalf("temporary=%v want=false", got)
	}
	if got, _ := payload["disableMemory"].(bool); !got {
		t.Fatalf("disableMemory=%v want=true", got)
	}
	if got, _ := payload["customPersonality"].(string); got != "image mode" {
		t.Fatalf("customPersonality=%q want=%q", got, "image mode")
	}
	respMeta, ok := payload["responseMetadata"].(map[string]interface{})
	if !ok {
		t.Fatalf("responseMetadata missing")
	}
	reqDetails, ok := respMeta["requestModelDetails"].(map[string]interface{})
	if !ok || reqDetails["modelId"] != "imagine-image-edit" {
		t.Fatalf("requestModelDetails=%#v", reqDetails)
	}
	if _, ok := payload["deviceEnvInfo"].(map[string]interface{}); !ok {
		t.Fatalf("deviceEnvInfo missing")
	}
	if got, _ := payload["modelMode"].(string); got != spec.ModelMode {
		t.Fatalf("modelMode=%q want=%q", got, spec.ModelMode)
	}
	if got, _ := payload["disableSearch"].(bool); got {
		t.Fatalf("disableSearch=%v want=false", got)
	}
	if _, ok := payload["fileAttachments"].([]string); !ok {
		t.Fatalf("fileAttachments missing")
	}
	if _, ok := payload["imageAttachments"].([]string); !ok {
		t.Fatalf("imageAttachments missing")
	}
}

func TestBuildVideoPayload_UsesGrokConfigFlags(t *testing.T) {
	temporary := false
	disableMemory := true
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != defaultCreatePostPath {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"post":{"id":"post-123"}}`))
	}))
	defer server.Close()
	cfg := &config.Config{
		GrokTemporary:         &temporary,
		GrokDisableMemory:     &disableMemory,
		GrokCustomInstruction: "video mode",
		GrokAPIBaseURL:        server.URL,
	}
	h := &Handler{cfg: cfg, client: New(cfg)}
	spec := ModelSpec{ID: "grok-imagine-video", UpstreamModel: "imagine-video-gen", ModelMode: "MODEL_MODE_AUTO", IsVideo: true}
	req := &ChatCompletionsRequest{}

	payload, err := h.buildChatPayload(context.Background(), "", spec, "make a clip", nil, nil, &VideoConfig{
		AspectRatio:    "3:2",
		VideoLength:    6,
		ResolutionName: "480p",
		Preset:         "normal",
	}, req)
	if err != nil {
		t.Fatalf("buildChatPayload error: %v", err)
	}
	if got, _ := payload["temporary"].(bool); got {
		t.Fatalf("temporary=%v want=false", got)
	}
	if got, _ := payload["disableMemory"].(bool); !got {
		t.Fatalf("disableMemory=%v want=true", got)
	}
	if got, _ := payload["customPersonality"].(string); got != "video mode" {
		t.Fatalf("customPersonality=%q want=%q", got, "video mode")
	}
	respMeta, ok := payload["responseMetadata"].(map[string]interface{})
	if !ok {
		t.Fatalf("responseMetadata missing")
	}
	reqDetails, ok := respMeta["requestModelDetails"].(map[string]interface{})
	if !ok || reqDetails["modelId"] != "imagine-video-gen" {
		t.Fatalf("requestModelDetails=%#v", reqDetails)
	}
	if got, _ := payload["modelMode"].(string); got != spec.ModelMode {
		t.Fatalf("modelMode=%q want=%q", got, spec.ModelMode)
	}
	if got, _ := payload["sendFinalMetadata"].(bool); !got {
		t.Fatalf("sendFinalMetadata=%v want=true", got)
	}
	if got, _ := payload["enableImageStreaming"].(bool); !got {
		t.Fatalf("enableImageStreaming=%v want=true", got)
	}
	if got, _ := payload["disableSearch"].(bool); got {
		t.Fatalf("disableSearch=%v want=false", got)
	}
	modelCfg, ok := respMeta["modelConfigOverride"].(map[string]interface{})
	if !ok {
		t.Fatalf("modelConfigOverride missing")
	}
	modelMap, ok := modelCfg["modelMap"].(map[string]interface{})
	if !ok {
		t.Fatalf("modelMap missing")
	}
	videoCfg, ok := modelMap["videoGenModelConfig"].(map[string]interface{})
	if !ok {
		t.Fatalf("videoGenModelConfig missing")
	}
	if got := videoCfg["videoLength"]; got != 6 {
		t.Fatalf("videoLength=%#v want=6", got)
	}
}

func TestDetectPublicBaseURL(t *testing.T) {
	req := httptest.NewRequest("GET", "http://internal/grok/v1/images/edits", nil)
	req.Header.Set("X-Forwarded-Proto", "https")
	req.Header.Set("X-Forwarded-Host", "example.com")
	if got := detectPublicBaseURL(req); got != "https://example.com" {
		t.Fatalf("detectPublicBaseURL()=%q want=%q", got, "https://example.com")
	}
}

func TestChatCompletionsRequest_UnmarshalLooseTypes(t *testing.T) {
	raw := []byte(`{
		"model":"grok-4.20-0309",
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
		"model":"grok-4.20-0309",
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
		"model":"grok-4.20-0309",
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
		"model":"grok-4.20-0309",
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
		"model":"grok-4.20-0309",
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

func TestApplyDefaultChatStream_StreamFalseOverride(t *testing.T) {
	stream := false
	h := &Handler{
		cfg: &config.Config{
			Stream: &stream,
		},
	}
	req := ChatCompletionsRequest{
		StreamProvided: false,
	}
	h.applyDefaultChatStream(&req)
	if req.Stream != false {
		t.Fatalf("stream=%v want=false (config override)", req.Stream)
	}
}

func TestImagesGenerationsRequest_UnmarshalLooseTypes(t *testing.T) {
	raw := []byte(`{
		"model":"grok-imagine-image",
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
