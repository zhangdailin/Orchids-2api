package orchids

import (
	"context"
	"strings"

	"orchids-api/internal/debug"
	"orchids-api/internal/upstream"
)

type orchidsTransport string

const (
	orchidsTransportSSE orchidsTransport = "sse"
	orchidsTransportWS  orchidsTransport = "ws"
)

func (c *Client) resolveTransport() orchidsTransport {
	if c == nil || c.config == nil {
		return orchidsTransportSSE
	}
	mode := strings.ToLower(strings.TrimSpace(c.config.UpstreamMode))
	if mode == "ws" || mode == "websocket" {
		return orchidsTransportWS
	}
	return orchidsTransportSSE
}

func (c *Client) dispatchTransport(
	ctx context.Context,
	transport orchidsTransport,
	req upstream.UpstreamRequest,
	onMessage func(upstream.SSEMessage),
	logger *debug.Logger,
) error {
	switch transport {
	case orchidsTransportWS:
		return c.sendRequestWS(ctx, req, onMessage, logger)
	default:
		return c.sendRequestSSE(ctx, req, onMessage, logger)
	}
}
