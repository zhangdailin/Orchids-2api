package orchids

import (
	"fmt"
	"math/rand/v2"
	"strings"

	"orchids-api/internal/config"
	"orchids-api/internal/upstream"
)

func orchidsProjectID(cfg *config.Config, req upstream.UpstreamRequest) string {
	if value := strings.TrimSpace(req.ProjectID); value != "" {
		return value
	}
	if cfg != nil {
		return strings.TrimSpace(cfg.ProjectID)
	}
	return ""
}

func orchidsThinkingMode(req upstream.UpstreamRequest) string {
	if req.NoThinking {
		return "disabled"
	}
	return "enabled"
}

func orchidsMaxTokens(cfg *config.Config) int {
	if cfg != nil && cfg.ContextMaxTokens > 0 {
		return cfg.ContextMaxTokens
	}
	return 12000
}

func orchidsChatSessionID(req upstream.UpstreamRequest) string {
	chatSessionID := strings.TrimSpace(req.ChatSessionID)
	if chatSessionID == "" {
		chatSessionID = fmt.Sprintf("chat_%d", rand.IntN(90000000)+10000000)
	}
	return chatSessionID
}

func (c *Client) buildSSEAgentRequest(req upstream.UpstreamRequest) OrchidsRequest {
	return buildOrchidsRequest(req, c.config)
}
