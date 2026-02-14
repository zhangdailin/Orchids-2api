package grok

import (
	"context"
	"crypto/sha1"
	"encoding/base64"
	"encoding/hex"
	"bytes"
	"encoding/json"
	"fmt"
	"image"
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"orchids-api/internal/config"
	"orchids-api/internal/loadbalancer"
	"orchids-api/internal/store"
)

func sanitizeText(s string) string {
	if s == "" {
		return s
	}
	// Drop replacement chars introduced by invalid UTF-8 boundaries.
	s = strings.ReplaceAll(s, "\uFFFD", "")
	return s
}

func formatImageMarkdown(u string) string {
	u = strings.TrimSpace(u)
	if u == "" {
		return ""
	}
	// Blank lines around images improve rendering in some clients.
	return "\n\n![](" + u + ")\n\n"
}

var reImageURLInText = regexp.MustCompile(`https?://[^\s"')>]+\.(?:png|jpe?g|webp|gif)(?:\?[^\s"')>]*)?`)

func extractImageURLsFromText(s string) []string {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil
	}
	m := reImageURLInText.FindAllString(s, -1)
	if len(m) == 0 {
		return nil
	}
	return uniqueStrings(m)
}

type scoredURL struct {
	u     string
	score int
}

// preferFullOverPart drops "-part-0" preview variants when the corresponding full URL is present.
// This is part of the stable contract:
// - Never emit -part-0 when full exists.
func preferFullOverPart(urls []string) []string {
	if len(urls) == 0 {
		return urls
	}
	set := map[string]struct{}{}
	for _, u := range urls {
		set[u] = struct{}{}
	}
	out := make([]string, 0, len(urls))
	for _, u := range urls {
		if strings.Contains(u, "-part-0/") {
			full := strings.ReplaceAll(u, "-part-0/", "/")
			if _, ok := set[full]; ok {
				continue
			}
		}
		out = append(out, u)
	}
	return out
}

func normalizeImageURLs(urls []string, n int) []string {
	urls = uniqueStrings(urls)
	filtered := make([]string, 0, len(urls))
	for _, u := range urls {
		if isLikelyImageURL(u) {
			filtered = append(filtered, u)
		}
	}
	urls = preferFullOverPart(filtered)
	if n > 0 && len(urls) > n {
		urls = urls[:n]
	}
	return urls
}

func appendImageCandidates(urls []string, debugHTTP []string, debugAsset []string, n int) []string {
	if n <= 0 {
		n = 4
	}
	if len(urls) > 0 {
		return urls
	}

	// 1) Prefer direct image URLs from observed http strings.
	for _, u := range debugHTTP {
		if isLikelyImageURL(u) {
			urls = append(urls, u)
			if len(urls) >= n {
				break
			}
		}
	}
	urls = normalizeImageURLs(urls, n)
	if len(urls) >= n {
		return urls
	}

	// 2) Parse JSON card strings or asset-like strings from debugAsset and collect up to n.
	for _, p := range debugAsset {
		if len(urls) >= n {
			break
		}
		p = strings.TrimSpace(p)
		if p == "" || strings.Contains(p, "grok-3") || strings.Contains(p, "grok-4") {
			continue
		}
		if strings.HasPrefix(p, "{") {
			preferred := extractPreferredImageURLsFromJSONText(p)
			if len(preferred) == 0 {
				preferred = extractImageURLsFromText(p)
			}
			for _, u := range preferred {
				if isLikelyImageURL(u) {
					urls = append(urls, u)
					if len(urls) >= n {
						break
					}
				}
			}
			urls = normalizeImageURLs(urls, n)
			continue
		}

		if strings.HasPrefix(p, "http://") || strings.HasPrefix(p, "https://") {
			if isLikelyImageURL(p) {
				urls = append(urls, p)
			}
		} else if isLikelyImageAssetPath(p) {
			urls = append(urls, "https://assets.grok.com/"+strings.TrimPrefix(p, "/"))
		}
		urls = normalizeImageURLs(urls, n)
	}
	return normalizeImageURLs(urls, n)
}

func extractPreferredImageURLsFromJSONText(s string) []string {
	s = strings.TrimSpace(s)
	if s == "" || !strings.HasPrefix(s, "{") {
		return nil
	}
	var v interface{}
	if err := json.Unmarshal([]byte(s), &v); err != nil {
		return nil
	}
	var out []scoredURL
	var walk func(x interface{}, keyHint string)
	walk = func(x interface{}, keyHint string) {
		switch t := x.(type) {
		case map[string]interface{}:
			for k, vv := range t {
				walk(vv, k)
			}
		case []interface{}:
			for _, vv := range t {
				walk(vv, keyHint)
			}
		case string:
			u := strings.TrimSpace(t)
			if !isLikelyImageURL(u) {
				return
			}
			lk := strings.ToLower(strings.TrimSpace(keyHint))
			score := 50
			if strings.Contains(lk, "original") {
				score = 100
			} else if strings.Contains(lk, "thumbnail") || strings.Contains(lk, "thumb") {
				score = 10
			} else if strings.Contains(lk, "link") {
				score = 5
			}
			out = append(out, scoredURL{u: u, score: score})
		}
	}
	walk(v, "")
	if len(out) == 0 {
		return nil
	}
	// Dedup keeping best score.
	best := map[string]int{}
	for _, it := range out {
		if cur, ok := best[it.u]; !ok || it.score > cur {
			best[it.u] = it.score
		}
	}
	items := make([]scoredURL, 0, len(best))
	for u, sc := range best {
		items = append(items, scoredURL{u: u, score: sc})
	}
	sort.Slice(items, func(i, j int) bool {
		if items[i].score == items[j].score {
			return items[i].u < items[j].u
		}
		return items[i].score > items[j].score
	})
	res := make([]string, 0, len(items))
	for _, it := range items {
		res = append(res, it.u)
	}
	return res
}

func isLikelyImageURL(u string) bool {
	u = strings.TrimSpace(u)
	if u == "" {
		return false
	}
	if strings.HasPrefix(u, "/grok/v1/files/image/") {
		return true
	}
	lu := strings.ToLower(u)
	if strings.HasPrefix(lu, "http://") || strings.HasPrefix(lu, "https://") {
		// Quick allow if it clearly ends with an image extension (ignore query).
		cut := lu
		if q := strings.IndexByte(cut, '?'); q >= 0 {
			cut = cut[:q]
		}
		if strings.HasSuffix(cut, ".jpg") || strings.HasSuffix(cut, ".jpeg") || strings.HasSuffix(cut, ".png") || strings.HasSuffix(cut, ".webp") || strings.HasSuffix(cut, ".gif") {
			return true
		}
		// assets.grok.com generated image paths
		if strings.Contains(lu, "assets.grok.com/") && (strings.Contains(lu, "/generated/") || strings.Contains(lu, "/image")) {
			return true
		}
		return false
	}
	return false
}

func isLikelyImageAssetPath(p string) bool {
	p = strings.TrimSpace(p)
	if p == "" {
		return false
	}
	// Reject JSON blobs or echoed prompts.
	if strings.HasPrefix(p, "{") || strings.Contains(p, "Image Generation:") {
		return false
	}
	// Reject anything with whitespace (asset paths/urls shouldn't contain spaces/newlines).
	if strings.ContainsAny(p, " \t\r\n") {
		return false
	}
	lp := strings.ToLower(p)
	if strings.HasSuffix(lp, ".jpg") || strings.HasSuffix(lp, ".jpeg") || strings.HasSuffix(lp, ".png") || strings.HasSuffix(lp, ".webp") || strings.HasSuffix(lp, ".gif") {
		return true
	}
	return false
}

// stripLeadingAngleNoise was an experimental cleanup for leaked markup fragments like '<<<'.
// It proved unreliable in practice and could interfere with legitimate content.
// Kept as a no-op for compatibility with older code paths.
func stripLeadingAngleNoise(s string) string { return s }

const maxEditImageBytes = 50 * 1024 * 1024

var cacheBaseDir = filepath.Join("data", "tmp")

type Handler struct {
	cfg    *config.Config
	lb     *loadbalancer.LoadBalancer
	client *Client
}

func NewHandler(cfg *config.Config, lb *loadbalancer.LoadBalancer) *Handler {
	return &Handler{
		cfg:    cfg,
		lb:     lb,
		client: New(cfg),
	}
}

func (h *Handler) selectAccount(ctx context.Context) (*store.Account, string, error) {
	if h.lb == nil {
		return nil, "", fmt.Errorf("load balancer not configured")
	}
	acc, err := h.lb.GetNextAccountExcludingByChannel(ctx, nil, "grok")
	if err != nil {
		return nil, "", err
	}
	raw := strings.TrimSpace(acc.ClientCookie)
	if raw == "" {
		raw = strings.TrimSpace(acc.RefreshToken)
	}
	token := parseTokenValue(raw)
	if token == "" {
		return nil, "", fmt.Errorf("grok account token is empty")
	}
	return acc, token, nil
}

func (h *Handler) ensureModelEnabled(ctx context.Context, modelID string) error {
	if h == nil || h.lb == nil || h.lb.Store == nil {
		return nil
	}
	modelID = strings.TrimSpace(modelID)
	if modelID == "" {
		return nil
	}
	m, err := h.lb.Store.GetModelByModelID(ctx, modelID)
	if err != nil || m == nil {
		return fmt.Errorf("model not found")
	}
	if !m.Status.Enabled() {
		return fmt.Errorf("model not available")
	}
	mChannel := strings.TrimSpace(m.Channel)
	if mChannel == "" {
		mChannel = "grok"
	}
	if !strings.EqualFold(mChannel, "grok") {
		return fmt.Errorf("model not found")
	}
	return nil
}

func (h *Handler) trackAccount(acc *store.Account) func() {
	if h == nil || h.lb == nil || acc == nil || acc.ID == 0 {
		return func() {}
	}
	h.lb.AcquireConnection(acc.ID)
	return func() {
		h.lb.ReleaseConnection(acc.ID)
	}
}

func (h *Handler) markAccountStatus(ctx context.Context, acc *store.Account, err error) {
	if acc == nil || err == nil || h.lb == nil {
		return
	}
	status := classifyAccountStatusFromError(err.Error())
	if status == "" {
		return
	}
	h.lb.MarkAccountStatus(ctx, acc, status)
}

func (h *Handler) syncGrokQuota(acc *store.Account, headers http.Header) {
	if acc == nil || h.lb == nil || h.lb.Store == nil {
		return
	}
	info := parseRateLimitInfo(headers)
	if info == nil || (info.Limit <= 0 && info.Remaining <= 0) {
		return
	}
	limit := info.Limit
	remaining := info.Remaining
	if remaining < 0 {
		remaining = 0
	}
	if limit <= 0 && remaining > 0 {
		limit = remaining
	}
	acc.UsageLimit = float64(limit)
	acc.UsageCurrent = float64(remaining)
	if !info.ResetAt.IsZero() {
		acc.QuotaResetAt = info.ResetAt
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := h.lb.Store.UpdateAccount(ctx, acc); err != nil {
		slog.Warn("grok quota update failed", "account_id", acc.ID, "error", err)
	}
}

func (h *Handler) HandleModels(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	resp := map[string]interface{}{
		"object": "list",
		"data":   make([]map[string]interface{}, 0, len(SupportedModels)),
	}
	data := resp["data"].([]map[string]interface{})
	for _, m := range SupportedModels {
		data = append(data, map[string]interface{}{
			"id":       m.ID,
			"object":   "model",
			"created":  0,
			"owned_by": "grok",
		})
	}
	resp["data"] = data
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}

func detectPublicBaseURL(r *http.Request) string {
	proto := strings.TrimSpace(r.Header.Get("X-Forwarded-Proto"))
	if proto == "" {
		if r.TLS != nil {
			proto = "https"
		} else {
			proto = "http"
		}
	}
	host := strings.TrimSpace(r.Header.Get("X-Forwarded-Host"))
	if host == "" {
		host = strings.TrimSpace(r.Host)
	}
	if host == "" {
		return ""
	}
	return proto + "://" + host
}

func (h *Handler) HandleChatCompletions(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req ChatCompletionsRequest
	raw, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "invalid body", http.StatusBadRequest)
		return
	}
	if err := json.Unmarshal(raw, &req); err != nil {
		http.Error(w, "invalid json", http.StatusBadRequest)
		return
	}
	if err := req.Validate(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if err := h.ensureModelEnabled(r.Context(), req.Model); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	spec, ok := ResolveModel(req.Model)
	if !ok {
		http.Error(w, "model not found", http.StatusBadRequest)
		return
	}

	text, attachments, err := extractMessageAndAttachments(req.Messages, spec.IsVideo)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if strings.TrimSpace(text) == "" && len(attachments) == 0 {
		http.Error(w, "empty message", http.StatusBadRequest)
		return
	}

	// Retry once on transient account failures (e.g. 403/429) by switching account.
	var (
		acc  *store.Account
		token string
		resp *http.Response
	)
	for attempt := 0; attempt < 2; attempt++ {
		acc, token, err = h.selectAccount(r.Context())
		if err != nil {
			http.Error(w, "no available grok token: "+err.Error(), http.StatusServiceUnavailable)
			return
		}
		release := h.trackAccount(acc)

		fileAttachments, upErr := h.uploadAttachmentInputs(r.Context(), token, attachments)
		if upErr != nil {
			h.markAccountStatus(r.Context(), acc, upErr)
			release()
			http.Error(w, "attachment upload failed: "+upErr.Error(), http.StatusBadGateway)
			return
		}

		payload, buildErr := h.buildChatPayload(r.Context(), token, spec, text, fileAttachments, req.VideoConfig)
		if buildErr != nil {
			h.markAccountStatus(r.Context(), acc, buildErr)
			release()
			http.Error(w, buildErr.Error(), http.StatusBadGateway)
			return
		}

		resp, err = h.client.doChat(r.Context(), token, payload)
		if err != nil {
			status := classifyAccountStatusFromError(err.Error())
			h.markAccountStatus(r.Context(), acc, err)
			release()
			// Switch account once for 403/429.
			if attempt == 0 && (status == "403" || status == "429") {
				continue
			}
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		// Success
		defer release()
		break
	}
	defer resp.Body.Close()
	h.syncGrokQuota(acc, resp.Header)

	publicBase := detectPublicBaseURL(r)
	hasAttachments := len(attachments) > 0
	if req.Stream {
		h.streamChat(w, req.Model, spec, token, publicBase, hasAttachments, text, resp.Body)
		return
	}
	h.collectChat(w, req.Model, spec, token, publicBase, hasAttachments, text, resp.Body)
}

func (h *Handler) buildChatPayload(ctx context.Context, token string, spec ModelSpec, text string, fileAttachments []string, videoCfg *VideoConfig) (map[string]interface{}, error) {
	payload := h.client.chatPayload(spec, text, true, 0)
	if len(fileAttachments) > 0 {
		payload["fileAttachments"] = fileAttachments
	}

	if !spec.IsVideo {
		return payload, nil
	}

	if videoCfg == nil {
		videoCfg = &VideoConfig{}
	}
	videoCfg.Normalize()

	postID, err := h.client.createMediaPost(ctx, token, "MEDIA_POST_TYPE_VIDEO", text, "")
	if err != nil {
		return nil, fmt.Errorf("create video post failed: %w", err)
	}

	modeFlag := videoPresetFlag(videoCfg.Preset)
	message := strings.TrimSpace(text)
	if modeFlag != "" {
		message = strings.TrimSpace(message + " " + modeFlag)
	}

	return map[string]interface{}{
		"temporary":        true,
		"modelName":        spec.UpstreamModel,
		"message":          message,
		"toolOverrides":    map[string]interface{}{"videoGen": true},
		"enableSideBySide": true,
		"deviceEnvInfo": map[string]interface{}{
			"darkModeEnabled":  false,
			"devicePixelRatio": 2,
			"screenWidth":      1920,
			"screenHeight":     1080,
			"viewportWidth":    1920,
			"viewportHeight":   1080,
		},
		"responseMetadata": map[string]interface{}{
			"modelConfigOverride": map[string]interface{}{
				"modelMap": map[string]interface{}{
					"videoGenModelConfig": map[string]interface{}{
						"aspectRatio":    videoCfg.AspectRatio,
						"parentPostId":   postID,
						"resolutionName": videoCfg.ResolutionName,
						"videoLength":    videoCfg.VideoLength,
					},
				},
			},
		},
	}, nil
}

func videoPresetFlag(preset string) string {
	switch strings.ToLower(strings.TrimSpace(preset)) {
	case "fun":
		return "--mode=extremely-crazy"
	case "normal":
		return "--mode=normal"
	case "spicy":
		return "--mode=extremely-spicy-or-crazy"
	case "", "custom":
		return "--mode=custom"
	default:
		return "--mode=custom"
	}
}

func (h *Handler) uploadAttachmentInputs(ctx context.Context, token string, inputs []AttachmentInput) ([]string, error) {
	if len(inputs) == 0 {
		return nil, nil
	}
	out := make([]string, 0, len(inputs))
	for _, item := range inputs {
		fileID, fileURI, err := h.uploadSingleInput(ctx, token, item.Data)
		if err != nil {
			return nil, err
		}
		id := strings.TrimSpace(fileID)
		if id == "" {
			id = strings.TrimSpace(fileURI)
		}
		if id != "" {
			out = append(out, id)
		}
	}
	return uniqueStrings(out), nil
}

func (h *Handler) uploadSingleInput(ctx context.Context, token, input string) (string, string, error) {
	data := strings.TrimSpace(input)
	if data == "" {
		return "", "", fmt.Errorf("empty attachment")
	}
	if isRemoteURL(data) {
		var err error
		data, err = fetchRemoteAsDataURI(data, 30*time.Second)
		if err != nil {
			return "", "", err
		}
	}
	filename, contentBase64, mime, err := parseDataURI(data)
	if err != nil {
		// Fallback for plain base64 payloads.
		filename = "file.bin"
		mime = "application/octet-stream"
		contentBase64 = data
	}
	return h.client.uploadFile(ctx, token, filename, mime, contentBase64)
}

type streamMarkupFilter struct {
	pending  string
	inTool   bool
	inRender bool
}

// Streaming sanitizer/tokenizer.
// Goal: never leak tool/render markup, never corrupt UTF-8, and keep streaming responsive.
func (f *streamMarkupFilter) feed(chunk string) string {
	if f == nil || chunk == "" {
		return ""
	}
	f.pending += chunk
	// Bound memory
	if len(f.pending) > 64*1024 {
		f.pending = f.pending[len(f.pending)-64*1024:]
	}

	const toolStart = "xai:tool_usage_card"
	const toolEnd = "</xai:tool_usage_card>"
	const renderStart = "<grok:render"
	const renderEnd = "</grok:render>"

	var out strings.Builder

	for {
		lower := strings.ToLower(f.pending)

		if f.inTool {
			end := strings.Index(lower, toolEnd)
			if end < 0 {
				// wait for more data
				break
			}
			f.pending = f.pending[end+len(toolEnd):]
			f.inTool = false
			continue
		}
		if f.inRender {
			end := strings.Index(lower, renderEnd)
			if end < 0 {
				break
			}
			f.pending = f.pending[end+len(renderEnd):]
			f.inRender = false
			continue
		}

		idxTool := strings.Index(lower, toolStart)
		idxRender := strings.Index(lower, renderStart)
		idx := -1
		kind := ""
		if idxTool >= 0 {
			idx = idxTool
			kind = "tool"
		}
		if idxRender >= 0 && (idx < 0 || idxRender < idx) {
			idx = idxRender
			kind = "render"
		}

		if idx < 0 {
			// No markers. Emit everything except a tail to avoid cutting potential markers.
			keep := 512
			if len(f.pending) <= keep {
				break
			}
			safe := validUTF8Prefix(f.pending[:len(f.pending)-keep])
			safe = stripLeadingAngleNoise(sanitizeText(safe))
			if safe != "" {
				out.WriteString(safe)
			}
			f.pending = f.pending[len(f.pending)-keep:]
			break
		}

		// Emit prefix before marker
		prefix := validUTF8Prefix(f.pending[:idx])
		prefix = stripLeadingAngleNoise(sanitizeText(prefix))
		if prefix != "" {
			out.WriteString(prefix)
		}
		f.pending = f.pending[idx:]
		if kind == "tool" {
			f.inTool = true
		} else {
			f.inRender = true
		}
	}

	return out.String()
}

func (f *streamMarkupFilter) flush() string {
	if f == nil {
		return ""
	}
	if f.inTool || f.inRender {
		return ""
	}
	if strings.TrimSpace(f.pending) == "" {
		return ""
	}
	out := stripLeadingAngleNoise(sanitizeText(stripToolAndRenderMarkup(validUTF8Prefix(f.pending))))
	f.pending = ""
	return out
}

func validUTF8Prefix(s string) string {
	if s == "" || utf8.ValidString(s) {
		return s
	}
	// Trim bytes until valid UTF-8.
	b := []byte(s)
	for len(b) > 0 {
		b = b[:len(b)-1]
		if utf8.Valid(b) {
			return string(b)
		}
	}
	return ""
}

// NOTE: streamMarkupFilter.feed is implemented earlier in this file.

func (h *Handler) streamChat(w http.ResponseWriter, model string, spec ModelSpec, token string, publicBase string, hasAttachments bool, userPrompt string, body io.Reader) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	flusher, _ := w.(http.Flusher)

	id := "chatcmpl_" + randomHex(8)
	sentRole := false
	lastMessage := ""
	sawToken := false
	sentAny := false
	var rawAll strings.Builder
	// Image URL stream handling: prefer full image variants over -part-0 previews.
	seenFull := map[string]bool{}
	pendingPart := map[string]string{}
	emitted := map[string]bool{}

	var mf *streamMarkupFilter
	if !hasAttachments {
		mf = &streamMarkupFilter{}
	}

	emitChunk := func(delta map[string]interface{}, finish interface{}) {
		chunk := map[string]interface{}{
			"id":      id,
			"object":  "chat.completion.chunk",
			"created": time.Now().Unix(),
			"model":   model,
			"choices": []map[string]interface{}{
				{
					"index":         0,
					"delta":         delta,
					"logprobs":      nil,
					"finish_reason": finish,
				},
			},
		}
		writeSSE(w, "", encodeJSON(chunk))
		if flusher != nil {
			flusher.Flush()
		}
		sentAny = true
	}

	emitImageURL := func(raw string) {
		raw = strings.TrimSpace(raw)
		if raw == "" {
			return
		}
		// Track preview vs full variants on the raw upstream URL.
		if strings.Contains(raw, "-part-0/") {
			full := strings.ReplaceAll(raw, "-part-0/", "/")
			if seenFull[full] {
				return
			}
			pendingPart[full] = raw
			return
		}
		seenFull[raw] = true
		if emitted[raw] {
			return
		}

		val, errV := h.imageOutputValue(context.Background(), token, raw, "url")
		if errV != nil || strings.TrimSpace(val) == "" {
			val = raw
		}
		if publicBase != "" && strings.HasPrefix(val, "/") {
			val = publicBase + val
		}
		if md := formatImageMarkdown(val); md != "" {
			emitChunk(map[string]interface{}{"content": md}, nil)
			emitted[raw] = true
		}
	}

	err := parseUpstreamLines(body, func(resp map[string]interface{}) error {
		if !sentRole {
			emitChunk(map[string]interface{}{"role": "assistant"}, nil)
			sentRole = true
		}
		if tokenDelta, ok := resp["token"].(string); ok && tokenDelta != "" {
			rawAll.WriteString(tokenDelta)
			sawToken = true
			if mf == nil {
				// Vision Q/A path: keep text intact but strip full tool/render blocks when present.
				cleaned := stripToolAndRenderMarkup(tokenDelta)
				cleaned = stripLeadingAngleNoise(sanitizeText(cleaned))
				if cleaned != "" {
					emitChunk(map[string]interface{}{"content": cleaned}, nil)
				}
			} else {
				if cleaned := mf.feed(tokenDelta); cleaned != "" {
					cleaned = stripLeadingAngleNoise(cleaned)
					if cleaned != "" {
						emitChunk(map[string]interface{}{"content": cleaned}, nil)
					}
				}
			}
		}
		if mr, ok := resp["modelResponse"].(map[string]interface{}); ok {
			if msg, ok := mr["message"].(string); ok && strings.TrimSpace(msg) != "" && msg != lastMessage {
				lastMessage = msg
				rawAll.WriteString(msg)
				if !sawToken {
					if mf == nil {
						cleaned := stripToolAndRenderMarkup(msg)
						cleaned = stripLeadingAngleNoise(sanitizeText(cleaned))
						if cleaned != "" {
							emitChunk(map[string]interface{}{"content": cleaned}, nil)
						}
					} else {
						if cleaned := mf.feed(msg); cleaned != "" {
							cleaned = stripLeadingAngleNoise(cleaned)
							if cleaned != "" {
								emitChunk(map[string]interface{}{"content": cleaned}, nil)
							}
						}
					}
				}
				if strings.Contains(msg, "<grok:render") || strings.Contains(msg, "tool_usage_card") {
					slog.Debug("grok message contains render/tool markup", "has_modelResponse", true)
				}
			}
			for _, u := range extractImageURLs(mr) {
				emitImageURL(u)
			}
			// Fallback: tool/card payloads may include image URLs outside of the known keys.
			for _, u := range extractRenderableImageLinks(mr) {
				emitImageURL(u)
			}
		}
		// Broader fallback: sometimes URLs live outside modelResponse.
		for _, u := range extractRenderableImageLinks(resp) {
			emitImageURL(u)
		}
		if spec.IsVideo {
			if progress, videoURL, _, ok := extractVideoProgress(resp); ok {
				if progress > 0 && progress < 100 {
					emitChunk(map[string]interface{}{"content": fmt.Sprintf("正在生成视频中，当前进度%d%%\n", progress)}, nil)
				}
				if progress >= 100 && strings.TrimSpace(videoURL) != "" {
					finalURL := strings.TrimSpace(videoURL)
					if name, err := h.cacheMediaURL(context.Background(), token, finalURL, "video"); err == nil && name != "" {
						finalURL = "/grok/v1/files/video/" + name
					}
					emitChunk(map[string]interface{}{"content": finalURL}, nil)
				}
			}
		}
		return nil
	})
	if err != nil {
		slog.Warn("grok stream parse failed", "error", err)
		if !sentAny {
			http.Error(w, "stream parse error: "+err.Error(), http.StatusBadGateway)
			return
		}
		emitChunk(map[string]interface{}{"content": "\n[上游响应解析失败]\n"}, nil)
	}

	// Flush any remaining buffered text (avoids "no content" when stream ends quickly).
	if mf != nil {
		if tail := mf.flush(); tail != "" {
			tail = stripLeadingAngleNoise(tail)
			if tail != "" {
				emitChunk(map[string]interface{}{"content": tail}, nil)
			}
		}
	}
	// Emit any pending part-0 previews only if we never saw a full variant.
	// Try to fetch/emit the full variant first; if it doesn't exist, fall back to the preview.
	for full, part := range pendingPart {
		if seenFull[full] {
			continue
		}
		// Try full (cache through this server for client reachability).
		if name, err := h.cacheMediaURL(context.Background(), token, full, "image"); err == nil && name != "" {
			val := "/grok/v1/files/image/" + name
			if publicBase != "" && strings.HasPrefix(val, "/") {
				val = publicBase + val
			}
			if md := formatImageMarkdown(val); md != "" {
				emitChunk(map[string]interface{}{"content": md}, nil)
			}
			continue
		}
		// Fall back to preview.
		emitImageURL(part)
	}

	// If Grok emitted search_images tool cards, generate equivalent images and append as Markdown.
	// IMPORTANT: do NOT run this fallback when the user provided image attachments (vision Q/A),
	// otherwise we may accidentally generate unrelated images and/or corrupt the answer.
	rawText := rawAll.String()
	args := parseSearchImagesArgsFromText(rawText)
	if !hasAttachments {
		desc := strings.TrimSpace(userPrompt)
		ld := strings.ToLower(desc)
		looksLikeImageReq := desc != "" && (strings.Contains(desc, "图片") || strings.Contains(desc, "照片") || strings.Contains(ld, "image") || strings.Contains(ld, "picture"))

		// 1) If tool args exist, use them.
		// 2) If we see search_images markup but args missing, fallback to user prompt.
		if len(args) == 0 && strings.Contains(rawText, "search_images") && looksLikeImageReq {
			args = []SearchImagesArgs{{ImageDescription: desc, NumberOfImages: inferRequestedImageCount(desc, 4)}}
		}
		// 3) If we see grok render image cards but no URLs, also fallback to user prompt.
		if len(args) == 0 && strings.Contains(rawText, "grok:render") && looksLikeImageReq {
			args = []SearchImagesArgs{{ImageDescription: desc, NumberOfImages: inferRequestedImageCount(desc, 4)}}
		}
		// 4) Some responses only include xai tool usage cards (no explicit search_images args).
		if len(args) == 0 && strings.Contains(rawText, "tool_usage_card") && looksLikeImageReq {
			args = []SearchImagesArgs{{ImageDescription: desc, NumberOfImages: inferRequestedImageCount(desc, 4)}}
		}
		// 5) If the user clearly asked for images but Grok returned no usable image URLs/cards,
		// just generate images from the user prompt (Cherry Studio compatibility).
		if len(args) == 0 && looksLikeImageReq {
			args = []SearchImagesArgs{{ImageDescription: desc, NumberOfImages: inferRequestedImageCount(desc, 4)}}
		}
	}
	if len(args) > 0 {
		imSpec, ok := ResolveModel("grok-imagine-1.0")
		if ok {
			for _, a := range args {
				n := a.NumberOfImages
				if n > 4 {
					n = 4
				}
				var urls []string
				var debugHTTP []string
				var debugAsset []string
				// grok upstream often returns 2 images per call; loop to reach n.
				for attempt := 0; attempt < 3; attempt++ {
					cur := normalizeImageURLs(urls, n)
					if len(cur) >= n {
						urls = cur
						break
					}
					need := n - len(cur)
					payload := h.client.chatPayload(imSpec, "Image Generation: "+a.ImageDescription, true, need)
					resp2, err2 := h.client.doChat(context.Background(), token, payload)
					if err2 != nil {
						break
					}
					_ = parseUpstreamLines(resp2.Body, func(line map[string]interface{}) error {
						if mr, ok := line["modelResponse"].(map[string]interface{}); ok {
							urls = append(urls, extractImageURLs(mr)...)
							urls = append(urls, extractRenderableImageLinks(mr)...)
							if h.cfg != nil && h.cfg.GrokDebugImageFallback {
								debugHTTP = append(debugHTTP, collectHTTPStrings(mr, 20)...)
								debugAsset = append(debugAsset, collectAssetLikeStrings(mr, 40)...)
							}
						}
						urls = append(urls, extractRenderableImageLinks(line)...)
						if h.cfg != nil && h.cfg.GrokDebugImageFallback {
							debugHTTP = append(debugHTTP, collectHTTPStrings(line, 20)...)
							debugAsset = append(debugAsset, collectAssetLikeStrings(line, 40)...)
						}
						return nil
					})
					resp2.Body.Close()
				}
				if h.cfg != nil && h.cfg.GrokDebugImageFallback {
					debugHTTP = uniqueStrings(debugHTTP)
					debugAsset = uniqueStrings(debugAsset)
					if len(debugHTTP) > 0 {
						slog.Info("grok imagine fallback: observed http urls", "count", len(debugHTTP), "urls", debugHTTP)
					} else {
						slog.Info("grok imagine fallback: no http urls observed")
					}
					if len(debugAsset) > 0 {
						slog.Info("grok imagine fallback: observed asset-like strings", "count", len(debugAsset), "items", debugAsset)
					}
				}
				urls = normalizeImageURLs(urls, n)
				urls = appendImageCandidates(urls, debugHTTP, debugAsset, n)
				if len(urls) == 0 {
					emitChunk(map[string]interface{}{"content": "\n[图片生成未返回可用链接]\n"}, nil)
				}
				var imgOut strings.Builder
				wroteSep := false
				for _, u := range urls {
					val, errV := h.imageOutputValue(context.Background(), token, u, "url")
					if errV != nil || strings.TrimSpace(val) == "" {
						val = u
					}
					if publicBase != "" && strings.HasPrefix(val, "/") {
						val = publicBase + val
					}
					if !wroteSep {
						imgOut.WriteString("\n\n")
						wroteSep = true
					}
					imgOut.WriteString("![](")
					imgOut.WriteString(val)
					imgOut.WriteString(")\n")
				}
				if imgOut.Len() > 0 {
					emitChunk(map[string]interface{}{"content": imgOut.String()}, nil)
				}
			}
		}
	}

	emitChunk(map[string]interface{}{}, "stop")
	writeSSE(w, "", "[DONE]")
	if flusher != nil {
		flusher.Flush()
	}
}

func (h *Handler) collectChat(w http.ResponseWriter, model string, spec ModelSpec, token string, publicBase string, hasAttachments bool, userPrompt string, body io.Reader) {
	id := "chatcmpl_" + randomHex(8)
	var content strings.Builder
	lastMessage := ""
	sawToken := false
	videoURL := ""
	var imageCandidates []string

	err := parseUpstreamLines(body, func(resp map[string]interface{}) error {
		if tokenDelta, ok := resp["token"].(string); ok && tokenDelta != "" {
			sawToken = true
			content.WriteString(tokenDelta)
		}
		if mr, ok := resp["modelResponse"].(map[string]interface{}); ok {
			if msg, ok := mr["message"].(string); ok && strings.TrimSpace(msg) != "" && msg != lastMessage {
				lastMessage = msg
				if !sawToken {
					content.WriteString(msg)
				}
				if strings.Contains(msg, "<grok:render") || strings.Contains(msg, "tool_usage_card") {
					slog.Debug("grok message contains render/tool markup", "has_modelResponse", true)
				}
			}
			imageCandidates = append(imageCandidates, extractImageURLs(mr)...)
			imageCandidates = append(imageCandidates, extractRenderableImageLinks(mr)...)
		}
		imageCandidates = append(imageCandidates, extractRenderableImageLinks(resp)...)
		if spec.IsVideo {
			if progress, vurl, _, ok := extractVideoProgress(resp); ok && progress >= 100 && strings.TrimSpace(vurl) != "" {
				videoURL = strings.TrimSpace(vurl)
			}
		}
		return nil
	})
	if err != nil {
		http.Error(w, "stream parse error: "+err.Error(), http.StatusBadGateway)
		return
	}

	if videoURL != "" {
		if name, err := h.cacheMediaURL(context.Background(), token, videoURL, "video"); err == nil && name != "" {
			videoURL = "/grok/v1/files/video/" + name
		}
		if content.Len() > 0 {
			content.WriteString("\n")
		}
		content.WriteString(videoURL)
	}

	finalContent := stripToolAndRenderMarkup(content.String())
	finalContent = stripLeadingAngleNoise(sanitizeText(finalContent))

	// Append any collected image links as Markdown, after text cleanup.
	imgs := normalizeImageURLs(imageCandidates, 8)
	for _, u := range imgs {
		val, errV := h.imageOutputValue(context.Background(), token, u, "url")
		if errV != nil || strings.TrimSpace(val) == "" {
			val = u
		}
		if publicBase != "" && strings.HasPrefix(val, "/") {
			val = publicBase + val
		}
		finalContent += formatImageMarkdown(val)
	}
	// If Grok returned search_images tool cards, run an equivalent image generation as a compatibility fallback.
	// This makes OpenAI-compatible clients (e.g. Cherry Studio) able to display images.
	args := parseSearchImagesArgsFromText(finalContent)
	if !hasAttachments {
		desc := strings.TrimSpace(userPrompt)
		ld := strings.ToLower(desc)
		looksLikeImageReq := desc != "" && (strings.Contains(desc, "图片") || strings.Contains(desc, "照片") || strings.Contains(ld, "image") || strings.Contains(ld, "picture"))
		if len(args) == 0 && strings.Contains(finalContent, "search_images") && looksLikeImageReq {
			args = []SearchImagesArgs{{ImageDescription: desc, NumberOfImages: 4}}
		}
		if len(args) == 0 && strings.Contains(finalContent, "grok:render") && looksLikeImageReq {
			args = []SearchImagesArgs{{ImageDescription: desc, NumberOfImages: 4}}
		}
		if len(args) == 0 && strings.Contains(finalContent, "tool_usage_card") && looksLikeImageReq {
			args = []SearchImagesArgs{{ImageDescription: desc, NumberOfImages: 4}}
		}
		if len(args) == 0 && looksLikeImageReq {
			args = []SearchImagesArgs{{ImageDescription: desc, NumberOfImages: 4}}
		}
	}
	if len(args) > 0 {
		cleaned := stripToolAndRenderMarkup(finalContent)
		finalContent = stripLeadingAngleNoise(sanitizeText(cleaned))

		imSpec, ok := ResolveModel("grok-imagine-1.0")
		if ok {
			for _, a := range args {
				n := a.NumberOfImages
				if n > 4 {
					n = 4 // keep it small; the original tool can request many
				}
				var urls []string
				var debugHTTP []string
				var debugAsset []string
				// grok upstream often returns 2 images per call; loop to reach n.
				for attempt := 0; attempt < 3; attempt++ {
					cur := normalizeImageURLs(urls, n)
					if len(cur) >= n {
						urls = cur
						break
					}
					need := n - len(cur)
					payload := h.client.chatPayload(imSpec, "Image Generation: "+a.ImageDescription, true, need)
					resp2, err2 := h.client.doChat(context.Background(), token, payload)
					if err2 != nil {
						break
					}
					_ = parseUpstreamLines(resp2.Body, func(line map[string]interface{}) error {
						if mr, ok := line["modelResponse"].(map[string]interface{}); ok {
							urls = append(urls, extractImageURLs(mr)...)
							urls = append(urls, extractRenderableImageLinks(mr)...)
							if h.cfg != nil && h.cfg.GrokDebugImageFallback {
								debugHTTP = append(debugHTTP, collectHTTPStrings(mr, 20)...)
								debugAsset = append(debugAsset, collectAssetLikeStrings(mr, 40)...)
							}
						}
						urls = append(urls, extractRenderableImageLinks(line)...)
						if h.cfg != nil && h.cfg.GrokDebugImageFallback {
							debugHTTP = append(debugHTTP, collectHTTPStrings(line, 20)...)
							debugAsset = append(debugAsset, collectAssetLikeStrings(line, 40)...)
						}
						return nil
					})
					resp2.Body.Close()
				}
				if h.cfg != nil && h.cfg.GrokDebugImageFallback {
					debugHTTP = uniqueStrings(debugHTTP)
					debugAsset = uniqueStrings(debugAsset)
					if len(debugHTTP) > 0 {
						slog.Info("grok imagine fallback: observed http urls", "count", len(debugHTTP), "urls", debugHTTP)
					} else {
						slog.Info("grok imagine fallback: no http urls observed")
					}
					if len(debugAsset) > 0 {
						slog.Info("grok imagine fallback: observed asset-like strings", "count", len(debugAsset), "items", debugAsset)
					}
				}
				urls = normalizeImageURLs(urls, n)
				if len(urls) == 0 {
					urls = appendImageCandidates(urls, uniqueStrings(debugHTTP), uniqueStrings(debugAsset), n)
				}
				wroteSep := false
				for _, u := range urls {
					val, errV := h.imageOutputValue(context.Background(), token, u, "url")
					if errV != nil || strings.TrimSpace(val) == "" {
						val = u
					}
					if publicBase != "" && strings.HasPrefix(val, "/") {
						val = publicBase + val
					}
					if !wroteSep {
						finalContent += "\n\n"
						wroteSep = true
					}
					finalContent += "![]("
					finalContent += val
					finalContent += ")\n"
				}
			}
		}
	}

	resp := map[string]interface{}{
		"id":      id,
		"object":  "chat.completion",
		"created": time.Now().Unix(),
		"model":   model,
		"choices": []map[string]interface{}{
			{
				"index": 0,
				"message": map[string]interface{}{
					"role":    "assistant",
					"content": finalContent,
				},
				"finish_reason": "stop",
			},
		},
		"usage": map[string]interface{}{
			"prompt_tokens":     0,
			"completion_tokens": 0,
			"total_tokens":      0,
		},
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}

func inferRequestedImageCount(s string, def int) int {
	s = strings.TrimSpace(s)
	if s == "" {
		return def
	}
	// Common Chinese numerals.
	s = strings.ReplaceAll(s, "两张", "2张")
	s = strings.ReplaceAll(s, "二张", "2张")
	s = strings.ReplaceAll(s, "俩张", "2张")
	s = strings.ReplaceAll(s, "三张", "3张")
	s = strings.ReplaceAll(s, "四张", "4张")
	// ASCII digits.
	for _, n := range []int{4, 3, 2, 1} {
		if strings.Contains(s, fmt.Sprintf("%d张", n)) {
			return n
		}
		if strings.Contains(strings.ToLower(s), fmt.Sprintf("%d images", n)) {
			return n
		}
	}
	return def
}

func normalizeImageResponseFormat(format string) string {
	switch strings.ToLower(strings.TrimSpace(format)) {
	case "b64_json", "base64":
		return "b64_json"
	case "url", "":
		return "url"
	default:
		return "url"
	}
}

func imageResponseField(format string) string {
	if normalizeImageResponseFormat(format) == "b64_json" {
		return "b64_json"
	}
	return "url"
}

func imageUsagePayload() map[string]interface{} {
	return map[string]interface{}{
		"total_tokens":  0,
		"input_tokens":  0,
		"output_tokens": 0,
		"input_tokens_details": map[string]interface{}{
			"text_tokens":  0,
			"image_tokens": 0,
		},
	}
}

func mediaExtFromMime(mediaType, mimeType, rawURL string) string {
	m := strings.ToLower(strings.TrimSpace(strings.Split(mimeType, ";")[0]))
	switch m {
	case "image/jpeg":
		return ".jpg"
	case "image/png":
		return ".png"
	case "image/webp":
		return ".webp"
	case "image/gif":
		return ".gif"
	case "video/mp4":
		return ".mp4"
	case "video/webm":
		return ".webm"
	}
	trim := strings.TrimSpace(rawURL)
	if idx := strings.Index(trim, "?"); idx >= 0 {
		trim = trim[:idx]
	}
	ext := strings.ToLower(strings.TrimSpace(filepath.Ext(trim)))
	if ext != "" && len(ext) <= 10 {
		return ext
	}
	if strings.EqualFold(mediaType, "video") {
		return ".mp4"
	}
	return ".jpg"
}

func imageDimsFromBytes(data []byte) (int, int) {
	if len(data) == 0 {
		return 0, 0
	}
	cfg, _, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		return 0, 0
	}
	return cfg.Width, cfg.Height
}

func (h *Handler) cacheMediaURL(ctx context.Context, token, rawURL, mediaType string) (string, error) {
	mediaType = strings.ToLower(strings.TrimSpace(mediaType))
	if mediaType != "video" {
		mediaType = "image"
	}
	trimURL := strings.TrimSpace(rawURL)
	lurl := strings.ToLower(trimURL)
	// Never cache known low-res thumbnail hosts; they lead to blurry results.
	if mediaType == "image" && strings.Contains(lurl, "encrypted-tbn0.gstatic.com") {
		return "", fmt.Errorf("skip thumbnail url")
	}
	// If the client can't reach assets.grok.com (common in some regions), caching through this server
	// is required for images to display at all.
	forceCache := mediaType == "image" && strings.Contains(lurl, "assets.grok.com/")

	data, mimeType, err := h.client.downloadAsset(ctx, token, rawURL)
	if err != nil {
		return "", err
	}
	// Heuristic: avoid caching tiny/low-res images (often thumbnails/previews).
	if mediaType == "image" {
		w, hgt := imageDimsFromBytes(data)
		// For assets.grok.com, caching is required for display (clients may not reach grok CDN).
		if forceCache {
			// Always cache (even previews). We already avoid emitting -part-0 when full exists.
		} else {
			if (w > 0 && hgt > 0 && (w < 900 || hgt < 900)) || len(data) < 60*1024 {
				slog.Debug("skip caching low-res image", "url", trimURL, "bytes", len(data), "w", w, "h", hgt)
				return "", fmt.Errorf("skip low-res image")
			}
		}
	}
	return h.cacheMediaBytes(rawURL, mediaType, data, mimeType)
}

func (h *Handler) imageOutputValue(ctx context.Context, token, url, format string) (string, error) {
	if normalizeImageResponseFormat(format) == "url" {
		trim := strings.TrimSpace(url)
		// Stable contract: prefer full over -part-0. If we only got a preview URL,
		// try the full variant first.
		if strings.Contains(trim, "-part-0/") {
			full := strings.ReplaceAll(trim, "-part-0/", "/")
			if name, err := h.cacheMediaURL(ctx, token, full, "image"); err == nil && name != "" {
				return "/grok/v1/files/image/" + name, nil
			}
		}
		if name, err := h.cacheMediaURL(ctx, token, trim, "image"); err == nil && name != "" {
			return "/grok/v1/files/image/" + name, nil
		}
		return trim, nil
	}
	raw, _, err := h.client.downloadAsset(ctx, token, url)
	if err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(raw), nil
}

func (h *Handler) cacheMediaBytes(rawURL, mediaType string, data []byte, mimeType string) (string, error) {
	mediaType = strings.ToLower(strings.TrimSpace(mediaType))
	if mediaType != "video" {
		mediaType = "image"
	}
	if len(data) == 0 {
		return "", fmt.Errorf("empty media data")
	}

	dir := filepath.Join(cacheBaseDir, mediaType)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}

	sum := sha1.Sum([]byte(strings.TrimSpace(rawURL)))
	name := hex.EncodeToString(sum[:]) + mediaExtFromMime(mediaType, mimeType, rawURL)
	fullPath := filepath.Join(dir, name)

	if info, statErr := os.Stat(fullPath); statErr == nil && info.Mode().IsRegular() && info.Size() > 0 {
		return name, nil
	}

	tmp := fullPath + ".tmp-" + randomHex(4)
	if writeErr := os.WriteFile(tmp, data, 0o644); writeErr != nil {
		_ = os.Remove(tmp)
		return "", writeErr
	}
	if renameErr := os.Rename(tmp, fullPath); renameErr != nil {
		_ = os.Remove(tmp)
		return "", renameErr
	}
	return name, nil
}

func (h *Handler) streamImageGeneration(w http.ResponseWriter, body io.Reader, token, format string, n int) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	flusher, _ := w.(http.Flusher)

	field := imageResponseField(format)
	var urls []string
	targetIndex := -1

	_ = parseUpstreamLines(body, func(resp map[string]interface{}) error {
		if index, progress, ok := extractImageProgress(resp); ok {
			outIndex := index
			if n == 1 {
				if targetIndex < 0 {
					targetIndex = index
				}
				if index != targetIndex {
					return nil
				}
				outIndex = 0
			}
			data := map[string]interface{}{
				"type":     "image_generation.partial_image",
				field:      "",
				"index":    outIndex,
				"progress": progress,
			}
			writeSSE(w, "image_generation.partial_image", encodeJSON(data))
			if flusher != nil {
				flusher.Flush()
			}
		}
		if mr, ok := resp["modelResponse"].(map[string]interface{}); ok {
			urls = append(urls, extractImageURLs(mr)...)
		}
		return nil
	})

	urls = uniqueStrings(urls)
	if n > 0 && len(urls) > n {
		urls = urls[:n]
	}

	for i, u := range urls {
		val, err := h.imageOutputValue(context.Background(), token, u, format)
		if err != nil {
			slog.Warn("grok image stream convert failed", "url", u, "error", err)
			if field == "url" {
				val = u
			}
		}
		data := map[string]interface{}{
			"type":  "image_generation.completed",
			field:   val,
			"index": i,
			"usage": imageUsagePayload(),
		}
		writeSSE(w, "image_generation.completed", encodeJSON(data))
		if flusher != nil {
			flusher.Flush()
		}
	}
}

func (h *Handler) HandleImagesGenerations(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req ImagesGenerationsRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid json", http.StatusBadRequest)
		return
	}
	req.Normalize()
	req.ResponseFormat = normalizeImageResponseFormat(req.ResponseFormat)
	req.Prompt = strings.TrimSpace(req.Prompt)
	if req.Prompt == "" {
		http.Error(w, "prompt is required", http.StatusBadRequest)
		return
	}
	if req.N < 1 || req.N > 10 {
		http.Error(w, "n must be between 1 and 10", http.StatusBadRequest)
		return
	}
	if req.Stream && req.N > 2 {
		http.Error(w, "streaming is only supported when n=1 or n=2", http.StatusBadRequest)
		return
	}
	if err := h.ensureModelEnabled(r.Context(), req.Model); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	spec, ok := ResolveModel(req.Model)
	if !ok || !spec.IsImage || spec.ID == "grok-imagine-1.0-edit" {
		http.Error(w, "image model not supported", http.StatusBadRequest)
		return
	}

	acc, token, err := h.selectAccount(r.Context())
	if err != nil {
		http.Error(w, "no available grok token: "+err.Error(), http.StatusServiceUnavailable)
		return
	}
	release := h.trackAccount(acc)
	defer release()

	onePayload := h.client.chatPayload(spec, "Image Generation: "+req.Prompt, true, req.N)
	if req.Stream {
		resp, err := h.client.doChat(r.Context(), token, onePayload)
		if err != nil {
			h.markAccountStatus(r.Context(), acc, err)
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		defer resp.Body.Close()
		h.syncGrokQuota(acc, resp.Header)
		h.streamImageGeneration(w, resp.Body, token, req.ResponseFormat, req.N)
		return
	}

	callsNeeded := (req.N + 1) / 2
	if callsNeeded < 1 {
		callsNeeded = 1
	}

	var urls []string
	var debugHTTP []string
	var debugAsset []string
	for i := 0; i < callsNeeded; i++ {
		resp, err := h.client.doChat(r.Context(), token, onePayload)
		if err != nil {
			h.markAccountStatus(r.Context(), acc, err)
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		h.syncGrokQuota(acc, resp.Header)
		err = parseUpstreamLines(resp.Body, func(line map[string]interface{}) error {
			if mr, ok := line["modelResponse"].(map[string]interface{}); ok {
				urls = append(urls, extractImageURLs(mr)...)
				debugHTTP = append(debugHTTP, collectHTTPStrings(mr, 50)...)
				debugAsset = append(debugAsset, collectAssetLikeStrings(mr, 100)...)
			}
			debugHTTP = append(debugHTTP, collectHTTPStrings(line, 50)...)
			debugAsset = append(debugAsset, collectAssetLikeStrings(line, 100)...)
			return nil
		})
		resp.Body.Close()
		if err != nil {
			http.Error(w, "stream parse error: "+err.Error(), http.StatusBadGateway)
			return
		}
	}

	urls = normalizeImageURLs(urls, req.N)
	if len(urls) == 0 {
		urls = appendImageCandidates(urls, uniqueStrings(debugHTTP), uniqueStrings(debugAsset), req.N)
	}
	if len(urls) == 0 {
		http.Error(w, "no image generated", http.StatusBadGateway)
		return
	}

	field := imageResponseField(req.ResponseFormat)
	data := make([]map[string]interface{}, 0, len(urls))
	for _, u := range urls {
		val, err := h.imageOutputValue(r.Context(), token, u, req.ResponseFormat)
		if err != nil {
			slog.Warn("grok image convert failed", "url", u, "error", err)
			if field == "url" {
				val = u
			} else {
				val = ""
			}
		}
		data = append(data, map[string]interface{}{field: val})
	}

	out := map[string]interface{}{
		"created": time.Now().Unix(),
		"data":    data,
		"usage":   imageUsagePayload(),
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(out)
}

func (h *Handler) buildImageEditPayload(spec ModelSpec, prompt string, imageURLs []string, parentPostID string) map[string]interface{} {
	imageEditCfg := map[string]interface{}{
		"imageReferences": imageURLs,
	}
	if strings.TrimSpace(parentPostID) != "" {
		imageEditCfg["parentPostId"] = strings.TrimSpace(parentPostID)
	}
	return map[string]interface{}{
		"temporary":                 true,
		"modelName":                 spec.UpstreamModel,
		"message":                   strings.TrimSpace(prompt),
		"enableImageGeneration":     true,
		"returnImageBytes":          false,
		"returnRawGrokInXaiRequest": false,
		"enableImageStreaming":      true,
		"imageGenerationCount":      2,
		"forceConcise":              false,
		"toolOverrides":             map[string]interface{}{"imageGen": true},
		"enableSideBySide":          true,
		"sendFinalMetadata":         true,
		"isReasoning":               false,
		"disableTextFollowUps":      true,
		"responseMetadata": map[string]interface{}{
			"modelConfigOverride": map[string]interface{}{
				"modelMap": map[string]interface{}{
					"imageEditModel":       "imagine",
					"imageEditModelConfig": imageEditCfg,
				},
			},
		},
		"disableMemory":   false,
		"forceSideBySide": false,
	}
}

func isAllowedEditImageMime(mime string) bool {
	switch strings.ToLower(strings.TrimSpace(mime)) {
	case "image/png", "image/jpeg", "image/webp":
		return true
	default:
		return false
	}
}

func (h *Handler) HandleImagesEdits(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if err := r.ParseMultipartForm(80 << 20); err != nil {
		http.Error(w, "invalid multipart form", http.StatusBadRequest)
		return
	}

	prompt := strings.TrimSpace(r.FormValue("prompt"))
	if prompt == "" {
		http.Error(w, "prompt is required", http.StatusBadRequest)
		return
	}
	model := strings.TrimSpace(r.FormValue("model"))
	if model == "" {
		model = "grok-imagine-1.0-edit"
	}
	n := parseIntLoose(r.FormValue("n"), 1)
	if n < 1 || n > 10 {
		http.Error(w, "n must be between 1 and 10", http.StatusBadRequest)
		return
	}
	stream := parseBoolLoose(r.FormValue("stream"), false)
	if stream && n > 2 {
		http.Error(w, "streaming is only supported when n=1 or n=2", http.StatusBadRequest)
		return
	}
	responseFormat := normalizeImageResponseFormat(r.FormValue("response_format"))

	spec, ok := ResolveModel(model)
	if !ok || !spec.IsImage || spec.ID != "grok-imagine-1.0-edit" {
		http.Error(w, "image edit model not supported", http.StatusBadRequest)
		return
	}
	if err := h.ensureModelEnabled(r.Context(), model); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	files := r.MultipartForm.File["image"]
	if len(files) == 0 {
		files = r.MultipartForm.File["image[]"]
	}
	if len(files) == 0 {
		http.Error(w, "image is required", http.StatusBadRequest)
		return
	}
	if len(files) > 16 {
		http.Error(w, "too many images. maximum is 16", http.StatusBadRequest)
		return
	}

	acc, token, err := h.selectAccount(r.Context())
	if err != nil {
		http.Error(w, "no available grok token: "+err.Error(), http.StatusServiceUnavailable)
		return
	}
	release := h.trackAccount(acc)
	defer release()

	imageURLs := make([]string, 0, len(files))
	for _, fh := range files {
		file, err := fh.Open()
		if err != nil {
			http.Error(w, "failed to read image file", http.StatusBadRequest)
			return
		}
		data, err := io.ReadAll(io.LimitReader(file, maxEditImageBytes+1))
		file.Close()
		if err != nil {
			http.Error(w, "failed to read image file", http.StatusBadRequest)
			return
		}
		if len(data) == 0 {
			http.Error(w, "file content is empty", http.StatusBadRequest)
			return
		}
		if len(data) > maxEditImageBytes {
			http.Error(w, "image file too large. maximum is 50MB", http.StatusBadRequest)
			return
		}
		mime := strings.ToLower(strings.TrimSpace(fh.Header.Get("Content-Type")))
		if mime == "image/jpg" {
			mime = "image/jpeg"
		}
		if !isAllowedEditImageMime(mime) {
			mime = mimeFromFilename(strings.TrimSpace(fh.Filename))
			if mime == "image/jpg" {
				mime = "image/jpeg"
			}
		}
		if !isAllowedEditImageMime(mime) {
			http.Error(w, "unsupported image type. supported: png, jpg, webp", http.StatusBadRequest)
			return
		}

		dataURI := dataURIFromBytes(mime, data)
		_, fileURI, err := h.uploadSingleInput(r.Context(), token, dataURI)
		if err != nil {
			h.markAccountStatus(r.Context(), acc, err)
			http.Error(w, "image upload failed: "+err.Error(), http.StatusBadGateway)
			return
		}
		u := strings.TrimSpace(fileURI)
		if u == "" {
			http.Error(w, "image upload failed: empty file uri", http.StatusBadGateway)
			return
		}
		if !strings.HasPrefix(strings.ToLower(u), "http://") && !strings.HasPrefix(strings.ToLower(u), "https://") {
			u = "https://assets.grok.com/" + strings.TrimLeft(u, "/")
		}
		imageURLs = append(imageURLs, u)
	}

	parentPostID := ""
	if len(imageURLs) > 0 {
		if postID, err := h.client.createMediaPost(r.Context(), token, "MEDIA_POST_TYPE_IMAGE", "", imageURLs[0]); err == nil {
			parentPostID = postID
		} else {
			slog.Warn("grok image edit create post failed, continue without parentPostId", "error", err)
		}
	}

	rawPayload := h.buildImageEditPayload(spec, prompt, imageURLs, parentPostID)

	if stream {
		resp, err := h.client.doChat(r.Context(), token, rawPayload)
		if err != nil {
			h.markAccountStatus(r.Context(), acc, err)
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		defer resp.Body.Close()
		h.syncGrokQuota(acc, resp.Header)
		h.streamImageGeneration(w, resp.Body, token, responseFormat, n)
		return
	}

	callsNeeded := (n + 1) / 2
	if callsNeeded < 1 {
		callsNeeded = 1
	}

	var urls []string
	for i := 0; i < callsNeeded; i++ {
		resp, err := h.client.doChat(r.Context(), token, rawPayload)
		if err != nil {
			h.markAccountStatus(r.Context(), acc, err)
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		h.syncGrokQuota(acc, resp.Header)
		err = parseUpstreamLines(resp.Body, func(line map[string]interface{}) error {
			if mr, ok := line["modelResponse"].(map[string]interface{}); ok {
				urls = append(urls, extractImageURLs(mr)...)
			}
			return nil
		})
		resp.Body.Close()
		if err != nil {
			http.Error(w, "stream parse error: "+err.Error(), http.StatusBadGateway)
			return
		}
	}

	urls = uniqueStrings(urls)
	if len(urls) == 0 {
		http.Error(w, "no image generated", http.StatusBadGateway)
		return
	}
	if len(urls) > n {
		urls = urls[:n]
	}

	field := imageResponseField(responseFormat)
	data := make([]map[string]interface{}, 0, len(urls))
	for _, u := range urls {
		val, err := h.imageOutputValue(r.Context(), token, u, responseFormat)
		if err != nil {
			slog.Warn("grok image edit convert failed", "url", u, "error", err)
			if field == "url" {
				val = u
			}
		}
		data = append(data, map[string]interface{}{field: val})
	}

	out := map[string]interface{}{
		"created": time.Now().Unix(),
		"data":    data,
		"usage":   imageUsagePayload(),
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(out)
}

func sanitizeCachedFilename(raw string) string {
	name := strings.TrimSpace(raw)
	name = strings.ReplaceAll(name, "\\", "-")
	name = strings.ReplaceAll(name, "/", "-")
	name = strings.TrimSpace(name)
	if name == "" || strings.Contains(name, "..") {
		return ""
	}
	return name
}

func parseFilesPath(rawPath string) (mediaType string, fileName string, ok bool) {
	if !strings.HasPrefix(rawPath, "/grok/v1/files/") {
		return "", "", false
	}
	path := strings.TrimPrefix(rawPath, "/grok/v1/files/")
	path = strings.TrimSpace(path)
	if path == "" {
		return "", "", false
	}
	parts := strings.SplitN(path, "/", 2)
	if len(parts) != 2 {
		return "", "", false
	}
	mediaType = strings.ToLower(strings.TrimSpace(parts[0]))
	if mediaType != "image" && mediaType != "video" {
		return "", "", false
	}
	fileName = sanitizeCachedFilename(parts[1])
	if fileName == "" {
		return "", "", false
	}
	return mediaType, fileName, true
}

func (h *Handler) HandleFiles(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	mediaType, fileName, ok := parseFilesPath(r.URL.Path)
	if !ok {
		http.Error(w, "file not found", http.StatusNotFound)
		return
	}

	fullPath := filepath.Join(cacheBaseDir, mediaType, fileName)
	info, err := os.Stat(fullPath)
	if err != nil || !info.Mode().IsRegular() {
		http.Error(w, "file not found", http.StatusNotFound)
		return
	}

	ctype := mime.TypeByExtension(strings.ToLower(filepath.Ext(fileName)))
	if ctype == "" {
		if mediaType == "video" {
			ctype = "video/mp4"
		} else {
			ctype = "image/jpeg"
		}
	}
	w.Header().Set("Content-Type", ctype)
	w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	http.ServeFile(w, r, fullPath)
}

func (h *Handler) HandleAdminVoiceToken(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	voice := strings.TrimSpace(r.URL.Query().Get("voice"))
	if voice == "" {
		voice = "ara"
	}
	personality := strings.TrimSpace(r.URL.Query().Get("personality"))
	if personality == "" {
		personality = "assistant"
	}
	speed := 1.0
	if raw := strings.TrimSpace(r.URL.Query().Get("speed")); raw != "" {
		if v, err := strconv.ParseFloat(raw, 64); err == nil && v > 0 {
			speed = v
		}
	}

	acc, token, err := h.selectAccount(r.Context())
	if err != nil {
		http.Error(w, "no available grok token: "+err.Error(), http.StatusServiceUnavailable)
		return
	}

	data, err := h.client.getVoiceToken(r.Context(), token, voice, personality, speed)
	if err != nil {
		h.markAccountStatus(r.Context(), acc, err)
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	respToken, _ := data["token"].(string)
	respToken = strings.TrimSpace(respToken)
	if respToken == "" {
		http.Error(w, "upstream returned no voice token", http.StatusBadGateway)
		return
	}

	out := map[string]interface{}{
		"token":            respToken,
		"url":              "wss://livekit.grok.com",
		"participant_name": "",
		"room_name":        "",
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(out)
}

func (h *Handler) LogSummary() {
	slog.Info("grok go-native endpoints enabled")
}
