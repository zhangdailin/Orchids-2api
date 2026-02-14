package grok

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"
)

// generateImagesFallback calls Grok image generation (grok-imagine-1.0) to produce image URLs.
// This is used ONLY as a fallback when chat did not return any extractable image links.
func uniqueFirstN(in []string, n int) []string {
	seen := map[string]bool{}
	out := make([]string, 0, n)
	for _, s := range in {
		s = strings.TrimSpace(s)
		if s == "" {
			continue
		}
		if seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
		if len(out) >= n {
			break
		}
	}
	return out
}

func (h *Handler) generateImagesFallback(ctx context.Context, token string, prompt string, n int) ([]string, string) {
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

	var urls []string
	var debugHTTP []string
	var debugAsset []string
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
			slog.Info("grok imagine fallback: attempt", "i", i+1, "max", maxAttempts, "variant", v, "seed", seed)
		}
		if err := ctx.Err(); err != nil {
			lastErr = err.Error()
			break
		}
		resp, err := h.client.doChat(ctx, token, payload)
		if err != nil {
			lastErr = err.Error()
			if h.cfg != nil && h.cfg.GrokDebugImageFallback {
				slog.Warn("grok imagine fallback: upstream error", "err", lastErr)
			}
			break
		}
		before := len(urls)
		_ = parseUpstreamLines(resp.Body, func(line map[string]interface{}) error {
			if mr, ok := line["modelResponse"].(map[string]interface{}); ok {
				urls = append(urls, extractImageURLs(mr)...)
				if h.cfg != nil && h.cfg.GrokDebugImageFallback {
					debugHTTP = append(debugHTTP, collectHTTPStrings(mr, 50)...)
					debugAsset = append(debugAsset, collectAssetLikeStrings(mr, 100)...)
				}
			}
			urls = append(urls, extractImageURLs(line)...)
			if h.cfg != nil && h.cfg.GrokDebugImageFallback {
				debugHTTP = append(debugHTTP, collectHTTPStrings(line, 50)...)
				debugAsset = append(debugAsset, collectAssetLikeStrings(line, 100)...)
			}
			return nil
		})
		resp.Body.Close()
		urls = normalizeImageURLs(urls, 0)
		after := len(urls)
		if h.cfg != nil && h.cfg.GrokDebugImageFallback {
			slog.Info("grok imagine fallback: attempt result", "new_urls", after-before, "total_urls", after, "http_strings", len(debugHTTP), "asset_strings", len(debugAsset))
		}
	}

	urls = normalizeImageURLs(urls, n)
	if len(urls) == 0 && h.cfg != nil && h.cfg.GrokDebugImageFallback {
		slog.Warn("grok imagine fallback: finished with no urls", "reason_hint", lastErr, "http_strings", len(debugHTTP), "asset_strings", len(debugAsset), "http_sample", uniqueFirstN(debugHTTP, 8), "asset_sample", uniqueFirstN(debugAsset, 8))
	}
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
