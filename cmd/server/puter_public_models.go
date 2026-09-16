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

type puterPublicModelDetailsResponse struct {
	Models []puterPublicModelDetails `json:"models"`
}

type puterPublicModelDetails struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

type puterPublicModelChoice struct {
	ID   string
	Name string
}

const puterPublicModelDetailsURL = "https://api.puter.com/puterai/chat/models/details"

// fetchPuterPublicModelChoices reads Puter's documented model catalog and
// treats it as the availability source verbatim. Publishing is the catalog's
// decision; this gateway only decides how a published identifier is routed.
func fetchPuterPublicModelChoices(ctx context.Context, proxyFunc func(*http.Request) (*url.URL, error)) ([]puterPublicModelChoice, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, puterPublicModelDetailsURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")

	transport := &http.Transport{Proxy: http.ProxyFromEnvironment}
	if proxyFunc != nil {
		transport.Proxy = proxyFunc
	}
	client := &http.Client{Timeout: 12 * time.Second, Transport: transport}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("puter model details fetch failed: %d", resp.StatusCode)
	}

	var payload puterPublicModelDetailsResponse
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return nil, err
	}
	models := normalizePuterPublicModelDetails(payload.Models)
	if len(models) == 0 {
		return nil, fmt.Errorf("puter model details contained no current gateway models")
	}
	return models, nil
}

func normalizePuterPublicModelDetails(rawModels []puterPublicModelDetails) []puterPublicModelChoice {
	seen := make(map[string]struct{}, len(rawModels))
	out := make([]puterPublicModelChoice, 0, len(rawModels))
	for _, raw := range rawModels {
		id := strings.ToLower(strings.TrimSpace(raw.ID))
		if id == "" {
			continue
		}
		// No local narrowing: the upstream catalog is the availability source,
		// and filtering it through a compiled-in list is what made this channel
		// publish a fixed generation regardless of what the account could run.
		if _, exists := seen[id]; exists {
			continue
		}
		seen[id] = struct{}{}
		name := strings.TrimSpace(raw.Name)
		if name == "" {
			name = id
		}
		out = append(out, puterPublicModelChoice{ID: id, Name: name})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}
