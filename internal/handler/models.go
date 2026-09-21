package handler

import (
	"context"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/goccy/go-json"

	apperrors "orchids-api/internal/errors"
	"orchids-api/internal/middleware"
	"orchids-api/internal/modelpolicy"
	"orchids-api/internal/store"
	"orchids-api/internal/warp"
)

type PublicModelResponse struct {
	ID            string   `json:"id"`
	Object        string   `json:"object"`
	Created       int64    `json:"created"`
	OwnedBy       string   `json:"owned_by"`
	Capabilities  []string `json:"capabilities,omitempty"`
	Provider      string   `json:"provider,omitempty"`
	UpstreamModel string   `json:"upstream_model,omitempty"`
	BillingTier   string   `json:"billing_tier,omitempty"`
	BillingSource string   `json:"billing_source,omitempty"`
	// ContextLength is the model's real input-token window, as observed from the
	// channel's own catalog. It is omitted when nothing was observed, because a
	// client that reads a wrong number budgets against the wrong number: too low
	// and it reports a context overflow after the model already answered, too high
	// and it keeps sending a history the upstream will refuse.
	ContextLength int `json:"context_length,omitempty"`
	// MaxInputTokens carries the same observation under the name some clients
	// look for. Both are emitted so a client that only reads one still finds it.
	MaxInputTokens int `json:"max_input_tokens,omitempty"`
	// MaxOutputTokens is the declared output budget where the catalog publishes
	// one. Zero means unobserved and is omitted.
	MaxOutputTokens int `json:"max_output_tokens,omitempty"`
}

type PublicModelsListResponse struct {
	Object string                `json:"object"`
	Data   []PublicModelResponse `json:"data"`
}

// legacyModelCreated is the placeholder the API used before a route row carried
// its own creation time. It stays only for rows stored by an older build.
const legacyModelCreated = 1677610602

func publicModelResponse(id, ownedBy string, createdAt time.Time) PublicModelResponse {
	created := int64(legacyModelCreated)
	if !createdAt.IsZero() {
		created = createdAt.Unix()
	}
	return PublicModelResponse{ID: id, Object: "model", Created: created, OwnedBy: ownedBy}
}

func isVisiblePublicModel(m *store.Model, filterChannel string) (string, bool) {
	if m == nil {
		return "", false
	}

	mChannel := strings.TrimSpace(m.Channel)
	if filterChannel != "" && !strings.EqualFold(mChannel, filterChannel) {
		return mChannel, false
	}
	if !m.Status.Enabled() {
		return mChannel, false
	}
	if strings.EqualFold(mChannel, "grok") && !modelpolicy.IsVisibleGrokModel(m.ModelID, m.Verified) {
		return mChannel, false
	}
	return mChannel, true
}

func (h *Handler) visibleWarpModelSet(ctx context.Context) map[string]struct{} {
	if h == nil || h.loadBalancer == nil || h.loadBalancer.Store == nil {
		return nil
	}
	accounts, err := h.loadBalancer.Store.GetEnabledAccounts(ctx)
	if err != nil || len(accounts) == 0 {
		return nil
	}
	choices, err := warp.LoadAccountModelChoices(ctx, h.loadBalancer.Store)
	if err != nil || choices == nil || len(choices.Accounts) == 0 {
		return nil
	}
	out := map[string]struct{}{}
	for _, acc := range accounts {
		if acc == nil || !strings.EqualFold(strings.TrimSpace(acc.AccountType), "warp") {
			continue
		}
		// Public visibility follows the upstream discovery cache for every
		// account tier; do not reduce free accounts to a synthetic default.
		for _, modelID := range choices.Accounts[strconv.FormatInt(acc.ID, 10)] {
			if modelID = strings.TrimSpace(modelID); modelID != "" {
				out[modelID] = struct{}{}
			}
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func (h *Handler) warpModelVisible(ctx context.Context, modelID string) bool {
	visible := h.visibleWarpModelSet(ctx)
	if visible == nil {
		return true
	}
	resolvedModelID := normalizeRequestedModelID(modelID)
	if resolvedModelID == "" {
		return true
	}
	_, ok := visible[resolvedModelID]
	return ok
}

// externalPublicModelID is the name clients see for a route row. Grok routes
// carry a plane qualifier internally (console/, build/) that grok2api never
// publishes; every other channel's row is already the public name.
func externalPublicModelID(channel, internalID string) string {
	if strings.EqualFold(strings.TrimSpace(channel), "grok") {
		if external := modelpolicy.ExternalPublicID(internalID); external != "" {
			return external
		}
	}
	return normalizeRequestedModelID(internalID)
}

func containsPublicModel(items []PublicModelResponse, id string) bool {
	for _, item := range items {
		if strings.EqualFold(strings.TrimSpace(item.ID), strings.TrimSpace(id)) {
			return true
		}
	}
	return false
}

func appendGrokCompatibilityAliases(items []PublicModelResponse, entry PublicModelResponse) []PublicModelResponse {
	if !strings.EqualFold(entry.OwnedBy, "grok") {
		return items
	}
	base := modelpolicy.GrokModelSlug(entry.ID)
	aliases := make([]string, 0, 6)
	if strings.Contains(strings.TrimSpace(entry.ID), "/") {
		aliases = append(aliases, base)
	}
	levels := modelpolicy.SupportedReasoningEfforts(entry.ID)
	if len(levels) >= 2 {
		for _, level := range levels {
			aliases = append(aliases, base+"-"+level)
		}
	}
	for _, alias := range aliases {
		duplicate := false
		for _, existing := range items {
			if strings.EqualFold(existing.ID, alias) {
				duplicate = true
				break
			}
		}
		if duplicate {
			continue
		}
		copy := entry
		copy.ID = alias
		items = append(items, copy)
	}
	return items
}

func (h *Handler) HandleModels(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		apperrors.New("invalid_request_error", "Method not allowed", http.StatusMethodNotAllowed).WriteResponse(w)
		return
	}

	w.Header().Set("Content-Type", "application/json")

	// Determine channel filter based on path prefix
	filterChannel := channelFromPath(r.URL.Path)

	ctx := r.Context()
	if h.loadBalancer == nil || h.loadBalancer.Store == nil {
		apperrors.New("api_error", "Model store not configured", http.StatusServiceUnavailable).WriteResponse(w)
		return
	}
	allModels, err := h.loadBalancer.Store.ListModels(ctx)
	if err != nil {
		apperrors.New("api_error", "Failed to fetch models: "+err.Error(), http.StatusInternalServerError).WriteResponse(w)
		return
	}
	var warpVisible map[string]struct{}
	if filterChannel == "" || strings.EqualFold(filterChannel, "warp") {
		warpVisible = h.visibleWarpModelSet(ctx)
	}
	var publicModels []PublicModelResponse
	// One read of the observed catalogs answers every row below, so the model
	// list reports the same window the request path forwards upstream.
	contextWindows := h.observedModelContextWindows(ctx)
	for _, m := range allModels {
		mChannel, ok := isVisiblePublicModel(m, filterChannel)
		if !ok {
			continue
		}
		if isWarpVirtualModel(m.ModelID) {
			continue
		}
		if strings.EqualFold(mChannel, "warp") && warpVisible != nil {
			modelID := normalizeRequestedModelID(m.ModelID)
			if _, ok := warpVisible[modelID]; !ok {
				continue
			}
		}
		// The provider qualifier is a routing detail: grok2api publishes the bare
		// name (ExternalPublicID) and keeps the qualified one as an input alias.
		// A Grok route named console/grok-4.3 therefore appears as grok-4.3, and
		// the two spellings still resolve to the same route.
		publicID := externalPublicModelID(mChannel, m.ModelID)
		if !middleware.APIKeyAllowsModel(ctx, m.ModelID) && !middleware.APIKeyAllowsModel(ctx, publicID) {
			continue
		}
		// One public entry per external ID: routes that differ only by plane are
		// the same public model (the admin plane lists them grouped).
		if containsPublicModel(publicModels, publicID) {
			continue
		}

		entry := publicModelResponse(publicID, mChannel, m.CreatedAt)
		entry.Capabilities = m.Capabilities
		entry.Provider = m.Provider
		entry.UpstreamModel = m.UpstreamModel
		entry.BillingTier = m.BillingTier
		entry.BillingSource = m.BillingSource
		// The window is looked up by the route's own id first: that is what the
		// channel's catalog was keyed by when it was observed. The public alias is
		// the fallback for a channel that publishes a different spelling.
		input, output := h.modelContextWindow(ctx, contextWindows, mChannel, m.ModelID, publicID)
		entry.ContextLength = input
		entry.MaxInputTokens = input
		entry.MaxOutputTokens = output
		publicModels = append(publicModels, entry)
		publicModels = appendGrokCompatibilityAliases(publicModels, entry)
	}

	// Codex-family clients ask for a richer catalog that carries the context
	// window, input modalities and reasoning levels the OpenAI list omits.
	if strings.TrimSpace(r.URL.Query().Get("client_version")) != "" {
		writeCodexModelCatalog(w, r, newCodexModelCatalog(publicModels))
		return
	}

	resp := PublicModelsListResponse{
		Object: "list",
		Data:   publicModels,
	}

	if err := json.NewEncoder(w).Encode(resp); err != nil {
		apperrors.New("api_error", "Failed to encode response", http.StatusInternalServerError).WriteResponse(w)
	}
}

// HandleModelByID is optional for public API but good for completeness
func (h *Handler) HandleModelByID(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		apperrors.New("invalid_request_error", "Method not allowed", http.StatusMethodNotAllowed).WriteResponse(w)
		return
	}

	w.Header().Set("Content-Type", "application/json")

	// Extract ID from path
	// Paths could be: /v1/models/{id}, /warp/v1/models/{id}, /puter/v1/models/{id}, /grok/v1/models/{id}
	path := r.URL.Path
	var id string
	if strings.HasPrefix(path, "/warp/v1/models/") {
		id = strings.TrimPrefix(path, "/warp/v1/models/")
	} else if strings.HasPrefix(path, "/puter/v1/models/") {
		id = strings.TrimPrefix(path, "/puter/v1/models/")
	} else if strings.HasPrefix(path, "/grok/v1/models/") {
		id = strings.TrimPrefix(path, "/grok/v1/models/")
	} else {
		id = strings.TrimPrefix(path, "/v1/models/")
	}

	if id == "" {
		apperrors.New("invalid_request_error", "Model ID required", http.StatusBadRequest).WriteResponse(w)
		return
	}
	if isWarpVirtualModel(id) {
		apperrors.New("invalid_request_error", "Model not found", http.StatusNotFound).WriteResponse(w)
		return
	}
	if !middleware.APIKeyAllowsModel(r.Context(), id) {
		apperrors.New("invalid_request_error", "Model not found", http.StatusNotFound).WriteResponse(w)
		return
	}

	ctx := r.Context()
	if h.loadBalancer == nil || h.loadBalancer.Store == nil {
		apperrors.New("api_error", "Model store not configured", http.StatusServiceUnavailable).WriteResponse(w)
		return
	}

	filterChannel := channelFromPath(path)
	var (
		m   *store.Model
		err error
	)
	if filterChannel != "" {
		m, err = h.loadBalancer.Store.GetModelByChannelAndModelID(ctx, filterChannel, id)
	} else {
		m, err = h.loadBalancer.Store.GetModelByModelID(ctx, id)
	}
	if err != nil || m == nil {
		// The catalog collapses "<family>-<effort>" variants into one entry and
		// advertises the *family* slug, so a client validating a catalog entry
		// asks for a name no row carries. Resolve it onto the variant the request
		// path would use; answering 404 here is what pushes a client back to
		// guessing suffixes.
		if variant := h.resolveEffortModelVariant(ctx, id, "", filterChannel); variant != "" && variant != id {
			if filterChannel != "" {
				m, err = h.loadBalancer.Store.GetModelByChannelAndModelID(ctx, filterChannel, variant)
			} else {
				m, err = h.loadBalancer.Store.GetModelByModelID(ctx, variant)
			}
		}
	}
	if err != nil || m == nil {
		apperrors.New("invalid_request_error", "Model not found", http.StatusNotFound).WriteResponse(w)
		return
	}
	mChannel, ok := isVisiblePublicModel(m, filterChannel)
	if !ok {
		apperrors.New("invalid_request_error", "Model not found", http.StatusNotFound).WriteResponse(w)
		return
	}
	if strings.EqualFold(mChannel, "warp") && !h.warpModelVisible(ctx, m.ModelID) {
		apperrors.New("invalid_request_error", "Model not found", http.StatusNotFound).WriteResponse(w)
		return
	}

	resp := publicModelResponse(m.ModelID, mChannel, m.CreatedAt)
	// The single-model lookup answers with the same window the list publishes, so
	// a client that validates one catalog entry learns the real number instead of
	// falling back to its own default.
	contextWindows := h.observedModelContextWindows(ctx)
	input, output := h.modelContextWindow(ctx, contextWindows, mChannel, m.ModelID, normalizeRequestedModelID(id))
	resp.ContextLength = input
	resp.MaxInputTokens = input
	resp.MaxOutputTokens = output

	if err := json.NewEncoder(w).Encode(resp); err != nil {
		apperrors.New("api_error", "Failed to encode response", http.StatusInternalServerError).WriteResponse(w)
	}
}
