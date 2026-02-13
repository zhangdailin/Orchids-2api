package grok

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"orchids-api/internal/config"
)

const (
	defaultBaseURL        = "https://grok.com"
	defaultAssetsBaseURL  = "https://assets.grok.com"
	defaultChatPath       = "/rest/app-chat/conversations/new"
	defaultUploadFilePath = "/rest/app-chat/upload-file"
	defaultCreatePostPath = "/rest/media/post/create"
	defaultLivekitPath    = "/rest/livekit/tokens"
	defaultRateLimitsPath = "/rest/rate-limits"
	defaultUA             = "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/136.0.0.0 Safari/537.36"
)

type Client struct {
	cfg         *config.Config
	httpClient  *http.Client
	assetClient *http.Client
}

func New(cfg *config.Config) *Client {
	timeout := 120 * time.Second
	if cfg != nil && cfg.RequestTimeout > 0 {
		timeout = time.Duration(cfg.RequestTimeout) * time.Second
	}
	baseProxy := resolveGrokProxy(cfg, strings.TrimSpace(getProxyField(cfg, "base")))
	if baseProxy == nil {
		baseProxy = resolveFallbackProxy(cfg)
	}
	assetProxy := resolveGrokProxy(cfg, strings.TrimSpace(getProxyField(cfg, "asset")))
	if assetProxy == nil {
		assetProxy = baseProxy
	}

	baseClient := newHTTPClient(cfg, timeout, baseProxy)
	assetClient := baseClient
	if assetProxy != nil && (baseProxy == nil || assetProxy.String() != baseProxy.String()) {
		assetClient = newHTTPClient(cfg, timeout, assetProxy)
	}
	return &Client{
		cfg:         cfg,
		httpClient:  baseClient,
		assetClient: assetClient,
	}
}

func (c *Client) baseURL() string {
	if c.cfg != nil && strings.TrimSpace(c.cfg.GrokAPIBaseURL) != "" {
		return strings.TrimRight(strings.TrimSpace(c.cfg.GrokAPIBaseURL), "/")
	}
	return defaultBaseURL
}

func (c *Client) userAgent() string {
	if c.cfg != nil && strings.TrimSpace(c.cfg.GrokUserAgent) != "" {
		return strings.TrimSpace(c.cfg.GrokUserAgent)
	}
	return defaultUA
}

func (c *Client) headers(token string) http.Header {
	h := http.Header{}
	h.Set("Accept", "*/*")
	// Let net/http manage Accept-Encoding so gzip is transparently decoded.
	// Manually setting it disables automatic decompression and can break JSON parsing.
	h.Set("Accept-Language", "zh-CN,zh;q=0.9,en;q=0.8")
	h.Set("Cache-Control", "no-cache")
	h.Set("Content-Type", "application/json")
	h.Set("Origin", "https://grok.com")
	h.Set("Pragma", "no-cache")
	h.Set("Referer", "https://grok.com/")
	h.Set("Priority", "u=1, i")
	h.Set("Sec-Ch-Ua", `"Google Chrome";v="136", "Chromium";v="136", "Not(A:Brand";v="24"`)
	h.Set("Sec-Ch-Ua-Platform", `"macOS"`)
	h.Set("Sec-Fetch-Dest", "empty")
	h.Set("Sec-Fetch-Mode", "cors")
	h.Set("Sec-Fetch-Site", "same-origin")
	h.Set("User-Agent", c.userAgent())
	h.Set("sec-ch-ua", `"Chromium";v="131", "Google Chrome";v="131", "Not_A Brand";v="24"`)
	h.Set("sec-ch-ua-mobile", "?0")
	h.Set("sec-ch-ua-platform", `"Windows"`)
	h.Set("Sec-Fetch-Dest", "empty")
	h.Set("Sec-Fetch-Mode", "cors")
	h.Set("Sec-Fetch-Site", "same-origin")
	h.Set("Priority", "u=1, i")
	h.Set("x-statsig-id", buildStatsigID())
	h.Set("x-xai-request-id", randomHex(16))
	cookie := "sso=" + token + "; sso-rw=" + token
	if c.cfg != nil && strings.TrimSpace(c.cfg.GrokCFClearance) != "" {
		cookie += "; cf_clearance=" + strings.TrimSpace(c.cfg.GrokCFClearance)
	}
	if c.cfg != nil && strings.TrimSpace(c.cfg.GrokCFBM) != "" {
		cookie += "; __cf_bm=" + strings.TrimSpace(c.cfg.GrokCFBM)
	}
	h.Set("Cookie", cookie)
	return h
}

func (c *Client) chatPayload(spec ModelSpec, text string, noMemory bool) map[string]interface{} {
	payload := map[string]interface{}{
		"temporary":             true,
		"modelName":             spec.UpstreamModel,
		"message":               text,
		"fileAttachments":       []string{},
		"imageAttachments":      []string{},
		"disableSearch":         false,
		"enableImageGeneration": true,
		"returnImageBytes":      false,
		"enableImageStreaming":  true,
		"imageGenerationCount":  2,
		"forceConcise":          false,
		"toolOverrides":         map[string]interface{}{},
		"enableSideBySide":      true,
		"sendFinalMetadata":     true,
		"responseMetadata": map[string]interface{}{
			"modelConfigOverride": map[string]interface{}{
				"modelMap": map[string]interface{}{},
			},
			"requestModelDetails": map[string]interface{}{
				"modelId": spec.UpstreamModel,
			},
		},
		"disableMemory": noMemory,
		"deviceEnvInfo": map[string]interface{}{
			"darkModeEnabled":  false,
			"devicePixelRatio": 2,
			"screenWidth":      2056,
			"screenHeight":     1329,
			"viewportWidth":    2056,
			"viewportHeight":   1083,
		},
	}
	if strings.TrimSpace(spec.ModelMode) != "" {
		payload["modelMode"] = spec.ModelMode
	}
	return payload
}

func (c *Client) clientForAsset(asset bool) *http.Client {
	if asset && c.assetClient != nil {
		return c.assetClient
	}
	return c.httpClient
}

func (c *Client) doRequest(ctx context.Context, reqURL string, method string, body []byte, headers http.Header, okStatus int, asset bool) (*http.Response, error) {
	if okStatus == 0 {
		okStatus = http.StatusOK
	}
	maxRetries := 0
	if c.cfg != nil && c.cfg.MaxRetries > 0 {
		maxRetries = c.cfg.MaxRetries
	}
	retryStatuses := map[int]struct{}{http.StatusUnauthorized: {}, http.StatusForbidden: {}, http.StatusTooManyRequests: {}}
	baseDelay := 1 * time.Second
	if c.cfg != nil && c.cfg.RetryDelay > 0 {
		baseDelay = time.Duration(c.cfg.RetryDelay) * time.Millisecond
	}
	retry429Delay := time.Duration(0)
	if c.cfg != nil && c.cfg.Retry429Interval > 0 {
		retry429Delay = time.Duration(c.cfg.Retry429Interval) * time.Second
	}

	var lastStatus int
	var lastBody string
	lastDelay := baseDelay

	for attempt := 0; attempt <= maxRetries; attempt++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		req, err := http.NewRequestWithContext(ctx, method, reqURL, bytes.NewReader(body))
		if err != nil {
			return nil, err
		}
		req.Header = headers.Clone()
		req.Header.Set("x-xai-request-id", randomHex(16))

		resp, err := c.clientForAsset(asset).Do(req)
		if err == nil && resp.StatusCode == okStatus {
			return resp, nil
		}

		if err != nil {
			if attempt >= maxRetries {
				return nil, err
			}
			delay := backoffDelay(baseDelay, retry429Delay, lastDelay, attempt, 0, 0)
			lastDelay = delay
			if !sleepWithContext(ctx, delay) {
				return nil, ctx.Err()
			}
			continue
		}

		lastStatus = resp.StatusCode
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		_ = resp.Body.Close()
		lastBody = string(raw)

		if _, ok := retryStatuses[lastStatus]; !ok || attempt >= maxRetries {
			return nil, fmt.Errorf("grok upstream status=%d body=%s", lastStatus, lastBody)
		}

		retryAfter := parseRetryAfter(resp.Header.Get("Retry-After"))
		delay := backoffDelay(baseDelay, retry429Delay, lastDelay, attempt, lastStatus, retryAfter)
		lastDelay = delay
		if !sleepWithContext(ctx, delay) {
			return nil, ctx.Err()
		}
	}

	if lastStatus == 0 {
		return nil, fmt.Errorf("grok upstream request failed")
	}
	return nil, fmt.Errorf("grok upstream status=%d body=%s", lastStatus, lastBody)
}

func (c *Client) doChat(ctx context.Context, token string, payload map[string]interface{}) (*http.Response, error) {
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}

	reqURL := c.baseURL() + defaultChatPath
	return c.doRequest(ctx, reqURL, http.MethodPost, body, c.headers(token), http.StatusOK, false)
}

func (c *Client) VerifyToken(ctx context.Context, token, modelID string) (*RateLimitInfo, error) {
	info, err := c.GetUsage(ctx, token, modelID)
	if err == nil {
		return info, nil
	}

	model := strings.TrimSpace(modelID)
	if model == "" {
		model = "grok-3"
	}
	spec, ok := ResolveModel(model)
	if !ok {
		spec, ok = ResolveModel("grok-3")
		if !ok {
			return nil, fmt.Errorf("grok default model not available")
		}
	}
	payload := c.chatPayload(spec, "ping", true)
	resp, chatErr := c.doChat(ctx, token, payload)
	if chatErr != nil {
		return nil, chatErr
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	return parseRateLimitInfo(resp.Header), nil
}

func (c *Client) GetUsage(ctx context.Context, token, modelID string) (*RateLimitInfo, error) {
	model := strings.TrimSpace(modelID)
	if model == "" {
		model = "grok-3"
	}
	spec, ok := ResolveModel(model)
	if !ok {
		spec, ok = ResolveModel("grok-3")
		if !ok {
			return nil, fmt.Errorf("grok default model not available")
		}
	}

	payload := map[string]interface{}{
		"requestKind": "DEFAULT",
		"modelName":   strings.TrimSpace(spec.UpstreamModel),
	}
	if strings.TrimSpace(spec.UpstreamModel) == "" {
		payload["modelName"] = "grok-4-1-thinking-1129"
	}

	raw, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}

	reqURL := c.baseURL() + defaultRateLimitsPath
	resp, err := c.doRequest(ctx, reqURL, http.MethodPost, raw, c.headers(token), http.StatusOK, false)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var payloadResp map[string]interface{}
	if err := json.NewDecoder(resp.Body).Decode(&payloadResp); err != nil {
		return nil, err
	}
	if info := parseRateLimitPayload(payloadResp); info != nil {
		return info, nil
	}
	return parseRateLimitInfo(resp.Header), nil
}

func (c *Client) uploadFile(ctx context.Context, token, fileName, fileMimeType, contentBase64 string) (string, string, error) {
	payload := map[string]string{
		"fileName":     strings.TrimSpace(fileName),
		"fileMimeType": strings.TrimSpace(fileMimeType),
		"content":      strings.TrimSpace(contentBase64),
	}
	if payload["fileName"] == "" {
		payload["fileName"] = "file.bin"
	}
	if payload["fileMimeType"] == "" {
		payload["fileMimeType"] = "application/octet-stream"
	}
	if payload["content"] == "" {
		return "", "", fmt.Errorf("empty upload content")
	}

	raw, err := json.Marshal(payload)
	if err != nil {
		return "", "", err
	}

	reqURL := c.baseURL() + defaultUploadFilePath
	headers := c.headers(token)
	headers.Set("Referer", "https://grok.com/files")
	resp, err := c.doRequest(ctx, reqURL, http.MethodPost, raw, headers, http.StatusOK, false)
	if err != nil {
		return "", "", err
	}
	defer resp.Body.Close()

	var out struct {
		FileMetadataID string `json:"fileMetadataId"`
		FileURI        string `json:"fileUri"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", "", err
	}
	if strings.TrimSpace(out.FileMetadataID) == "" && strings.TrimSpace(out.FileURI) == "" {
		return "", "", fmt.Errorf("grok upload returned empty identifiers")
	}
	return strings.TrimSpace(out.FileMetadataID), strings.TrimSpace(out.FileURI), nil
}

func (c *Client) createMediaPost(ctx context.Context, token, mediaType, prompt, mediaURL string) (string, error) {
	payload := map[string]string{
		"mediaType": strings.TrimSpace(mediaType),
	}
	if strings.EqualFold(payload["mediaType"], "MEDIA_POST_TYPE_IMAGE") && strings.TrimSpace(mediaURL) != "" {
		payload["mediaUrl"] = strings.TrimSpace(mediaURL)
	} else {
		payload["prompt"] = strings.TrimSpace(prompt)
	}
	if payload["mediaType"] == "" {
		payload["mediaType"] = "MEDIA_POST_TYPE_VIDEO"
	}

	raw, err := json.Marshal(payload)
	if err != nil {
		return "", err
	}

	reqURL := c.baseURL() + defaultCreatePostPath
	headers := c.headers(token)
	headers.Set("Referer", "https://grok.com/imagine")
	resp, err := c.doRequest(ctx, reqURL, http.MethodPost, raw, headers, http.StatusOK, false)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	var out struct {
		Post struct {
			ID string `json:"id"`
		} `json:"post"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", err
	}
	if strings.TrimSpace(out.Post.ID) == "" {
		return "", fmt.Errorf("grok create post returned empty post id")
	}
	return strings.TrimSpace(out.Post.ID), nil
}

func (c *Client) downloadAsset(ctx context.Context, token, rawURL string) ([]byte, string, error) {
	link := strings.TrimSpace(rawURL)
	if link == "" {
		return nil, "", fmt.Errorf("empty asset url")
	}
	if !strings.HasPrefix(strings.ToLower(link), "http://") && !strings.HasPrefix(strings.ToLower(link), "https://") {
		if !strings.HasPrefix(link, "/") {
			link = "/" + link
		}
		link = defaultAssetsBaseURL + link
	}

	headers := c.headers(token)
	headers.Set("Content-Type", "")
	headers.Set("Accept-Encoding", "identity")
	headers.Set("Referer", "https://grok.com/")
	resp, err := c.doRequest(ctx, link, http.MethodGet, nil, headers, http.StatusOK, true)
	if err != nil {
		return nil, "", err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, "", err
	}
	mime := strings.TrimSpace(strings.Split(resp.Header.Get("Content-Type"), ";")[0])
	return data, mime, nil
}

func (c *Client) getVoiceToken(ctx context.Context, token, voice, personality string, speed float64) (map[string]interface{}, error) {
	if strings.TrimSpace(voice) == "" {
		voice = "ara"
	}
	if strings.TrimSpace(personality) == "" {
		personality = "assistant"
	}
	if speed <= 0 {
		speed = 1.0
	}

	sessionPayload, err := json.Marshal(map[string]interface{}{
		"voice":          strings.TrimSpace(voice),
		"personality":    strings.TrimSpace(personality),
		"playback_speed": speed,
		"enable_vision":  false,
		"turn_detection": map[string]interface{}{"type": "server_vad"},
	})
	if err != nil {
		return nil, err
	}

	payload := map[string]interface{}{
		"sessionPayload":       string(sessionPayload),
		"requestAgentDispatch": false,
		"livekitUrl":           "wss://livekit.grok.com",
		"params": map[string]interface{}{
			"enable_markdown_transcript": "true",
		},
	}

	raw, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}

	reqURL := c.baseURL() + defaultLivekitPath
	resp, err := c.doRequest(ctx, reqURL, http.MethodPost, raw, c.headers(token), http.StatusOK, false)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	out := map[string]interface{}{}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}
	return out, nil
}

func getProxyField(cfg *config.Config, kind string) string {
	if cfg == nil {
		return ""
	}
	switch kind {
	case "base":
		return cfg.GrokBaseProxyURL
	case "asset":
		return cfg.GrokAssetProxyURL
	default:
		return ""
	}
}

func resolveFallbackProxy(cfg *config.Config) *url.URL {
	if cfg == nil {
		return nil
	}
	proxyAddr := strings.TrimSpace(cfg.ProxyHTTPS)
	if proxyAddr == "" {
		proxyAddr = strings.TrimSpace(cfg.ProxyHTTP)
	}
	return resolveGrokProxy(cfg, proxyAddr)
}

func resolveGrokProxy(cfg *config.Config, proxyAddr string) *url.URL {
	proxyAddr = strings.TrimSpace(proxyAddr)
	if proxyAddr == "" {
		return nil
	}
	u, err := url.Parse(proxyAddr)
	if err != nil {
		return nil
	}
	if cfg != nil && u.User == nil && strings.TrimSpace(cfg.ProxyUser) != "" {
		u.User = url.UserPassword(strings.TrimSpace(cfg.ProxyUser), strings.TrimSpace(cfg.ProxyPass))
	}
	return u
}

func newHTTPClient(cfg *config.Config, timeout time.Duration, proxyURL *url.URL) *http.Client {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	if proxyURL != nil {
		transport.Proxy = http.ProxyURL(proxyURL)
	}
	if cfg != nil && cfg.GrokUseUTLS {
		transport = nil
	}
	var rt http.RoundTripper
	if cfg != nil && cfg.GrokUseUTLS {
		rt = newUTLSTransport(proxyURL)
	} else {
		rt = transport
	}
	return &http.Client{
		Timeout:   timeout,
		Transport: rt,
	}
}

func parseRetryAfter(raw string) time.Duration {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return 0
	}
	if secs, err := strconv.Atoi(raw); err == nil {
		if secs <= 0 {
			return 0
		}
		return time.Duration(secs) * time.Second
	}
	if t, err := http.ParseTime(raw); err == nil {
		delay := time.Until(t)
		if delay > 0 {
			return delay
		}
	}
	return 0
}

func backoffDelay(baseDelay, retry429Delay, lastDelay time.Duration, attempt int, status int, retryAfter time.Duration) time.Duration {
	maxDelay := 30 * time.Second
	if retryAfter > 0 {
		if retryAfter > maxDelay {
			return maxDelay
		}
		return retryAfter
	}

	if status == http.StatusTooManyRequests {
		if retry429Delay > 0 {
			if retry429Delay > maxDelay {
				return maxDelay
			}
			return retry429Delay
		}
		if lastDelay <= 0 {
			lastDelay = baseDelay
		}
		upper := lastDelay * 3
		if upper < baseDelay {
			upper = baseDelay
		}
		delay := baseDelay + time.Duration(rand.Int63n(int64(upper-baseDelay)+1))
		if delay > maxDelay {
			return maxDelay
		}
		return delay
	}

	if baseDelay <= 0 {
		baseDelay = 500 * time.Millisecond
	}
	exp := baseDelay * time.Duration(1<<max(0, attempt))
	if exp > maxDelay {
		exp = maxDelay
	}
	if exp <= 0 {
		return baseDelay
	}
	return time.Duration(rand.Int63n(int64(exp) + 1))
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}
