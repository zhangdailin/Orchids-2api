package warp

import (
	"context"
	"fmt"
	"net/http"
	"strings"
)

type ModelChoice struct {
	ID                string
	Name              string
	BaseModelName     string
	ReasoningLevel    string
	DisableReason     string
	Provider          string
	VisionSupported   bool
	CreditMultiplier  *float64
	RequestMultiplier int
	EnabledHosts      []string
	ContextWindow     ModelContextWindow
}

type ModelContextWindow struct {
	Min     uint32
	Max     uint32
	Default uint32
}

type FeatureModelChoices struct {
	AgentMode        FeatureModelGroup
	Coding           FeatureModelGroup
	CliAgent         FeatureModelGroup
	ComputerUseAgent FeatureModelGroup
}

type FeatureModelGroup struct {
	DefaultID string
	Choices   []ModelChoice
}

func AgentModeModelChoices(features *FeatureModelChoices) []ModelChoice {
	if features == nil {
		return nil
	}
	return mergeWarpModelChoices(features.AgentMode.DefaultID, features.AgentMode.Choices)
}

const getFeatureModelChoicesQuery = `query GetFeatureModelChoices($requestContext: RequestContext!) {
  user(requestContext: $requestContext) {
    __typename
    ... on UserOutput {
      user {
        workspaces {
          featureModelChoice {
            agentMode {
              defaultId
              choices {
                id
                displayName
				baseModelName
				reasoningLevel
				disableReason
				visionSupported
				provider
				usageMetadata {
				  creditMultiplier
				  requestMultiplier
				}
				hostConfigs {
				  enabled
				  modelRoutingHost
				}
				contextWindow {
				  isConfigurable
				  min
				  max
				  default
				}
              }
            }
            coding {
              defaultId
              choices {
                id
                displayName
				disableReason
              }
            }
            cliAgent {
              defaultId
              choices {
                id
                displayName
				disableReason
              }
            }
            computerUseAgent {
              defaultId
              choices {
                id
                displayName
				disableReason
              }
            }
          }
        }
      }
    }
    ... on UserFacingError {
      error {
        message
      }
    }
  }
}`

func (c *Client) FetchDiscoveredFeatureModelChoices(ctx context.Context) (*FeatureModelChoices, string, error) {
	client, err := c.ensureAuthenticated(ctx, false)
	if err != nil {
		return nil, "", err
	}

	features, err := fetchFeatureModelChoices(ctx, client, c.session.currentJWT())
	if err != nil {
		return nil, "", fmt.Errorf("warp feature model discovery failed: %w", err)
	}
	if features == nil || len(features.AgentMode.Choices) == 0 {
		return nil, "", fmt.Errorf("warp feature model discovery returned no agent mode choices")
	}
	return features, "feature_model_choice_all", nil
}

func fetchFeatureModelChoices(ctx context.Context, client *http.Client, jwt string) (*FeatureModelChoices, error) {
	payload := map[string]interface{}{
		"query":         getFeatureModelChoicesQuery,
		"operationName": "GetFeatureModelChoices",
		"variables": map[string]interface{}{
			"requestContext": requestContextPayload(),
		},
	}

	var resp struct {
		Data struct {
			User struct {
				Type  string `json:"__typename"`
				Error struct {
					Message string `json:"message"`
				} `json:"error"`
				User struct {
					Workspaces []struct {
						FeatureModelChoice struct {
							AgentMode        featureModelGroupResponse `json:"agentMode"`
							Coding           featureModelGroupResponse `json:"coding"`
							CliAgent         featureModelGroupResponse `json:"cliAgent"`
							ComputerUseAgent featureModelGroupResponse `json:"computerUseAgent"`
						} `json:"featureModelChoice"`
					} `json:"workspaces"`
				} `json:"user"`
			} `json:"user"`
		} `json:"data"`
		Errors []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}
	if err := doGraphQL(ctx, client, warpGraphQLV2URL, jwt, "GetFeatureModelChoices", payload, &resp); err != nil {
		return nil, err
	}
	if len(resp.Errors) > 0 {
		return nil, fmt.Errorf("warp graphql: %s", resp.Errors[0].Message)
	}
	if !strings.EqualFold(strings.TrimSpace(resp.Data.User.Type), "UserOutput") {
		if msg := strings.TrimSpace(resp.Data.User.Error.Message); msg != "" {
			return nil, fmt.Errorf("warp graphql: %s", msg)
		}
		return nil, fmt.Errorf("warp graphql returned %q for feature model choices", strings.TrimSpace(resp.Data.User.Type))
	}

	features := &FeatureModelChoices{}
	for _, workspace := range resp.Data.User.User.Workspaces {
		choice := workspace.FeatureModelChoice
		features.AgentMode = mergeFeatureModelGroup(features.AgentMode, normalizeFeatureModelGroup(choice.AgentMode))
		features.Coding = mergeFeatureModelGroup(features.Coding, normalizeFeatureModelGroup(choice.Coding))
		features.CliAgent = mergeFeatureModelGroup(features.CliAgent, normalizeFeatureModelGroup(choice.CliAgent))
		features.ComputerUseAgent = mergeFeatureModelGroup(features.ComputerUseAgent, normalizeFeatureModelGroup(choice.ComputerUseAgent))
	}
	return features, nil
}

type featureModelGroupResponse struct {
	DefaultID string `json:"defaultId"`
	Choices   []struct {
		ID              string `json:"id"`
		DisplayName     string `json:"displayName"`
		BaseModelName   string `json:"baseModelName"`
		ReasoningLevel  string `json:"reasoningLevel"`
		DisableReason   string `json:"disableReason"`
		VisionSupported bool   `json:"visionSupported"`
		Provider        string `json:"provider"`
		UsageMetadata   struct {
			CreditMultiplier  *float64 `json:"creditMultiplier"`
			RequestMultiplier int      `json:"requestMultiplier"`
		} `json:"usageMetadata"`
		HostConfigs []struct {
			Enabled          bool   `json:"enabled"`
			ModelRoutingHost string `json:"modelRoutingHost"`
		} `json:"hostConfigs"`
		ContextWindow struct {
			Min     uint32 `json:"min"`
			Max     uint32 `json:"max"`
			Default uint32 `json:"default"`
		} `json:"contextWindow"`
	} `json:"choices"`
}

func normalizeFeatureModelGroup(raw featureModelGroupResponse) FeatureModelGroup {
	group := FeatureModelGroup{
		DefaultID: normalizeModelID(raw.DefaultID),
		Choices:   make([]ModelChoice, 0, len(raw.Choices)),
	}
	for _, choice := range raw.Choices {
		if strings.TrimSpace(choice.DisableReason) != "" {
			continue
		}
		if normalized, ok := normalizeWarpModelChoice(choice.ID, choice.DisplayName); ok {
			normalized.BaseModelName = strings.TrimSpace(choice.BaseModelName)
			normalized.ReasoningLevel = strings.TrimSpace(choice.ReasoningLevel)
			normalized.Provider = strings.TrimSpace(choice.Provider)
			normalized.VisionSupported = choice.VisionSupported
			normalized.CreditMultiplier = choice.UsageMetadata.CreditMultiplier
			normalized.RequestMultiplier = choice.UsageMetadata.RequestMultiplier
			normalized.ContextWindow = ModelContextWindow{
				Min:     choice.ContextWindow.Min,
				Max:     choice.ContextWindow.Max,
				Default: choice.ContextWindow.Default,
			}
			for _, host := range choice.HostConfigs {
				if host.Enabled && strings.TrimSpace(host.ModelRoutingHost) != "" {
					normalized.EnabledHosts = append(normalized.EnabledHosts, strings.TrimSpace(host.ModelRoutingHost))
				}
			}
			group.Choices = append(group.Choices, normalized)
		}
	}
	defaultAvailable := false
	for _, choice := range group.Choices {
		if choice.ID == group.DefaultID {
			defaultAvailable = true
			break
		}
	}
	if !defaultAvailable && len(group.Choices) > 0 {
		group.DefaultID = group.Choices[0].ID
	}
	return group
}

func mergeFeatureModelGroup(current, next FeatureModelGroup) FeatureModelGroup {
	if current.DefaultID == "" {
		current.DefaultID = next.DefaultID
	}
	current.Choices = mergeWarpModelChoices(current.DefaultID, current.Choices, next.Choices)
	return current
}

func normalizeWarpModelChoice(id, name string) (ModelChoice, bool) {
	id = normalizeModelID(id)
	if id == "" {
		return ModelChoice{}, false
	}
	name = strings.TrimSpace(name)
	if name == "" {
		name = id
	}
	return ModelChoice{
		ID:   id,
		Name: name,
	}, true
}

func mergeWarpModelChoices(defaultID string, groups ...[]ModelChoice) []ModelChoice {
	defaultID = normalizeModelID(defaultID)

	out := make([]ModelChoice, 0)
	seen := map[string]struct{}{}
	for _, group := range groups {
		for _, choice := range group {
			normalized, ok := normalizeWarpModelChoice(choice.ID, choice.Name)
			if !ok {
				continue
			}
			normalized.BaseModelName = choice.BaseModelName
			normalized.ReasoningLevel = choice.ReasoningLevel
			normalized.DisableReason = choice.DisableReason
			normalized.Provider = choice.Provider
			normalized.VisionSupported = choice.VisionSupported
			normalized.CreditMultiplier = choice.CreditMultiplier
			normalized.RequestMultiplier = choice.RequestMultiplier
			normalized.EnabledHosts = append([]string(nil), choice.EnabledHosts...)
			normalized.ContextWindow = choice.ContextWindow
			if _, exists := seen[normalized.ID]; exists {
				continue
			}
			seen[normalized.ID] = struct{}{}
			out = append(out, normalized)
		}
	}

	if defaultID == "" || len(out) < 2 {
		return out
	}
	for i := 1; i < len(out); i++ {
		if out[i].ID != defaultID {
			continue
		}
		defaultChoice := out[i]
		copy(out[1:i+1], out[0:i])
		out[0] = defaultChoice
		break
	}
	return out
}
