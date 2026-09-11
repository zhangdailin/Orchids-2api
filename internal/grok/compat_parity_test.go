package grok

import (
	"context"
	"github.com/goccy/go-json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"orchids-api/internal/config"
)

func TestResolveModel_RejectsRemovedAliases(t *testing.T) {
	for _, id := range []string{"gork-4.20-0309", "grok-4-1-thinking", "grok-imagine-1.0"} {
		if _, ok := ResolveModel(id); ok {
			t.Fatalf("ResolveModel(%s) should fail", id)
		}
	}
}

func TestResolveModel_Grok42AliasesRejected(t *testing.T) {
	if _, ok := ResolveModel("grok-4.2"); ok {
		t.Fatalf("ResolveModel(grok-4.2) should fail")
	}
	if _, ok := ResolveModel("grok-4-2"); ok {
		t.Fatalf("ResolveModel(grok-4-2) should fail")
	}
}

func TestResolveModel_Grok420Rejected(t *testing.T) {
	if _, ok := ResolveModel("grok-420"); ok {
		t.Fatalf("ResolveModel(grok-420) should fail")
	}
}

func TestResolveModel_CurrentBuildMappings(t *testing.T) {
	cases := []struct {
		modelID       string
		wantUpstream  string
		wantModelMode string
		wantModeID    string
	}{
		{modelID: "grok-4.5", wantUpstream: "grok-4.5", wantModelMode: ""},
		{modelID: "grok-4.6", wantUpstream: "grok-4.6", wantModelMode: ""},
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

func TestResolveModel_ImagineMappingsMatchGrok2API(t *testing.T) {
	cases := []struct {
		modelID       string
		wantUpstream  string
		wantModelMode string
	}{
		{modelID: "grok-imagine-image-lite", wantUpstream: "grok-imagine-image-lite", wantModelMode: "MODEL_MODE_FAST"},
		{modelID: "grok-imagine-image", wantUpstream: "grok-imagine-image", wantModelMode: "MODEL_MODE_AUTO"},
		{modelID: "grok-imagine-image-2.0", wantUpstream: "grok-imagine-image-2.0", wantModelMode: "MODEL_MODE_AUTO"},
		{modelID: "grok-imagine-image-quality", wantUpstream: "grok-imagine-image-quality-lite", wantModelMode: "MODEL_MODE_AUTO"},
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

func TestResolveModel_LegacyAppChatModelsDeprecated(t *testing.T) {
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
		if _, ok := ResolveModel(id); ok {
			t.Fatalf("ResolveModel(%s) should be removed", id)
		}
		if !IsDeprecatedModelID(id) {
			t.Fatalf("%s should be deprecated", id)
		}
	}
}

func TestResolveModel_AcceptsCurrentBuildModels(t *testing.T) {
	for _, id := range []string{
		"grok-4.5",
		"grok-4.6",
	} {
		if _, ok := ResolveModel(id); !ok {
			t.Fatalf("ResolveModel(%s) should succeed", id)
		}
	}
}

func TestGrok45RoutesToBuildCLI(t *testing.T) {
	spec, ok := ResolveModel("grok-4.5")
	if !ok {
		t.Fatal("ResolveModel(grok-4.5) = false, want true")
	}
	if !modelRoutedToCLI(spec, &config.Config{}) {
		t.Fatal("grok-4.5 should route through the official Build CLI OAuth path")
	}
}

func TestResolveModel_Grok420BetaHyphenAliasRejected(t *testing.T) {
	if _, ok := ResolveModel("grok-4-20-beta"); ok {
		t.Fatalf("ResolveModel(grok-4-20-beta) should fail")
	}
}

func TestResolveModel_RejectsUnknownImagineModel(t *testing.T) {
	if _, ok := ResolveModel("grok-imagine-2.0"); ok {
		t.Fatalf("ResolveModel(grok-imagine-2.0) should fail")
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

func TestChatCompletionsRequestValidate_LeavesSamplingToUpstream(t *testing.T) {
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
	if req.Temperature != nil {
		t.Fatalf("temperature default mismatch: got=%v", req.Temperature)
	}
	if req.TopP != nil {
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

	req.ToolChoice = map[string]interface{}{
		"type":     "function",
		"function": map[string]interface{}{},
	}
	if err := req.Validate(); err == nil {
		t.Fatal("expected malformed forced tool_choice error")
	}

	req.Tools = nil
	req.ToolChoice = "required"
	if err := req.Validate(); err == nil {
		t.Fatal("expected required tool_choice without tools error")
	}
}

func TestWebToolValidationRejectsUnsupportedDefinitions(t *testing.T) {
	base := ChatCompletionsRequest{
		Model:    "grok-4.20-0309",
		Messages: []ChatMessage{{Role: "user", Content: "hello"}},
	}
	for _, tools := range [][]ToolDef{
		{{Type: "function", Function: map[string]interface{}{"name": "invalid name"}}},
		{{Type: "function", Function: map[string]interface{}{"name": "weather"}}, {Type: "function", Function: map[string]interface{}{"name": "weather"}}},
		{{Type: "function", Function: map[string]interface{}{"name": "weather", "description": strings.Repeat("x", maxToolDescriptionBytes+1)}}},
	} {
		base.Tools = tools
		if err := validateWebToolDefinitions(base.Tools); err == nil {
			t.Fatalf("expected tool validation error for %#v", tools)
		}
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
		t.Fatalf("normalizeImageEditSize should reject unsupported edit size")
	}
}

func TestReplaceImageEditPlaceholders_MatchesGrok2API(t *testing.T) {
	got := replaceImageEditPlaceholders("blend @IMAGE1 with @image2 and keep @IMAGE3", []imageEditReference{
		{fileID: "file-a"},
		{fileID: "file-b"},
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

	payload := h.buildImageEditPayload(spec, "edit this", []string{"metadata-1"}, "2:3")
	if got, _ := payload["temporary"].(bool); got {
		t.Fatalf("temporary=%v want=false", got)
	}
	if got, _ := payload["disableMemory"].(bool); !got {
		t.Fatalf("disableMemory=%v want=true", got)
	}
	if got, _ := payload["customPersonality"].(string); got != "image mode" {
		t.Fatalf("customPersonality=%q want=%q", got, "image mode")
	}
	input := payload["mediaGenInput"].(map[string]interface{})["imageToImage"].(map[string]interface{})
	if input["aspectRatio"] != "2:3" || input["inputAssets"].([]string)[0] != "metadata-1" {
		t.Fatalf("image edit input=%#v", input)
	}
	if payload["responseMetadata"] != nil {
		t.Fatal("legacy image model override leaked")
	}
}

func TestBuildVideoPayload_UsesGrokConfigFlags(t *testing.T) {
	h := &Handler{}
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
	respMeta, ok := payload["responseMetadata"].(map[string]interface{})
	if !ok {
		t.Fatalf("responseMetadata missing")
	}
	if got, _ := payload["sendFinalMetadata"].(bool); !got {
		t.Fatalf("sendFinalMetadata=%v want=true", got)
	}
	if got, _ := payload["enableImageStreaming"].(bool); !got {
		t.Fatalf("enableImageStreaming=%v want=true", got)
	}
	modelCfg, ok := respMeta["modelConfigOverride"].(map[string]interface{})
	if !ok {
		t.Fatalf("modelConfigOverride missing")
	}
	modelMap, ok := modelCfg["modelMap"].(map[string]interface{})
	if !ok {
		t.Fatalf("modelMap missing")
	}
	if len(modelMap) != 0 {
		t.Fatalf("modelMap=%#v want empty", modelMap)
	}
	mediaInput := payload["mediaGenInput"].(map[string]interface{})
	videoCfg := mediaInput["textToVideo"].(map[string]interface{})
	if got := videoCfg["duration"]; got != 6 {
		t.Fatalf("duration=%#v want=6", got)
	}
	if got := payload["message"]; got != "make a clip --mode=custom" {
		t.Fatalf("message=%#v", got)
	}
	if got := payload["kind"]; got != "CONVERSATION_KIND_IMAGINE" {
		t.Fatalf("kind=%#v", got)
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

// An omitted n defaults to 1, matching OpenAI and grok2api. An explicit
// out-of-range value stays out of range so the handler can still reject it.
func TestImagesGenerationsRequest_OmittedNDefaultsToOne(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
		want int
	}{
		{"absent", `{"model":"grok-imagine-image","prompt":"hello"}`, 1},
		{"null", `{"model":"grok-imagine-image","prompt":"hello","n":null}`, 1},
		{"explicit", `{"model":"grok-imagine-image","prompt":"hello","n":3}`, 3},
		{"explicit zero", `{"model":"grok-imagine-image","prompt":"hello","n":0}`, 0},
		{"explicit eleven", `{"model":"grok-imagine-image","prompt":"hello","n":11}`, 11},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var req ImagesGenerationsRequest
			if err := json.Unmarshal([]byte(tc.body), &req); err != nil {
				t.Fatalf("unmarshal failed: %v", err)
			}
			if req.N != tc.want {
				t.Fatalf("n=%d want=%d", req.N, tc.want)
			}
		})
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
