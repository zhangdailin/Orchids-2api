package handler

import (
	"net/http"
	"strings"

	"github.com/goccy/go-json"

	"orchids-api/internal/debug"
	apperrors "orchids-api/internal/errors"
	"orchids-api/internal/middleware"
)

// HandleCountTokens handles /v1/messages/count_tokens requests.
func (h *Handler) HandleCountTokens(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req ClaudeRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "Invalid request body", http.StatusBadRequest)
		return
	}
	if !middleware.APIKeyAllowsModel(r.Context(), req.Model) {
		apperrors.New("permission_error", "API key is not allowed to use model "+strings.TrimSpace(req.Model), http.StatusForbidden).WriteResponse(w)
		return
	}

	logger := debug.NewForContext(r.Context(), h.config.DebugEnabled, h.config.DebugLogSSE)
	defer logger.Close()
	logger.LogIncomingRequest(req)

	breakdown := inputTokenBreakdown{}
	profile := ""
	channel := channelFromPath(r.URL.Path)
	if channel == "warp" {
		if warpBD, warpProfile, err := estimateWarpInputTokenBreakdown("", req.Model, req.Messages, req.System, req.Tools, len(req.Tools) == 0, ""); err == nil {
			breakdown = warpBD
			profile = warpProfile
		}
	}
	if breakdown.Total == 0 && channel == "puter" {
		breakdown = estimateInputTokenBreakdown(extractUserText(req.Messages), req.Tools)
		profile = "puter"
	}
	if breakdown.Total == 0 {
		builtPrompt := strings.TrimSpace(extractUserText(req.Messages))
		breakdown = estimateInputTokenBreakdown(builtPrompt, req.Tools)
		if profile == "" {
			profile = channel
		}
	}

	w.Header().Set("Content-Type", "application/json")
	resp := map[string]interface{}{
		"input_tokens":   breakdown.Total,
		"prompt_profile": profile,
		"breakdown": map[string]int{
			"base_prompt_tokens":    breakdown.BasePromptTokens,
			"system_context_tokens": breakdown.SystemContextTokens,
			"history_tokens":        breakdown.HistoryTokens,
			"tools_tokens":          breakdown.ToolsTokens,
		},
	}
	if err := json.NewEncoder(w).Encode(resp); err != nil {
		_ = err
	}
}
