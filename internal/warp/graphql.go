package warp

import (
	"bytes"
	"context"
	"fmt"
	"io"
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
	RequestLimitRefreshDuration  string `json:"requestLimitRefreshDuration"`
	PlanName                     string `json:"planName"`
	PlanTier                     string `json:"planTier"`
	Remaining                    int    `json:"remaining"`
}

type BonusGrant struct {
	RequestCreditsGranted   int    `json:"requestCreditsGranted"`
	RequestCreditsRemaining int    `json:"requestCreditsRemaining"`
	Expiration              string `json:"expiration"`
	Reason                  string `json:"reason"`
	UserFacingMessage       string `json:"userFacingMessage"`
}

const getRequestLimitInfoQuery = `query GetRequestLimitInfo($requestContext: RequestContext!) {
  user(requestContext: $requestContext) {
    __typename
    ... on UserOutput {
      user {
        workspaces {
          uid
          bonusGrantsInfo {
            grants {
              createdAt
              costCents
              expiration
              reason
              userFacingMessage
              requestCreditsGranted
              requestCreditsRemaining
            }
            spendingInfo {
              currentMonthCreditsPurchased
              currentMonthPeriodEnd
              currentMonthSpendCents
            }
          }
        }
        requestLimitInfo {
          isUnlimited
          nextRefreshTime
          requestLimit
          requestsUsedSinceLastRefresh
          requestLimitRefreshDuration
          isUnlimitedAutosuggestions
          acceptedAutosuggestionsLimit
          acceptedAutosuggestionsSinceLastRefresh
          isUnlimitedVoice
          voiceRequestLimit
          voiceRequestsUsedSinceLastRefresh
          voiceTokenLimit
          voiceTokensUsedSinceLastRefresh
          isUnlimitedCodebaseIndices
          maxCodebaseIndices
          maxFilesPerRepo
          embeddingGenerationBatchSize
          requestLimitPooling
        }
        bonusGrants {
          createdAt
          costCents
          expiration
          reason
          userFacingMessage
          requestCreditsGranted
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

const refundCreditsMutation = `mutation RefundCredits($requestId: String!, $reason: String) {
  refundCredits(requestId: $requestId, reason: $reason) {
    success
    creditsRefunded
    newBalance
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
						RequestLimitRefreshDuration  string  `json:"requestLimitRefreshDuration"`
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
	bonusRemaining := 0
	for _, grant := range bonuses {
		if grant.RequestCreditsRemaining > 0 {
			bonusRemaining += grant.RequestCreditsRemaining
		}
	}
	remaining := requestLimit - used + bonusRemaining
	if remaining < 0 {
		remaining = 0
	}

	return &RequestLimitInfo{
		IsUnlimited:                  info.IsUnlimited,
		NextRefreshTime:              strings.TrimSpace(info.NextRefreshTime),
		RequestLimit:                 requestLimit,
		RequestsUsedSinceLastRefresh: used,
		RequestLimitRefreshDuration:  strings.TrimSpace(info.RequestLimitRefreshDuration),
		Remaining:                    remaining,
	}, bonuses, nil
}

func refundCredits(ctx context.Context, client *http.Client, jwt, requestID, reason string) error {
	requestID = strings.TrimSpace(requestID)
	if requestID == "" {
		return fmt.Errorf("warp request id is empty")
	}

	payload := map[string]interface{}{
		"query": refundCreditsMutation,
		"variables": map[string]interface{}{
			"requestId": requestID,
			"reason":    strings.TrimSpace(reason),
		},
	}

	var resp struct {
		Data struct {
			RefundCredits struct {
				Success         bool    `json:"success"`
				CreditsRefunded float64 `json:"creditsRefunded"`
				NewBalance      float64 `json:"newBalance"`
			} `json:"refundCredits"`
		} `json:"data"`
		Errors []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}
	if err := doGraphQL(ctx, client, warpGraphQLURL, jwt, "RefundCredits", payload, &resp); err != nil {
		return err
	}
	if len(resp.Errors) > 0 {
		return fmt.Errorf("warp refund: %s", resp.Errors[0].Message)
	}
	if !resp.Data.RefundCredits.Success {
		return fmt.Errorf("warp refund failed for request %s", requestID)
	}
	return nil
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
	req.Header.Set("X-Warp-Client-ID", "warp-app")
	req.Header.Set("X-Warp-Client-Version", clientVersion)
	req.Header.Set("X-Warp-OS-Category", clientOSName)
	req.Header.Set("X-Warp-OS-Name", clientOSName)
	req.Header.Set("X-Warp-OS-Version", clientOSVersion)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "*/*")
	req.Header.Set("Accept-Encoding", "gzip,br")
	req.Header.Set("User-Agent", userAgent)

	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("warp graphql request: %w", err)
	}
	defer resp.Body.Close()

	bodyBytes, err := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
	if err != nil {
		return fmt.Errorf("warp graphql read body: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return &HTTPStatusError{
			Operation:  "graphql request",
			StatusCode: resp.StatusCode,
			RetryAfter: parseRetryAfterHeader(resp.Header.Get("Retry-After"), time.Now()),
		}
	}
	if err := json.Unmarshal(bodyBytes, target); err != nil {
		return fmt.Errorf("warp graphql decode response: %w", err)
	}
	return nil
}

func (c *Client) GetRequestLimitInfo(ctx context.Context) (*RequestLimitInfo, []BonusGrant, error) {
	if c == nil || c.session == nil {
		return nil, nil, fmt.Errorf("warp session not initialized")
	}
	client := c.authHTTPClient()
	if err := c.session.ensureToken(ctx, client); err != nil {
		return nil, nil, err
	}
	return fetchRequestLimitInfo(ctx, client, c.session.currentJWT())
}

func (c *Client) RefundCredits(ctx context.Context, reason string) error {
	if c == nil || c.session == nil {
		return fmt.Errorf("warp session not initialized")
	}
	client := c.authHTTPClient()
	if err := c.session.ensureToken(ctx, client); err != nil {
		return err
	}
	return refundCredits(ctx, client, c.session.currentJWT(), c.session.currentRequestID(), reason)
}

func requestContextPayload() map[string]interface{} {
	return map[string]interface{}{
		"clientContext": map[string]interface{}{
			"version": clientVersion,
		},
		"osContext": map[string]interface{}{
			"category":           clientOSName,
			"linuxKernelVersion": nil,
			"name":               clientOSName,
			"version":            clientOSVersion,
		},
	}
}
