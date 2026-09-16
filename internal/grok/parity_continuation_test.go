package grok

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/goccy/go-json"

	"orchids-api/internal/store"
)

func TestResolveNewBuildAndConsoleImageCapabilities(t *testing.T) {
	composer, ok := ResolveModel("grok-composer-2.5-fast")
	if !ok || composer.Upstream != UpstreamCLI || !composer.SupportsConversation() {
		t.Fatalf("composer spec=%#v ok=%v", composer, ok)
	}
	image, ok := ResolveModel("Console/grok-imagine-image-2.0")
	if !ok || image.Upstream != UpstreamConsole || !image.IsImage || image.SupportsConversation() {
		t.Fatalf("Console image spec=%#v ok=%v", image, ok)
	}
	video, ok := ResolveModel("Build/grok-imagine-video-1.5")
	if !ok || video.Upstream != UpstreamCLI || !video.IsVideo || video.SupportsConversation() {
		t.Fatalf("Build video spec=%#v ok=%v", video, ok)
	}
}

func TestImagesRequestPreservesAdvancedParameters(t *testing.T) {
	var req ImagesGenerationsRequest
	if err := json.Unmarshal([]byte(`{
		"model":"grok-imagine-image-2.0","prompt":"cat","n":"1",
		"partial_images":"2","aspect_ratio":"19.5:9","resolution":"2k",
		"quality":"medium","storage_options":null,"stream":"true"
	}`), &req); err != nil {
		t.Fatal(err)
	}
	if req.PartialImages == nil || *req.PartialImages != 2 || req.AspectRatio != "19.5:9" || req.Resolution != "2k" || req.Quality != "medium" {
		t.Fatalf("advanced image request fields lost: %#v", req)
	}
}

func TestNormalizeImageAspectRatioMatchesCompatibilitySet(t *testing.T) {
	for input, want := range map[string]string{
		"auto": "auto", "4:3": "4:3", "19.5:9": "19.5:9",
		"1536x1024": "3:2", "1024x1536": "2:3",
	} {
		got, err := normalizeImageAspectRatio(input, "")
		if err != nil || got != want {
			t.Fatalf("normalizeImageAspectRatio(%q)=(%q,%v), want %q", input, got, err, want)
		}
	}
	if _, err := normalizeImageAspectRatio("7:5", ""); err == nil {
		t.Fatal("unsupported ratio was accepted")
	}
}

func TestImagineWSRequestIncludesRequestedGenerationCount(t *testing.T) {
	payload := buildImagineWSRequestMessage("cat", "4:3", false, true, 3)
	item := payload["item"].(map[string]interface{})
	content := item["content"].([]map[string]interface{})
	properties := content[0]["properties"].(map[string]interface{})
	if properties["aspect_ratio"] != "4:3" || properties["num_generations"] != 3 {
		t.Fatalf("Imagine properties=%#v", properties)
	}
}

func TestBuildNamespaceAliasRestoredInJSONAndSSE(t *testing.T) {
	payload := map[string]interface{}{"tools": []interface{}{
		map[string]interface{}{"type": "namespace", "name": "crm", "tools": []interface{}{
			map[string]interface{}{"type": "function", "name": "lookup", "parameters": map[string]interface{}{"type": "object"}},
		}},
	}}
	aliases := collectBuildToolAliases(payload)
	if aliases["crm__lookup"].Namespace != "crm" {
		t.Fatalf("aliases=%#v", aliases)
	}

	jsonSource := io.NopCloser(strings.NewReader(`{"output":[{"type":"function_call","name":"crm__lookup","arguments":"{}"}]}`))
	converted, err := io.ReadAll(rewriteBuildToolAliasResponse(jsonSource, "application/json", aliases))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(converted), `"name":"lookup"`) || !strings.Contains(string(converted), `"namespace":"crm"`) {
		t.Fatalf("JSON alias was not restored: %s", converted)
	}

	sseSource := io.NopCloser(strings.NewReader("event: response.output_item.added\n" +
		"data: {\"type\":\"response.output_item.added\",\"item\":{\"type\":\"function_call\",\"name\":\"crm__lookup\"}}\n\n"))
	converted, err = io.ReadAll(rewriteBuildToolAliasResponse(sseSource, "text/event-stream", aliases))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(converted), `"namespace":"crm"`) || !strings.Contains(string(converted), `event: response.output_item.added`) {
		t.Fatalf("SSE alias was not restored: %s", converted)
	}
}

func TestAnthropicServerSearchHistoryUsesNativeResponsesItem(t *testing.T) {
	req := anthropicMessagesRequest{
		Model: "grok-4.5", MaxTokens: 64,
		Tools: []anthropicTool{{Type: "web_search_20250305", Name: "web_search"}},
		Messages: []anthropicMessage{{Role: "assistant", Content: []interface{}{
			map[string]interface{}{"type": "server_tool_use", "id": "srv_1", "name": "web_search", "input": map[string]interface{}{"query": "orchids"}},
			map[string]interface{}{"type": "web_search_tool_result", "tool_use_id": "srv_1", "content": []interface{}{
				map[string]interface{}{"type": "web_search_result", "url": "https://example.com/source"},
			}},
		}}, {Role: "user", Content: "continue"}},
	}
	chat, err := anthropicRequestToChat(req)
	if err != nil {
		t.Fatal(err)
	}
	payload, err := (&Handler{}).responsesPayloadFromChat(ModelSpec{UpstreamModel: "grok-4.5"}, &chat, true)
	if err != nil {
		t.Fatal(err)
	}
	input := payload["input"].([]interface{})
	call, _ := input[0].(map[string]interface{})
	if call["type"] != "web_search_call" || call["status"] != "completed" {
		t.Fatalf("native search call=%#v input=%#v", call, input)
	}
	action := call["action"].(map[string]interface{})
	if action["query"] != "orchids" || len(action["sources"].([]interface{})) != 1 {
		t.Fatalf("native search action=%#v", action)
	}
}

func TestTrustedConsoleImageURL(t *testing.T) {
	for _, value := range []string{"https://assets.grok.com/a.png", "https://imagine-public.x.ai/a.png", "http://127.0.0.1:8080/v1/a.png"} {
		if !trustedConsoleImageURL(value, "http://127.0.0.1:8080/v1") {
			t.Fatalf("trusted URL rejected: %s", value)
		}
	}
	if trustedConsoleImageURL("https://example.com/a.png", "https://console.x.ai/v1") {
		t.Fatal("untrusted Console image URL accepted")
	}
}

func TestConsoleImageEditRejectsMoreThanThreeInputs(t *testing.T) {
	var body bytes.Buffer
	form := multipart.NewWriter(&body)
	_ = form.WriteField("model", "console/grok-imagine-image-2.0")
	_ = form.WriteField("prompt", "edit")
	for index := 0; index < 4; index++ {
		part, err := form.CreateFormFile("image[]", fmt.Sprintf("input-%d.png", index))
		if err != nil {
			t.Fatal(err)
		}
		_, _ = part.Write([]byte("png"))
	}
	_ = form.Close()
	// The model must exist in the observed catalog for the request to reach the
	// input-count check; an empty store now rejects it as unknown first.
	h, s, mini := setupValidationHandler(t)
	defer func() {
		_ = s.Close()
		mini.Close()
	}()
	if err := s.CreateModel(context.Background(), &store.Model{
		Channel: "Grok", ModelID: "console/grok-imagine-image-2.0",
		Name: "Console Grok Imagine Image 2.0", Status: store.ModelStatusAvailable,
		Verified: true, Capabilities: []string{store.CapabilityImage, store.CapabilityImageEdit},
	}); err != nil {
		t.Fatalf("CreateModel() error = %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/v1/images/edits", &body)
	req.Header.Set("Content-Type", form.FormDataContentType())
	rec := httptest.NewRecorder()
	h.HandleImagesEdits(rec, req)
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "at most 3") {
		t.Fatalf("status=%d body=%q", rec.Code, rec.Body.String())
	}
}

func TestBuildToolSearchStreamHidesInternalArgumentEvents(t *testing.T) {
	payload := map[string]interface{}{"tools": []interface{}{
		map[string]interface{}{"type": "namespace", "name": "crm", "tools": []interface{}{
			map[string]interface{}{"type": "function", "name": "lookup", "parameters": map[string]interface{}{"type": "object"}},
		}},
		map[string]interface{}{"type": "tool_search", "execution": "client", "parameters": map[string]interface{}{"type": "object"}},
	}}
	aliases := collectBuildToolAliases(payload)
	source := strings.Join([]string{
		"event: response.output_item.added",
		`data: {"type":"response.output_item.added","item":{"id":"item_1","type":"function_call","call_id":"call_1","name":"tool_search","arguments":""}}`,
		"",
		"event: response.function_call_arguments.delta",
		`data: {"type":"response.function_call_arguments.delta","item_id":"item_1","delta":"{\"goal\":"}`,
		"",
		"event: response.function_call_arguments.done",
		`data: {"type":"response.function_call_arguments.done","item_id":"item_1","arguments":"{\"goal\":\"crm\"}"}`,
		"",
		"event: response.output_item.done",
		`data: {"type":"response.output_item.done","item":{"id":"item_1","type":"function_call","call_id":"call_1","name":"tool_search","arguments":"{\"goal\":\"crm\"}"}}`,
		"",
		"event: response.completed",
		`data: {"type":"response.completed","response":{"tools":[{"type":"function","name":"crm__lookup"},{"type":"function","name":"tool_search"}],"output":[{"type":"function_call","name":"tool_search","arguments":"{\"goal\":\"crm\"}"}]}}`,
		"",
		"data: [DONE]",
		"",
	}, "\n")
	converted, err := io.ReadAll(rewriteBuildToolAliasResponse(io.NopCloser(strings.NewReader(source)), "text/event-stream", aliases))
	if err != nil {
		t.Fatal(err)
	}
	text := string(converted)
	if strings.Contains(text, "response.function_call_arguments") || strings.Contains(text, `"name":"tool_search"`) {
		t.Fatalf("internal tool_search events leaked:\n%s", text)
	}
	for _, expected := range []string{`"type":"tool_search_call"`, `"goal":"crm"`, `"type":"namespace"`, `"name":"crm"`, "data: [DONE]"} {
		if !strings.Contains(text, expected) {
			t.Fatalf("missing %s:\n%s", expected, text)
		}
	}
}

func TestBuildVideoPayloadAndCreateResponse(t *testing.T) {
	job := &videoJob{Prompt: "animate", InputReferences: []string{
		"https://example.com/one.png", "https://example.com/two.png",
	}}
	payload, err := buildCLIVideoPayload(job, ModelSpec{UpstreamModel: "grok-imagine-video-1.5"}, &VideoConfig{
		VideoLength: 6, AspectRatio: "16:9", ResolutionName: "720p",
	})
	if err != nil {
		t.Fatal(err)
	}
	references := payload["reference_images"].([]map[string]interface{})
	if payload["model"] != "grok-imagine-video-1.5" || len(references) != 2 || references[0]["image_url"] != "https://example.com/one.png" {
		t.Fatalf("Build video payload=%#v", payload)
	}
	if id, err := parseBuildVideoCreate([]byte(`{"request_id":"video_123"}`)); err != nil || id != "video_123" {
		t.Fatalf("parseBuildVideoCreate()=(%q,%v)", id, err)
	}
	job.InputReferences = []string{"data:image/png;base64,AAAA"}
	if _, err := buildCLIVideoPayload(job, ModelSpec{UpstreamModel: "grok-imagine-video-1.5"}, &VideoConfig{}); err == nil {
		t.Fatal("Build video accepted a non-public reference URL")
	}
}

func TestTrustedBuildVideoURL(t *testing.T) {
	for _, value := range []string{"https://assets.grok.com/videos/a.mp4", "https://cdn.x.ai/a.mp4", "https://foo.videos.x.ai/a.mp4"} {
		if !trustedBuildVideoURL(value) {
			t.Fatalf("trusted Build video URL rejected: %s", value)
		}
	}
	for _, value := range []string{"http://assets.grok.com/a.mp4", "https://example.com/a.mp4"} {
		if trustedBuildVideoURL(value) {
			t.Fatalf("untrusted Build video URL accepted: %s", value)
		}
	}
}

func TestBuildVideo15AllowsText1080pButNotReferences(t *testing.T) {
	spec, ok := ResolveModel("build/grok-imagine-video-1.5")
	if !ok {
		t.Fatal("Build video model was not resolved")
	}
	cfg, err := validateVideoConfigForModel(&VideoConfig{
		VideoLength: 6, ResolutionName: "1080P", Preset: "normal", Size: "1280x720",
	}, spec, 0)
	if err != nil || cfg.ResolutionName != "1080p" {
		t.Fatalf("text 1080p config=%#v err=%v", cfg, err)
	}
	if _, err := validateVideoConfigForModel(&VideoConfig{
		VideoLength: 6, ResolutionName: "1080p", Preset: "normal", Size: "1280x720",
	}, spec, 1); err == nil {
		t.Fatal("Build reference-image 1080p was accepted")
	}
	legacy, _ := ResolveModel("grok-imagine-video")
	if _, err := validateVideoConfigForModel(&VideoConfig{
		VideoLength: 6, ResolutionName: "1080p", Preset: "normal", Size: "1280x720",
	}, legacy, 0); err == nil {
		t.Fatal("legacy Web video 1080p was accepted")
	}
}
