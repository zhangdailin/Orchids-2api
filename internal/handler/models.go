package handler

import (
	"github.com/goccy/go-json"
	"net/http"
	"strings"

	apperrors "orchids-api/internal/errors"
	"orchids-api/internal/modelpolicy"
	"orchids-api/internal/store"
)

type PublicModelResponse struct {
	ID      string `json:"id"`
	Object  string `json:"object"`
	Created int64  `json:"created"`
	OwnedBy string `json:"owned_by"`
}

type PublicModelsListResponse struct {
	Object string                `json:"object"`
	Data   []PublicModelResponse `json:"data"`
}

func normalizePublicModelChannel(channel string) string {
	channel = strings.TrimSpace(channel)
	if channel == "" {
		return "orchids"
	}
	return channel
}

func isVisiblePublicModel(m *store.Model, filterChannel string) (string, bool) {
	if m == nil {
		return "", false
	}

	mChannel := normalizePublicModelChannel(m.Channel)
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

	var publicModels []PublicModelResponse
	for _, m := range allModels {
		mChannel, ok := isVisiblePublicModel(m, filterChannel)
		if !ok {
			continue
		}

		publicModels = append(publicModels, PublicModelResponse{
			ID:      m.ModelID, // Use the actual model ID (e.g. "claude-3-opus") not the DB ID
			Object:  "model",
			Created: 1677610602, // Echo a static timestamp or 0 if unknown
			OwnedBy: mChannel,
		})
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
	// Paths could be: /v1/models/{id}, /orchids/v1/models/{id}, /warp/v1/models/{id}, /bolt/v1/models/{id}, /puter/v1/models/{id}, /grok/v1/models/{id}
	path := r.URL.Path
	var id string
	if strings.HasPrefix(path, "/orchids/v1/models/") {
		id = strings.TrimPrefix(path, "/orchids/v1/models/")
	} else if strings.HasPrefix(path, "/warp/v1/models/") {
		id = strings.TrimPrefix(path, "/warp/v1/models/")
	} else if strings.HasPrefix(path, "/bolt/v1/models/") {
		id = strings.TrimPrefix(path, "/bolt/v1/models/")
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
	if err != nil {
		apperrors.New("invalid_request_error", "Model not found", http.StatusNotFound).WriteResponse(w)
		return
	}
	mChannel, ok := isVisiblePublicModel(m, filterChannel)
	if !ok {
		apperrors.New("invalid_request_error", "Model not found", http.StatusNotFound).WriteResponse(w)
		return
	}

	resp := PublicModelResponse{
		ID:      m.ModelID,
		Object:  "model",
		Created: 1677610602,
		OwnedBy: mChannel,
	}

	if err := json.NewEncoder(w).Encode(resp); err != nil {
		apperrors.New("api_error", "Failed to encode response", http.StatusInternalServerError).WriteResponse(w)
	}
}
