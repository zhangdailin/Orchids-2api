package orchids

import (
	"context"
	"fmt"
	"net/http"

	"github.com/gorilla/websocket"

	"orchids-api/internal/util"
)

func (c *Client) dialWSConnection(ctx context.Context) (*websocket.Conn, error) {
	if c.config == nil {
		return nil, fmt.Errorf("config is nil")
	}

	token, err := c.getWSToken()
	if err != nil {
		return nil, fmt.Errorf("failed to get ws token: %w", err)
	}

	wsURL := c.buildWSURL(token)
	if wsURL == "" {
		return nil, fmt.Errorf("ws url not configured")
	}

	headers := http.Header{
		"User-Agent": []string{orchidsWSUserAgent},
		"Origin":     []string{orchidsWSOrigin},
	}

	proxyFunc := http.ProxyFromEnvironment
	if c.config != nil {
		proxyFunc = util.ProxyFunc(c.config.ProxyHTTP, c.config.ProxyHTTPS, c.config.ProxyUser, c.config.ProxyPass, c.config.ProxyBypass)
	}

	dialer := websocket.Dialer{
		HandshakeTimeout: orchidsWSConnectTimeout,
		Proxy:            proxyFunc,
	}

	conn, _, err := dialer.DialContext(ctx, wsURL, headers)
	if err != nil {
		return nil, fmt.Errorf("failed to dial WebSocket: %w", err)
	}

	return conn, nil
}
