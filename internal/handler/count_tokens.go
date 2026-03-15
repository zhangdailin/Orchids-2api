package handler

import (
	"github.com/goccy/go-json"
	"net/http"

	"orchids-api/internal/debug"
	"orchids-api/internal/orchids"
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

	logger := debug.New(h.config.DebugEnabled, h.config.DebugLogSSE)
	defer logger.Close()
	logger.LogIncomingRequest(req)

	breakdown := inputTokenBreakdown{}
	profile := ""
	if channelFromPath(r.URL.Path) == "warp" {
		if warpBD, warpProfile, err := estimateWarpInputTokenBreakdown("", req.Model, req.Messages, req.Tools, len(req.Tools) == 0); err == nil {
			breakdown = warpBD
			profile = warpProfile
		}
	}
	if breakdown.Total == 0 {
		builtPrompt, promptHistory, meta := orchids.BuildCodeFreeMaxPromptAndHistoryWithMeta(
			req.Messages,
			req.System,
			true, /* noThinking */
		)
		breakdown = estimateOrchidsInputTokenBreakdown(builtPrompt, promptHistory)
		profile = meta.Profile
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
		// Log error but we can't do much else since headers are written
		_ = err
	}
}
