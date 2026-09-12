package grok

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/goccy/go-json"

	"orchids-api/internal/middleware"
	"orchids-api/internal/store"
)

type captureResponseWriter struct {
	header http.Header
	body   bytes.Buffer
	code   int
}

func newCaptureResponseWriter() *captureResponseWriter {
	return &captureResponseWriter{header: make(http.Header), code: http.StatusOK}
}

func (w *captureResponseWriter) Header() http.Header {
	return w.header
}

func (w *captureResponseWriter) WriteHeader(code int) {
	if code != 0 {
		w.code = code
	}
}

func (w *captureResponseWriter) Write(p []byte) (int, error) {
	return w.body.Write(p)
}

func (w *captureResponseWriter) Flush() {}

type ResponsesCreateRequest struct {
	Model              string                   `json:"model"`
	Input              interface{}              `json:"input"`
	Instructions       string                   `json:"instructions,omitempty"`
	Stream             bool                     `json:"stream,omitempty"`
	StreamProvided     bool                     `json:"-"`
	Reasoning          map[string]interface{}   `json:"reasoning,omitempty"`
	Temperature        *float64                 `json:"temperature,omitempty"`
	TopP               *float64                 `json:"top_p,omitempty"`
	MaxOutputTokens    *int                     `json:"max_output_tokens,omitempty"`
	Tools              []map[string]interface{} `json:"tools,omitempty"`
	ToolChoice         interface{}              `json:"tool_choice,omitempty"`
	ParallelToolCalls  *bool                    `json:"parallel_tool_calls,omitempty"`
	PreviousResponseID string                   `json:"previous_response_id,omitempty"`
	Store              *bool                    `json:"store,omitempty"`
	Metadata           map[string]interface{}   `json:"metadata,omitempty"`
	Truncation         string                   `json:"truncation,omitempty"`
	Include            []string                 `json:"include,omitempty"`
	Background         *bool                    `json:"background,omitempty"`
	PromptCacheKey     string                   `json:"prompt_cache_key,omitempty"`
}

func (r *ResponsesCreateRequest) UnmarshalJSON(data []byte) error {
	type rawResponsesCreateRequest struct {
		Model              interface{}              `json:"model"`
		Input              interface{}              `json:"input"`
		Instructions       interface{}              `json:"instructions,omitempty"`
		Stream             interface{}              `json:"stream,omitempty"`
		Reasoning          map[string]interface{}   `json:"reasoning,omitempty"`
		Temperature        interface{}              `json:"temperature,omitempty"`
		TopP               interface{}              `json:"top_p,omitempty"`
		MaxOutputTokens    interface{}              `json:"max_output_tokens,omitempty"`
		Tools              []map[string]interface{} `json:"tools,omitempty"`
		ToolChoice         interface{}              `json:"tool_choice,omitempty"`
		ParallelToolCalls  interface{}              `json:"parallel_tool_calls,omitempty"`
		PreviousResponseID interface{}              `json:"previous_response_id,omitempty"`
		Store              interface{}              `json:"store,omitempty"`
		Metadata           map[string]interface{}   `json:"metadata,omitempty"`
		Truncation         interface{}              `json:"truncation,omitempty"`
		Include            []string                 `json:"include,omitempty"`
		Background         interface{}              `json:"background,omitempty"`
		PromptCacheKey     interface{}              `json:"prompt_cache_key,omitempty"`
	}

	var raw rawResponsesCreateRequest
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	stream, err := parseLooseBoolAny(raw.Stream)
	if err != nil {
		return err
	}
	temp, err := parseLooseFloatAny(raw.Temperature)
	if err != nil {
		return err
	}
	topP, err := parseLooseFloatAny(raw.TopP)
	if err != nil {
		return err
	}
	maxOutputTokens, err := parseLooseIntAny(raw.MaxOutputTokens)
	if err != nil {
		return err
	}
	var rawMap map[string]json.RawMessage
	_ = json.Unmarshal(data, &rawMap)
	_, streamProvided := rawMap["stream"]
	var parallel *bool
	if _, ok := rawMap["parallel_tool_calls"]; ok {
		v, err := parseLooseBoolAnyForField(raw.ParallelToolCalls, "parallel_tool_calls")
		if err != nil {
			return err
		}
		parallel = &v
	}
	var store *bool
	if _, ok := rawMap["store"]; ok {
		v, err := parseLooseBoolAnyForField(raw.Store, "store")
		if err != nil {
			return err
		}
		store = &v
	}
	var background *bool
	if _, ok := rawMap["background"]; ok {
		v, err := parseLooseBoolAnyForField(raw.Background, "background")
		if err != nil {
			return err
		}
		background = &v
	}
	var maxOutput *int
	if _, ok := rawMap["max_output_tokens"]; ok {
		maxOutput = &maxOutputTokens
	}

	r.Model = parseLooseStringAny(raw.Model)
	r.Input = raw.Input
	r.Instructions = parseLooseStringAny(raw.Instructions)
	r.Stream = stream
	r.StreamProvided = streamProvided
	r.Reasoning = raw.Reasoning
	r.Temperature = temp
	r.TopP = topP
	r.MaxOutputTokens = maxOutput
	r.Tools = raw.Tools
	r.ToolChoice = raw.ToolChoice
	r.ParallelToolCalls = parallel
	r.PreviousResponseID = parseLooseStringAny(raw.PreviousResponseID)
	r.Store = store
	r.Metadata = raw.Metadata
	r.Truncation = parseLooseStringAny(raw.Truncation)
	r.Include = raw.Include
	r.Background = background
	r.PromptCacheKey = parseLooseStringAny(raw.PromptCacheKey)
	return nil
}

func (h *Handler) HandleResponses(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodPost) {
		return
	}
	// Keep the original object for the Build CLI route.  The CLI upstream
	// speaks Responses natively, so translating it through Chat Completions
	// would drop valid fields such as previous_response_id and metadata.
	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "invalid json", http.StatusBadRequest)
		return
	}
	var req ResponsesCreateRequest
	if err := json.Unmarshal(body, &req); err != nil {
		http.Error(w, "invalid json", http.StatusBadRequest)
		return
	}
	req.Model = normalizeModelID(req.Model)
	if !requireAPIKeyModel(w, r, req.Model) {
		return
	}
	h.applyDefaultResponsesStream(&req)
	var nativePayload map[string]interface{}
	if err := json.Unmarshal(body, &nativePayload); err != nil || nativePayload == nil {
		http.Error(w, "invalid json", http.StatusBadRequest)
		return
	}
	if _, provided := nativePayload["stream"]; !provided {
		nativePayload["stream"] = req.Stream
	}
	identityMessages, _ := responsesInputToMessages(req.Input)
	session := prepareGrokSession(r, req.Model, req.PromptCacheKey, identityMessages)
	if session.Key != "" {
		nativePayload["prompt_cache_key"] = session.Key
		r = r.WithContext(withGrokSession(r.Context(), session))
		if session.Replay {
			h.applyNativeReasoningReplay(req.Model, session.Key, nativePayload)
		}
	}

	spec, resolved := h.resolveConversationModel(r.Context(), req.Model)
	if resolved && modelRoutedToCLI(spec, h.cfg) {
		h.handleNativeCLIResponses(w, r, req.Model, spec, nativePayload)
		return
	}
	if !resolved {
		http.Error(w, modelNotFoundMessage(req.Model), http.StatusBadRequest)
		return
	}
	if !spec.SupportsConversation() {
		http.Error(w, fmt.Sprintf("model %s does not support responses", req.Model), http.StatusBadRequest)
		return
	}
	if previousID := strings.TrimSpace(req.PreviousResponseID); previousID != "" {
		owner := strings.TrimSpace(middleware.APIKeyFingerprint(r.Context()))
		if owner == "" {
			owner = "anonymous"
		}
		previous, lookupErr := h.getStoredResponse(r, previousID, owner)
		if lookupErr != nil {
			writeStoredResponseLookupError(w, lookupErr, "previous response not found")
			return
		}
		if previous == nil || len(previous.Body) == 0 || previous.Provider == ProviderBuild {
			writeResponsesAPIError(w, http.StatusNotFound, "response_not_found", "previous response not found")
			return
		}
		if previous.Provider != providerForModelSpec(spec) {
			writeResponsesAPIError(w, http.StatusBadRequest, "invalid_request_error", "previous response provider is incompatible")
			return
		}
		req.Input, err = expandStoredResponseInput(previous.Body, req.Input)
		if err != nil {
			writeResponsesAPIError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
			return
		}
		if previous.PromptCacheKey != "" {
			session = grokSessionContext{Key: previous.PromptCacheKey, Replay: true, Model: req.Model}
			req.PromptCacheKey = previous.PromptCacheKey
			r = r.WithContext(withGrokSession(r.Context(), session))
		}
	}
	if err := validateResponsesCompatibility(req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	chatReq, err := chatRequestFromResponses(req)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	raw, err := json.Marshal(chatReq)
	if err != nil {
		http.Error(w, "failed to build chat request", http.StatusInternalServerError)
		return
	}

	subReq := r.Clone(context.WithValue(r.Context(), chatSourceOperationKey{}, "responses"))
	subReq.Method = http.MethodPost
	subReq.URL.Path = "/v1/chat/completions"
	// Preserve the inbound headers: the bridge must not drop the credential that
	// authorized the request (the Anthropic Messages bridge clones them too), and
	// keeping them lets downstream code observe the same request identity.
	subReq.Header = r.Header.Clone()
	subReq.Header.Set("Content-Type", "application/json")
	subReq.Body = io.NopCloser(bytes.NewReader(raw))
	subReq.ContentLength = int64(len(raw))

	if chatReq.Stream {
		h.withChatStream(subReq, func(status int, header http.Header, reader io.Reader) {
			if status < 200 || status >= 300 {
				for key, values := range header {
					w.Header()[key] = values
				}
				w.WriteHeader(status)
				_, _ = io.Copy(w, reader)
				return
			}
			writeResponsesStreamFromChatReaderRequest(w, req, reader)
		})
		return
	}

	rec := newCaptureResponseWriter()
	h.HandleChatCompletions(rec, subReq)
	if rec.code < 200 || rec.code >= 300 {
		copyCapturedResponse(w, rec)
		return
	}
	var chat map[string]interface{}
	if err := json.Unmarshal(rec.body.Bytes(), &chat); err != nil {
		http.Error(w, "chat response parse error: "+err.Error(), http.StatusBadGateway)
		return
	}
	response := responsesObjectFromChat(req.Model, chat)
	if len(req.Metadata) > 0 {
		response["metadata"] = req.Metadata
	}
	if strings.TrimSpace(req.Truncation) != "" {
		response["truncation"] = req.Truncation
	}
	if req.Store != nil && *req.Store {
		encoded, encodeErr := json.Marshal(response)
		if encodeErr != nil {
			http.Error(w, "failed to store response", http.StatusInternalServerError)
			return
		}
		owner := strings.TrimSpace(middleware.APIKeyFingerprint(r.Context()))
		if owner == "" {
			owner = "anonymous"
		}
		if saveErr := h.saveStoredResponse(r, &store.StoredResponse{
			ResponseID: parseLooseStringAny(response["id"]), OwnerHash: owner, Model: req.Model,
			Provider: providerForModelSpec(spec), PromptCacheKey: sessionFromContext(r.Context()).Key,
			ContentType: "application/json", Body: encoded,
		}); saveErr != nil {
			http.Error(w, "failed to store response", http.StatusServiceUnavailable)
			return
		}
	}
	writeJSON(w, response)
}

func providerForModelSpec(spec ModelSpec) string {
	if spec.Upstream == UpstreamConsole || strings.TrimSpace(spec.ConsoleModel) != "" {
		return ProviderConsole
	}
	if spec.Upstream == UpstreamCLI {
		return ProviderBuild
	}
	return ProviderWeb
}

func expandStoredResponseInput(responseBody []byte, current interface{}) (interface{}, error) {
	var previous map[string]interface{}
	if json.Unmarshal(responseBody, &previous) != nil {
		return nil, fmt.Errorf("stored response is invalid")
	}
	output := interfaceSlice(previous["output"])
	if len(output) == 0 {
		return nil, fmt.Errorf("stored response has no output")
	}
	currentItems := make([]interface{}, 0)
	switch value := current.(type) {
	case string:
		if strings.TrimSpace(value) != "" {
			currentItems = append(currentItems, map[string]interface{}{
				"type": "message", "role": "user", "content": []interface{}{map[string]interface{}{"type": "input_text", "text": value}},
			})
		}
	case []interface{}:
		currentItems = append(currentItems, value...)
	default:
		return nil, fmt.Errorf("input must be a string or an array")
	}
	if len(currentItems) == 0 {
		return nil, fmt.Errorf("input is required")
	}
	combined := make([]interface{}, 0, len(output)+len(currentItems))
	combined = append(combined, output...)
	combined = append(combined, currentItems...)
	return combined, nil
}

// handleNativeCLIResponses proxies Build OAuth Responses requests without a
// Chat-Completions compatibility conversion.  Besides preserving the official
// request and event schema, this keeps SSE streaming realtime and bounded by
// the normal HTTP backpressure instead of buffering the whole completion.
func (h *Handler) handleNativeCLIResponses(w http.ResponseWriter, r *http.Request, modelID string, spec ModelSpec, payload map[string]interface{}) {
	h.handleNativeCLIResponsesAt(w, r, modelID, spec, payload, "/responses", true)
}

func copyNativeCLIResponseHeaders(dst, src http.Header) {
	// Forward only end-to-end response metadata. Hop-by-hop headers must not be
	// copied because net/http owns the downstream connection.
	for _, key := range []string{"Content-Type", "Cache-Control", "X-Request-Id", "X-Request-ID", "X-Grok2api-Compatibility-Warnings", "X-Grok2api-Reasoning-Recovery"} {
		if values, ok := src[key]; ok {
			dst.Del(key)
			for _, value := range values {
				dst.Add(key, value)
			}
		}
	}
}

func streamNativeCLIResponse(w http.ResponseWriter, body io.Reader) {
	flusher, _ := w.(http.Flusher)
	buf := make([]byte, 32*1024)
	for {
		n, err := body.Read(buf)
		if n > 0 {
			_, _ = w.Write(buf[:n])
			if flusher != nil {
				flusher.Flush()
			}
		}
		if err == io.EOF {
			return
		}
		if err != nil {
			return
		}
	}
}

func validateResponsesCompatibility(req ResponsesCreateRequest) error {
	if req.Store != nil && *req.Store && req.Stream {
		return fmt.Errorf("store=true requires stream=false for this provider")
	}
	if truncation := strings.ToLower(strings.TrimSpace(req.Truncation)); truncation != "" && truncation != "auto" && truncation != "disabled" {
		return fmt.Errorf("truncation must be auto or disabled")
	}
	if req.Background != nil && *req.Background {
		return fmt.Errorf("background=true is not supported")
	}
	return nil
}

func (h *Handler) applyDefaultResponsesStream(req *ResponsesCreateRequest) {
	if req == nil || req.StreamProvided {
		return
	}
	req.Stream = h.defaultChatStream()
}

func chatRequestFromResponses(req ResponsesCreateRequest) (ChatCompletionsRequest, error) {
	model := normalizeModelID(req.Model)
	if strings.TrimSpace(model) == "" {
		return ChatCompletionsRequest{}, fmt.Errorf("model is required")
	}
	messages, err := responsesInputToMessages(req.Input)
	if err != nil {
		return ChatCompletionsRequest{}, err
	}
	if instructions := strings.TrimSpace(req.Instructions); instructions != "" {
		messages = append([]ChatMessage{{Role: "system", Content: instructions}}, messages...)
	}
	reasoningEffort := responsesReasoningEffort(req.Reasoning)
	out := ChatCompletionsRequest{
		sourceOperation:   "responses",
		Model:             model,
		Messages:          messages,
		Stream:            req.Stream,
		StreamProvided:    true,
		ReasoningEffort:   reasoningEffort,
		Temperature:       req.Temperature,
		TopP:              req.TopP,
		Tools:             responsesToolsToChatTools(req.Tools),
		ToolChoice:        responsesToolChoiceToChat(req.ToolChoice),
		ParallelToolCalls: req.ParallelToolCalls,
		MaxTokens:         req.MaxOutputTokens,
		PromptCacheKey:    req.PromptCacheKey,
	}
	return out, nil
}

func responsesInputToMessages(input interface{}) ([]ChatMessage, error) {
	switch v := input.(type) {
	case nil:
		return nil, fmt.Errorf("input is required")
	case string:
		if strings.TrimSpace(v) == "" {
			return nil, fmt.Errorf("input is required")
		}
		return []ChatMessage{{Role: "user", Content: v}}, nil
	case []interface{}:
		messages := make([]ChatMessage, 0, len(v))
		for _, raw := range v {
			item, _ := raw.(map[string]interface{})
			if item == nil {
				continue
			}
			itemType := strings.ToLower(strings.TrimSpace(fmt.Sprint(item["type"])))
			if itemType == "" {
				if strings.TrimSpace(fmt.Sprint(item["role"])) != "" {
					itemType = "message"
				}
			}
			switch itemType {
			case "function_call":
				name := parseLooseStringAny(item["name"])
				if name == "" {
					continue
				}
				args := "{}"
				if rawArgs := item["arguments"]; rawArgs != nil {
					switch x := rawArgs.(type) {
					case string:
						if strings.TrimSpace(x) != "" {
							args = strings.TrimSpace(x)
						}
					default:
						if buf, err := json.Marshal(x); err == nil {
							args = string(buf)
						}
					}
				}
				messages = append(messages, ChatMessage{
					Role:    "assistant",
					Content: nil,
					ToolCalls: []ToolCall{{
						ID:   strings.TrimSpace(fmt.Sprint(item["call_id"])),
						Type: "function",
						Function: map[string]interface{}{
							"name":      name,
							"arguments": args,
						},
					}},
				})
			case "function_call_output":
				messages = append(messages, ChatMessage{
					Role:       "tool",
					ToolCallID: strings.TrimSpace(fmt.Sprint(item["call_id"])),
					Content:    strings.TrimSpace(fmt.Sprint(item["output"])),
				})
			case "custom_tool_call":
				name := firstNonEmpty(parseLooseStringAny(item["name"]), "custom_tool")
				messages = append(messages, ChatMessage{Role: "assistant", Content: nil, ToolCalls: []ToolCall{{
					ID: firstNonEmpty(parseLooseStringAny(item["call_id"]), parseLooseStringAny(item["id"])), Type: "function",
					Function: map[string]interface{}{"name": name, "arguments": firstNonNil(item["input"], item["arguments"], "{}")},
				}}})
			case "custom_tool_call_output":
				messages = append(messages, ChatMessage{Role: "tool", ToolCallID: parseLooseStringAny(item["call_id"]), Content: parseLooseStringAny(item["output"])})
			case "reasoning":
				messages = append(messages, ChatMessage{
					Role: "assistant", Content: "",
					ReasoningContent: responsesReasoningSummary(item), ReasoningEncryptedContent: parseLooseStringAny(item["encrypted_content"]),
				})
			case "message":
				role := parseLooseStringAny(item["role"])
				if role == "" {
					role = "user"
				}
				messages = append(messages, ChatMessage{
					Role:    role,
					Content: normalizeResponsesMessageContent(item["content"]),
				})
			}
		}
		if len(messages) == 0 {
			return nil, fmt.Errorf("input is required")
		}
		return messages, nil
	default:
		return nil, fmt.Errorf("input must be a string or an array")
	}
}

func responsesReasoningSummary(item map[string]interface{}) string {
	parts := make([]string, 0)
	for _, raw := range interfaceSlice(item["summary"]) {
		part, _ := raw.(map[string]interface{})
		if text := parseLooseStringAny(part["text"]); text != "" {
			parts = append(parts, text)
		}
	}
	return strings.Join(parts, "")
}

func normalizeResponsesMessageContent(content interface{}) interface{} {
	parts, ok := content.([]interface{})
	if !ok {
		return content
	}
	out := make([]interface{}, 0, len(parts))
	for _, raw := range parts {
		part, _ := raw.(map[string]interface{})
		if part == nil {
			continue
		}
		ptype := strings.ToLower(strings.TrimSpace(fmt.Sprint(part["type"])))
		switch ptype {
		case "input_text", "output_text":
			out = append(out, map[string]interface{}{"type": "text", "text": fmt.Sprint(part["text"])})
		case "input_image", "image":
			if url := responsesPartURL(part, []string{"image_url", "source"}, []string{"url"}); url != "" {
				out = append(out, map[string]interface{}{"type": "image_url", "image_url": map[string]interface{}{"url": url}})
			}
		case "input_file", "file":
			url := responsesPartURL(part, []string{"file", "file_url", "source", "file_data"}, []string{"url", "file_url", "data", "file_data"})
			if url == "" {
				url = parseLooseStringAny(part["file_id"])
			}
			if url != "" {
				// The chat layer validates the portable file shape, so carry the
				// resolved reference as file_data instead of a nested url. This
				// also makes the chat->Responses->chat round trip lossless.
				out = append(out, map[string]interface{}{"type": "file", "file": map[string]interface{}{"file_data": url}})
			}
		default:
			out = append(out, part)
		}
	}
	return out
}

func responsesPartURL(part map[string]interface{}, keys, nestedKeys []string) string {
	for _, key := range keys {
		raw := part[key]
		switch v := raw.(type) {
		case string:
			if strings.TrimSpace(v) != "" {
				return strings.TrimSpace(v)
			}
		case map[string]interface{}:
			for _, nestedKey := range nestedKeys {
				if s := parseLooseStringAny(v[nestedKey]); s != "" {
					return s
				}
			}
		}
	}
	return ""
}

func responsesToolsToChatTools(tools []map[string]interface{}) []ToolDef {
	out := make([]ToolDef, 0, len(tools))
	for _, tool := range tools {
		if !strings.EqualFold(strings.TrimSpace(fmt.Sprint(tool["type"])), "function") {
			continue
		}
		if fn, _ := tool["function"].(map[string]interface{}); fn != nil {
			if strings.TrimSpace(fmt.Sprint(fn["name"])) != "" {
				out = append(out, ToolDef{Type: "function", Function: fn})
			}
			continue
		}
		name := parseLooseStringAny(tool["name"])
		if name == "" {
			continue
		}
		out = append(out, ToolDef{Type: "function", Function: map[string]interface{}{
			"name":        name,
			"description": strings.TrimSpace(fmt.Sprint(tool["description"])),
			"parameters":  firstNonNil(tool["parameters"], map[string]interface{}{}),
		}})
	}
	return out
}

func responsesToolChoiceToChat(choice interface{}) interface{} {
	if choice == nil {
		return nil
	}
	m, _ := choice.(map[string]interface{})
	if m == nil || !strings.EqualFold(strings.TrimSpace(fmt.Sprint(m["type"])), "function") {
		return choice
	}
	if _, ok := m["function"].(map[string]interface{}); ok {
		return choice
	}
	name := parseLooseStringAny(m["name"])
	if name == "" {
		return choice
	}
	return map[string]interface{}{"type": "function", "function": map[string]interface{}{"name": name}}
}

func responsesReasoningEffort(reasoning map[string]interface{}) *string {
	if len(reasoning) == 0 {
		return nil
	}
	if effort := strings.ToLower(strings.TrimSpace(fmt.Sprint(reasoning["effort"]))); effort != "" && effort != "<nil>" {
		return &effort
	}
	return nil
}

func firstNonNil(values ...interface{}) interface{} {
	for _, v := range values {
		if v != nil {
			return v
		}
	}
	return nil
}

func responsesObjectFromChat(model string, chat map[string]interface{}) map[string]interface{} {
	output := responsesOutputFromChat(chat)
	finish := ""
	if choices := interfaceSlice(chat["choices"]); len(choices) > 0 {
		choice, _ := choices[0].(map[string]interface{})
		finish = streamString(choice["finish_reason"])
	}
	status, details := responseStatusFromFinish(finish)
	result := map[string]interface{}{"id": "resp_" + randomHex(12), "object": "response", "created_at": time.Now().Unix(), "status": status, "model": firstNonEmpty(interfaceString(chat["model"]), model), "output": output, "parallel_tool_calls": true, "tool_choice": "auto", "usage": responsesUsageFromChat(chat["usage"])}
	if details != nil {
		result["incomplete_details"] = details
	}
	if chat["error"] != nil {
		result["status"] = "failed"
		result["error"] = chat["error"]
	}
	for _, raw := range output {
		item, _ := raw.(map[string]interface{})
		if item["type"] != "web_search_call" {
			item["status"] = status
		}
	}
	return result
}

func responsesOutputFromChat(chat map[string]interface{}) []interface{} {
	out := []interface{}{}
	choices := interfaceSlice(chat["choices"])
	if len(choices) == 0 {
		return out
	}
	choice, _ := choices[0].(map[string]interface{})
	message, _ := choice["message"].(map[string]interface{})
	if message == nil {
		return out
	}
	if thoughts := interfaceSlice(message["x_grok_reasoning"]); len(thoughts) > 0 {
		for _, raw := range thoughts {
			item, ok := raw.(map[string]interface{})
			if !ok {
				continue
			}
			copy := cloneStringInterfaceMap(item)
			copy["id"] = "rs_" + randomHex(12)
			copy["status"] = "completed"
			out = append(out, copy)
		}
	} else {
		text := streamString(firstNonNil(message["reasoning_content"], message["reasoning"]))
		signature := streamString(message["reasoning_encrypted_content"])
		if text != "" || signature != "" {
			item := map[string]interface{}{"id": "rs_" + randomHex(12), "type": "reasoning", "status": "completed", "summary": []interface{}{}}
			if text != "" {
				item["summary"] = []interface{}{map[string]interface{}{"type": "summary_text", "text": text}}
			}
			if signature != "" {
				item["encrypted_content"] = signature
			}
			out = append(out, item)
		}
	}
	seen := map[string]bool{}
	for _, raw := range interfaceSlice(message["x_grok_searches"]) {
		search, ok := raw.(map[string]interface{})
		if !ok {
			continue
		}
		key := searchIdentity(search)
		if seen[key] {
			continue
		}
		seen[key] = true
		item := cloneStringInterfaceMap(search)
		if interfaceString(item["id"]) == "" {
			item["id"] = "ws_" + randomHex(12)
		}
		out = append(out, item)
	}
	for _, raw := range interfaceSlice(message["tool_calls"]) {
		call, _ := raw.(map[string]interface{})
		if item := responseFunctionCallItem(call); item != nil {
			out = append(out, item)
		}
	}
	parts := []interface{}{}
	if text := streamString(message["content"]); text != "" {
		parts = append(parts, map[string]interface{}{"type": "output_text", "text": text, "annotations": responseAnnotations(message["annotations"])})
	}
	if text := streamString(message["refusal"]); text != "" {
		parts = append(parts, map[string]interface{}{"type": "refusal", "refusal": text})
	}
	if len(parts) > 0 {
		out = append(out, map[string]interface{}{"id": "msg_" + randomHex(12), "type": "message", "status": "completed", "role": "assistant", "content": parts})
	}
	return out
}

func responseFunctionCallItem(call map[string]interface{}) map[string]interface{} {
	if call == nil {
		return nil
	}
	fn, _ := call["function"].(map[string]interface{})
	name := parseLooseStringAny(fn["name"])
	if name == "" {
		return nil
	}
	args := streamString(fn["arguments"])
	if args == "" {
		args = "{}"
	}
	return map[string]interface{}{
		"id":        "fc_" + randomHex(12),
		"type":      "function_call",
		"call_id":   firstNonEmpty(streamString(call["id"]), "call_"+randomHex(12)),
		"name":      name,
		"arguments": args,
		"status":    "completed",
	}
}

func responsesUsageFromChat(raw interface{}) map[string]interface{} {
	usage, _ := raw.(map[string]interface{})
	input := interfaceToInt(firstNonNil(usage["prompt_tokens"], usage["input_tokens"]))
	output := interfaceToInt(firstNonNil(usage["completion_tokens"], usage["output_tokens"]))
	total := interfaceToInt(usage["total_tokens"])
	if total == 0 {
		total = input + output
	}
	result := cloneStringInterfaceMap(usage)
	if result == nil {
		result = map[string]interface{}{}
	}
	delete(result, "prompt_tokens")
	delete(result, "completion_tokens")
	delete(result, "prompt_tokens_details")
	delete(result, "completion_tokens_details")
	result["input_tokens"] = input
	result["output_tokens"] = output
	result["total_tokens"] = total
	for target, source := range map[string]string{"input_tokens_details": "prompt_tokens_details", "output_tokens_details": "completion_tokens_details"} {
		if details, ok := firstNonNil(usage[target], usage[source]).(map[string]interface{}); ok {
			result[target] = cloneStringInterfaceMap(details)
		}
	}
	return result
}

func copyCapturedResponse(w http.ResponseWriter, rec *captureResponseWriter) {
	for k, values := range rec.Header() {
		for _, v := range values {
			w.Header().Add(k, v)
		}
	}
	code := rec.code
	if code == 0 {
		code = http.StatusOK
	}
	w.WriteHeader(code)
	_, _ = w.Write(rec.body.Bytes())
}
