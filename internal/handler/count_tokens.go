package handler

import (
	"encoding/json"
	"net/http"
	"strings"

	"orchids-api/internal/debug"
	"orchids-api/internal/prompt"
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

	toolCallMode := strings.ToLower(strings.TrimSpace(h.config.ToolCallMode))
	if toolCallMode == "" {
		toolCallMode = "proxy"
	}
	if isPlanMode(req.Messages) {
		toolCallMode = "proxy"
	}

	effectiveTools := req.Tools
	if !h.config.DisableToolFilter && (toolCallMode == "auto" || toolCallMode == "internal") {
		effectiveTools = filterSupportedTools(effectiveTools)
	}

	conversationKey := conversationKeyForRequest(r, req)
	opts := prompt.PromptOptions{
		Context:          r.Context(),
		ConversationID:   conversationKey,
		MaxTokens:        h.config.ContextMaxTokens,
		SummaryMaxTokens: h.config.ContextSummaryMaxTokens,
		KeepTurns:        h.config.ContextKeepTurns,
		SummaryCache:     h.summaryCache,
	}

	builtPrompt := prompt.BuildPromptV2WithOptions(prompt.ClaudeAPIRequest{
		Model:    req.Model,
		Messages: req.Messages,
		System:   req.System,
		Tools:    effectiveTools,
		Stream:   false,
	}, opts)

	inputTokens := h.estimateInputTokens(r.Context(), req.Model, builtPrompt)

	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(map[string]int{
		"input_tokens": inputTokens,
	}); err != nil {
		// Log error but we can't do much else since headers are written
		_ = err
	}
}
