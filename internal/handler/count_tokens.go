package handler

import (
	"encoding/json"
	"net/http"

	"orchids-api/internal/debug"
	"orchids-api/internal/orchids"
	"orchids-api/internal/tiktoken"
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

	maxTokens := 12000
	if h.config != nil && h.config.ContextMaxTokens > 0 {
		maxTokens = h.config.ContextMaxTokens
	}
	builtPrompt, aiClientHistory := orchids.BuildAIClientPromptAndHistory(req.Messages, req.System, req.Model, true /* noThinking */, "" /* workdir */, maxTokens)

	// Estimate tokens for prompt + chatHistory (aiclient format)
	totalTokens := tiktoken.EstimateTextTokens(builtPrompt)
	for _, item := range aiClientHistory {
		if c, ok := item["content"]; ok {
			totalTokens += tiktoken.EstimateTextTokens(c) + 15
		}
	}
	// Include tool definitions to avoid under-estimating real upstream input.
	if len(req.Tools) > 0 {
		if rawTools, err := json.Marshal(req.Tools); err == nil {
			totalTokens += tiktoken.EstimateTextTokens(string(rawTools))
		}
	}

	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(map[string]int{
		"input_tokens": totalTokens,
	}); err != nil {
		// Log error but we can't do much else since headers are written
		_ = err
	}
}
