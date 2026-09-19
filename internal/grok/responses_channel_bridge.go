package grok

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/goccy/go-json"

	"orchids-api/internal/middleware"
	"orchids-api/internal/store"
)

// bridgedResponseProvider labels records written by the chat bridge. Grok's
// resource handler serves any non-Build record straight from the shared store,
// so this label is what keeps a bridged response apart from a native one.
const bridgedResponseProvider = "chat-bridge"

// ResponsesStore is the persistence the bridge needs to store a Response, serve
// it back and delete it. *store.Store satisfies it against Redis;
// store.MemoryResponseStore satisfies it in-process.
type ResponsesStore interface {
	SaveStoredResponse(ctx context.Context, response *store.StoredResponse, ttl time.Duration) error
	GetStoredResponse(ctx context.Context, responseID, ownerHash string) (*store.StoredResponse, error)
	DeleteStoredResponse(ctx context.Context, responseID, ownerHash string) error
}

// ResponsesBridgeOptions configures the bridge's response store.
//
// The store is what makes store=true, previous_response_id and
// GET/DELETE /responses/{id} work on channels that have no native Responses
// storage. Without one the bridge still serves stateless requests, which is what
// Codex does by default (it resends the whole conversation every turn).
type ResponsesBridgeOptions struct {
	// Store is the shared response store. Nil falls back to the process-wide
	// in-process store, so the bridge keeps store=true, previous_response_id and
	// resource retrieval working on a gateway started without a response
	// backend. A multi-replica gateway must pass the shared store: an in-process
	// record is only visible to the replica that wrote it.
	Store ResponsesStore
	// TTL overrides the stored-response lifetime; zero uses the default.
	TTL time.Duration
}

// memoryFallbackWarned keeps the "no shared store" warning to one line per
// process instead of one per request.
var memoryFallbackWarned sync.Once

// store returns the response store the bridge reads and writes. It is never nil:
// a gateway with no response backend gets the in-process fallback rather than a
// hard failure on every stored-response request.
func (o ResponsesBridgeOptions) store() ResponsesStore {
	if o.Store != nil {
		return o.Store
	}
	memoryFallbackWarned.Do(func() {
		slog.Warn("Responses bridge has no shared response store; falling back to an in-process store. " +
			"stored responses are only visible to this process — configure Redis for a multi-replica deployment")
	})
	return store.DefaultMemoryResponseStore()
}

func (o ResponsesBridgeOptions) ttl() time.Duration {
	if o.TTL > 0 {
		return o.TTL
	}
	return defaultStoredResponseTTL
}

func responsesOwnerHash(ctx context.Context) string {
	if owner := strings.TrimSpace(middleware.APIKeyFingerprint(ctx)); owner != "" {
		return owner
	}
	return "anonymous"
}

// responsesChatPath maps a Responses endpoint onto the Chat Completions
// endpoint of the same channel prefix, so "/workbuddy/v1/responses" is served
// by "/workbuddy/v1/chat/completions" and the channel keeps deciding which
// upstream pool the request uses. The unified "/v1/responses" keeps its own
// path, which leaves channel selection to the model.
func responsesChatPath(path string) string {
	trimmed := strings.TrimRight(strings.TrimSpace(path), "/")
	for _, suffix := range []string{"/responses/compact", "/responses"} {
		if strings.HasSuffix(trimmed, suffix) {
			return strings.TrimSuffix(trimmed, suffix) + "/chat/completions"
		}
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
func ResponsesBridgeHandler(chat http.HandlerFunc, opts ResponsesBridgeOptions) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !requireMethod(w, r, http.MethodPost) {
			return
		}
		body, err := readBoundedJSONBody(w, r)
		if err != nil {
			return
		}
		var req ResponsesCreateRequest
		if err := json.Unmarshal(body, &req); err != nil {
			writeGrokError(w, http.StatusBadRequest, "invalid json")
			return
		}
		req.Model = normalizeModelID(req.Model)
		r = r.WithContext(middleware.WithRequestModel(r.Context(), req.Model))
		if !requireAPIKeyModel(w, r, req.Model) {
			return
		}
		// The bridge can always persist a response, streamed or not, because it
		// writes the terminal object the client saw rather than the raw stream.
		if err := validateResponsesCompatibilityFor(req, true); err != nil {
			writeGrokUpstreamError(w, err)
			return
		}
		if !expandBridgedPreviousResponse(w, r, &req, opts) {
			return
		}
		chatReq, err := chatRequestFromResponses(req)
		if err != nil {
			writeGrokUpstreamError(w, err)
			return
		}
		raw, err := json.Marshal(chatReq)
		if err != nil {
			writeGrokError(w, http.StatusInternalServerError, "failed to build chat request")
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
			onComplete := bridgedResponseRecorder(r, req, opts)
			streamThroughChat(subReq, chat, func(status int, header http.Header, reader io.Reader) {
				if status < 200 || status >= 300 {
					for key, values := range header {
						w.Header()[key] = values
					}
					w.WriteHeader(status)
					_, _ = io.Copy(w, reader)
					return
				}
				writeResponsesStreamFromChatReaderRequestWithHook(w, req, reader, onComplete)
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
			writeGrokUpstreamError(w, err)
			return
		}
		response := responsesObjectFromChat(req.Model, chatBody)
		applyBridgedResponseExtras(response, req)
		if storeRequested(req) {
			if err := saveBridgedResponse(r, req, response, opts); err != nil {
				writeGrokError(w, http.StatusServiceUnavailable, "failed to store response")
				return
			}
		}
		writeJSON(w, response)
	}
}

// ModelDispatcher routes a unified request by model: models the Grok handler
// serves natively keep it, every other model goes to the bridged handler, which
// resolves its channel from the model store. It is what makes a single base URL
// ("/v1") usable for every channel's models.
//
// Only POST bodies are inspected, and only to read the model: the body is
// handed to the chosen handler untouched. The model that decided the routing is
// published on the context, so the chosen handler and anything it calls can read
// the same resolution instead of looking it up a second time.
//
// isNativeModel errors rather than guessing. A lookup failure means the channel
// is unknown, and sending an unknown model to the native handler answers with
// Grok's "model does not exist" — a 400 that hides the real problem (a store
// outage, or a model that belongs to another channel). Such a request goes to
// the bridged handler, which resolves channels properly and reports a
// channel-aware error.
func ModelDispatcher(native, bridged http.HandlerFunc, isNativeModel func(context.Context, string) (bool, error)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			native(w, r)
			return
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			// The body is already half-read; neither handler can produce a
			// meaningful answer, so fail where the fault is.
			writeResponsesAPIError(w, http.StatusBadRequest, "invalid_request_error", "failed to read request body")
			return
		}
		// The body is only inspected to pick a provider; both handlers parse it
		// themselves, so it is handed back untouched.
		r.Body = io.NopCloser(bytes.NewReader(body))
		r.ContentLength = int64(len(body))

		var probe struct {
			Model string `json:"model"`
		}
		if err := json.Unmarshal(body, &probe); err != nil {
			// A malformed body has no model to route on. The native handler owns
			// the error response for its own wire format.
			native(w, r)
			return
		}
		if isNativeModel != nil {
			nativeModel, lookupErr := isNativeModel(r.Context(), probe.Model)
			if lookupErr == nil {
				// Publish the model only when the routing decision is trustworthy;
				// otherwise the bridged handler must resolve the channel itself.
				r = r.WithContext(middleware.WithRequestModel(r.Context(), probe.Model))
				if nativeModel {
					native(w, r)
					return
				}
			}
		}
		bridged(w, r)
	}
}

// ResponsesResourceHandler retrieves or deletes a stored response. Records are
// served from the bridge's store, which is the shared Redis store when one is
// configured and the in-process fallback otherwise. A miss answers with the
// Responses response_not_found envelope instead of Go's plain-text 404, so a
// client can tell "not stored here" from "no such route".
//
// The path is parsed before the method so the sibling endpoints below a response
// id (/cancel, /input_items) never look like a response id themselves.
func ResponsesResourceHandler(opts ResponsesBridgeOptions) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if action := responsesSubResourceAction(r.URL.Path); action != "" {
			responsesSubResourceHandler(action, opts)(w, r)
			return
		}
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
		st := opts.store()
		owner := responsesOwnerHash(r.Context())
		record, err := st.GetStoredResponse(r.Context(), responseID, owner)
		if err != nil {
			writeStoredResponseLookupError(w, err, "response not found")
			return
		}
		if r.Method == http.MethodDelete {
			if err := st.DeleteStoredResponse(r.Context(), responseID, owner); err != nil {
				writeGrokError(w, http.StatusServiceUnavailable, "failed to delete response")
				return
			}
			writeJSON(w, map[string]interface{}{"id": responseID, "object": "response.deleted", "deleted": true})
			return
		}
		if len(record.Body) == 0 {
			writeResponsesAPIError(w, http.StatusNotFound, "response_not_found", "response not found")
			return
		}
		contentType := strings.TrimSpace(record.ContentType)
		if contentType == "" {
			contentType = "application/json"
		}
		w.Header().Set("Content-Type", contentType)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(record.Body)
	}
}

// ResponsesChannelSubpath serves the Responses endpoints that hang off
// /responses for a chat-completions-only channel:
//
//   - POST /responses/            behaves like POST /responses (trailing slash)
//   - POST /responses/compact     runs the compaction request as an ordinary
//     completion: these channels have no native compact endpoint, and the
//     client's payload is a normal summarisation turn
//   - GET|DELETE /responses/{id}  served from the response store
//   - POST /responses/{id}/cancel and GET /responses/{id}/input_items
//     served from the response store (see responses_subresource.go)
func ResponsesChannelSubpath(chat http.HandlerFunc, opts ResponsesBridgeOptions) http.HandlerFunc {
	create := ResponsesBridgeHandler(chat, opts)
	resource := ResponsesResourceHandler(opts)
	return func(w http.ResponseWriter, r *http.Request) {
		path := strings.TrimRight(strings.TrimSpace(r.URL.Path), "/")
		if strings.HasSuffix(path, "/responses") || strings.HasSuffix(path, "/responses/compact") {
			create(w, r)
			return
		}
		resource(w, r)
	}
}

func storeRequested(req ResponsesCreateRequest) bool {
	return req.Store != nil && *req.Store
}

func applyBridgedResponseExtras(response map[string]interface{}, req ResponsesCreateRequest) {
	if response == nil {
		return
	}
	if len(req.Metadata) > 0 {
		response["metadata"] = req.Metadata
	}
	if strings.TrimSpace(req.Truncation) != "" {
		response["truncation"] = req.Truncation
	}
}

func saveBridgedResponse(r *http.Request, req ResponsesCreateRequest, response map[string]interface{}, opts ResponsesBridgeOptions) error {
	encoded, err := json.Marshal(response)
	if err != nil {
		return err
	}
	return opts.store().SaveStoredResponse(r.Context(), &store.StoredResponse{
		ResponseID:  parseLooseStringAny(response["id"]),
		OwnerHash:   responsesOwnerHash(r.Context()),
		Model:       req.Model,
		Provider:    bridgedResponseProvider,
		ContentType: "application/json",
		Body:        encoded,
		InputItems:  responsesInputItemsJSON(req.Input),
	}, opts.ttl())
}

// bridgedResponseRecorder persists the response the stream just finished with.
// A stream cannot report a storage failure to the client any more, so the
// failure is logged and the next turn sees response_not_found.
func bridgedResponseRecorder(r *http.Request, req ResponsesCreateRequest, opts ResponsesBridgeOptions) func(map[string]interface{}) {
	if !storeRequested(req) {
		return nil
	}
	return func(response map[string]interface{}) {
		if !strings.EqualFold(parseLooseStringAny(response["status"]), "completed") {
			return
		}
		applyBridgedResponseExtras(response, req)
		if err := saveBridgedResponse(r, req, response, opts); err != nil {
			slog.Warn("Failed to store bridged response", "model", req.Model, "error", err)
		}
	}
}

// expandBridgedPreviousResponse prepends the stored conversation to the current
// input when the client continues a stored response. It writes the error
// response and returns false when the request cannot be continued.
func expandBridgedPreviousResponse(w http.ResponseWriter, r *http.Request, req *ResponsesCreateRequest, opts ResponsesBridgeOptions) bool {
	previousID := strings.TrimSpace(req.PreviousResponseID)
	if previousID == "" {
		return true
	}
	previous, err := opts.store().GetStoredResponse(r.Context(), previousID, responsesOwnerHash(r.Context()))
	if err != nil {
		if errors.Is(err, store.ErrNoRows) {
			writeResponsesAPIError(w, http.StatusNotFound, "response_not_found", "previous response not found")
			return false
		}
		writeStoredResponseLookupError(w, err, "previous response not found")
		return false
	}
	expanded, err := expandStoredResponseInput(previous.Body, req.Input)
	if err != nil {
		writeResponsesAPIError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return false
	}
	req.Input = expanded
	return true
}
