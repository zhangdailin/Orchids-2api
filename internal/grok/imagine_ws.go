package grok

import (
	"context"
	"encoding/base64"
	"fmt"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/goccy/go-json"
	"github.com/gorilla/websocket"

	"orchids-api/internal/util"
)

const (
	defaultImagineWSURL      = "wss://grok.com/ws/imagine/listen"
	imagineWSConnectTimeout  = 15 * time.Second
	imagineWSRoundTimeout    = 120 * time.Second
	imagineWSReadTimeout     = 15 * time.Second
	imagineWSInterRoundPause = 500 * time.Millisecond
)

type imagineWSEvent struct {
	Type     string
	ImageID  string
	Order    int
	Progress int
	URL      string
	Blob     string
	Width    int
	Height   int
	Final    bool
	Error    string
}

type imagineWSSlot struct {
	id       string
	order    int
	width    int
	height   int
	lastURL  string
	lastBlob string
	done     bool
	progress int
}

func imageModelUsesImagineWS(modelID string) bool {
	switch normalizeModelID(modelID) {
	case "grok-imagine-image", "grok-imagine-image-pro":
		return true
	default:
		return false
	}
}

func imageModelUsesProImagineWS(modelID string) bool {
	return normalizeModelID(modelID) == "grok-imagine-image-pro"
}

func isImageGenerationModel(modelID string) bool {
	switch normalizeModelID(modelID) {
	case "grok-imagine-image-lite", "grok-imagine-image", "grok-imagine-image-pro":
		return true
	default:
		return false
	}
}

func isImageEditModel(modelID string) bool {
	return normalizeModelID(modelID) == "grok-imagine-image-edit"
}

func buildImagineWSResetMessage() map[string]interface{} {
	return map[string]interface{}{
		"type":      "conversation.item.create",
		"timestamp": time.Now().UnixMilli(),
		"item": map[string]interface{}{
			"type":    "message",
			"content": []map[string]interface{}{{"type": "reset"}},
		},
	}
}

func buildImagineWSRequestMessage(prompt, aspectRatio string, nsfw bool, pro bool) map[string]interface{} {
	requestID := randomHex(16)
	if requestID == "" {
		requestID = fmt.Sprintf("%d", time.Now().UnixNano())
	}
	return map[string]interface{}{
		"type":      "conversation.item.create",
		"timestamp": time.Now().UnixMilli(),
		"item": map[string]interface{}{
			"type": "message",
			"content": []map[string]interface{}{{
				"requestId": requestID,
				"text":      strings.TrimSpace(prompt),
				"type":      "input_text",
				"properties": map[string]interface{}{
					"section_count":       0,
					"is_kids_mode":        false,
					"enable_nsfw":         nsfw,
					"skip_upsampler":      false,
					"enable_side_by_side": true,
					"is_initial":          false,
					"aspect_ratio":        resolveAspectRatio(aspectRatio),
					"enable_pro":          pro,
				},
			}},
		},
	}
}

func (c *Client) imagineWSHeaders(token string) http.Header {
	h := c.headers(token)
	h.Del("Content-Type")
	h.Set("Origin", "https://grok.com")
	h.Set("Referer", "https://grok.com/imagine")
	h.Set("Sec-Fetch-Dest", "websocket")
	h.Set("Sec-Fetch-Mode", "websocket")
	return h
}

func (c *Client) dialImagineWS(ctx context.Context, token string) (*websocket.Conn, *http.Response, error) {
	if c == nil {
		return nil, nil, fmt.Errorf("grok client not configured")
	}
	proxyFunc := util.ProxyFuncFromConfig(c.cfg)
	if proxyURL := resolveGrokProxy(c.cfg, strings.TrimSpace(getProxyField(c.cfg, "base"))); proxyURL != nil {
		var bypass []string
		if c.cfg != nil {
			bypass = c.cfg.ProxyBypass
		}
		proxyFunc = util.ProxyFuncFromURL(proxyURL, bypass)
	}
	dialer := websocket.Dialer{
		HandshakeTimeout: imagineWSConnectTimeout,
		Proxy:            proxyFunc,
	}
	return dialer.DialContext(ctx, defaultImagineWSURL, c.imagineWSHeaders(token))
}

func parseImagineWSImageID(rawURL string) string {
	u := strings.TrimSpace(rawURL)
	if u == "" {
		return ""
	}
	lower := strings.ToLower(u)
	marker := "/images/"
	idx := strings.Index(lower, marker)
	if idx < 0 {
		return ""
	}
	rest := u[idx+len(marker):]
	for i, r := range rest {
		if r == '.' || r == '/' || r == '?' || r == '#' {
			return strings.TrimSpace(rest[:i])
		}
	}
	return strings.TrimSpace(rest)
}

func imagineFinalEvent(slot *imagineWSSlot) imagineWSEvent {
	if slot == nil {
		return imagineWSEvent{}
	}
	return imagineWSEvent{
		Type:    "image",
		ImageID: slot.id,
		Order:   slot.order,
		URL:     slot.lastURL,
		Blob:    slot.lastBlob,
		Width:   slot.width,
		Height:  slot.height,
		Final:   true,
	}
}

func clampImagineProgress(v int) int {
	if v < 10 {
		return 10
	}
	if v > 99 {
		return 99
	}
	return v
}

func (h *Handler) streamImagineWSImages(ctx context.Context, sess *chatAccountSession, prompt, aspectRatio string, n int, nsfw bool, pro bool) (<-chan imagineWSEvent, <-chan error) {
	events := make(chan imagineWSEvent)
	errs := make(chan error, 1)
	go func() {
		defer close(events)
		defer close(errs)
		errs <- h.runImagineWSImages(ctx, sess, prompt, aspectRatio, n, nsfw, pro, events)
	}()
	return events, errs
}

func (h *Handler) runImagineWSImages(ctx context.Context, sess *chatAccountSession, prompt, aspectRatio string, n int, nsfw bool, pro bool, events chan<- imagineWSEvent) error {
	if n < 1 {
		n = 1
	}
	client := h.currentClient()
	if client == nil {
		return fmt.Errorf("grok client not configured")
	}
	collected := 0
	maxRounds := n * 3
	if maxRounds < 3 {
		maxRounds = 3
	}
	var lastErr error
	for round := 0; round < maxRounds && collected < n; round++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		roundCtx, cancel := context.WithTimeout(ctx, imagineWSRoundTimeout)
		conn, resp, err := client.dialImagineWS(roundCtx, sess.token)
		if err != nil {
			cancel()
			status := 0
			if resp != nil {
				status = resp.StatusCode
			}
			lastErr = fmt.Errorf("imagine websocket dial failed status=%d: %w", status, err)
			h.markAccountStatus(ctx, sess.acc, lastErr)
			return lastErr
		}
		roundFinals, err := runImagineWSRound(roundCtx, conn, prompt, aspectRatio, nsfw, pro, n-collected, events)
		_ = conn.Close()
		cancel()
		if err != nil {
			lastErr = err
			h.markAccountStatus(ctx, sess.acc, err)
			return err
		}
		collected += roundFinals
		if collected >= n {
			return nil
		}
		if !sleepWithContext(ctx, imagineWSInterRoundPause) {
			return ctx.Err()
		}
	}
	if lastErr != nil {
		return lastErr
	}
	return fmt.Errorf("no image generated")
}

func runImagineWSRound(ctx context.Context, conn *websocket.Conn, prompt, aspectRatio string, nsfw bool, pro bool, needed int, events chan<- imagineWSEvent) (int, error) {
	if conn == nil {
		return 0, fmt.Errorf("empty websocket connection")
	}
	if err := conn.WriteJSON(buildImagineWSResetMessage()); err != nil {
		return 0, err
	}
	if err := conn.WriteJSON(buildImagineWSRequestMessage(prompt, aspectRatio, nsfw, pro)); err != nil {
		return 0, err
	}
	slots := map[string]*imagineWSSlot{}
	finals := 0
	deadline := time.Now().Add(imagineWSRoundTimeout)
	for finals < needed {
		if err := ctx.Err(); err != nil {
			return finals, err
		}
		readDeadline := time.Now().Add(imagineWSReadTimeout)
		if readDeadline.After(deadline) {
			readDeadline = deadline
		}
		_ = conn.SetReadDeadline(readDeadline)
		mt, raw, err := conn.ReadMessage()
		if err != nil {
			if ne, ok := err.(net.Error); ok && ne.Timeout() && time.Now().Before(deadline) {
				continue
			}
			for _, slot := range slots {
				if !slot.done && strings.TrimSpace(slot.lastBlob) != "" {
					slot.done = true
					finals++
					if !sendImagineEvent(ctx, events, imagineFinalEvent(slot)) {
						return finals, ctx.Err()
					}
				}
			}
			if finals > 0 {
				return finals, nil
			}
			return finals, err
		}
		if mt != websocket.TextMessage {
			continue
		}
		var msg map[string]interface{}
		if err := json.Unmarshal(raw, &msg); err != nil {
			continue
		}
		switch strings.ToLower(strings.TrimSpace(fmt.Sprint(msg["type"]))) {
		case "json":
			status := strings.TrimSpace(fmt.Sprint(msg["current_status"]))
			imageID := strings.TrimSpace(fmt.Sprint(msg["image_id"]))
			if imageID == "" {
				imageID = strings.TrimSpace(fmt.Sprint(msg["job_id"]))
			}
			if imageID == "" {
				continue
			}
			switch status {
			case "start_stage":
				slot := &imagineWSSlot{
					id:       imageID,
					order:    intFromAny(msg["order"]),
					width:    intFromAny(msg["width"]),
					height:   intFromAny(msg["height"]),
					progress: 10,
				}
				slots[imageID] = slot
				if !sendImagineEvent(ctx, events, imagineWSEvent{Type: "progress", ImageID: imageID, Order: slot.order, Progress: 10}) {
					return finals, ctx.Err()
				}
			case "completed":
				slot := slots[imageID]
				if slot == nil || slot.done {
					continue
				}
				slot.done = true
				if moderated, _ := msg["moderated"].(bool); moderated {
					if !sendImagineEvent(ctx, events, imagineWSEvent{Type: "moderated", ImageID: imageID, Order: slot.order}) {
						return finals, ctx.Err()
					}
					continue
				}
				finals++
				if !sendImagineEvent(ctx, events, imagineFinalEvent(slot)) {
					return finals, ctx.Err()
				}
			}
		case "image":
			urlValue := strings.TrimSpace(fmt.Sprint(msg["url"]))
			imageID := parseImagineWSImageID(urlValue)
			if imageID == "" {
				continue
			}
			slot := slots[imageID]
			if slot == nil || slot.done {
				continue
			}
			slot.lastURL = urlValue
			slot.lastBlob = strings.TrimSpace(fmt.Sprint(msg["blob"]))
			progress := clampImagineProgress(intFromAny(msg["percentage_complete"]))
			if progress > slot.progress {
				slot.progress = progress
				if !sendImagineEvent(ctx, events, imagineWSEvent{Type: "progress", ImageID: imageID, Order: slot.order, Progress: progress}) {
					return finals, ctx.Err()
				}
			}
		case "error":
			code := strings.TrimSpace(fmt.Sprint(msg["err_code"]))
			text := strings.TrimSpace(fmt.Sprint(msg["err_msg"]))
			if text == "" {
				text = string(raw)
			}
			if code != "" {
				text = code + ": " + text
			}
			return finals, fmt.Errorf("imagine websocket error: %s", text)
		}
	}
	return finals, nil
}

func sendImagineEvent(ctx context.Context, events chan<- imagineWSEvent, ev imagineWSEvent) bool {
	select {
	case <-ctx.Done():
		return false
	case events <- ev:
		return true
	}
}

func (h *Handler) imagineImageOutputValue(ctx context.Context, token string, ev imagineWSEvent, format string) (string, error) {
	blob := strings.TrimSpace(ev.Blob)
	if blob != "" {
		if strings.HasPrefix(blob, "data:image/") {
			if normalizeImageResponseFormat(format) == "b64_json" {
				if idx := strings.Index(blob, ","); idx >= 0 {
					return blob[idx+1:], nil
				}
			}
			name, err := h.cacheImageDataURI(blob, ev.URL)
			if err == nil && name != "" {
				return "/grok/v1/files/image/" + name, nil
			}
		} else if raw, err := base64.StdEncoding.DecodeString(blob); err == nil && len(raw) > 0 {
			if normalizeImageResponseFormat(format) == "b64_json" {
				return blob, nil
			}
			name, cacheErr := h.cacheMediaBytes(firstNonEmpty(ev.URL, ev.ImageID), "image", raw, mimeFromFilename(ev.URL))
			if cacheErr == nil && name != "" {
				return "/grok/v1/files/image/" + name, nil
			}
		}
	}
	return h.imageOutputValue(ctx, token, ev.URL, format)
}

func (h *Handler) cacheImageDataURI(dataURI, rawURL string) (string, error) {
	_, b64, mime, err := parseDataURI(dataURI)
	if err != nil {
		return "", err
	}
	raw, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		return "", err
	}
	return h.cacheMediaBytes(firstNonEmpty(rawURL, dataURI), "image", raw, mime)
}
