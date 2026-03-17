package main

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/goccy/go-json"
)

type puterPublicModelsResponse struct {
	Models []string `json:"models"`
}

type puterPublicModelChoice struct {
	ID   string
	Name string
}

const puterPublicModelsURL = "https://puter.com/puterai/chat/models"

var puterChatCompletionProviders = map[string]struct{}{
	"anthropic": {},
	"deepseek":  {},
	"mistralai": {},
	"x-ai":      {},
}

func fetchPuterPublicModelChoices(ctx context.Context, proxyFunc func(*http.Request) (*url.URL, error)) ([]puterPublicModelChoice, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, puterPublicModelsURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/144.0.0.0 Safari/537.36")

	client := &http.Client{
		Timeout: 12 * time.Second,
		Transport: &http.Transport{
			Proxy: http.ProxyFromEnvironment,
		},
	}
	if proxyFunc != nil {
		client.Transport = &http.Transport{Proxy: proxyFunc}
	}

	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("puter models fetch failed: %d", resp.StatusCode)
	}

	var payload puterPublicModelsResponse
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return nil, err
	}
	return normalizePuterPublicModels(payload.Models), nil
}

func normalizePuterPublicModels(rawModels []string) []puterPublicModelChoice {
	if len(rawModels) == 0 {
		return nil
	}

	seen := make(map[string]struct{}, len(rawModels))
	out := make([]puterPublicModelChoice, 0, len(rawModels))
	for _, raw := range rawModels {
		id, ok := normalizePuterModelID(raw)
		if !ok || id == "" {
			continue
		}
		if _, exists := seen[id]; exists {
			continue
		}
		seen[id] = struct{}{}
		out = append(out, puterPublicModelChoice{ID: id, Name: id})
	}

	sort.Slice(out, func(i, j int) bool {
		return out[i].ID < out[j].ID
	})
	return out
}

func normalizePuterModelID(raw string) (string, bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", false
	}

	prefix, remainder, ok := strings.Cut(raw, ":")
	if !ok {
		return "", false
	}
	prefix = strings.TrimSpace(prefix)
	// puter.com 的公开模型接口会列出很多 provider，但当前我们接入的是
	// `puter-chat-completion` 这条聊天接口；实测 Anthropic、DeepSeek、Mistral、
	// xAI(Grok) 可以跑通，而 OpenAI/Google 仍会返回
	// `no_implementation_available`，这里先保守过滤掉，避免展示可选但实际不可用的模型。
	if _, allowed := puterChatCompletionProviders[prefix]; !allowed {
		return "", false
	}

	_, modelID, ok := strings.Cut(strings.TrimSpace(remainder), "/")
	if !ok {
		return "", false
	}
	modelID = strings.TrimSpace(modelID)
	if modelID == "" {
		return "", false
	}
	return modelID, true
}
