package warp

import (
	"bytes"
	"context"
	"fmt"
	"mime"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/goccy/go-json"
)

type RequestLimitInfo struct {
	IsUnlimited                  bool   `json:"isUnlimited"`
	NextRefreshTime              string `json:"nextRefreshTime"`
	RequestLimit                 int    `json:"requestLimit"`
	RequestsUsedSinceLastRefresh int    `json:"requestsUsedSinceLastRefresh"`
}

type BonusGrant struct {
	RequestCreditsRemaining int `json:"requestCreditsRemaining"`
}

const getRequestLimitInfoQuery = `query GetRequestLimitInfo($requestContext: RequestContext!) {
  user(requestContext: $requestContext) {
    __typename
    ... on UserOutput {
      user {
        workspaces {
		  bonusGrantsInfo {
			grants {
			  requestCreditsRemaining
			}
		  }
        }
        requestLimitInfo {
          isUnlimited
          nextRefreshTime
          requestLimit
          requestsUsedSinceLastRefresh
		}
		bonusGrants {
		  requestCreditsRemaining
        }
      }
    }
    ... on UserFacingError {
      error {
        __typename
        ... on SharedObjectsLimitExceeded {
          limit
          objectType
          message
        }
        ... on PersonalObjectsLimitExceeded {
          limit
          objectType
          message
        }
        ... on AccountDelinquencyError {
          message
        }
      }
    }
  }
}`

func fetchRequestLimitInfo(ctx context.Context, client *http.Client, jwt string) (*RequestLimitInfo, []BonusGrant, error) {
	payload := map[string]interface{}{
		"query":         getRequestLimitInfoQuery,
		"operationName": "GetRequestLimitInfo",
		"variables": map[string]interface{}{
			"requestContext": requestContextPayload(),
		},
	}

	var resp struct {
		Data struct {
			User struct {
				Type string `json:"__typename"`
				User struct {
					Workspaces []struct {
						BonusGrantsInfo struct {
							Grants []BonusGrant `json:"grants"`
						} `json:"bonusGrantsInfo"`
					} `json:"workspaces"`
					RequestLimitInfo struct {
						IsUnlimited                  bool    `json:"isUnlimited"`
						NextRefreshTime              string  `json:"nextRefreshTime"`
						RequestLimit                 float64 `json:"requestLimit"`
						RequestsUsedSinceLastRefresh float64 `json:"requestsUsedSinceLastRefresh"`
					} `json:"requestLimitInfo"`
					BonusGrants []BonusGrant `json:"bonusGrants"`
				} `json:"user"`
			} `json:"user"`
		} `json:"data"`
		Errors []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}
	if err := doGraphQL(ctx, client, warpGraphQLV2URL, jwt, "GetRequestLimitInfo", payload, &resp); err != nil {
		return nil, nil, err
	}
	if len(resp.Errors) > 0 {
		return nil, nil, fmt.Errorf("warp graphql: %s", resp.Errors[0].Message)
	}
	if !strings.EqualFold(strings.TrimSpace(resp.Data.User.Type), "UserOutput") {
		return nil, nil, fmt.Errorf("warp graphql returned %q for request limit info", strings.TrimSpace(resp.Data.User.Type))
	}

	info := resp.Data.User.User.RequestLimitInfo
	requestLimit := int(info.RequestLimit)
	used := int(info.RequestsUsedSinceLastRefresh)
	if used < 0 {
		used = 0
	}

	bonuses := resp.Data.User.User.BonusGrants
	if len(bonuses) == 0 {
		for _, workspace := range resp.Data.User.User.Workspaces {
			if len(workspace.BonusGrantsInfo.Grants) == 0 {
				continue
			}
			bonuses = append(bonuses, workspace.BonusGrantsInfo.Grants...)
		}
	}
	return &RequestLimitInfo{
		IsUnlimited:                  info.IsUnlimited,
		NextRefreshTime:              strings.TrimSpace(info.NextRefreshTime),
		RequestLimit:                 requestLimit,
		RequestsUsedSinceLastRefresh: used,
	}, bonuses, nil
}

func doGraphQL(ctx context.Context, client *http.Client, endpointURL, jwt, operationName string, body interface{}, target interface{}) error {
	data, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("warp graphql marshal request: %w", err)
	}

	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	endpoint := strings.TrimSpace(endpointURL)
	if endpoint == "" {
		endpoint = warpGraphQLURL
	}
	if op := strings.TrimSpace(operationName); op != "" && strings.Contains(endpoint, "/graphql/v2") {
		endpoint = endpoint + "?op=" + url.QueryEscape(op)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(data))
	if err != nil {
		return fmt.Errorf("warp graphql create request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(jwt))
	applyWarpClientHeaders(req)
	applyWarpExperimentHeaders(req, jwt)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "*/*")
	req.Header.Set("Accept-Encoding", "gzip")

	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("warp graphql request: %w", err)
	}
	defer resp.Body.Close()

	bodyBytes, err := readLimitedBody(resp, 2<<20)
	if err != nil {
		return fmt.Errorf("warp graphql read body: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return &HTTPStatusError{
			Operation:       "graphql request",
			StatusCode:      resp.StatusCode,
			ErrorCode:       resp.Header.Get("X-Warp-Error-Code"),
			RetryAfterDelay: parseRetryAfterHeader(resp.Header.Get("Retry-After"), time.Now()),
			Body:            strings.TrimSpace(string(bodyBytes)),
		}
	}
	if err := json.Unmarshal(bodyBytes, target); err != nil {
		contentType, _, _ := mime.ParseMediaType(resp.Header.Get("Content-Type"))
		preview := strings.TrimSpace(string(bodyBytes))
		if len(preview) > 240 {
			preview = preview[:240]
		}
		return fmt.Errorf("warp graphql decode response (content-type %q, body %q): %w", contentType, preview, err)
	}
	return nil
}

func (c *Client) GetRequestLimitInfo(ctx context.Context) (*RequestLimitInfo, []BonusGrant, error) {
	client, err := c.ensureAuthenticated(ctx, false)
	if err != nil {
		return nil, nil, err
	}
	return fetchRequestLimitInfo(ctx, client, c.session.currentJWT())
}

func requestContextPayload() map[string]interface{} {
	return map[string]interface{}{
		"clientContext": map[string]interface{}{
			"version": clientVersion,
		},
		"osContext": map[string]interface{}{
			"category":           warpOSCategory(),
			"linuxKernelVersion": nil,
			"name":               warpOSCategory(),
			"version":            "",
		},
	}
}
