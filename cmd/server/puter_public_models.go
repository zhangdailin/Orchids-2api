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
	ID            string                 `json:"id"`
	PuterID       string                 `json:"puterId"`
	Name          string                 `json:"name"`
	Provider      string                 `json:"provider"`
	InputCostKey  string                 `json:"input_cost_key"`
	OutputCostKey string                 `json:"output_cost_key"`
	Costs         map[string]interface{} `json:"costs"`
}

type puterPublicModelChoice struct {
	ID            string
	Name          string
	Provider      string
	UpstreamModel string
	Free          bool
	PricingKnown  bool
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
		pricingKnown, free := puterZeroCost(raw.Costs, raw.InputCostKey, raw.OutputCostKey)
		upstreamModel := strings.TrimSpace(raw.PuterID)
		if upstreamModel == "" {
			upstreamModel = id
		}
		out = append(out, puterPublicModelChoice{
			ID: id, Name: name,
			Provider:      strings.ToLower(strings.TrimSpace(raw.Provider)),
			UpstreamModel: upstreamModel,
			Free:          free, PricingKnown: pricingKnown,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// puterZeroCost accepts only an explicit pair of zero input/output prices using
// the cost keys the upstream row itself declares. Missing keys and partial price
// objects stay unknown rather than turning a cached-token zero into "free".
func puterZeroCost(costs map[string]interface{}, inputKey, outputKey string) (known, free bool) {
	inputKey = strings.TrimSpace(inputKey)
	outputKey = strings.TrimSpace(outputKey)
	if len(costs) == 0 || inputKey == "" || outputKey == "" {
		return false, false
	}
	input, inputOK := costs[inputKey].(float64)
	output, outputOK := costs[outputKey].(float64)
	if !inputOK || !outputOK {
		return false, false
	}
	return true, input == 0 && output == 0
}
