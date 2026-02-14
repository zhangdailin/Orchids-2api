package grok

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"orchids-api/internal/store"
)

// generateImagesViaImagesEndpoint implements scheme-1: when chat wants images but no URLs are returned,
// generate images using the same logic as /grok/v1/images/generations (single-image calls + prompt variants).
//
// It intentionally selects its own Grok account (and may switch once on 403/429), so chat and image generation
// do not have to share the same account state.
func (h *Handler) generateImagesViaImagesEndpoint(ctx context.Context, prompt string, n int) ([]string, string) {
	prompt = strings.TrimSpace(prompt)
	if prompt == "" {
		return nil, "empty-prompt"
	}
	if n <= 0 {
		n = 1
	}

	spec, ok := ResolveModel("grok-imagine-1.0")
	if !ok {
		return nil, "model-not-found"
	}

	selectAndTrack := func() (*store.Account, string, func(), error) {
		acc, token, err := h.selectAccount(ctx)
		if err != nil {
			return nil, "", func() {}, err
		}
		release := h.trackAccount(acc)
		return acc, token, release, nil
	}

	acc, token, release, err := selectAndTrack()
	if err != nil {
		return nil, "no-token"
	}
	defer release()

	switched := false
	doChatWithSwitch := func(payload map[string]interface{}) (*http.Response, error) {
		resp, err := h.client.doChat(ctx, token, payload)
		if err == nil {
			return resp, nil
		}
		status := classifyAccountStatusFromError(err.Error())
		h.markAccountStatus(ctx, acc, err)
		if !switched && (status == "403" || status == "429") {
			switched = true
			release()
			acc2, token2, release2, err2 := selectAndTrack()
			if err2 != nil {
				return nil, err
			}
			acc, token, release = acc2, token2, release2
			resp2, err3 := h.client.doChat(ctx, token, payload)
			if err3 == nil {
				return resp2, nil
			}
			status2 := classifyAccountStatusFromError(err3.Error())
			h.markAccountStatus(ctx, acc, err3)
			_ = status2
			return nil, err3
		}
		return nil, err
	}

	var urls []string
	maxAttempts := n * 4
	if maxAttempts < 4 {
		maxAttempts = 4
	}
	deadline := time.Now().Add(60 * time.Second)
	variants := []string{"安福路白天街拍", "外滩夜景街拍", "南京路人潮街拍", "法租界梧桐街拍", "弄堂市井街拍", "陆家嘴现代街拍", "地铁口街拍", "雨天街拍"}
	lastErr := ""

	for i := 0; i < maxAttempts; i++ {
		if time.Now().After(deadline) {
			break
		}
		cur := normalizeImageURLs(urls, 0)
		if len(cur) >= n {
			urls = cur
			break
		}
		v := variants[i%len(variants)]
		seed := randomHex(4)
		prompt2 := fmt.Sprintf("%s\n\n请生成与之前不同的一张图片：%s。要求不同人物/不同构图/不同光线。（seed %s #%d）", prompt, v, seed, i+1)
		payload := h.client.chatPayload(spec, "Image Generation: "+strings.TrimSpace(prompt2), true, 1)
		if h.cfg != nil && h.cfg.GrokDebugImageFallback {
			slog.Info("images-endpoint fallback: attempt", "i", i+1, "max", maxAttempts, "variant", v, "seed", seed)
		}
		if err := ctx.Err(); err != nil {
			lastErr = err.Error()
			break
		}
		resp, err := doChatWithSwitch(payload)
		if err != nil {
			lastErr = err.Error()
			break
		}
		before := len(urls)
		_ = parseUpstreamLines(resp.Body, func(line map[string]interface{}) error {
			if mr, ok := line["modelResponse"].(map[string]interface{}); ok {
				urls = append(urls, extractImageURLs(mr)...)
			}
			urls = append(urls, extractImageURLs(line)...)
			return nil
		})
		resp.Body.Close()
		urls = normalizeImageURLs(urls, 0)
		after := len(urls)
		if h.cfg != nil && h.cfg.GrokDebugImageFallback {
			slog.Info("images-endpoint fallback: attempt result", "new_urls", after-before, "total_urls", after)
		}
	}

	urls = normalizeImageURLs(urls, n)
	if len(urls) == 0 && strings.Contains(lastErr, "status=429") {
		return nil, "rate-limited"
	}
	if len(urls) == 0 && strings.Contains(strings.ToLower(lastErr), "context deadline") {
		return nil, "timeout"
	}
	if len(urls) == 0 && lastErr != "" {
		return nil, "upstream-error"
	}
	if len(urls) == 0 {
		return nil, "no-urls"
	}
	return urls, "ok"
}
