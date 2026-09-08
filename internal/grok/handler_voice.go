package grok

import (
	"bytes"
	"fmt"
	"io"
	"math"
	"mime"
	"mime/multipart"
	"net/http"
	"net/url"
	"strings"
	"unicode/utf8"

	"github.com/goccy/go-json"
)

const (
	maxVoiceRequestBytes  = 64 << 20
	maxVoiceResponseBytes = 128 << 20
)

type ttsAPIRequest struct {
	Model                    string                 `json:"model"`
	Text                     string                 `json:"text"`
	VoiceID                  string                 `json:"voice_id,omitempty"`
	Language                 string                 `json:"language"`
	OutputFormat             map[string]interface{} `json:"output_format,omitempty"`
	Speed                    *float64               `json:"speed,omitempty"`
	OptimizeStreamingLatency interface{}            `json:"optimize_streaming_latency,omitempty"`
	TextNormalization        *bool                  `json:"text_normalization,omitempty"`
	WithTimestamps           *bool                  `json:"with_timestamps,omitempty"`
}

type openAISpeechRequest struct {
	Model                    string                 `json:"model"`
	Input                    string                 `json:"input"`
	Voice                    string                 `json:"voice"`
	ResponseFormat           string                 `json:"response_format"`
	Speed                    *float64               `json:"speed,omitempty"`
	Language                 string                 `json:"language,omitempty"`
	VoiceID                  string                 `json:"voice_id,omitempty"`
	OutputFormat             map[string]interface{} `json:"output_format,omitempty"`
	OptimizeStreamingLatency interface{}            `json:"optimize_streaming_latency,omitempty"`
	TextNormalization        *bool                  `json:"text_normalization,omitempty"`
	WithTimestamps           *bool                  `json:"with_timestamps,omitempty"`
}

type sttResponse struct {
	Text     string    `json:"text"`
	Language string    `json:"language"`
	Duration float64   `json:"duration"`
	Words    []sttWord `json:"words"`
}

type sttWord struct {
	Text    string  `json:"text"`
	Start   float64 `json:"start"`
	End     float64 `json:"end"`
	Speaker *int    `json:"speaker,omitempty"`
}

type consoleVoiceRequestError struct {
	status int
	code   string
	err    error
}

func (e *consoleVoiceRequestError) Error() string {
	if e == nil || e.err == nil {
		return "Console voice request failed"
	}
	return e.err.Error()
}

func voiceModelForCapability(modelID, capability string) (ModelSpec, bool) {
	modelID = normalizeModelID(modelID)
	spec, ok := ResolveModel(modelID)
	if !ok {
		return ModelSpec{}, false
	}
	switch capability {
	case "tts":
		return spec, spec.IsTTS
	case "stt":
		return spec, spec.IsSTT
	case "realtime":
		return spec, spec.IsRealtime
	default:
		return ModelSpec{}, false
	}
}

func (h *Handler) requireVoiceModel(w http.ResponseWriter, r *http.Request, modelID, capability string) (ModelSpec, bool) {
	if !requireAPIKeyModel(w, r, modelID) {
		return ModelSpec{}, false
	}
	spec, ok := voiceModelForCapability(modelID, capability)
	if !ok {
		writeResponsesAPIError(w, http.StatusBadRequest, "invalid_model", fmt.Sprintf("model %s does not support %s", modelID, capability))
		return ModelSpec{}, false
	}
	spec = h.applyPersistedRoute(r.Context(), spec)
	if err := h.ensureModelCapability(r.Context(), modelID, capability); err != nil {
		writeResponsesAPIError(w, http.StatusBadRequest, "invalid_model", modelValidationMessage(modelID, err))
		return ModelSpec{}, false
	}
	return spec, true
}

func validateTTSRequest(req *ttsAPIRequest) error {
	if req == nil {
		return fmt.Errorf("invalid TTS request")
	}
	req.Text = strings.TrimSpace(req.Text)
	req.Language = strings.TrimSpace(req.Language)
	req.VoiceID = strings.TrimSpace(req.VoiceID)
	if req.Text == "" || req.Language == "" {
		return fmt.Errorf("text and language are required")
	}
	if utf8.RuneCountInString(req.Text) > 15000 {
		return fmt.Errorf("text must not exceed 15000 characters")
	}
	if req.Speed != nil && (math.IsNaN(*req.Speed) || math.IsInf(*req.Speed, 0) || *req.Speed < 0.25 || *req.Speed > 4) {
		return fmt.Errorf("speed must be between 0.25 and 4.0")
	}
	if format := req.OutputFormat; format != nil {
		if codec, ok := format["codec"].(string); ok {
			format["codec"] = strings.ToLower(strings.TrimSpace(codec))
		}
		for _, key := range []string{"sample_rate", "bit_rate"} {
			if value, exists := format[key]; exists {
				number, ok := numericValue(value)
				if !ok || number <= 0 || math.Trunc(number) != number {
					return fmt.Errorf("output_format.%s must be a positive integer", key)
				}
			}
		}
	}
	if req.OptimizeStreamingLatency != nil {
		value, ok := integerValue(req.OptimizeStreamingLatency)
		if !ok || value < 0 || value > 4 {
			return fmt.Errorf("optimize_streaming_latency must be an integer from 0 to 4")
		}
		req.OptimizeStreamingLatency = fmt.Sprint(value)
	}
	return nil
}

func numericValue(value interface{}) (float64, bool) {
	switch typed := value.(type) {
	case float64:
		return typed, !math.IsNaN(typed) && !math.IsInf(typed, 0)
	case float32:
		return float64(typed), true
	case int:
		return float64(typed), true
	case int64:
		return float64(typed), true
	case json.Number:
		parsed, err := typed.Float64()
		return parsed, err == nil
	case string:
		var parsed json.Number = json.Number(strings.TrimSpace(typed))
		number, err := parsed.Float64()
		return number, err == nil
	default:
		return 0, false
	}
}

func integerValue(value interface{}) (int, bool) {
	number, ok := numericValue(value)
	if !ok || math.Trunc(number) != number {
		return 0, false
	}
	return int(number), true
}

// HandleTTS implements the Console-native POST /v1/tts contract.
func (h *Handler) HandleTTS(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodPost) {
		return
	}
	if !strings.HasPrefix(strings.ToLower(strings.TrimSpace(r.Header.Get("Content-Type"))), "application/json") {
		writeResponsesAPIError(w, http.StatusUnsupportedMediaType, "invalid_request", "TTS requires application/json")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxVoiceRequestBytes)
	var req ttsAPIRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeResponsesAPIError(w, http.StatusBadRequest, "invalid_request", "invalid TTS request")
		return
	}
	req.Model = normalizeModelID(firstNonEmpty(req.Model, "grok-voice-latest"))
	if _, ok := h.requireVoiceModel(w, r, req.Model, "tts"); !ok {
		return
	}
	modelID := req.Model
	if err := validateTTSRequest(&req); err != nil {
		writeResponsesAPIError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	// Model selects the local route; Console's /tts body does not accept it.
	req.Model = ""
	body, err := json.Marshal(req)
	if err != nil {
		writeResponsesAPIError(w, http.StatusInternalServerError, "internal_error", "failed to encode TTS request")
		return
	}
	h.forwardConsoleVoice(w, r, modelID, http.MethodPost, "tts", body, http.Header{
		"Content-Type": []string{"application/json"},
		"Accept":       []string{"*/*"},
	})
}

// HandleAudioSpeech maps the OpenAI /v1/audio/speech and /v1/audio/tasks
// request shape to Console TTS without changing the response bytes.
func (h *Handler) HandleAudioSpeech(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodPost) {
		return
	}
	if !strings.HasPrefix(strings.ToLower(strings.TrimSpace(r.Header.Get("Content-Type"))), "application/json") {
		writeResponsesAPIError(w, http.StatusUnsupportedMediaType, "invalid_request", "audio speech requires application/json")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxVoiceRequestBytes)
	var input openAISpeechRequest
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		writeResponsesAPIError(w, http.StatusBadRequest, "invalid_request", "invalid audio speech request")
		return
	}
	if strings.TrimSpace(input.Input) == "" {
		writeResponsesAPIError(w, http.StatusBadRequest, "invalid_request", "input is required")
		return
	}
	format := input.OutputFormat
	if format == nil {
		format = map[string]interface{}{}
	}
	if _, exists := format["codec"]; !exists {
		codec := mapOpenAIAudioFormat(input.ResponseFormat)
		if codec == "" {
			writeResponsesAPIError(w, http.StatusBadRequest, "invalid_request", "unsupported response_format")
			return
		}
		format["codec"] = codec
	}
	payload := ttsAPIRequest{
		Model: input.Model, Text: input.Input, Language: firstNonEmpty(input.Language, "en"),
		VoiceID: firstNonEmpty(input.VoiceID, mapOpenAIVoiceID(input.Voice)), OutputFormat: format,
		Speed: input.Speed, OptimizeStreamingLatency: input.OptimizeStreamingLatency,
		TextNormalization: input.TextNormalization, WithTimestamps: input.WithTimestamps,
	}
	body, err := json.Marshal(payload)
	if err != nil {
		writeResponsesAPIError(w, http.StatusInternalServerError, "internal_error", "failed to map audio speech request")
		return
	}
	subRequest := r.Clone(r.Context())
	subRequest.Body = io.NopCloser(bytes.NewReader(body))
	subRequest.ContentLength = int64(len(body))
	subRequest.Header = r.Header.Clone()
	subRequest.Header.Set("Content-Type", "application/json")
	h.HandleTTS(w, subRequest)
}

func mapOpenAIAudioFormat(value string) string {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "", "mp3":
		return "mp3"
	case "opus", "ogg":
		return "opus"
	case "aac":
		return "aac"
	case "flac":
		return "flac"
	case "wav", "wave":
		return "wav"
	case "pcm", "pcm16":
		return "pcm"
	default:
		return ""
	}
}

func mapOpenAIVoiceID(value string) string {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "alloy", "verse":
		return "ara"
	case "echo", "ballad":
		return "eve"
	case "fable", "coral":
		return "sal"
	case "onyx", "ash":
		return "rex"
	case "nova", "sage":
		return "leo"
	case "shimmer", "marin":
		return "sia"
	default:
		return strings.TrimSpace(value)
	}
}

// HandleTTSVoices proxies GET /v1/tts/voices and /v1/tts/voices/{voice_id}.
func (h *Handler) HandleTTSVoices(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodGet) {
		return
	}
	modelID := normalizeModelID(firstNonEmpty(r.URL.Query().Get("model"), "grok-voice-latest"))
	if _, ok := h.requireVoiceModel(w, r, modelID, "tts"); !ok {
		return
	}
	path := "tts/voices"
	if marker := strings.Index(r.URL.Path, "/tts/voices/"); marker >= 0 {
		voiceID := strings.Trim(strings.TrimSpace(r.URL.Path[marker+len("/tts/voices/"):]), "/")
		if voiceID == "" || strings.Contains(voiceID, "/") {
			writeResponsesAPIError(w, http.StatusBadRequest, "invalid_request", "voice_id is required")
			return
		}
		path += "/" + url.PathEscape(voiceID)
	}
	h.forwardConsoleVoice(w, r, modelID, http.MethodGet, path, nil, http.Header{"Accept": []string{"application/json"}})
}

// HandleSTT serves both the JSON/multipart HTTP API and the streaming
// WebSocket endpoint selected by GET Upgrade requests.
func (h *Handler) HandleSTT(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet {
		h.handleVoiceWebSocket(w, r, "stt", "grok-stt")
		return
	}
	if !requireMethod(w, r, http.MethodPost) {
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxVoiceRequestBytes)
	body, err := io.ReadAll(r.Body)
	if err != nil {
		writeResponsesAPIError(w, http.StatusRequestEntityTooLarge, "request_too_large", "STT request exceeds 64 MiB")
		return
	}
	contentType := strings.TrimSpace(r.Header.Get("Content-Type"))
	modelID, hasInput, upstreamBody, upstreamContentType, err := prepareSTTRequest(body, contentType)
	if err != nil {
		writeResponsesAPIError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	modelID = normalizeModelID(firstNonEmpty(modelID, "grok-stt"))
	if _, ok := h.requireVoiceModel(w, r, modelID, "stt"); !ok {
		return
	}
	if !hasInput {
		writeResponsesAPIError(w, http.StatusBadRequest, "invalid_request", "STT requires file or url")
		return
	}
	h.forwardConsoleVoice(w, r, modelID, http.MethodPost, "stt", upstreamBody, http.Header{
		"Content-Type": []string{upstreamContentType},
		"Accept":       []string{"application/json"},
	})
}

// HandleAudioTranscriptions maps the losslessly supported subset of OpenAI's
// transcription API to Console STT. Parameters that Console cannot represent
// are rejected instead of being silently discarded.
func (h *Handler) HandleAudioTranscriptions(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodPost) {
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxVoiceRequestBytes)
	body, err := io.ReadAll(r.Body)
	if err != nil {
		writeResponsesAPIError(w, http.StatusRequestEntityTooLarge, "request_too_large", "audio transcription request exceeds 64 MiB")
		return
	}
	modelID, hasInput, upstreamBody, upstreamContentType, responseFormat, err := prepareOpenAITranscriptionRequest(body, r.Header.Get("Content-Type"))
	if err != nil {
		writeResponsesAPIError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	modelID = normalizeModelID(firstNonEmpty(modelID, "grok-stt"))
	if _, ok := h.requireVoiceModel(w, r, modelID, "stt"); !ok {
		return
	}
	if !hasInput {
		writeResponsesAPIError(w, http.StatusBadRequest, "invalid_request", "audio transcription requires file or url")
		return
	}
	resp, sess, err := h.doConsoleVoice(r, modelID, http.MethodPost, "stt", upstreamBody, http.Header{
		"Content-Type": []string{upstreamContentType},
		"Accept":       []string{"application/json"},
	})
	if sess != nil {
		defer sess.Close()
	}
	if err != nil {
		writeConsoleVoiceRequestError(w, err)
		return
	}
	defer resp.Body.Close()
	data, readErr := io.ReadAll(io.LimitReader(resp.Body, maxVoiceResponseBytes+1))
	if readErr != nil {
		writeResponsesAPIError(w, http.StatusBadGateway, "upstream_error", "failed to read Console STT response")
		return
	}
	if len(data) > maxVoiceResponseBytes {
		writeResponsesAPIError(w, http.StatusBadGateway, "response_too_large", "Console STT response exceeds 128 MiB")
		return
	}
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		copyVoiceResponseHeaders(w.Header(), resp.Header)
		w.WriteHeader(resp.StatusCode)
		_, _ = w.Write(data)
		return
	}
	var result sttResponse
	if json.Unmarshal(data, &result) != nil {
		writeResponsesAPIError(w, http.StatusBadGateway, "upstream_error", "invalid Console STT response")
		return
	}
	writeOpenAITranscriptionResponse(w, responseFormat, result)
}

func prepareOpenAITranscriptionRequest(body []byte, contentType string) (string, bool, []byte, string, string, error) {
	mediaType, params, err := mime.ParseMediaType(strings.TrimSpace(contentType))
	if err != nil {
		return "", false, nil, "", "", fmt.Errorf("invalid Content-Type")
	}
	responseFormat := "json"
	switch strings.ToLower(mediaType) {
	case "application/json":
		var payload map[string]interface{}
		if json.Unmarshal(body, &payload) != nil || payload == nil {
			return "", false, nil, "", "", fmt.Errorf("invalid audio transcription JSON request")
		}
		if value, exists := payload["response_format"]; exists && value != nil {
			text, ok := value.(string)
			if !ok {
				return "", false, nil, "", "", fmt.Errorf("response_format must be a string")
			}
			if strings.TrimSpace(text) != "" {
				responseFormat = strings.ToLower(strings.TrimSpace(text))
			}
		}
		if value, exists := payload["prompt"]; exists && hasTranscriptionValues(value) {
			return "", false, nil, "", "", fmt.Errorf("prompt is not supported by Console STT")
		}
		if value, exists := payload["temperature"]; exists && !isZeroTranscriptionTemperature(value) {
			return "", false, nil, "", "", fmt.Errorf("non-zero temperature is not supported by Console STT")
		}
		if value, exists := payload["timestamp_granularities"]; exists && hasTranscriptionValues(value) {
			return "", false, nil, "", "", fmt.Errorf("timestamp_granularities is not supported by Console STT")
		}
		model, hasInput, upstreamBody, upstreamType, prepareErr := prepareSTTRequest(body, contentType)
		if prepareErr != nil {
			return "", false, nil, "", "", prepareErr
		}
		if err := validateOpenAITranscriptionFormat(responseFormat); err != nil {
			return "", false, nil, "", "", err
		}
		return model, hasInput, upstreamBody, upstreamType, responseFormat, nil
	case "multipart/form-data":
		boundary := strings.TrimSpace(params["boundary"])
		if boundary == "" {
			return "", false, nil, "", "", fmt.Errorf("multipart boundary is required")
		}
		model, hasInput, rewritten, rewrittenType, foundFormat, rewriteErr := rewriteOpenAITranscriptionMultipart(body, boundary)
		if rewriteErr != nil {
			return "", false, nil, "", "", rewriteErr
		}
		if foundFormat != "" {
			responseFormat = foundFormat
		}
		if err := validateOpenAITranscriptionFormat(responseFormat); err != nil {
			return "", false, nil, "", "", err
		}
		return model, hasInput, rewritten, rewrittenType, responseFormat, nil
	default:
		return "", false, nil, "", "", fmt.Errorf("audio transcription requires application/json or multipart/form-data")
	}
}

func rewriteOpenAITranscriptionMultipart(body []byte, boundary string) (string, bool, []byte, string, string, error) {
	reader := multipart.NewReader(bytes.NewReader(body), boundary)
	var output bytes.Buffer
	writer := multipart.NewWriter(&output)
	model, responseFormat, hasInput := "", "", false
	for {
		part, err := reader.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			return "", false, nil, "", "", fmt.Errorf("invalid audio transcription multipart request")
		}
		value, readErr := io.ReadAll(io.LimitReader(part, maxVoiceRequestBytes+1))
		_ = part.Close()
		if readErr != nil || len(value) > maxVoiceRequestBytes {
			return "", false, nil, "", "", fmt.Errorf("invalid audio transcription multipart part")
		}
		name := part.FormName()
		text := strings.TrimSpace(string(value))
		skip := false
		switch name {
		case "model":
			model = text
		case "file":
			hasInput = hasInput || len(value) > 0
		case "url":
			hasInput = hasInput || text != ""
		case "response_format":
			responseFormat, skip = strings.ToLower(text), true
		case "prompt":
			if text != "" {
				return "", false, nil, "", "", fmt.Errorf("prompt is not supported by Console STT")
			}
			skip = true
		case "temperature":
			if !isZeroTranscriptionTemperature(text) {
				return "", false, nil, "", "", fmt.Errorf("non-zero temperature is not supported by Console STT")
			}
			skip = true
		case "timestamp_granularities", "timestamp_granularities[]":
			if text != "" {
				return "", false, nil, "", "", fmt.Errorf("timestamp_granularities is not supported by Console STT")
			}
			skip = true
		}
		if skip {
			continue
		}
		destination, createErr := writer.CreatePart(part.Header)
		if createErr != nil {
			return "", false, nil, "", "", fmt.Errorf("failed to build Console STT multipart request")
		}
		if _, writeErr := destination.Write(value); writeErr != nil {
			return "", false, nil, "", "", fmt.Errorf("failed to build Console STT multipart request")
		}
	}
	if err := writer.Close(); err != nil {
		return "", false, nil, "", "", fmt.Errorf("failed to build Console STT multipart request")
	}
	return model, hasInput, output.Bytes(), writer.FormDataContentType(), responseFormat, nil
}

func validateOpenAITranscriptionFormat(value string) error {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "json", "verbose_json", "text":
		return nil
	default:
		return fmt.Errorf("response_format must be json, verbose_json, or text")
	}
}

func isZeroTranscriptionTemperature(value interface{}) bool {
	if value == nil || strings.TrimSpace(fmt.Sprint(value)) == "" {
		return true
	}
	number, ok := numericValue(value)
	return ok && number == 0
}

func hasTranscriptionValues(value interface{}) bool {
	switch typed := value.(type) {
	case nil:
		return false
	case []interface{}:
		return len(typed) > 0
	case []string:
		return len(typed) > 0
	case string:
		return strings.TrimSpace(typed) != ""
	default:
		return true
	}
}

func writeOpenAITranscriptionResponse(w http.ResponseWriter, responseFormat string, result sttResponse) {
	if responseFormat == "text" {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, result.Text)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	payload := map[string]interface{}{"text": result.Text}
	if responseFormat == "verbose_json" {
		payload["task"] = "transcribe"
		payload["language"] = result.Language
		payload["duration"] = result.Duration
		if len(result.Words) > 0 {
			words := make([]map[string]interface{}, 0, len(result.Words))
			for _, word := range result.Words {
				item := map[string]interface{}{"word": word.Text, "start": word.Start, "end": word.End}
				if word.Speaker != nil {
					item["speaker"] = *word.Speaker
				}
				words = append(words, item)
			}
			payload["words"] = words
		}
	}
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(payload)
}

func prepareSTTRequest(body []byte, contentType string) (string, bool, []byte, string, error) {
	mediaType, params, err := mime.ParseMediaType(contentType)
	if err != nil {
		return "", false, nil, "", fmt.Errorf("invalid Content-Type")
	}
	switch strings.ToLower(mediaType) {
	case "application/json":
		var payload map[string]interface{}
		if json.Unmarshal(body, &payload) != nil || payload == nil {
			return "", false, nil, "", fmt.Errorf("invalid STT JSON request")
		}
		model, _ := payload["model"].(string)
		urlValue, _ := payload["url"].(string)
		var multipartBody bytes.Buffer
		writer := multipart.NewWriter(&multipartBody)
		for _, key := range []string{"model", "url", "audio_format", "sample_rate", "language", "format", "multichannel", "channels", "diarize", "filler_words", "vad_threshold"} {
			value, exists := payload[key]
			if !exists || value == nil {
				continue
			}
			text := strings.TrimSpace(fmt.Sprint(value))
			if text != "" {
				_ = writer.WriteField(key, text)
			}
		}
		if terms, ok := payload["keyterm"].([]interface{}); ok {
			for _, term := range terms {
				if text := strings.TrimSpace(fmt.Sprint(term)); text != "" {
					_ = writer.WriteField("keyterm", text)
				}
			}
		}
		if err := writer.Close(); err != nil {
			return "", false, nil, "", fmt.Errorf("failed to build STT multipart request")
		}
		return model, strings.TrimSpace(urlValue) != "", multipartBody.Bytes(), writer.FormDataContentType(), nil
	case "multipart/form-data":
		boundary := strings.TrimSpace(params["boundary"])
		if boundary == "" {
			return "", false, nil, "", fmt.Errorf("multipart boundary is required")
		}
		reader := multipart.NewReader(bytes.NewReader(body), boundary)
		model, hasInput := "", false
		for {
			part, nextErr := reader.NextPart()
			if nextErr == io.EOF {
				break
			}
			if nextErr != nil {
				return "", false, nil, "", fmt.Errorf("invalid STT multipart request")
			}
			value, readErr := io.ReadAll(io.LimitReader(part, maxVoiceRequestBytes+1))
			_ = part.Close()
			if readErr != nil || len(value) > maxVoiceRequestBytes {
				return "", false, nil, "", fmt.Errorf("invalid STT multipart part")
			}
			switch part.FormName() {
			case "model":
				model = strings.TrimSpace(string(value))
			case "url":
				hasInput = hasInput || strings.TrimSpace(string(value)) != ""
			case "file":
				hasInput = hasInput || len(value) > 0
			}
		}
		return model, hasInput, body, contentType, nil
	default:
		return "", false, nil, "", fmt.Errorf("STT requires application/json or multipart/form-data")
	}
}

func (h *Handler) forwardConsoleVoice(w http.ResponseWriter, r *http.Request, modelID, method, path string, body []byte, headers http.Header) {
	resp, sess, err := h.doConsoleVoice(r, modelID, method, path, body, headers)
	if sess != nil {
		defer sess.Close()
	}
	if err != nil {
		writeConsoleVoiceRequestError(w, err)
		return
	}
	defer resp.Body.Close()
	copyVoiceResponseHeaders(w.Header(), resp.Header)
	if resp.ContentLength > maxVoiceResponseBytes {
		writeResponsesAPIError(w, http.StatusBadGateway, "response_too_large", "Console voice response exceeds 128 MiB")
		return
	}
	w.WriteHeader(resp.StatusCode)
	// Streaming TTS responses may not have a Content-Length. Copy them through
	// without buffering or silently cutting a valid audio stream.
	_, _ = io.Copy(w, resp.Body)
}

func (h *Handler) doConsoleVoice(r *http.Request, modelID, method, path string, body []byte, headers http.Header) (*http.Response, *chatAccountSession, error) {
	if h == nil || h.currentClient() == nil {
		return nil, nil, &consoleVoiceRequestError{
			status: http.StatusServiceUnavailable, code: "service_unavailable", err: fmt.Errorf("grok client not configured"),
		}
	}
	sess, err := h.openConsoleAccountSession(r.Context(), nil, modelID)
	if err != nil {
		return nil, nil, &consoleVoiceRequestError{
			status: http.StatusServiceUnavailable, code: "account_unavailable", err: fmt.Errorf("no available Grok Console account: %w", err),
		}
	}
	resp, err := h.currentClient().doConsoleDPoPRequestWithHeaders(withRateLimitAccount(r.Context(), sess.acc), sess.token, method, h.consoleURL(path), body, headers)
	if err != nil {
		if markAllGrokAccountStatuses(err) {
			h.markAccountStatus(r.Context(), sess.acc, err)
		}
		sess.Close()
		return nil, nil, &consoleVoiceRequestError{status: upstreamHTTPResponseStatus(err), code: "upstream_error", err: err}
	}
	return resp, sess, nil
}

func writeConsoleVoiceRequestError(w http.ResponseWriter, err error) {
	if typed, ok := err.(*consoleVoiceRequestError); ok {
		writeResponsesAPIError(w, typed.status, typed.code, typed.Error())
		return
	}
	writeResponsesAPIError(w, http.StatusBadGateway, "upstream_error", err.Error())
}

func copyVoiceResponseHeaders(destination, source http.Header) {
	for _, key := range []string{"Content-Type", "Content-Disposition", "X-Request-Id"} {
		for _, value := range source.Values(key) {
			destination.Add(key, value)
		}
	}
}
