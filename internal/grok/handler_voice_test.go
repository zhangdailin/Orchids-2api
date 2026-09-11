package grok

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"orchids-api/internal/config"
)

func TestVoiceModelCapabilitiesAreIsolatedFromConversation(t *testing.T) {
	tests := []struct {
		model      string
		capability string
	}{
		{"grok-voice-latest", "tts"},
		{"grok-voice-think-fast-2.0", "realtime"},
		{"grok-stt", "stt"},
	}
	for _, test := range tests {
		spec, ok := voiceModelForCapability(test.model, test.capability)
		if !ok {
			t.Fatalf("%s does not expose %s", test.model, test.capability)
		}
		if spec.SupportsConversation() {
			t.Fatalf("voice-only model %s unexpectedly supports conversation", test.model)
		}
	}
	if _, ok := voiceModelForCapability("grok-stt", "tts"); ok {
		t.Fatal("STT model unexpectedly supports TTS")
	}
}

func TestValidateTTSRequest(t *testing.T) {
	validSpeed := 1.25
	valid := &ttsAPIRequest{
		Text: " hello ", Language: " en ", Speed: &validSpeed,
		OutputFormat:             map[string]interface{}{"codec": "MP3", "sample_rate": float64(24000)},
		OptimizeStreamingLatency: "4",
	}
	if err := validateTTSRequest(valid); err != nil {
		t.Fatalf("validateTTSRequest() error = %v", err)
	}
	if valid.Text != "hello" || valid.Language != "en" || valid.OutputFormat["codec"] != "mp3" || valid.OptimizeStreamingLatency != "4" {
		t.Fatalf("normalized request = %#v", valid)
	}
	badSpeed := 0.2
	if err := validateTTSRequest(&ttsAPIRequest{Text: "x", Language: "en", Speed: &badSpeed}); err == nil {
		t.Fatal("invalid TTS speed was accepted")
	}
	if err := validateTTSRequest(&ttsAPIRequest{Text: "x", Language: "en", OptimizeStreamingLatency: 2.5}); err == nil {
		t.Fatal("fractional optimize_streaming_latency was accepted")
	}
}

// output_format is rebuilt into the canonical Console shape: a bare
// sample_rate/bit_rate request defaults to mp3, unknown keys never reach
// upstream, and an empty object is omitted entirely.
func TestValidateTTSRequestNormalizesOutputFormat(t *testing.T) {
	for _, tc := range []struct {
		name  string
		input map[string]interface{}
		want  map[string]interface{}
	}{
		{"bare sample rate defaults to mp3", map[string]interface{}{"sample_rate": float64(24000)}, map[string]interface{}{"codec": "mp3", "sample_rate": 24000}},
		{"bare bit rate defaults to mp3", map[string]interface{}{"bit_rate": float64(128000)}, map[string]interface{}{"codec": "mp3", "bit_rate": 128000}},
		{"unknown keys dropped", map[string]interface{}{"codec": "wav", "format": "ignored"}, map[string]interface{}{"codec": "wav"}},
		{"empty object omitted", map[string]interface{}{}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := &ttsAPIRequest{Text: "x", Language: "en", OutputFormat: tc.input}
			if err := validateTTSRequest(req); err != nil {
				t.Fatalf("validateTTSRequest() error = %v", err)
			}
			if len(req.OutputFormat) != len(tc.want) {
				t.Fatalf("output_format = %#v want %#v", req.OutputFormat, tc.want)
			}
			for key, want := range tc.want {
				if req.OutputFormat[key] != want {
					t.Fatalf("output_format[%q] = %#v want %#v", key, req.OutputFormat[key], want)
				}
			}
		})
	}
}

func TestPrepareSTTJSONBuildsConsoleMultipart(t *testing.T) {
	model, hasInput, body, contentType, err := prepareSTTRequest([]byte(`{
		"model":"grok-stt","url":"https://example.com/audio.wav","language":"en",
		"diarize":true,"channels":2,"keyterm":["Codex","Grok"]
	}`), "application/json")
	if err != nil {
		t.Fatalf("prepareSTTRequest() error = %v", err)
	}
	if model != "grok-stt" || !hasInput {
		t.Fatalf("model=%q hasInput=%v", model, hasInput)
	}
	_, params, err := mime.ParseMediaType(contentType)
	if err != nil || params["boundary"] == "" {
		t.Fatalf("contentType=%q err=%v", contentType, err)
	}
	reader := multipart.NewReader(strings.NewReader(string(body)), params["boundary"])
	values := map[string][]string{}
	for {
		part, nextErr := reader.NextPart()
		if nextErr == io.EOF {
			break
		}
		if nextErr != nil {
			t.Fatal(nextErr)
		}
		value, _ := io.ReadAll(part)
		values[part.FormName()] = append(values[part.FormName()], string(value))
	}
	if values["url"][0] != "https://example.com/audio.wav" || values["diarize"][0] != "true" || len(values["keyterm"]) != 2 {
		t.Fatalf("multipart values = %#v", values)
	}
}

func TestConsoleDPoPRequestPreservesVoiceHeaders(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/tts" || r.Method != http.MethodPost {
			t.Fatalf("request = %s %s", r.Method, r.URL.Path)
		}
		if !strings.HasPrefix(r.Header.Get("Authorization"), "DPoP ") || r.Header.Get("DPoP") == "" {
			t.Fatalf("missing DPoP headers: %#v", r.Header)
		}
		if r.Header.Get("Content-Type") != "application/json" || r.Header.Get("Accept") != "audio/mpeg" {
			t.Fatalf("voice headers = %#v", r.Header)
		}
		w.Header().Set("Content-Type", "audio/mpeg")
		_, _ = w.Write([]byte("audio"))
	}))
	defer server.Close()

	client := New(&config.Config{
		GrokConsoleBaseURL: server.URL + "/v1",
		GrokEgressEnabled:  true,
		GrokEgressNodes:    []config.EgressNodeConfig{{Name: "direct-voice", Scope: "console"}},
	})
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	token := "console-sso"
	client.dpop.store(dpopCacheKey(token), dpopSession{
		accessToken: "access-token", privateKey: key, publicJWK: publicDPoPJWK(&key.PublicKey), expiresAt: time.Now().Add(time.Minute),
	})
	resp, err := client.doConsoleDPoPRequestWithHeaders(context.Background(), token, http.MethodPost, server.URL+"/v1/tts", []byte(`{"text":"hi"}`), http.Header{
		"Content-Type": []string{"application/json"}, "Accept": []string{"audio/mpeg"},
	})
	if err != nil {
		t.Fatalf("doConsoleDPoPRequestWithHeaders() error = %v", err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	if string(data) != "audio" {
		t.Fatalf("body=%q", data)
	}
}

func TestOpenAIAudioMappings(t *testing.T) {
	if got := mapOpenAIVoiceID("alloy"); got != "ara" {
		t.Fatalf("alloy maps to %q", got)
	}
	if got := mapOpenAIVoiceID("custom-voice"); got != "custom-voice" {
		t.Fatalf("custom voice maps to %q", got)
	}
	if got := mapOpenAIAudioFormat("wav"); got != "wav" {
		t.Fatalf("wav maps to %q", got)
	}
	if got := mapOpenAIAudioFormat("midi"); got != "" {
		t.Fatalf("midi maps to %q", got)
	}
}

func TestConsoleURLUsesConfiguredBase(t *testing.T) {
	h := &Handler{cfg: &config.Config{GrokConsoleBaseURL: "https://console.example/custom/v1/"}}
	if got := h.consoleURL("/tts"); got != "https://console.example/custom/v1/tts" {
		t.Fatalf("consoleURL()=%q", got)
	}
}

func TestVideoIDFromGenerationsPath(t *testing.T) {
	if got := videoIDFromPath("/v1/videos/generations/video_123/content"); got != "video_123" {
		t.Fatalf("videoIDFromPath()=%q", got)
	}
}

func TestVoiceOnlyModelsAreRejectedByConversationHandlers(t *testing.T) {
	h := NewHandler(&config.Config{}, nil)
	tests := []struct {
		name string
		path string
		body string
		call func(http.ResponseWriter, *http.Request)
		want string
	}{
		{
			name: "chat", path: "/v1/chat/completions",
			body: `{"model":"grok-voice-latest","messages":[{"role":"user","content":"hello"}],"stream":false}`,
			call: h.HandleChatCompletions, want: "does not support chat completions",
		},
		{
			name: "responses", path: "/v1/responses",
			body: `{"model":"grok-stt","input":"hello","stream":false}`,
			call: h.HandleResponses, want: "does not support responses",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, test.path, strings.NewReader(test.body))
			req.Header.Set("Content-Type", "application/json")
			rec := httptest.NewRecorder()
			test.call(rec, req)
			if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), test.want) {
				t.Fatalf("status=%d body=%q", rec.Code, rec.Body.String())
			}
		})
	}
}

func TestVoiceCompatibilityHandlersValidateBeforeUpstream(t *testing.T) {
	h := NewHandler(&config.Config{}, nil)
	tests := []struct {
		name   string
		method string
		path   string
		body   string
		header string
		call   func(http.ResponseWriter, *http.Request)
	}{
		{
			name: "audio speech input", method: http.MethodPost, path: "/v1/audio/speech",
			body: `{"model":"grok-voice-latest","voice":"alloy"}`, header: "application/json", call: h.HandleAudioSpeech,
		},
		{
			name: "native tts language", method: http.MethodPost, path: "/v1/tts",
			body: `{"model":"grok-voice-latest","text":"hello"}`, header: "application/json", call: h.HandleTTS,
		},
		{
			name: "realtime upgrade", method: http.MethodGet, path: "/v1/realtime",
			call: h.HandleRealtime,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			req := httptest.NewRequest(test.method, test.path, strings.NewReader(test.body))
			if test.header != "" {
				req.Header.Set("Content-Type", test.header)
			}
			rec := httptest.NewRecorder()
			test.call(rec, req)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status=%d body=%q", rec.Code, rec.Body.String())
			}
		})
	}
}

func TestPrepareOpenAITranscriptionJSON(t *testing.T) {
	model, hasInput, _, contentType, responseFormat, err := prepareOpenAITranscriptionRequest([]byte(`{
		"model":"grok-stt","url":"https://example.com/audio.wav","response_format":"verbose_json","temperature":0
	}`), "application/json")
	if err != nil {
		t.Fatalf("prepareOpenAITranscriptionRequest() error = %v", err)
	}
	if model != "grok-stt" || !hasInput || !strings.HasPrefix(contentType, "multipart/form-data;") || responseFormat != "verbose_json" {
		t.Fatalf("model=%q hasInput=%v contentType=%q responseFormat=%q", model, hasInput, contentType, responseFormat)
	}
	for _, body := range []string{
		`{"url":"https://example.com/a.wav","response_format":"srt"}`,
		`{"url":"https://example.com/a.wav","prompt":"spell Codex"}`,
		`{"url":"https://example.com/a.wav","temperature":0.2}`,
		`{"url":"https://example.com/a.wav","timestamp_granularities":["word"]}`,
	} {
		if _, _, _, _, _, err := prepareOpenAITranscriptionRequest([]byte(body), "application/json"); err == nil {
			t.Fatalf("unsupported transcription request was accepted: %s", body)
		}
	}
}

func TestPrepareOpenAITranscriptionMultipartStripsCompatibilityFields(t *testing.T) {
	var source bytes.Buffer
	writer := multipart.NewWriter(&source)
	_ = writer.WriteField("model", "grok-stt")
	_ = writer.WriteField("response_format", "text")
	_ = writer.WriteField("temperature", "0")
	part, err := writer.CreateFormFile("file", "sample.wav")
	if err != nil {
		t.Fatal(err)
	}
	_, _ = part.Write([]byte("audio"))
	_ = writer.Close()

	model, hasInput, upstreamBody, upstreamType, responseFormat, err := prepareOpenAITranscriptionRequest(source.Bytes(), writer.FormDataContentType())
	if err != nil {
		t.Fatalf("prepareOpenAITranscriptionRequest() error = %v", err)
	}
	if model != "grok-stt" || !hasInput || responseFormat != "text" {
		t.Fatalf("model=%q hasInput=%v responseFormat=%q", model, hasInput, responseFormat)
	}
	_, params, _ := mime.ParseMediaType(upstreamType)
	reader := multipart.NewReader(bytes.NewReader(upstreamBody), params["boundary"])
	seen := map[string]bool{}
	for {
		part, nextErr := reader.NextPart()
		if nextErr == io.EOF {
			break
		}
		if nextErr != nil {
			t.Fatal(nextErr)
		}
		seen[part.FormName()] = true
		_ = part.Close()
	}
	if !seen["model"] || !seen["file"] || seen["response_format"] || seen["temperature"] {
		t.Fatalf("rewritten multipart fields = %#v", seen)
	}
}

func TestWriteOpenAITranscriptionResponse(t *testing.T) {
	speaker := 2
	result := sttResponse{
		Text: "hello", Language: "en", Duration: 1.25,
		Words: []sttWord{{Text: "hello", Start: 0, End: 1.25, Speaker: &speaker}},
	}
	for _, test := range []struct {
		format      string
		contentType string
		contains    string
	}{
		{format: "text", contentType: "text/plain; charset=utf-8", contains: "hello"},
		{format: "json", contentType: "application/json", contains: `"text":"hello"`},
		{format: "verbose_json", contentType: "application/json", contains: `"word":"hello"`},
	} {
		rec := httptest.NewRecorder()
		writeOpenAITranscriptionResponse(rec, test.format, result)
		if rec.Code != http.StatusOK || rec.Header().Get("Content-Type") != test.contentType || !strings.Contains(rec.Body.String(), test.contains) {
			t.Fatalf("format=%s status=%d contentType=%q body=%q", test.format, rec.Code, rec.Header().Get("Content-Type"), rec.Body.String())
		}
	}
}

func TestDialConsoleVoiceWebSocketUsesDPoPAndConfiguredBase(t *testing.T) {
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/realtime" || r.URL.Query().Get("model") != "grok-voice-latest" {
			t.Errorf("request URL = %s", r.URL.String())
			http.Error(w, "bad URL", http.StatusBadRequest)
			return
		}
		if !strings.HasPrefix(r.Header.Get("Authorization"), "DPoP ") || r.Header.Get("DPoP") == "" {
			t.Errorf("missing DPoP websocket headers: %#v", r.Header)
			http.Error(w, "missing auth", http.StatusUnauthorized)
			return
		}
		connection, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Errorf("upgrade: %v", err)
			return
		}
		_ = connection.Close()
	}))
	defer server.Close()

	client := New(&config.Config{GrokConsoleBaseURL: server.URL + "/v1"})
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	token := "console-ws-sso"
	client.dpop.store(dpopCacheKey(token), dpopSession{
		accessToken: "access-token", privateKey: key, publicJWK: publicDPoPJWK(&key.PublicKey), expiresAt: time.Now().Add(time.Minute),
	})
	connection, response, releaseEgress, err := client.dialConsoleVoiceWebSocket(context.Background(), token, "realtime", "grok-voice-latest")
	defer releaseEgress()
	if response != nil && response.Body != nil {
		defer response.Body.Close()
	}
	if err != nil {
		t.Fatalf("dialConsoleVoiceWebSocket() error = %v", err)
	}
	if connection == nil {
		t.Fatal("dialConsoleVoiceWebSocket() returned nil connection")
	}
	_ = connection.Close()
}
