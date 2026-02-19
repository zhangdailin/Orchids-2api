package orchids

import (
	"fmt"
	"net/http"
	"time"

	"github.com/gorilla/websocket"

	"orchids-api/internal/util"
)

// createWSConnection creates a new WebSocket connection (factory for pool)
func (c *Client) createWSConnection() (*websocket.Conn, error) {
	if c.config == nil {
		return nil, fmt.Errorf("config is nil")
	}

	token, err := c.getWSToken()
	if err != nil {
		return nil, fmt.Errorf("failed to get ws token: %w", err)
	}

	wsURL := c.buildWSURLAIClient(token)
	if wsURL == "" {
		return nil, fmt.Errorf("ws url not configured")
	}

	headers := http.Header{
		"User-Agent": []string{"Mozilla/5.0"},
		"Origin":     []string{"https://orchids.app"},
	}

	proxyFunc := http.ProxyFromEnvironment
	if c.config != nil {
		proxyFunc = util.ProxyFunc(c.config.ProxyHTTP, c.config.ProxyHTTPS, c.config.ProxyUser, c.config.ProxyPass, c.config.ProxyBypass)
	}

	dialer := websocket.Dialer{
		HandshakeTimeout: 45 * time.Second,
		Proxy:            proxyFunc,
	}

	conn, _, err := dialer.Dial(wsURL, headers)
	if err != nil {
		return nil, fmt.Errorf("failed to dial WebSocket: %w", err)
	}

	return conn, nil
}
