package grok

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
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
	cfg        *config.Config
	httpClient *http.Client
}

func New(cfg *config.Config) *Client {
	timeout := 120 * time.Second
	if cfg != nil && cfg.RequestTimeout > 0 {
		timeout = time.Duration(cfg.RequestTimeout) * time.Second
	}
	return &Client{
		cfg: cfg,
		httpClient: &http.Client{
			Timeout: timeout,
		},
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

func (c *Client) doChat(ctx context.Context, token string, payload map[string]interface{}) (*http.Response, error) {
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}

	reqURL := c.baseURL() + defaultChatPath
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, reqURL, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header = c.headers(token)

	c.configureProxyTransport()

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		defer resp.Body.Close()
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, fmt.Errorf("grok upstream status=%d body=%s", resp.StatusCode, string(raw))
	}
	return resp, nil
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
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, reqURL, bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	req.Header = c.headers(token)

	c.configureProxyTransport()

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, fmt.Errorf("grok usage status=%d body=%s", resp.StatusCode, string(body))
	}

	var payloadResp map[string]interface{}
	if err := json.NewDecoder(resp.Body).Decode(&payloadResp); err != nil {
		return nil, err
	}
	if info := parseRateLimitPayload(payloadResp); info != nil {
		return info, nil
	}
	return parseRateLimitInfo(resp.Header), nil
}

func (c *Client) configureProxyTransport() {
	if c.cfg == nil {
		return
	}

	proxyAddr := strings.TrimSpace(c.cfg.ProxyHTTPS)
	if proxyAddr == "" {
		proxyAddr = strings.TrimSpace(c.cfg.ProxyHTTP)
	}
	if proxyAddr == "" {
		return
	}

	u, err := url.Parse(proxyAddr)
	if err != nil {
		return
	}

	// Support username/password configured separately in admin UI.
	if u.User == nil && strings.TrimSpace(c.cfg.ProxyUser) != "" {
		u.User = url.UserPassword(strings.TrimSpace(c.cfg.ProxyUser), strings.TrimSpace(c.cfg.ProxyPass))
	}

	c.httpClient.Transport = &http.Transport{
		Proxy: http.ProxyURL(u),
	}
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
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, reqURL, bytes.NewReader(raw))
	if err != nil {
		return "", "", err
	}
	req.Header = c.headers(token)
	req.Header.Set("Referer", "https://grok.com/files")

	c.configureProxyTransport()

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return "", "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return "", "", fmt.Errorf("grok upload status=%d body=%s", resp.StatusCode, string(body))
	}

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
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, reqURL, bytes.NewReader(raw))
	if err != nil {
		return "", err
	}
	req.Header = c.headers(token)
	req.Header.Set("Referer", "https://grok.com/imagine")

	c.configureProxyTransport()

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return "", fmt.Errorf("grok create post status=%d body=%s", resp.StatusCode, string(body))
	}

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

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, link, nil)
	if err != nil {
		return nil, "", err
	}
	req.Header = c.headers(token)
	req.Header.Set("Content-Type", "")
	req.Header.Set("Accept-Encoding", "identity")
	req.Header.Set("Referer", "https://grok.com/")

	c.configureProxyTransport()

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return nil, "", fmt.Errorf("grok asset status=%d body=%s", resp.StatusCode, string(body))
	}
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
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, reqURL, bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	req.Header = c.headers(token)

	c.configureProxyTransport()

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, fmt.Errorf("grok voice status=%d body=%s", resp.StatusCode, string(body))
	}
	out := map[string]interface{}{}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}
	return out, nil
}
