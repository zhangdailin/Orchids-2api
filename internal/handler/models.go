package handler

import (
	"encoding/json"
	"net/http"
	"strings"
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

func (h *Handler) HandleModels(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		h.writeErrorResponse(w, "invalid_request_error", "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	w.Header().Set("Content-Type", "application/json")

	// Determine channel filter based on path prefix
	filterChannel := channelFromPath(r.URL.Path)

	ctx := r.Context()
	if h.loadBalancer == nil || h.loadBalancer.Store == nil {
		h.writeErrorResponse(w, "api_error", "Model store not configured", http.StatusServiceUnavailable)
		return
	}
	allModels, err := h.loadBalancer.Store.ListModels(ctx)
	if err != nil {
		h.writeErrorResponse(w, "api_error", "Failed to fetch models: "+err.Error(), http.StatusInternalServerError)
		return
	}

	var publicModels []PublicModelResponse
	for _, m := range allModels {
		// If filtering is active (e.g. /orchids/v1/models), skip models from other channels
		if filterChannel != "" {
			mChannel := m.Channel
			if strings.TrimSpace(mChannel) == "" {
				mChannel = "orchids" // Default assumption
			}
			if !strings.EqualFold(mChannel, filterChannel) {
				continue
			}
		}

		// Only return enabled models for public API
		if !m.Status {
			continue
		}

		publicModels = append(publicModels, PublicModelResponse{
			ID:      m.ModelID, // Use the actual model ID (e.g. "claude-3-opus") not the DB ID
			Object:  "model",
			Created: 1677610602, // Echo a static timestamp or 0 if unknown
			OwnedBy: m.Channel,
		})
	}

	resp := PublicModelsListResponse{
		Object: "list",
		Data:   publicModels,
	}

	if err := json.NewEncoder(w).Encode(resp); err != nil {
		h.writeErrorResponse(w, "api_error", "Failed to encode response", http.StatusInternalServerError)
	}
}

// HandleModelByID is optional for public API but good for completeness
func (h *Handler) HandleModelByID(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		h.writeErrorResponse(w, "invalid_request_error", "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	w.Header().Set("Content-Type", "application/json")

	// Extract ID from path
	// Paths could be: /v1/models/{id}, /orchids/v1/models/{id}, /warp/v1/models/{id}
	path := r.URL.Path
	var id string
	if strings.HasPrefix(path, "/orchids/v1/models/") {
		id = strings.TrimPrefix(path, "/orchids/v1/models/")
	} else if strings.HasPrefix(path, "/warp/v1/models/") {
		id = strings.TrimPrefix(path, "/warp/v1/models/")
	} else {
		id = strings.TrimPrefix(path, "/v1/models/")
	}

	if id == "" {
		h.writeErrorResponse(w, "invalid_request_error", "Model ID required", http.StatusBadRequest)
		return
	}

	ctx := r.Context()
	if h.loadBalancer == nil || h.loadBalancer.Store == nil {
		h.writeErrorResponse(w, "api_error", "Model store not configured", http.StatusServiceUnavailable)
		return
	}

	m, err := h.loadBalancer.Store.GetModelByModelID(ctx, id)
	if err != nil {
		h.writeErrorResponse(w, "invalid_request_error", "Model not found", http.StatusNotFound)
		return
	}

	// Check channel filter if applicable
	filterChannel := channelFromPath(path)
	if filterChannel != "" {
		mChannel := m.Channel
		if strings.TrimSpace(mChannel) == "" {
			mChannel = "orchids"
		}
		if !strings.EqualFold(mChannel, filterChannel) {
			h.writeErrorResponse(w, "invalid_request_error", "Model not found in this channel", http.StatusNotFound)
			return
		}
	}

	resp := PublicModelResponse{
		ID:      m.ModelID,
		Object:  "model",
		Created: 1677610602,
		OwnedBy: m.Channel,
	}

	if err := json.NewEncoder(w).Encode(resp); err != nil {
		h.writeErrorResponse(w, "api_error", "Failed to encode response", http.StatusInternalServerError)
	}
}
