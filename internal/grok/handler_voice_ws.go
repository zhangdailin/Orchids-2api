package grok

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"

	"orchids-api/internal/grok/egress"
	"orchids-api/internal/util"
)

const (
	voiceWSHandshakeTimeout = 20 * time.Second
	voiceWSMessageLimit     = 16 << 20
	voiceWSIdleTimeout      = 2 * time.Minute
	voiceWSPingInterval     = 30 * time.Second
)

var consoleVoiceUpgrader = websocket.Upgrader{
	ReadBufferSize:    32 << 10,
	WriteBufferSize:   32 << 10,
	EnableCompression: true,
	CheckOrigin:       func(*http.Request) bool { return true },
}

// HandleRealtime proxies the Console realtime speech WebSocket.
func (h *Handler) HandleRealtime(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodGet) {
		return
	}
	h.handleVoiceWebSocket(w, r, "realtime", "grok-voice-latest")
}

func (h *Handler) handleVoiceWebSocket(w http.ResponseWriter, r *http.Request, path, defaultModel string) {
	if !websocket.IsWebSocketUpgrade(r) {
		status := http.StatusBadRequest
		if path == "stt" {
			status = http.StatusMethodNotAllowed
		}
		writeResponsesAPIError(w, status, "invalid_request", "a WebSocket Upgrade request is required")
		return
	}
	modelID := normalizeModelID(firstNonEmpty(r.URL.Query().Get("model"), defaultModel))
	capability := "realtime"
	if path == "stt" {
		capability = "stt"
	}
	spec, ok := h.requireVoiceModel(w, r, modelID, capability)
	if !ok {
		return
	}
	if h == nil || h.currentClient() == nil {
		writeResponsesAPIError(w, http.StatusServiceUnavailable, "service_unavailable", "grok client not configured")
		return
	}
	sess, err := h.openConsoleAccountSession(r.Context(), nil, modelID)
	if err != nil {
		writeResponsesAPIError(w, http.StatusServiceUnavailable, "account_unavailable", "no available Grok Console account: "+err.Error())
		return
	}
	defer sess.Close()

	upstream, response, releaseEgress, err := h.currentClient().dialConsoleVoiceWebSocket(r.Context(), sess.token, path, spec.UpstreamModel)
	if err != nil {
		if response != nil && response.Body != nil {
			_ = response.Body.Close()
		}
		if markAllGrokAccountStatuses(err) {
			h.markAccountStatus(r.Context(), sess.acc, err)
		}
		writeResponsesAPIError(w, upstreamHTTPResponseStatus(err), "upstream_error", err.Error())
		return
	}
	defer releaseEgress()
	defer upstream.Close()
	client, err := consoleVoiceUpgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	defer client.Close()
	client.SetReadLimit(voiceWSMessageLimit)
	upstream.SetReadLimit(voiceWSMessageLimit)
	configureVoiceWSDeadline(client)
	configureVoiceWSDeadline(upstream)

	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	var closeOnce sync.Once
	closeBoth := func() {
		closeOnce.Do(func() {
			_ = client.Close()
			_ = upstream.Close()
		})
	}
	go func() {
		<-ctx.Done()
		closeBoth()
	}()

	errCh := make(chan error, 2)
	go func() { errCh <- pumpVoiceWebSocket(client, upstream) }()
	go func() { errCh <- pumpVoiceWebSocket(upstream, client) }()
	ping := time.NewTicker(voiceWSPingInterval)
	defer ping.Stop()
	for {
		select {
		case <-errCh:
			closeBoth()
			return
		case <-ping.C:
			deadline := time.Now().Add(5 * time.Second)
			if client.WriteControl(websocket.PingMessage, nil, deadline) != nil || upstream.WriteControl(websocket.PingMessage, nil, deadline) != nil {
				closeBoth()
				return
			}
		case <-ctx.Done():
			closeBoth()
			return
		}
	}
}

func configureVoiceWSDeadline(conn *websocket.Conn) {
	if conn == nil {
		return
	}
	_ = conn.SetReadDeadline(time.Now().Add(voiceWSIdleTimeout))
	conn.SetPongHandler(func(string) error {
		return conn.SetReadDeadline(time.Now().Add(voiceWSIdleTimeout))
	})
}

func pumpVoiceWebSocket(source, destination *websocket.Conn) error {
	for {
		messageType, payload, err := source.ReadMessage()
		if err != nil {
			return err
		}
		if err := destination.WriteMessage(messageType, payload); err != nil {
			return err
		}
		_ = source.SetReadDeadline(time.Now().Add(voiceWSIdleTimeout))
	}
}

func (c *Client) dialConsoleVoiceWebSocket(ctx context.Context, token, path, model string) (*websocket.Conn, *http.Response, func(), error) {
	if c == nil {
		return nil, nil, func() {}, fmt.Errorf("grok client not configured")
	}
	path = strings.Trim(strings.TrimSpace(path), "/")
	if path != "realtime" && path != "stt" {
		return nil, nil, func() {}, fmt.Errorf("unsupported Console voice WebSocket path")
	}
	base := "https://console.x.ai/v1"
	if c.cfg != nil {
		base = c.cfg.GrokConsoleBaseURLOrDefault()
	}
	endpoint, err := url.Parse(strings.TrimRight(base, "/") + "/" + path)
	if err != nil || endpoint.Host == "" {
		return nil, nil, func() {}, fmt.Errorf("invalid Console voice WebSocket endpoint")
	}
	switch strings.ToLower(endpoint.Scheme) {
	case "https":
		endpoint.Scheme = "wss"
	case "http":
		endpoint.Scheme = "ws"
	case "wss", "ws":
	default:
		return nil, nil, func() {}, fmt.Errorf("unsupported Console voice WebSocket scheme")
	}
	query := endpoint.Query()
	if model = strings.TrimSpace(model); model != "" {
		query.Set("model", model)
	}
	endpoint.RawQuery = query.Encode()

	proofURL := *endpoint
	if proofURL.Scheme == "wss" {
		proofURL.Scheme = "https"
	} else {
		proofURL.Scheme = "http"
	}
	dialer := websocket.Dialer{
		HandshakeTimeout:  voiceWSHandshakeTimeout,
		Proxy:             util.ProxyFuncFromConfig(c.cfg),
		EnableCompression: true,
	}
	releaseEgress := func() {}
	var leaseNodeID string
	var leaseUserAgent string
	var leaseCFCookies string
	if c.egress != nil && c.egress.Enabled() {
		lease, acquireErr := c.egress.Acquire(ctx, "console", dpopCacheKey(token))
		if acquireErr != nil {
			return nil, nil, releaseEgress, fmt.Errorf("acquire Console voice egress: %w", acquireErr)
		}
		releaseEgress = lease.Release
		leaseNodeID = lease.NodeID
		leaseUserAgent = lease.UserAgent
		leaseCFCookies = lease.CFCookies
		proxyURL, parseErr := util.ParseProxyURL(lease.ProxyURL)
		if parseErr != nil {
			releaseEgress()
			return nil, nil, func() {}, fmt.Errorf("parse Console voice egress proxy: %w", parseErr)
		}
		if proxyURL != nil && strings.EqualFold(proxyURL.Scheme, "socks5h") {
			clone := *proxyURL
			clone.Scheme = "socks5"
			proxyURL = &clone
		}
		dialer.Proxy = http.ProxyURL(proxyURL)
	}
	succeeded := false
	defer func() {
		if !succeeded {
			releaseEgress()
		}
	}()
	for attempt := 0; attempt < 2; attempt++ {
		session, cacheKey, sessionErr := c.dpopSession(ctx, token)
		if sessionErr != nil {
			return nil, nil, func() {}, sessionErr
		}
		proofRequest, requestErr := http.NewRequestWithContext(ctx, http.MethodGet, proofURL.String(), nil)
		if requestErr != nil {
			return nil, nil, func() {}, requestErr
		}
		proofRequest.Header = c.consoleHeaders(token)
		if strings.TrimSpace(leaseUserAgent) != "" {
			proofRequest.Header.Set("User-Agent", leaseUserAgent)
			applyChromiumClientHints(proofRequest.Header, leaseUserAgent)
		}
		mergeCFCookies(proofRequest.Header, leaseCFCookies)
		// x-cluster is a Console /responses routing hint, not a voice WS header.
		proofRequest.Header.Set("Cache-Control", "no-cache")
		proofRequest.Header.Set("Pragma", "no-cache")
		proofRequest.Header.Set("Sec-Fetch-Mode", "websocket")
		proofRequest.Header.Set("Sec-Fetch-Dest", "empty")
		if requestErr := applyDPoPAuthorization(proofRequest, session); requestErr != nil {
			return nil, nil, func() {}, requestErr
		}
		connection, response, dialErr := dialer.DialContext(ctx, endpoint.String(), proofRequest.Header)
		if dialErr == nil {
			if response != nil && response.Body != nil {
				_ = response.Body.Close()
			}
			if leaseNodeID != "" {
				c.egress.FeedbackOutcome(leaseNodeID, egress.OutcomeSuccess)
			}
			succeeded = true
			return connection, response, releaseEgress, nil
		}
		if response == nil {
			if leaseNodeID != "" {
				c.egress.FeedbackOutcome(leaseNodeID, egress.OutcomeTransportError)
			}
			return nil, nil, func() {}, fmt.Errorf("dial Console voice WebSocket: %w", dialErr)
		}
		raw, headers := readBoundedResponse(response)
		dpopChallenge := response.StatusCode == http.StatusUnauthorized ||
			(response.StatusCode == http.StatusForbidden && IsDPoPProofRequiredBody(raw))
		if dpopChallenge && attempt == 0 {
			c.dpop.invalidate(cacheKey, session.accessToken)
			continue
		}
		upstreamErr := newUpstreamError(response.StatusCode, headers, raw, "")
		if leaseNodeID != "" {
			c.egress.FeedbackOutcome(leaseNodeID, egressOutcomeForKind(ClassifyUpstreamError(upstreamErr)))
		}
		return nil, response, func() {}, upstreamErr
	}
	return nil, nil, func() {}, fmt.Errorf("Console voice WebSocket DPoP retry exhausted")
}
