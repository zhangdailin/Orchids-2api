package warp

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"time"
)

const graphqlURL = "https://app.warp.dev/graphql/v2"

// RequestLimitInfo holds the user's request limit and usage information.
type RequestLimitInfo struct {
	IsUnlimited                 bool   `json:"isUnlimited"`
	NextRefreshTime             string `json:"nextRefreshTime"`
	RequestLimit                int    `json:"requestLimit"`
	RequestsUsedSinceLastRefresh int    `json:"requestsUsedSinceLastRefresh"`
	RequestLimitRefreshDuration string `json:"requestLimitRefreshDuration"`
}

// BonusGrant holds bonus credit grant information.
type BonusGrant struct {
	RequestCreditsGranted   int    `json:"requestCreditsGranted"`
	RequestCreditsRemaining int    `json:"requestCreditsRemaining"`
	Expiration              string `json:"expiration"`
	Reason                  string `json:"reason"`
}

// UsageMetadata holds credit/request multiplier info for a model choice.
type UsageMetadata struct {
	CreditMultiplier  float64 `json:"creditMultiplier"`
	RequestMultiplier float64 `json:"requestMultiplier"`
}

// ModelSpec holds cost/quality/speed ratings for a model choice.
// Values may be strings ("low","high") or numbers from the API.
type ModelSpec struct {
	Cost    interface{} `json:"cost"`
	Quality interface{} `json:"quality"`
	Speed   interface{} `json:"speed"`
}

// ModelChoice represents a single model option within a feature category.
type ModelChoice struct {
	DisplayName    string        `json:"displayName"`
	BaseModelName  string        `json:"baseModelName"`
	ID             string        `json:"id"`
	ReasoningLevel string        `json:"reasoningLevel"`
	UsageMetadata  UsageMetadata `json:"usageMetadata"`
	Description    string        `json:"description"`
	DisableReason  string        `json:"disableReason"`
	VisionSupported bool         `json:"visionSupported"`
	Spec           ModelSpec     `json:"spec"`
	Provider       string        `json:"provider"`
}

// FeatureModelCategory holds the default and available choices for a feature.
type FeatureModelCategory struct {
	DefaultID string        `json:"defaultId"`
	Choices   []ModelChoice `json:"choices"`
}

// FeatureModelChoices holds model choices for all feature categories.
type FeatureModelChoices struct {
	AgentMode *FeatureModelCategory `json:"agentMode"`
	Planning  *FeatureModelCategory `json:"planning"`
	Coding    *FeatureModelCategory `json:"coding"`
	CliAgent  *FeatureModelCategory `json:"cliAgent"`
}

// graphqlRequest is the generic GraphQL request envelope.
type graphqlRequest struct {
	Query     string      `json:"query"`
	Variables interface{} `json:"variables"`
}

// requestContext matches the Warp GraphQL RequestContext input type.
type requestContext struct {
	ClientContext clientContext `json:"clientContext"`
	OSContext     osContext     `json:"osContext"`
}

type clientContext struct {
	Version string `json:"version"`
}

type osContext struct {
	Category           string  `json:"category"`
	LinuxKernelVersion *string `json:"linuxKernelVersion"`
	Name               string  `json:"name"`
	Version            string  `json:"version"`
}

func defaultRequestContext() requestContext {
	return requestContext{
		ClientContext: clientContext{Version: clientVersion},
		OSContext: osContext{
			Category:           osCategory,
			LinuxKernelVersion: nil,
			Name:               osName,
			Version:            osVersion,
		},
	}
}

const getRequestLimitInfoQuery = `query GetRequestLimitInfo($requestContext: RequestContext!) {
  user(requestContext: $requestContext) {
    __typename
    ... on UserOutput {
      user {
        requestLimitInfo {
          isUnlimited
          nextRefreshTime
          requestLimit
          requestsUsedSinceLastRefresh
          requestLimitRefreshDuration
        }
        bonusGrants {
          requestCreditsGranted
          requestCreditsRemaining
          expiration
          reason
        }
      }
    }
    ... on UserFacingError {
      error { __typename message }
    }
  }
}`

const getFeatureModelChoicesQuery = `query GetFeatureModelChoices($requestContext: RequestContext!) {
  user(requestContext: $requestContext) {
    __typename
    ... on UserOutput {
      user {
        workspaces {
          featureModelChoice {
            agentMode { defaultId, choices { displayName, baseModelName, id, reasoningLevel, usageMetadata { creditMultiplier, requestMultiplier }, description, disableReason, visionSupported, spec { cost, quality, speed }, provider } }
            planning { defaultId, choices { displayName, baseModelName, id, reasoningLevel, usageMetadata { creditMultiplier, requestMultiplier }, description, disableReason, visionSupported, spec { cost, quality, speed }, provider } }
            coding { defaultId, choices { displayName, baseModelName, id, reasoningLevel, usageMetadata { creditMultiplier, requestMultiplier }, description, disableReason, visionSupported, spec { cost, quality, speed }, provider } }
            cliAgent { defaultId, choices { displayName, baseModelName, id, reasoningLevel, usageMetadata { creditMultiplier, requestMultiplier }, description, disableReason, visionSupported, spec { cost, quality, speed }, provider } }
          }
        }
      }
    }
    ... on UserFacingError {
      error { __typename message }
    }
  }
}`

// FetchRequestLimitInfo queries the Warp GraphQL API for the user's request
// limit info and bonus grants.
func FetchRequestLimitInfo(ctx context.Context, jwt string) (*RequestLimitInfo, []BonusGrant, error) {
	body := graphqlRequest{
		Query: getRequestLimitInfoQuery,
		Variables: map[string]interface{}{
			"requestContext": defaultRequestContext(),
		},
	}

	raw, err := doGraphQL(ctx, jwt, "GetRequestLimitInfo", body)
	if err != nil {
		return nil, nil, err
	}

	// Navigate: data.user.user
	data, _ := raw["data"].(map[string]interface{})
	if data == nil {
		return nil, nil, fmt.Errorf("warp graphql: missing data in response")
	}
	userWrapper, _ := data["user"].(map[string]interface{})
	if userWrapper == nil {
		return nil, nil, fmt.Errorf("warp graphql: missing user in response")
	}

	// Check for UserFacingError
	if typename, _ := userWrapper["__typename"].(string); typename == "UserFacingError" {
		errObj, _ := userWrapper["error"].(map[string]interface{})
		msg := "unknown error"
		if errObj != nil {
			if m, ok := errObj["message"].(string); ok {
				msg = m
			}
		}
		return nil, nil, fmt.Errorf("warp graphql: %s", msg)
	}

	userObj, _ := userWrapper["user"].(map[string]interface{})
	if userObj == nil {
		return nil, nil, fmt.Errorf("warp graphql: missing user object in response")
	}

	// Parse requestLimitInfo
	var limitInfo RequestLimitInfo
	if rli, ok := userObj["requestLimitInfo"]; ok && rli != nil {
		b, err := json.Marshal(rli)
		if err != nil {
			return nil, nil, fmt.Errorf("warp graphql: marshal requestLimitInfo: %w", err)
		}
		if err := json.Unmarshal(b, &limitInfo); err != nil {
			return nil, nil, fmt.Errorf("warp graphql: unmarshal requestLimitInfo: %w", err)
		}
	}

	// Parse bonusGrants
	var bonuses []BonusGrant
	if bg, ok := userObj["bonusGrants"]; ok && bg != nil {
		b, err := json.Marshal(bg)
		if err != nil {
			return nil, nil, fmt.Errorf("warp graphql: marshal bonusGrants: %w", err)
		}
		if err := json.Unmarshal(b, &bonuses); err != nil {
			return nil, nil, fmt.Errorf("warp graphql: unmarshal bonusGrants: %w", err)
		}
	}

	return &limitInfo, bonuses, nil
}

// FetchFeatureModelChoices queries the Warp GraphQL API for available model
// choices across feature categories.
func FetchFeatureModelChoices(ctx context.Context, jwt string) (*FeatureModelChoices, error) {
	body := graphqlRequest{
		Query: getFeatureModelChoicesQuery,
		Variables: map[string]interface{}{
			"requestContext": defaultRequestContext(),
		},
	}

	raw, err := doGraphQL(ctx, jwt, "GetFeatureModelChoices", body)
	if err != nil {
		return nil, err
	}

	// Navigate: data.user.user.workspaces[0].featureModelChoice
	data, _ := raw["data"].(map[string]interface{})
	if data == nil {
		return nil, fmt.Errorf("warp graphql: missing data in response")
	}
	userWrapper, _ := data["user"].(map[string]interface{})
	if userWrapper == nil {
		return nil, fmt.Errorf("warp graphql: missing user in response")
	}

	// Check for UserFacingError
	if typename, _ := userWrapper["__typename"].(string); typename == "UserFacingError" {
		errObj, _ := userWrapper["error"].(map[string]interface{})
		msg := "unknown error"
		if errObj != nil {
			if m, ok := errObj["message"].(string); ok {
				msg = m
			}
		}
		return nil, fmt.Errorf("warp graphql: UserFacingError: %s", msg)
	}

	userObj, _ := userWrapper["user"].(map[string]interface{})
	if userObj == nil {
		return nil, fmt.Errorf("warp graphql: missing user object in response")
	}

	workspaces, _ := userObj["workspaces"].([]interface{})
	if len(workspaces) == 0 {
		return nil, fmt.Errorf("warp graphql: no workspaces found")
	}

	ws, _ := workspaces[0].(map[string]interface{})
	if ws == nil {
		return nil, fmt.Errorf("warp graphql: invalid workspace object")
	}

	fmc, _ := ws["featureModelChoice"].(map[string]interface{})
	if fmc == nil {
		return nil, fmt.Errorf("warp graphql: missing featureModelChoice")
	}

	result := &FeatureModelChoices{}

	if v, ok := fmc["agentMode"]; ok && v != nil {
		cat, err := parseFeatureModelCategory(v)
		if err != nil {
			slog.Warn("warp graphql: parse agentMode failed", "error", err)
		} else {
			result.AgentMode = cat
		}
	}
	if v, ok := fmc["planning"]; ok && v != nil {
		cat, err := parseFeatureModelCategory(v)
		if err != nil {
			slog.Warn("warp graphql: parse planning failed", "error", err)
		} else {
			result.Planning = cat
		}
	}
	if v, ok := fmc["coding"]; ok && v != nil {
		cat, err := parseFeatureModelCategory(v)
		if err != nil {
			slog.Warn("warp graphql: parse coding failed", "error", err)
		} else {
			result.Coding = cat
		}
	}
	if v, ok := fmc["cliAgent"]; ok && v != nil {
		cat, err := parseFeatureModelCategory(v)
		if err != nil {
			slog.Warn("warp graphql: parse cliAgent failed", "error", err)
		} else {
			result.CliAgent = cat
		}
	}

	return result, nil
}

func parseFeatureModelCategory(v interface{}) (*FeatureModelCategory, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	var cat FeatureModelCategory
	if err := json.Unmarshal(b, &cat); err != nil {
		return nil, err
	}
	return &cat, nil
}

// doGraphQL sends a GraphQL request to the Warp API and returns the parsed
// JSON response.
func doGraphQL(ctx context.Context, jwt, operationName string, body graphqlRequest) (map[string]interface{}, error) {
	payload, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("warp graphql: marshal request: %w", err)
	}

	reqURL := graphqlURL + "?op=" + operationName

	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, reqURL, bytes.NewReader(payload))
	if err != nil {
		return nil, fmt.Errorf("warp graphql: create request: %w", err)
	}

	req.Header.Set("authorization", "Bearer "+jwt)
	req.Header.Set("content-type", "application/json")
	req.Header.Set("x-warp-client-id", clientID)
	req.Header.Set("x-warp-client-version", clientVersion)
	req.Header.Set("x-warp-os-category", osCategory)
	req.Header.Set("x-warp-os-name", osName)
	req.Header.Set("x-warp-os-version", osVersion)
	req.Header.Set("accept", "*/*")
	req.Header.Set("accept-encoding", "gzip")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("warp graphql %s: %w", operationName, err)
	}
	defer resp.Body.Close()

	var reader io.ReadCloser = resp.Body
	if resp.Header.Get("Content-Encoding") == "gzip" {
		reader, err = gzip.NewReader(resp.Body)
		if err != nil {
			return nil, fmt.Errorf("warp graphql: gzip decode: %w", err)
		}
		defer reader.Close()
	}

	respBody, err := io.ReadAll(reader)
	if err != nil {
		return nil, fmt.Errorf("warp graphql %s: read body: %w", operationName, err)
	}

	if resp.StatusCode != http.StatusOK {
		slog.Warn("warp graphql request failed", "op", operationName, "status", resp.StatusCode, "body", string(respBody))
		return nil, fmt.Errorf("warp graphql %s: HTTP %d: %s", operationName, resp.StatusCode, string(respBody))
	}

	var result map[string]interface{}
	if err := json.Unmarshal(respBody, &result); err != nil {
		return nil, fmt.Errorf("warp graphql %s: unmarshal response: %w", operationName, err)
	}

	// Check for top-level GraphQL errors
	if errs, ok := result["errors"]; ok {
		// An empty errors array is not an error
		if errSlice, isSlice := errs.([]interface{}); !isSlice || len(errSlice) > 0 {
			b, _ := json.Marshal(errs)
			return nil, fmt.Errorf("warp graphql %s: errors: %s", operationName, string(b))
		}
	}

	return result, nil
}

// GetRequestLimitInfo fetches the user's request limit info using the
// client's session JWT, ensuring the token is refreshed.
func (c *Client) GetRequestLimitInfo(ctx context.Context) (*RequestLimitInfo, []BonusGrant, error) {
	if c.session == nil {
		return nil, nil, fmt.Errorf("warp session not initialized")
	}
	cid := clientID
	if c.account != nil {
		cid = fmt.Sprintf("warp-%d", c.account.ID)
	}
	if err := c.session.ensureToken(ctx, c.httpClient, cid); err != nil {
		return nil, nil, fmt.Errorf("warp graphql: ensureToken: %w", err)
	}
	jwt := c.session.currentJWT()
	if jwt == "" {
		return nil, nil, fmt.Errorf("warp graphql: jwt missing")
	}
	return FetchRequestLimitInfo(ctx, jwt)
}

// GetFeatureModelChoices fetches available model choices using the client's
// session JWT, ensuring the token is refreshed.
func (c *Client) GetFeatureModelChoices(ctx context.Context) (*FeatureModelChoices, error) {
	if c.session == nil {
		return nil, fmt.Errorf("warp session not initialized")
	}
	cid := clientID
	if c.account != nil {
		cid = fmt.Sprintf("warp-%d", c.account.ID)
	}
	if err := c.session.ensureToken(ctx, c.httpClient, cid); err != nil {
		return nil, fmt.Errorf("warp graphql: ensureToken: %w", err)
	}
	jwt := c.session.currentJWT()
	if jwt == "" {
		return nil, fmt.Errorf("warp graphql: jwt missing")
	}
	return FetchFeatureModelChoices(ctx, jwt)
}
