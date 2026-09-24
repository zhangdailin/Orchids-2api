package grok

import (
	"fmt"
	"strings"

	"github.com/goccy/go-json"

	"orchids-api/internal/modelcatalog"
	"orchids-api/internal/store"
)

// buildReasoningProfileError is a request validation failure discovered only
// after Build account selection. Catalog capabilities are account-scoped, so
// validating before selection would either reject a valid route or silently
// send an unsupported effort to the selected credential.
type buildReasoningProfileError struct {
	model  string
	effort string
}

func (e *buildReasoningProfileError) Error() string {
	return fmt.Sprintf("reasoning.effort %q is not supported by model %s on the selected Build account", e.effort, e.model)
}

// cloneBuildPayload makes an attempt-local payload. Retry normalization may
// edit nested reasoning state, so a shallow map copy is not sufficient.
func cloneBuildPayload(payload map[string]interface{}) (map[string]interface{}, error) {
	if payload == nil {
		return map[string]interface{}{}, nil
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	var cloned map[string]interface{}
	if err := json.Unmarshal(raw, &cloned); err != nil {
		return nil, err
	}
	return cloned, nil
}

func buildCatalogProfile(acc *store.Account, upstreamModel string) (modelcatalog.Profile, bool) {
	if acc == nil {
		return modelcatalog.Profile{}, false
	}
	for _, raw := range acc.GrokModelCatalog {
		if strings.EqualFold(strings.TrimSpace(raw.ModelID), strings.TrimSpace(upstreamModel)) {
			return modelcatalog.Normalize(raw), true
		}
	}
	return modelcatalog.Profile{}, false
}

// buildPayloadForAccount derives an attempt payload from immutable request
// input, then applies the selected account's catalog profile. When old account
// records have no profile, the static compatibility policy remains the fallback.
func buildPayloadForAccount(immutable map[string]interface{}, acc *store.Account, upstreamModel string) (map[string]interface{}, error) {
	payload, err := cloneBuildPayload(immutable)
	if err != nil {
		return nil, err
	}
	profile, found := buildCatalogProfile(acc, upstreamModel)
	if !found {
		normalizeBuildReasoningEffort(payload, upstreamModel)
		return payload, nil
	}

	reasoning, _ := payload["reasoning"].(map[string]interface{})
	explicit := reasoning != nil && strings.TrimSpace(interfaceString(reasoning["effort"])) != ""
	if !explicit {
		if profile.SupportsReasoningEffort && profile.DefaultReasoningEffort != "" {
			if reasoning == nil {
				reasoning = map[string]interface{}{}
			}
			reasoning["effort"] = profile.DefaultReasoningEffort
			payload["reasoning"] = reasoning
		}
		return payload, nil
	}

	effort := strings.ToLower(strings.TrimSpace(interfaceString(reasoning["effort"])))
	allowed := make(map[string]struct{}, len(profile.ReasoningEfforts))
	for _, candidate := range profile.ReasoningEfforts {
		allowed[strings.ToLower(strings.TrimSpace(candidate))] = struct{}{}
	}
	accept := func(candidate string) bool {
		_, ok := allowed[candidate]
		return ok
	}

	normalized := effort
	if !accept(normalized) {
		switch effort {
		case "minimal":
			if accept("low") {
				normalized = "low"
			}
		case "max":
			if accept("xhigh") {
				normalized = "xhigh"
			} else if accept("high") {
				normalized = "high"
			}
		}
	}
	if !profile.SupportsReasoningEffort || !accept(normalized) {
		return nil, &buildReasoningProfileError{model: upstreamModel, effort: effort}
	}
	reasoning["effort"] = normalized
	payload["reasoning"] = reasoning
	return payload, nil
}

func replacePayload(dst map[string]interface{}, src map[string]interface{}) {
	clear(dst)
	for key, value := range src {
		dst[key] = value
	}
}
