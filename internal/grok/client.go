package grok

import (
	"bytes"
	"context"
	"encoding/binary"
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
	"orchids-api/internal/util"
)

const (
	defaultBaseURL          = "https://grok.com"
	defaultAssetsBaseURL    = "https://assets.grok.com"
	defaultAssetsListPath   = "/rest/assets"
	defaultAssetsDeletePath = "/rest/assets-metadata"
	defaultChatPath         = "/rest/app-chat/conversations/new"
	defaultUploadFilePath   = "/rest/app-chat/upload-file"
	defaultCreatePostPath   = "/rest/media/post/create"
	defaultLivekitPath      = "/rest/livekit/tokens"
	defaultRateLimitsPath   = "/rest/rate-limits"
	defaultAcceptTOSURL     = "https://accounts.x.ai/auth_mgmt.AuthManagement/SetTosAcceptedVersion"
	defaultSetBirthPath     = "/rest/auth/set-birth-date"
	defaultNSFWMgmtPath     = "/auth_mgmt.AuthManagement/UpdateUserFeatureControls"
	defaultUA               = "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/136.0.0.0 Safari/537.36"
)

type Client struct {
	cfg         *config.Config
	httpClient  *http.Client
	assetClient *http.Client
}

type NSFWEnableResult struct {
	Success     bool   `json:"success"`
	HTTPStatus  int    `json:"http_status"`
	GRPCStatus  int    `json:"grpc_status,omitempty"`
	GRPCMessage string `json:"grpc_message,omitempty"`
	Error       string `json:"error,omitempty"`
}

func New(cfg *config.Config) *Client {
	timeout := 120 * time.Second
	if cfg != nil && cfg.RequestTimeout > 0 {
		timeout = time.Duration(cfg.RequestTimeout) * time.Second
	}
	baseProxyOverride := strings.TrimSpace(getProxyField(cfg, "base"))
	assetProxyOverride := strings.TrimSpace(getProxyField(cfg, "asset"))
	baseProxy := resolveGrokProxy(cfg, baseProxyOverride)
	assetProxy := resolveGrokProxy(cfg, assetProxyOverride)

	var bypass []string
	if cfg != nil {
		bypass = cfg.ProxyBypass
	}
	baseProxyFunc := http.ProxyFromEnvironment
	if cfg != nil {
		baseProxyFunc = util.ProxyFunc(cfg.ProxyHTTP, cfg.ProxyHTTPS, cfg.ProxyUser, cfg.ProxyPass, cfg.ProxyBypass)
	}
	if baseProxy != nil {
		baseProxyFunc = util.ProxyFuncFromURL(baseProxy, bypass)
	}
	assetProxyFunc := baseProxyFunc
	if assetProxy != nil {
		assetProxyFunc = util.ProxyFuncFromURL(assetProxy, bypass)
	}

	baseClient := newHTTPClient(cfg, timeout, baseProxyFunc)
	assetClient := baseClient
	if assetProxy != nil && (baseProxy == nil || assetProxy.String() != baseProxy.String()) {
		assetClient = newHTTPClient(cfg, timeout, assetProxyFunc)
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

// baseHeaders 是预分配的请求头模板，包含所有固定的请求头
// 这避免了在每次请求时重新分配这些固定头，提升性能
var baseHeaders = http.Header{
	"Accept":             []string{"*/*"},
	"Accept-Language":    []string{"zh-CN,zh;q=0.9,en;q=0.8"},
	"Cache-Control":      []string{"no-cache"},
	"Content-Type":       []string{"application/json"},
	"Origin":             []string{"https://grok.com"},
	"Pragma":             []string{"no-cache"},
	"Referer":            []string{"https://grok.com/"},
	"Priority":           []string{"u=1, i"},
	"Sec-Ch-Ua":          []string{`"Google Chrome";v="136", "Chromium";v="136", "Not(A:Brand";v="24"`},
	"Sec-Ch-Ua-Platform": []string{`"macOS"`},
	"Sec-Fetch-Dest":     []string{"empty"},
	"Sec-Fetch-Mode":     []string{"cors"},
	"Sec-Fetch-Site":     []string{"same-origin"},
	"sec-ch-ua":          []string{`"Chromium";v="131", "Google Chrome";v="131", "Not_A Brand";v="24"`},
	"sec-ch-ua-mobile":   []string{"?0"},
	"sec-ch-ua-platform": []string{`"Windows"`},
}

func (c *Client) headers(token string) http.Header {
	// 从预分配的模板克隆请求头
	h := make(http.Header, len(baseHeaders))
	for k, v := range baseHeaders {
		h[k] = v
	}

	// 添加动态请求头
	h.Set("User-Agent", c.userAgent())
	h.Set("x-statsig-id", buildStatsigID())
	h.Set("x-xai-request-id", randomHex(16))

	// 构建 Cookie
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

func (c *Client) chatPayload(spec ModelSpec, text string, noMemory bool, imageCount int) map[string]interface{} {
	cnt := imageCount
	if cnt <= 0 {
		cnt = 2
	}
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
		"imageGenerationCount":  cnt,
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
	spec, ok := ResolveModelOrDynamic(model)
	if !ok {
		spec, ok = ResolveModel("grok-3")
		if !ok {
			return nil, fmt.Errorf("grok default model not available")
		}
	}
	payload := c.chatPayload(spec, "ping", true, 0)
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
	spec, ok := ResolveModelOrDynamic(model)
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

func (c *Client) listAssets(ctx context.Context, token string) ([]string, error) {
	token = NormalizeSSOToken(token)
	if strings.TrimSpace(token) == "" {
		return nil, fmt.Errorf("empty token")
	}

	params := url.Values{}
	params.Set("pageSize", "50")
	params.Set("orderBy", "ORDER_BY_LAST_USE_TIME")
	params.Set("source", "SOURCE_ANY")
	params.Set("isLatest", "true")

	assetIDs := make([]string, 0, 64)
	seenPageTokens := map[string]struct{}{}
	nextPageToken := ""
	for page := 0; page < 500; page++ {
		if page > 0 {
			if strings.TrimSpace(nextPageToken) == "" {
				break
			}
			if _, exists := seenPageTokens[nextPageToken]; exists {
				// Upstream returned a repeated page token; stop to avoid endless loop.
				break
			}
			seenPageTokens[nextPageToken] = struct{}{}
			params.Set("pageToken", nextPageToken)
		} else {
			params.Del("pageToken")
		}

		reqURL := c.baseURL() + defaultAssetsListPath + "?" + params.Encode()
		headers := c.headers(token)
		headers.Set("Origin", "https://grok.com")
		headers.Set("Referer", "https://grok.com/files")
		headers.Set("Content-Type", "application/json")

		resp, err := c.doRequest(ctx, reqURL, http.MethodGet, nil, headers, http.StatusOK, false)
		if err != nil {
			return nil, err
		}

		var payload struct {
			Assets []struct {
				AssetID string `json:"assetId"`
			} `json:"assets"`
			NextPageToken string `json:"nextPageToken"`
		}
		decodeErr := json.NewDecoder(resp.Body).Decode(&payload)
		resp.Body.Close()
		if decodeErr != nil {
			return nil, decodeErr
		}

		for _, item := range payload.Assets {
			id := strings.TrimSpace(item.AssetID)
			if id == "" {
				continue
			}
			assetIDs = append(assetIDs, id)
		}
		nextPageToken = strings.TrimSpace(payload.NextPageToken)
		if nextPageToken == "" {
			break
		}
	}

	return assetIDs, nil
}

func (c *Client) countAssets(ctx context.Context, token string) (int, error) {
	ids, err := c.listAssets(ctx, token)
	if err != nil {
		return 0, err
	}
	return len(ids), nil
}

func (c *Client) deleteAsset(ctx context.Context, token string, assetID string) error {
	token = NormalizeSSOToken(token)
	assetID = strings.TrimSpace(assetID)
	if token == "" {
		return fmt.Errorf("empty token")
	}
	if assetID == "" {
		return fmt.Errorf("empty asset id")
	}

	reqURL := c.baseURL() + defaultAssetsDeletePath + "/" + url.PathEscape(assetID)
	headers := c.headers(token)
	headers.Set("Origin", "https://grok.com")
	headers.Set("Referer", "https://grok.com/files")
	headers.Set("Content-Type", "application/json")

	resp, err := c.doRequest(ctx, reqURL, http.MethodDelete, nil, headers, http.StatusOK, false)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 2048))
	return nil
}

func (c *Client) clearAssets(ctx context.Context, token string) (total int, success int, failed int, err error) {
	assetIDs, err := c.listAssets(ctx, token)
	if err != nil {
		return 0, 0, 0, err
	}

	total = len(assetIDs)
	if total == 0 {
		return 0, 0, 0, nil
	}

	type job struct {
		assetID string
	}
	workerCount := 8
	if total < workerCount {
		workerCount = total
	}

	jobs := make(chan job)
	results := make(chan bool, total)

	for i := 0; i < workerCount; i++ {
		go func() {
			for item := range jobs {
				if err := c.deleteAsset(ctx, token, item.assetID); err != nil {
					results <- false
				} else {
					results <- true
				}
			}
		}()
	}

	for _, id := range assetIDs {
		jobs <- job{assetID: id}
	}
	close(jobs)

	for i := 0; i < total; i++ {
		if <-results {
			success++
		} else {
			failed++
		}
	}
	return total, success, failed, nil
}

func grpcWebEncode(payload []byte) []byte {
	out := make([]byte, 5+len(payload))
	out[0] = 0x00
	binary.BigEndian.PutUint32(out[1:5], uint32(len(payload)))
	copy(out[5:], payload)
	return out
}

func parseGRPCStatus(headers http.Header, body []byte) (int, string) {
	if headers != nil {
		rawStatus := strings.TrimSpace(firstHeaderValue(headers, "grpc-status", "Grpc-Status"))
		if rawStatus != "" {
			if code, err := strconv.Atoi(rawStatus); err == nil {
				return code, strings.TrimSpace(firstHeaderValue(headers, "grpc-message", "Grpc-Message"))
			}
		}
	}

	buf := body
	for len(buf) >= 5 {
		flag := buf[0]
		frameLen := int(binary.BigEndian.Uint32(buf[1:5]))
		buf = buf[5:]
		if frameLen < 0 || frameLen > len(buf) {
			break
		}
		frame := buf[:frameLen]
		buf = buf[frameLen:]

		// trailer frame
		if flag&0x80 == 0 {
			continue
		}
		lines := strings.Split(string(frame), "\r\n")
		code := -1
		msg := ""
		for _, line := range lines {
			line = strings.TrimSpace(line)
			if line == "" {
				continue
			}
			kv := strings.SplitN(line, ":", 2)
			if len(kv) != 2 {
				continue
			}
			k := strings.ToLower(strings.TrimSpace(kv[0]))
			v := strings.TrimSpace(kv[1])
			switch k {
			case "grpc-status":
				if n, err := strconv.Atoi(v); err == nil {
					code = n
				}
			case "grpc-message":
				msg = v
			}
		}
		if code >= 0 {
			return code, msg
		}
	}

	// If grpc-status is absent, treat as success by default.
	return -1, ""
}

func (c *Client) acceptTOS(ctx context.Context, token string) (int, error) {
	headers := c.headers(token)
	headers.Set("Origin", "https://accounts.x.ai")
	headers.Set("Referer", "https://accounts.x.ai/accept-tos")
	headers.Set("Content-Type", "application/grpc-web+proto")
	headers.Set("Accept", "*/*")
	headers.Set("Sec-Fetch-Dest", "empty")
	headers.Set("x-grpc-web", "1")
	headers.Set("x-user-agent", "connect-es/2.1.1")
	headers.Set("Cache-Control", "no-cache")
	headers.Set("Pragma", "no-cache")

	payload := grpcWebEncode([]byte{0x10, 0x01})
	resp, err := c.doRequest(ctx, defaultAcceptTOSURL, http.MethodPost, payload, headers, http.StatusOK, false)
	if err != nil {
		return parseUpstreamStatus(err), err
	}
	defer resp.Body.Close()

	statusCode := resp.StatusCode
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 512<<10))
	grpcStatus, grpcMsg := parseGRPCStatus(resp.Header, raw)
	if grpcStatus != -1 && grpcStatus != 0 {
		if grpcMsg == "" {
			return statusCode, fmt.Errorf("accept tos failed grpc_status=%d", grpcStatus)
		}
		return statusCode, fmt.Errorf("accept tos failed grpc_status=%d message=%s", grpcStatus, grpcMsg)
	}
	return statusCode, nil
}

func randomAdultBirthDate() string {
	now := time.Now().UTC()
	year := now.Year() - (20 + rand.Intn(29)) // [20,48]
	month := 1 + rand.Intn(12)
	day := 1 + rand.Intn(28)
	hour := rand.Intn(24)
	minute := rand.Intn(60)
	second := rand.Intn(60)
	millis := rand.Intn(1000)
	return fmt.Sprintf("%04d-%02d-%02dT%02d:%02d:%02d.%03dZ", year, month, day, hour, minute, second, millis)
}

func (c *Client) setBirthDate(ctx context.Context, token string) (int, error) {
	payload := map[string]string{
		"birthDate": randomAdultBirthDate(),
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return 0, err
	}

	headers := c.headers(token)
	headers.Set("Origin", "https://grok.com")
	headers.Set("Referer", "https://grok.com/?_s=home")
	headers.Set("Content-Type", "application/json")

	reqURL := c.baseURL() + defaultSetBirthPath
	resp, err := c.doRequest(ctx, reqURL, http.MethodPost, raw, headers, http.StatusOK, false)
	if err != nil {
		statusCode := parseUpstreamStatus(err)
		// Upstream may return 204 No Content when already set.
		if statusCode == http.StatusNoContent {
			return statusCode, nil
		}
		return statusCode, err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1024))
	return resp.StatusCode, nil
}

func (c *Client) enableNSFWFeature(ctx context.Context, token string) (int, int, string, error) {
	headers := c.headers(token)
	headers.Set("Origin", "https://grok.com")
	headers.Set("Referer", "https://grok.com/?_s=data")
	headers.Set("Content-Type", "application/grpc-web+proto")
	headers.Set("Accept", "*/*")
	headers.Set("Sec-Fetch-Dest", "empty")
	headers.Set("x-grpc-web", "1")
	headers.Set("x-user-agent", "connect-es/2.1.1")
	headers.Set("Cache-Control", "no-cache")
	headers.Set("Pragma", "no-cache")

	name := []byte("always_show_nsfw_content")
	inner := append([]byte{0x0a, byte(len(name))}, name...)
	protobuf := append([]byte{0x0a, 0x02, 0x10, 0x01, 0x12, byte(len(inner))}, inner...)
	payload := grpcWebEncode(protobuf)

	reqURL := c.baseURL() + defaultNSFWMgmtPath
	resp, err := c.doRequest(ctx, reqURL, http.MethodPost, payload, headers, http.StatusOK, false)
	if err != nil {
		return parseUpstreamStatus(err), -1, "", err
	}
	defer resp.Body.Close()

	statusCode := resp.StatusCode
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 512<<10))
	grpcStatus, grpcMsg := parseGRPCStatus(resp.Header, raw)
	if grpcStatus != -1 && grpcStatus != 0 {
		if grpcMsg == "" {
			return statusCode, grpcStatus, grpcMsg, fmt.Errorf("enable nsfw failed grpc_status=%d", grpcStatus)
		}
		return statusCode, grpcStatus, grpcMsg, fmt.Errorf("enable nsfw failed grpc_status=%d message=%s", grpcStatus, grpcMsg)
	}
	return statusCode, grpcStatus, grpcMsg, nil
}

func (c *Client) EnableNSFWDetailed(ctx context.Context, token string) NSFWEnableResult {
	result := NSFWEnableResult{
		Success:    false,
		HTTPStatus: 0,
		GRPCStatus: -1,
	}
	if strings.TrimSpace(token) == "" {
		result.Error = "empty token"
		return result
	}

	statusCode, err := c.acceptTOS(ctx, token)
	if statusCode > 0 {
		result.HTTPStatus = statusCode
	}
	if err != nil {
		result.Error = fmt.Sprintf("Accept ToS failed: %v", err)
		return result
	}

	statusCode, err = c.setBirthDate(ctx, token)
	if statusCode > 0 {
		result.HTTPStatus = statusCode
	}
	if err != nil {
		result.Error = fmt.Sprintf("Set birth date failed: %v", err)
		return result
	}

	statusCode, grpcStatus, grpcMsg, err := c.enableNSFWFeature(ctx, token)
	if statusCode > 0 {
		result.HTTPStatus = statusCode
	}
	result.GRPCStatus = grpcStatus
	result.GRPCMessage = grpcMsg
	if err != nil {
		result.Error = fmt.Sprintf("NSFW enable failed: %v", err)
		return result
	}

	result.Success = true
	result.Error = ""
	return result
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

func newHTTPClient(cfg *config.Config, timeout time.Duration, proxyFunc func(*http.Request) (*url.URL, error)) *http.Client {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.MaxIdleConns = 100
	transport.MaxIdleConnsPerHost = 20
	transport.IdleConnTimeout = 90 * time.Second
	if proxyFunc != nil {
		transport.Proxy = proxyFunc
	}
	if cfg != nil && cfg.GrokUseUTLS {
		transport = nil
	}
	var rt http.RoundTripper
	if cfg != nil && cfg.GrokUseUTLS {
		rt = newUTLSTransport(proxyFunc)
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

func parseUpstreamStatus(err error) int {
	if err == nil {
		return 0
	}
	raw := err.Error()
	idx := strings.Index(raw, "status=")
	if idx < 0 {
		return 0
	}
	rest := raw[idx+len("status="):]
	n := 0
	for n < len(rest) && rest[n] >= '0' && rest[n] <= '9' {
		n++
	}
	if n == 0 {
		return 0
	}
	code, convErr := strconv.Atoi(rest[:n])
	if convErr != nil {
		return 0
	}
	return code
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
