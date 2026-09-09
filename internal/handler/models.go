package handler

import (
	"context"
	"net/http"
	"strings"

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
}

type PublicModelsListResponse struct {
	Object string                `json:"object"`
	Data   []PublicModelResponse `json:"data"`
}

func publicModelResponse(id, ownedBy string) PublicModelResponse {
	return PublicModelResponse{ID: id, Object: "model", Created: 1677610602, OwnedBy: ownedBy}
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
		for _, modelID := range warp.EffectiveAccountModelIDs(acc, choices) {
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
	if filterChannel == "" || strings.EqualFold(filterChannel, "warp") {
		for _, modelID := range []string{warpChatModelID, warpAgentModelID} {
			if middleware.APIKeyAllowsModel(ctx, modelID) {
				publicModels = append(publicModels, publicModelResponse(modelID, "Warp"))
			}
		}
	}
	for _, m := range allModels {
		mChannel, ok := isVisiblePublicModel(m, filterChannel)
		if !ok {
			continue
		}
		if strings.EqualFold(mChannel, "warp") && isWarpVirtualModel(m.ModelID) {
			continue
		}
		if strings.EqualFold(mChannel, "warp") && warpVisible != nil {
			modelID := normalizeRequestedModelID(m.ModelID)
			if _, ok := warpVisible[modelID]; !ok {
				continue
			}
		}
		if !middleware.APIKeyAllowsModel(ctx, m.ModelID) {
			continue
		}

		entry := publicModelResponse(m.ModelID, mChannel)
		entry.Capabilities = m.Capabilities
		entry.Provider = m.Provider
		entry.UpstreamModel = m.UpstreamModel
		publicModels = append(publicModels, entry)
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
	if strings.EqualFold(filterChannel, "warp") {
		if m := warpVirtualModelRecord(id); m != nil {
			resp := publicModelResponse(m.ModelID, "Warp")
			if err := json.NewEncoder(w).Encode(resp); err != nil {
				apperrors.New("api_error", "Failed to encode response", http.StatusInternalServerError).WriteResponse(w)
			}
			return
		}
	}
	var (
		m   *store.Model
		err error
	)
	if filterChannel != "" {
		m, err = h.loadBalancer.Store.GetModelByChannelAndModelID(ctx, filterChannel, id)
	} else {
		m, err = h.loadBalancer.Store.GetModelByModelID(ctx, id)
	}
	if err != nil {
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

	resp := publicModelResponse(m.ModelID, mChannel)

	if err := json.NewEncoder(w).Encode(resp); err != nil {
		apperrors.New("api_error", "Failed to encode response", http.StatusInternalServerError).WriteResponse(w)
	}
}
