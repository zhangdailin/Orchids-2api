package grok

import (
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/goccy/go-json"

	apperrors "orchids-api/internal/errors"
	"orchids-api/internal/middleware"
	"orchids-api/internal/store"
)

const defaultStoredResponseTTL = 30 * 24 * time.Hour

func (h *Handler) handleNativeCLIResponsesAt(w http.ResponseWriter, r *http.Request, modelID string, spec ModelSpec, payload map[string]interface{}, upstreamPath string, saveOwnership bool) {
	spec.Upstream, spec.ConsoleModel = UpstreamCLI, ""
	started := time.Now()
	if err := validatePayloadReasoning(payload); err != nil {
		writeResponsesAPIError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}
	toolAliases := collectBuildToolAliases(payload)
	if err := normalizeBuildResponsesPayload(payload); err != nil {
		writeResponsesAPIError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}
	compatibilityWarnings := takeBuildCompatibilityWarnings(payload)
	if err := h.ensureModelCapability(r.Context(), modelID, store.CapabilityResponses); err != nil {
		writeResponsesAPIError(w, http.StatusBadRequest, "invalid_request_error", modelValidationMessage(modelID, err))
		return
	}
	if h == nil || h.buildClient() == nil {
		writeResponsesAPIError(w, http.StatusServiceUnavailable, "service_unavailable", "grok cli client not configured")
		return
	}

	ownerHash := middleware.APIKeyFingerprint(r.Context())
	previousID := strings.TrimSpace(parseLooseStringAny(payload["previous_response_id"]))
	var (
		sess   *chatAccountSession
		pinned bool
		err    error
	)
	if previousID != "" && ownerHash != "" {
		ownership, lookupErr := h.getStoredResponse(r, previousID, ownerHash)
		if lookupErr != nil {
			writeStoredResponseLookupError(w, lookupErr, "previous response not found")
			return
		}
		if ownership.Provider != ProviderBuild {
			writeResponsesAPIError(w, http.StatusBadRequest, "invalid_request_error", "previous response provider is incompatible")
			return
		}
		sess, err = h.openCLIAccountSessionByID(r.Context(), ownership.AccountID, spec.UpstreamModel)
		pinned = true
	} else {
		sess, err = h.openCLIAccountSession(r.Context(), nil, spec.UpstreamModel)
	}
	if err != nil {
		writeResponsesAPIError(w, http.StatusServiceUnavailable, "response_account_unavailable", err.Error())
		return
	}
	defer sess.Close()

	payload["model"] = spec.UpstreamModel
	call := func() (*http.Response, error) {
		if pinned {
			return h.buildClient().doResponsesAt(r.Context(), sess.acc, upstreamPath, payload)
		}
		return h.doCLIWithAutoSwitchAt(r.Context(), sess, payload, spec.UpstreamModel, upstreamPath)
	}
	resp, err := call()
	if err != nil && isReasoningReplayDecodeError(err) && !preservesClientCompaction(payload, err) {
		// Upstream rejected opaque reasoning it could not decode. Recovery stays
		// on the same account and plane: drop only the undecodable ciphers while
		// keeping readable summaries, and clear the server-side replay first so
		// the same stale cipher is never injected again. Client-held compaction
		// state is never rewritten.
		if session := sessionFromContext(r.Context()); session.Key != "" {
			h.clearReasoningReplay(r.Context(), modelID, session.Key)
		}
		if stripInjectedReasoningReplay(payload) {
			retryResp, retryErr := call()
			if retryErr == nil && retryResp != nil {
				retryResp.Header.Set("X-Grok2API-Reasoning-Recovery", "reasoning_encrypted_content_retry")
			}
			resp, err = retryResp, retryErr
		}
	}
	if err != nil {
		if markAllGrokAccountStatuses(err) {
			h.markAccountStatus(r.Context(), sess.acc, err)
		}
		writeResponsesAPIError(w, upstreamHTTPResponseStatus(err), "upstream_error", err.Error())
		return
	}
	defer resp.Body.Close()
	h.syncGrokQuota(sess.acc, resp.Header)
	copyNativeCLIResponseHeaders(w.Header(), resp.Header)
	if compatibilityWarnings != "" {
		w.Header().Set("X-Grok2API-Compatibility-Warnings", compatibilityWarnings)
	}
	w.WriteHeader(resp.StatusCode)
	responseBody := io.Reader(resp.Body)
	var rewritten io.ReadCloser
	if len(toolAliases) > 0 {
		rewritten = rewriteBuildToolAliasResponse(resp.Body, resp.Header.Get("Content-Type"), toolAliases)
		defer rewritten.Close()
		responseBody = rewritten
	}
	responseID, captured, result := copyNativeCLIResponseAndCaptureModel(w, responseBody, resp.Header.Get("Content-Type"), modelID)
	h.auditChatOutcome(r.Context(), sess.acc, &ChatCompletionsRequest{Model: modelID, startedAt: started}, result)
	if session := sessionFromContext(r.Context()); session.Replay && len(captured) > 0 && result.Err == nil {
		h.captureReasoningReplay(r.Context(), modelID, session.Key, captured)
	}

	if !saveOwnership || ownerHash == "" || responseID == "" || resp.StatusCode < 200 || resp.StatusCode >= 300 || result.Err != nil {
		return
	}
	if err := h.saveStoredResponse(r, &store.StoredResponse{
		ResponseID: responseID,
		OwnerHash:  ownerHash,
		AccountID:  sess.acc.ID,
		Model:      spec.UpstreamModel,
		Provider:   ProviderBuild,
	}); err != nil {
		slog.Error("failed to save response ownership", "response_id", responseID, "account_id", sess.acc.ID, "error", err)
	}
}

// HandleResponsesCompact forwards the native Build Responses compaction API.
// Compaction is deliberately non-streaming and is not stored as a normal
// response resource.
func (h *Handler) HandleResponsesCompact(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodPost) {
		return
	}
	var payload map[string]interface{}
	if !decodeJSONBody(w, r, &payload) || payload == nil {
		return
	}
	modelID := normalizeModelID(parseLooseStringAny(payload["model"]))
	if modelID == "" {
		writeResponsesAPIError(w, http.StatusBadRequest, "invalid_request_error", "model is required")
		return
	}
	if !requireAPIKeyModel(w, r, modelID) {
		return
	}
	spec, ok := h.resolveConversationModel(r.Context(), modelID)
	if !ok || !modelRoutedToCLI(spec, h.configSnapshot()) {
		writeResponsesAPIError(w, http.StatusBadRequest, "invalid_request_error", "responses compact requires a Grok Build model")
		return
	}
	payload["stream"] = false
	h.handleNativeCLIResponsesAt(w, r, modelID, spec, payload, "/responses/compact", false)
}

// HandleResponseResource retrieves or deletes a stored Build Responses
// resource through the exact account that created it.
func (h *Handler) HandleResponseResource(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodDelete {
		w.Header().Set("Allow", "GET, DELETE")
		writeResponsesAPIError(w, http.StatusMethodNotAllowed, "invalid_request_error", "method not allowed")
		return
	}
	responseID := responseIDFromResourcePath(r.URL.Path)
	if responseID == "" {
		writeResponsesAPIError(w, http.StatusBadRequest, "invalid_request_error", "response_id is required")
		return
	}
	ownerHash := middleware.APIKeyFingerprint(r.Context())
	if ownerHash == "" {
		ownerHash = "anonymous"
	}
	ownership, err := h.getStoredResponse(r, responseID, ownerHash)
	if err != nil {
		writeStoredResponseLookupError(w, err, "response not found")
		return
	}
	if ownership.Provider != ProviderBuild {
		if len(ownership.Body) == 0 {
			writeResponsesAPIError(w, http.StatusNotFound, "response_not_found", "response not found")
			return
		}
		if r.Method == http.MethodDelete {
			_ = h.deleteStoredResponse(r, responseID, ownerHash)
			writeJSON(w, map[string]interface{}{"id": responseID, "object": "response.deleted", "deleted": true})
			return
		}
		contentType := firstNonEmpty(strings.TrimSpace(ownership.ContentType), "application/json")
		w.Header().Set("Content-Type", contentType)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(ownership.Body)
		return
	}
	sess, err := h.openCLIAccountSessionByID(r.Context(), ownership.AccountID, ownership.Model)
	if err != nil {
		writeResponsesAPIError(w, http.StatusServiceUnavailable, "response_account_unavailable", err.Error())
		return
	}
	defer sess.Close()

	path := "/responses/" + url.PathEscape(responseID)
	resp, err := h.buildClient().doResponseResource(r.Context(), sess.acc, r.Method, path, r.URL.RawQuery)
	if err != nil {
		writeResponsesAPIError(w, http.StatusBadGateway, "upstream_error", err.Error())
		return
	}
	defer resp.Body.Close()
	copyNativeCLIResponseHeaders(w.Header(), resp.Header)
	w.WriteHeader(resp.StatusCode)
	streamNativeCLIResponse(w, resp.Body)
	if (r.Method == http.MethodDelete && resp.StatusCode >= 200 && resp.StatusCode < 300) || resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusGone {
		_ = h.deleteStoredResponse(r, responseID, ownerHash)
	}
}

func (h *Handler) getStoredResponse(r *http.Request, responseID, ownerHash string) (*store.StoredResponse, error) {
	if h == nil || h.lb == nil || h.lb.Store == nil {
		return nil, errors.New("response store not configured")
	}
	return h.lb.Store.GetStoredResponse(r.Context(), responseID, ownerHash)
}

func (h *Handler) saveStoredResponse(r *http.Request, response *store.StoredResponse) error {
	if h == nil || h.lb == nil || h.lb.Store == nil {
		return errors.New("response store not configured")
	}
	ttl := defaultStoredResponseTTL
	if h.configSnapshot() != nil && h.configSnapshot().ResponseStoreTTL > 0 {
		ttl = time.Duration(h.configSnapshot().ResponseStoreTTL) * time.Hour
	}
	return h.lb.Store.SaveStoredResponse(r.Context(), response, ttl)
}

func (h *Handler) deleteStoredResponse(r *http.Request, responseID, ownerHash string) error {
	if h == nil || h.lb == nil || h.lb.Store == nil {
		return errors.New("response store not configured")
	}
	return h.lb.Store.DeleteStoredResponse(r.Context(), responseID, ownerHash)
}

func responseIDFromResourcePath(path string) string {
	marker := "/responses/"
	index := strings.LastIndex(path, marker)
	if index < 0 {
		return ""
	}
	value := strings.Trim(strings.TrimSpace(path[index+len(marker):]), "/")
	if value == "" || strings.Contains(value, "/") || strings.EqualFold(value, "compact") {
		return ""
	}
	decoded, err := url.PathUnescape(value)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(decoded)
}

func copyNativeCLIResponseAndCaptureModel(w http.ResponseWriter, body io.Reader, contentType, model string) (responseID string, captured []byte, result chatOutcome) {
	fullCapture := newBoundedResponseCapture(8 << 20)
	defer func() {
		captured = fullCapture.data
		if result.Err != nil {
			result.Finish = "error"
		}
	}()
	if !strings.Contains(strings.ToLower(contentType), "text/event-stream") {
		raw, readErr := io.ReadAll(io.LimitReader(body, (8<<20)+1))
		if readErr != nil {
			result.Err = fmt.Errorf("upstream response could not be read within the response limit: %w", readErr)
			writeResponsesAPIError(w, http.StatusBadGateway, "upstream_error", "Upstream response unavailable")
			return
		}
		if len(raw) > 8<<20 {
			result.Err = fmt.Errorf("upstream response could not be read within the response limit")
			writeResponsesAPIError(w, http.StatusBadGateway, "upstream_error", "Upstream response unavailable")
			return
		}
		var response map[string]interface{}
		if result.Err = json.Unmarshal(raw, &response); result.Err != nil {
			writeResponsesAPIError(w, http.StatusBadGateway, "upstream_error", "Invalid upstream response")
			return
		}
		if response == nil {
			result.Err = fmt.Errorf("invalid upstream response JSON")
			return
		}
		responseID = interfaceString(response["id"])
		result.Usage = consoleUsage(response)
		result.Finish, result.Err = responseTerminalFinish("", response)
		if redactResponseError(response) {
			raw, _ = json.Marshal(response)
		}
		_, _ = io.MultiWriter(w, fullCapture).Write(raw)
		return
	}

	flusher, _ := w.(http.Flusher)
	target := io.MultiWriter(w, fullCapture)
	terminal, done := false, false
	failureCode, failureMessage := "", ""
	err := consumeCompatibleSSE(body, func(frame compatibleSSEEvent) error {
		var event map[string]interface{}
		kind := ""
		if frame.HasData() {
			data := frame.Data()
			if string(data) == "[DONE]" {
				done = true
				return io.EOF
			}
			if err := json.Unmarshal(data, &event); err != nil || event == nil {
				failureCode, failureMessage = "invalid_upstream_event", "upstream response event is not a JSON object"
				return fmt.Errorf("%s", failureMessage)
			}
			kind = firstNonEmpty(interfaceString(event["type"]), frame.Event)
			response, _ := event["response"].(map[string]interface{})
			if id := interfaceString(response["id"]); id != "" {
				responseID = id
			}
		}
		if redactResponseError(event) {
			raw, _ := json.Marshal(event)
			frame.data = []string{string(raw)}
		}
		if err := frame.writeTo(target); err != nil {
			result.Err = err
			return err
		}
		if flusher != nil {
			flusher.Flush()
		}
		if usage := consoleUsageFromStreamEvent(event); len(usage) > 0 {
			result.Usage = usage
		}
		item, _ := event["item"].(map[string]interface{})
		meaningful := strings.HasSuffix(kind, ".delta") && streamString(event["delta"]) != ""
		toolStart := kind == "response.output_item.added" && interfaceString(item["type"]) == "function_call" && interfaceString(item["name"]) != ""
		if result.FirstToken.IsZero() && (meaningful || toolStart) {
			result.FirstToken = time.Now()
		}
		switch kind {
		case "response.completed", "response.failed", "response.incomplete", "error":
			response, _ := event["response"].(map[string]interface{})
			if response == nil {
				response = event
			}
			result.Finish, result.Err = responseTerminalFinish(kind, response)
			terminal = true
			return io.EOF // A logical terminal must not wait for the socket to close.
		}
		return nil
	})
	if !terminal && result.Err != nil {
		return
	}
	if !terminal {
		if failureCode == "" {
			failureCode, failureMessage = "upstream_stream_incomplete", "upstream stream ended before a terminal response event"
			if err != nil && err != io.EOF {
				failureCode, failureMessage = "stream_read_error", "upstream response stream could not be read"
			} else if done {
				failureCode, failureMessage = "upstream_terminal_missing", "upstream sent [DONE] without a terminal response event"
			}
		}
		result.Err = fmt.Errorf("%s", failureMessage)
		if err != nil && err != io.EOF {
			result.Err = fmt.Errorf("%s: %w", failureMessage, err)
		}
		failure, _ := json.Marshal(map[string]interface{}{
			"type": "response.failed", "response": map[string]interface{}{
				"id": responseID, "object": "response", "status": "failed", "model": model,
				"error": map[string]interface{}{"code": failureCode, "message": failureMessage},
			},
		})
		frame := compatibleSSEEvent{Event: "response.failed", data: []string{string(failure)}}
		if err := frame.writeTo(target); err != nil {
			result.Err = err
			return
		}
	}
	// Emit exactly one DONE, after the terminal (including a synthesized failure).
	if err := (compatibleSSEEvent{data: []string{"[DONE]"}}).writeTo(target); err != nil {
		result.Err = err
	}
	if flusher != nil {
		flusher.Flush()
	}
	return
}

type boundedResponseCapture struct {
	data     []byte
	limit    int
	overflow bool
}

func newBoundedResponseCapture(limit int) *boundedResponseCapture {
	return &boundedResponseCapture{limit: limit, data: make([]byte, 0, min(limit, 64*1024))}
}

func (c *boundedResponseCapture) Write(p []byte) (int, error) {
	if remaining := c.limit - len(c.data); remaining > 0 {
		if len(p) > remaining {
			c.data = append(c.data, p[:remaining]...)
			c.overflow = true
		} else {
			c.data = append(c.data, p...)
		}
	} else if len(p) > 0 {
		c.overflow = true
	}
	return len(p), nil
}

func writeResponsesAPIError(w http.ResponseWriter, status int, code, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]interface{}{
		"error": map[string]interface{}{
			"message": message,
			"type":    "invalid_request_error",
			"code":    code,
		},
	})
}

// Only protocol error envelopes are rewritten; model output and tool arguments
// remain byte-for-byte unchanged on successful events.
func redactResponseError(event map[string]interface{}) bool {
	if event == nil {
		return false
	}
	changed := false
	if event["error"] != nil {
		message := fmt.Sprint(event["error"])
		event["error"] = map[string]interface{}{"code": "upstream_error", "message": apperrors.PublicMessage(message)}
		changed = true
	}
	if event["type"] == "error" {
		for _, key := range []string{"message", "detail", "code", "param"} {
			delete(event, key)
		}
		event["message"] = apperrors.PublicMessage(fmt.Sprint(event["message"]))
		event["code"] = "upstream_error"
		changed = true
	}
	if response, ok := event["response"].(map[string]interface{}); ok {
		changed = redactResponseError(response) || changed
	}
	return changed
}

func writeStoredResponseLookupError(w http.ResponseWriter, err error, notFoundMessage string) {
	if errors.Is(err, store.ErrNoRows) {
		writeResponsesAPIError(w, http.StatusNotFound, "response_not_found", notFoundMessage)
		return
	}
	writeResponsesAPIError(w, http.StatusServiceUnavailable, "response_store_unavailable", "response store unavailable")
}
