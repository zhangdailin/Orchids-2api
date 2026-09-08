package grok

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/goccy/go-json"
	"orchids-api/internal/config"
	"orchids-api/internal/store"
)

func TestRestrictionsToolsAreProviderScoped(t *testing.T) {
	tools := make([]ToolDef, 129)
	for index := range tools {
		tools[index] = ToolDef{Type: "function", Function: map[string]interface{}{"name": fmt.Sprintf("tool_%d", index)}}
	}
	tools[0].Function["name"] = strings.Repeat("Long", 30)
	tools[0].Function["description"] = strings.Repeat("d", maxToolDescriptionBytes+1)
	req := ChatCompletionsRequest{Model: "grok-4.6", Messages: []ChatMessage{{Role: "user", Content: "hi"}}, Tools: tools}
	if err := req.Validate(); err != nil {
		t.Fatal(err)
	}
	if err := validateWebToolDefinitions(tools); err == nil {
		t.Fatal("Web count/length guard must remain")
	}
	spec, _ := ResolveModel("grok-4.6")
	payload, err := (&Handler{}).responsesPayloadFromChat(spec, &req, true)
	if err != nil || len(interfaceMaps(payload["tools"])) != 129 {
		t.Fatalf("native tools rejected: %v", err)
	}
	for _, test := range []ToolDef{tools[0], {Type: "function", Function: map[string]interface{}{"name": "short", "description": strings.Repeat("d", maxToolDescriptionBytes+1)}}} {
		if err := validateWebToolDefinitions([]ToolDef{test}); err == nil {
			t.Fatal("Web individual limits lost")
		}
	}
}

func TestRestrictionsToolNamesRemainCaseSensitiveAndRoundTrip(t *testing.T) {
	tools := []ToolDef{{Type: "function", Function: map[string]interface{}{"name": "ReadFile"}}, {Type: "function", Function: map[string]interface{}{"name": "readfile"}}}
	req := ChatCompletionsRequest{Model: "grok-4.6", Messages: []ChatMessage{{Role: "user", Content: "hi"}}, Tools: tools,
		ToolChoice: map[string]interface{}{"type": "function", "function": map[string]interface{}{"name": "ReadFile"}}}
	if err := req.Validate(); err != nil {
		t.Fatal(err)
	}
	if err := validateWebToolDefinitions(tools); err != nil {
		t.Fatal(err)
	}
	req.ToolChoice.(map[string]interface{})["function"].(map[string]interface{})["name"] = "READFILE"
	if err := req.Validate(); err == nil {
		t.Fatal("forced names must match exactly")
	}
	if err := validateToolDefinitions(append(tools, tools[0])); err == nil {
		t.Fatal("exact duplicates must fail")
	}
	longName := strings.Repeat("Long", 40)
	declarations := []map[string]interface{}{{"type": "function", "name": "a b"}, {"type": "function", "name": "a_b_2"}, {"type": "function", "name": "a@b"}, {"type": "function", "name": longName}, {"type": "function", "name": "ReadFile"}, {"type": "function", "name": "readfile"}}
	payload := map[string]interface{}{"tools": declarations, "tool_choice": map[string]interface{}{"type": "function", "name": longName}, "input": []interface{}{map[string]interface{}{"type": "function_call", "name": longName, "call_id": "call_x", "arguments": "{}"}}}
	aliases := collectBuildToolAliases(payload)
	if err := normalizeBuildResponsesPayload(payload); err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, tool := range interfaceMaps(payload["tools"]) {
		name := tool["name"].(string)
		if seen[name] || len(name) > 128 {
			t.Fatalf("invalid alias %q", name)
		}
		seen[name] = true
	}
	alias := payload["tool_choice"].(map[string]interface{})["name"].(string)
	if interfaceMaps(payload["input"])[0]["name"] != alias {
		t.Fatal("history and declaration aliases differ")
	}
	raw, _ := json.Marshal(map[string]interface{}{"type": "function_call", "name": alias, "call_id": "call_x", "arguments": "{}"})
	var restored map[string]interface{}
	_ = json.Unmarshal(rewriteBuildToolAliasesJSON(raw, aliases), &restored)
	if restored["name"] != longName {
		t.Fatalf("round trip lost original name: %v", restored)
	}
}

func TestRestrictionsReasoningAliasesReachWire(t *testing.T) {
	for _, test := range []struct{ model, effort, want string }{{"grok-4.6", "max", "max"}, {"grok-4.5", "max", "max"}, {"grok-4.5", "xhigh", "xhigh"}, {"grok-3-mini", "medium", "medium"}, {"grok-3-mini-fast", "minimal", "minimal"}} {
		spec := ModelSpec{ID: test.model, UpstreamModel: test.model, Upstream: UpstreamCLI}
		payload := map[string]interface{}{"reasoning": map[string]interface{}{"effort": test.effort, "summary": "auto"}}
		if err := validatePayloadReasoning(payload); err != nil {
			t.Fatal(test, err)
		}
		if payload["reasoning"].(map[string]interface{})["effort"] != test.want {
			t.Fatal(test, payload)
		}
		request := &ChatCompletionsRequest{Model: test.model, Messages: []ChatMessage{{Role: "user", Content: "hi"}}, ReasoningEffort: &test.effort}
		if err := request.Validate(); err != nil {
			t.Fatal(test, err)
		}
		chat, err := (&Handler{}).responsesPayloadFromChat(spec, request, true)
		if err != nil || chat["reasoning"].(map[string]interface{})["effort"] != test.want {
			t.Fatal(test, chat, err)
		}
	}
}

func TestRestrictionsEmptyAndImageToolOutputs(t *testing.T) {
	for _, content := range []interface{}{"", []interface{}{map[string]interface{}{"type": "image_url", "image_url": map[string]interface{}{"url": "https://example.com/a.png", "detail": "high"}}}} {
		messages := []ChatMessage{{Role: "tool", ToolCallID: "call_a", Content: content}}
		if err := validateChatMessages(messages); err != nil {
			t.Fatal(err)
		}
		input, _ := responsesInputFromChatMessages(messages)
		item := input[0].(map[string]interface{})
		if item["call_id"] != "call_a" {
			t.Fatal(item)
		}
		if content == "" && item["output"] != "" {
			t.Fatal("empty result changed")
		}
		if parts, ok := item["output"].([]interface{}); ok {
			if parts[0].(map[string]interface{})["detail"] != "high" {
				t.Fatal(parts)
			}
		}
	}
	if err := validateChatMessages([]ChatMessage{{Role: "tool", Content: ""}}); err == nil {
		t.Fatal("missing call ID accepted")
	}
	image := map[string]interface{}{"type": "image_url", "image_url": map[string]interface{}{"url": "https://example.com/tool.png"}}
	for _, role := range []string{"tool", "assistant"} {
		message := ChatMessage{Role: role, ToolCallID: "call_a", Content: []interface{}{image}}
		if role == "assistant" {
			message.ToolCalls = []ToolCall{{ID: "call_a", Function: map[string]interface{}{"name": "read", "arguments": "{}"}}}
		}
		_, attachments, err := extractMessageAndAttachmentsWithTools([]ChatMessage{message}, false, []ToolDef{{Type: "function", Function: map[string]interface{}{"name": "read"}}}, nil, true)
		if err != nil || len(attachments) != 1 || attachments[0].Data != "https://example.com/tool.png" {
			t.Fatalf("Web %s image history lost: %v %v", role, attachments, err)
		}
	}
}

func TestRestrictionsConsoleChatRoutesImagesNatively(t *testing.T) {
	received := make(chan map[string]interface{}, 1)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var payload map[string]interface{}
		_ = json.NewDecoder(r.Body).Decode(&payload)
		received <- payload
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"resp_img","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"image received"}]}]}`)
	}))
	defer upstream.Close()
	h, s, mini := setupValidationHandler(t)
	defer s.Close()
	defer mini.Close()
	model := "console/grok-4.3"
	if err := s.CreateModel(context.Background(), &store.Model{Channel: "Grok", ModelID: model, Name: model, Status: store.ModelStatusAvailable, Verified: true}); err != nil {
		t.Fatal(err)
	}
	acc := &store.Account{AccountType: "grok", GrokProvider: ProviderConsole, ClientCookie: "sso=restriction-console", Enabled: true, Subscription: "super", Weight: 1, GrokModels: []string{"grok-4.3"}, GrokModelsSyncedAt: time.Now()}
	if err := s.CreateAccount(context.Background(), acc); err != nil {
		t.Fatal(err)
	}
	h.cfg = &config.Config{GrokConsoleBaseURL: upstream.URL + "/v1"}
	h.client = New(h.cfg)
	h.client.httpClient = upstream.Client()
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	h.client.dpop.store(dpopCacheKey(acc.ClientCookie), dpopSession{accessToken: "access", privateKey: key, publicJWK: publicDPoPJWK(&key.PublicKey), expiresAt: time.Now().Add(time.Minute)})
	body := `{"model":"console/grok-4.3","stream":false,"messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"https://example.com/pixel.png","detail":"high"}}]}]}`
	rec := httptest.NewRecorder()
	h.HandleChatCompletions(rec, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body)))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "image received") {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
	select {
	case payload := <-received:
		input := interfaceMaps(payload["input"])
		parts := interfaceMaps(input[0]["content"])
		if parts[0]["type"] != "input_image" || parts[0]["detail"] != "high" {
			t.Fatal(payload)
		}
	default:
		t.Fatal("Console was never called")
	}
}

func TestRestrictionsScopedCooldownAndPacing(t *testing.T) {
	old := teamCooldown
	teamCooldown = newTeamCooldownRegistry()
	defer func() { teamCooldown = old }()
	endpointRateLimiterMu.Lock()
	oldPacing := endpointRateLimiters
	endpointRateLimiters = map[string]*tokenBucket{}
	endpointRateLimiterMu.Unlock()
	defer func() {
		endpointRateLimiterMu.Lock()
		endpointRateLimiters = oldPacing
		endpointRateLimiterMu.Unlock()
	}()
	ctxA := withRateLimitAccount(context.Background(), &store.Account{ID: 1, TeamID: "restriction-team-a"})
	ctxSibling := withRateLimitAccount(context.Background(), &store.Account{ID: 2, TeamID: "restriction-team-a"})
	ctxB := withRateLimitAccount(context.Background(), &store.Account{ID: 3, TeamID: "restriction-team-b"})
	noteScopedRateLimit(ctxA, ProviderConsole, "tokenA", "grok-4.3", 429, http.Header{"Retry-After": []string{"2"}}, []byte(`{"error":"limited"}`))
	if remaining := teamCooldown.RetryAfterFor(RateLimitScopeRPM, ProviderConsole+":team:restriction-team-a", "grok-4.3"); remaining <= time.Second || remaining > 2*time.Second {
		t.Fatal(remaining)
	}
	for _, test := range []struct {
		ctx             context.Context
		provider, model string
		blocked         bool
	}{{ctxSibling, ProviderConsole, "grok-4.3", true}, {ctxB, ProviderConsole, "grok-4.3", false}, {ctxA, ProviderBuild, "grok-4.3", false}, {ctxA, ProviderConsole, "grok-4.6", false}} {
		ctx, cancel := context.WithTimeout(test.ctx, 20*time.Millisecond)
		err := waitScopedRateLimit(ctx, test.provider, "unused", test.model, 0)
		cancel()
		if (err != nil) != test.blocked {
			t.Fatalf("%s %s blocked=%v: %v", test.provider, test.model, test.blocked, err)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	for i := 0; i < 100; i++ {
		if err := waitScopedRateLimit(ctx, ProviderWeb, "unlimited-default", "m", 0); err != nil {
			t.Fatal(err)
		}
	}
	if err := waitScopedRateLimit(ctx, ProviderWeb, "paced-account-a", "m", 0.1); err != nil {
		t.Fatal(err)
	}
	short, stop := context.WithTimeout(ctx, 20*time.Millisecond)
	defer stop()
	if err := waitScopedRateLimit(short, ProviderWeb, "paced-account-a", "m", 0.1); err == nil {
		t.Fatal("configured pace ignored")
	}
	if err := waitScopedRateLimit(ctx, ProviderWeb, "paced-account-b", "m", 0.1); err != nil {
		t.Fatal("unrelated account blocked", err)
	}
}

func TestRestrictionsImageEditAspectRatios(t *testing.T) {
	for size, want := range map[string]string{"auto": "auto", "1024x1024": "1:1", "1024x1536": "2:3", "1536x1024": "3:2"} {
		if _, err := normalizeImageEditSize(size); err != nil {
			t.Fatal(err)
		}
		ratio, err := normalizeImageAspectRatio("", size)
		if err != nil || ratio != want {
			t.Fatal(size, ratio, err)
		}
		payload := (&Handler{}).buildImageEditPayload(ModelSpec{UpstreamModel: "imagine-image-edit"}, "edit", []string{"metadata-id"}, ratio)
		input := payload["mediaGenInput"].(map[string]interface{})["imageToImage"].(map[string]interface{})
		if input["aspectRatio"] != want {
			t.Fatal(input)
		}
	}
}

func TestRestrictionsBuildVideoAcceptsMultipartButRejectsInvalidData(t *testing.T) {
	data, _ := base64.StdEncoding.DecodeString("iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mNk+A8AAQUBAScY42YAAAAASUVORK5CYII=")
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	part, _ := writer.CreateFormFile("input_reference", "pixel.png")
	_, _ = part.Write(data)
	_ = writer.WriteField("prompt", "animate")
	_ = writer.Close()
	r := httptest.NewRequest(http.MethodPost, "/v1/videos", &body)
	r.Header.Set("Content-Type", writer.FormDataContentType())
	req, err := parseVideosRequest(r)
	if err != nil {
		t.Fatal(err)
	}
	defer r.MultipartForm.RemoveAll()
	refs, err := (&Handler{}).resolveBuildVideoReferences(context.Background(), req.InputReferences, "owner")
	if err != nil {
		t.Fatal(err)
	}
	payload, err := buildCLIVideoPayload(&videoJob{Prompt: "animate", InputReferences: refs}, ModelSpec{Upstream: UpstreamCLI}, &VideoConfig{VideoLength: 6, AspectRatio: "1:1", ResolutionName: "720p"})
	if err != nil || interfaceMaps(payload["reference_images"])[0]["image_url"] != refs[0] {
		t.Fatal(payload, err)
	}
	for _, value := range []string{"file:///secret.png", "http://example.com/p.png", "data:image/png;base64,broken", dataURIFromBytes("image/jpeg", data), dataURIFromBytes("image/png", []byte("not an image"))} {
		if _, err := validateBuildVideoReference(value); err == nil {
			t.Fatal("invalid reference accepted", value)
		}
	}
}

func TestRestrictionsBuildChatLongToolNameEndToEnd(t *testing.T) {
	received := make(chan map[string]interface{}, 1)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var payload map[string]interface{}
		_ = json.NewDecoder(r.Body).Decode(&payload)
		received <- payload
		choice, _ := payload["tool_choice"].(map[string]interface{})
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]interface{}{"id": "resp_tool", "status": "completed", "output": []interface{}{map[string]interface{}{"type": "function_call", "call_id": "call_long", "name": choice["name"], "arguments": "{}"}}})
	}))
	defer upstream.Close()
	h, s, mini := setupValidationHandler(t)
	defer s.Close()
	defer mini.Close()
	model := "grok-4.6"
	if err := s.CreateModel(context.Background(), &store.Model{Channel: "Grok", ModelID: model, Name: model, Status: store.ModelStatusAvailable, Verified: true}); err != nil {
		t.Fatal(err)
	}
	acc := &store.Account{AccountType: "grok", GrokProvider: ProviderBuild, CredentialType: "oauth", Enabled: true, OAuthAccessToken: jwtWithClaims(t, `{"sub":"restriction-user","team_id":"restriction-build"}`), OAuthExpiresAt: time.Now().Add(time.Hour), GrokModels: []string{model}, GrokModelsSyncedAt: time.Now()}
	if err := s.CreateAccount(context.Background(), acc); err != nil {
		t.Fatal(err)
	}
	h.cfg = &config.Config{GrokCLIBaseURL: upstream.URL + "/v1"}
	h.cliClient = NewCLIClient(h.cfg)
	h.cliClient.httpClient = upstream.Client()
	h.cliClient.oauth.httpClient = upstream.Client()
	longName := strings.Repeat("Tool", 50)
	tools := make([]ToolDef, 129)
	for i := range tools {
		tools[i] = ToolDef{Type: "function", Function: map[string]interface{}{"name": fmt.Sprintf("tool_%d", i)}}
	}
	tools[0].Function["name"] = longName
	body, _ := json.Marshal(ChatCompletionsRequest{Model: model, Messages: []ChatMessage{{Role: "user", Content: "use the tool"}}, Tools: tools, ToolChoice: map[string]interface{}{"type": "function", "function": map[string]interface{}{"name": longName}}})
	rec := httptest.NewRecorder()
	h.HandleChatCompletions(rec, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(body)))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), longName) || !strings.Contains(rec.Body.String(), "call_long") {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
	select {
	case payload := <-received:
		if len(interfaceMaps(payload["tools"])) != 129 {
			t.Fatal("tool list truncated")
		}
		if name := parseLooseStringAny(payload["tool_choice"].(map[string]interface{})["name"]); name == "" || len(name) > 128 {
			t.Fatal("invalid wire alias", name)
		}
	default:
		t.Fatal("Build was not called")
	}
}

func TestRestrictionsImageEditUsesMetadataAndRatioOnWire(t *testing.T) {
	requests := make(chan map[string]interface{}, 2)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var payload map[string]interface{}
		_ = json.NewDecoder(r.Body).Decode(&payload)
		payload["test_path"] = r.URL.Path
		payload["test_referer"] = r.Header.Get("Referer")
		requests <- payload
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == defaultUploadFilePath {
			_, _ = io.WriteString(w, `{"fileMetadataId":"metadata-edit","fileUri":"https://assets.grok.com/image"}`)
			return
		}
		_, _ = io.WriteString(w, `{"result":{}}`)
	}))
	defer upstream.Close()
	client := New(&config.Config{GrokAPIBaseURL: upstream.URL})
	client.httpClient = upstream.Client()
	h := &Handler{client: client}
	payload, err := h.buildImageEditPayloadFromInputs(context.Background(), "edit-token", ModelSpec{UpstreamModel: "imagine-image-edit"}, "edit @IMAGE1", []string{"data:image/png;base64,AA=="}, "3:2")
	if err != nil {
		t.Fatal(err)
	}
	resp, err := client.doRESTChat(context.Background(), "edit-token", payload)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	first, second := <-requests, <-requests
	if first["test_path"] != defaultUploadFilePath || second["test_path"] != defaultChatPath || second["test_referer"] != upstream.URL+"/imagine" {
		t.Fatal(first, second)
	}
	input := second["mediaGenInput"].(map[string]interface{})["imageToImage"].(map[string]interface{})
	if input["inputAssets"].([]interface{})[0] != "metadata-edit" || input["aspectRatio"] != "3:2" || input["prompt"] != "edit @metadata-edit" {
		t.Fatal(input)
	}
}
