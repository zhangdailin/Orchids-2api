package grok

import (
	"bytes"
	"context"
	"fmt"
	"github.com/goccy/go-json"
	"io"
	"net/http"
	"strings"
)

type imagesGenerationsResp struct {
	Data []map[string]interface{} `json:"data"`
}

func nestedStringValue(v interface{}, keys ...string) string {
	m, ok := v.(map[string]interface{})
	if !ok || len(keys) == 0 {
		return ""
	}
	cur := m
	for i, key := range keys {
		raw, ok := cur[key]
		if !ok {
			return ""
		}
		if i == len(keys)-1 {
			return strings.TrimSpace(fmt.Sprint(raw))
		}
		next, ok := raw.(map[string]interface{})
		if !ok {
			return ""
		}
		cur = next
	}
	return ""
}

func pickLocalImageValue(item map[string]interface{}, keys ...string) string {
	for _, key := range keys {
		raw, ok := item[key]
		if !ok {
			continue
		}
		if s, ok := raw.(string); ok {
			if v := strings.TrimSpace(s); v != "" {
				return v
			}
		}
	}
	return ""
}

func extractLocalImageGenerationValues(resp imagesGenerationsResp, responseFormat string) []string {
	field := imageResponseField(responseFormat)
	values := make([]string, 0, len(resp.Data))
	for _, item := range resp.Data {
		var v string
		if field == "b64_json" {
			v = pickLocalImageValue(item, "b64_json", "base64", "base64_data", "b64")
			if v == "" {
				v = nestedStringValue(item["image"], "b64_json")
			}
			if v == "" {
				v = nestedStringValue(item["image"], "base64")
			}
			if v == "" {
				v = nestedStringValue(item["image"], "base64_data")
			}
		} else {
			v = pickLocalImageValue(item, "url", "image_url", "imageUrl", "file_url", "fileUrl")
			if v == "" {
				v = nestedStringValue(item["image_url"], "url")
			}
			if v == "" {
				v = nestedStringValue(item["imageUrl"], "url")
			}
			if v == "" {
				v = nestedStringValue(item["image"], "url")
			}
			if v == "" {
				v = nestedStringValue(item["image"], "file_url")
			}
			if v == "" {
				v = nestedStringValue(item["image"], "fileUrl")
			}
		}
		if v != "" {
			values = append(values, v)
		}
	}
	return values
}

func (h *Handler) callLocalImagesGenerationsWithOptions(
	ctx context.Context,
	model string,
	prompt string,
	n int,
	size string,
	responseFormat string,
	nsfw *bool,
) ([]string, error) {
	prompt = strings.TrimSpace(prompt)
	if prompt == "" {
		return nil, fmt.Errorf("empty prompt")
	}
	if n <= 0 {
		n = 1
	}
	model = normalizeModelID(model)
	if strings.TrimSpace(model) == "" {
		model = "grok-imagine-image"
	}
	normalizedSize, err := normalizeImageSize(size)
	if err != nil {
		return nil, err
	}
	responseFormat = normalizeImageResponseFormat(responseFormat)
	// Reuse the same endpoint contract as /grok/v1/images/generations.
	url := fmt.Sprintf("http://127.0.0.1:%s/grok/v1/images/generations", h.cfg.Port)
	payload := map[string]any{
		"model":           model,
		"prompt":          prompt,
		"n":               n,
		"size":            normalizedSize,
		"response_format": responseFormat,
	}
	if nsfw != nil {
		payload["nsfw"] = *nsfw
	}
	b, _ := json.Marshal(payload)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(b))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	client := &http.Client{Transport: transport}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
	if resp.StatusCode/100 != 2 {
		return nil, fmt.Errorf("images endpoint status=%d body=%s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	var out imagesGenerationsResp
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, fmt.Errorf("decode images response: %w", err)
	}
	return extractLocalImageGenerationValues(out, responseFormat), nil
}
