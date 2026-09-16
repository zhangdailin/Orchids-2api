package grok

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"strings"

	"github.com/goccy/go-json"

	"orchids-api/internal/middleware"
)

// responsesChatPath maps a Responses endpoint onto the Chat Completions
// endpoint of the same channel prefix, so "/workbuddy/v1/responses" is served
// by "/workbuddy/v1/chat/completions" and the channel keeps deciding which
// upstream pool the request uses.
func responsesChatPath(path string) string {
	trimmed := strings.TrimRight(strings.TrimSpace(path), "/")
	if strings.HasSuffix(trimmed, "/responses") {
		return strings.TrimSuffix(trimmed, "/responses") + "/chat/completions"
	}
	return "/v1/chat/completions"
}

// ResponsesBridgeHandler serves the OpenAI Responses API on top of a channel
// that only implements Chat Completions.
//
// Codex defaults to the Responses wire API. Grok speaks it natively, but the
// Warp/Puter/WorkBuddy/Qoder channels only expose /v1/chat/completions, so
// without this bridge every request from Codex to those channels is a 404. The
// bridge reuses the channel's chat handler verbatim: account selection,
// retries, tool handling and streaming all stay where they already live.
func ResponsesBridgeHandler(chat http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !requireMethod(w, r, http.MethodPost) {
			return
		}
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
		r = r.WithContext(middleware.WithRequestModel(r.Context(), req.Model))
		if !requireAPIKeyModel(w, r, req.Model) {
			return
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
		subReq.URL.Path = responsesChatPath(r.URL.Path)
		// Keep the inbound headers: the inner handler must observe the same
		// request identity (and must not lose the credential that authorized
		// this request when per-key auth is enabled).
		subReq.Header = r.Header.Clone()
		subReq.Header.Set("Content-Type", "application/json")
		subReq.Body = io.NopCloser(bytes.NewReader(raw))
		subReq.ContentLength = int64(len(raw))

		if chatReq.Stream {
			streamThroughChat(subReq, chat, func(status int, header http.Header, reader io.Reader) {
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
		chat(rec, subReq)
		if rec.code < 200 || rec.code >= 300 {
			copyCapturedResponse(w, rec)
			return
		}
		var chatBody map[string]interface{}
		if err := json.Unmarshal(rec.body.Bytes(), &chatBody); err != nil {
			http.Error(w, "chat response parse error: "+err.Error(), http.StatusBadGateway)
			return
		}
		writeJSON(w, responsesObjectFromChat(req.Model, chatBody))
	}
}
