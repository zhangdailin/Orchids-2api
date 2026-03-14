package warp

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"testing"
)

func TestFetchRequestLimitInfo_UsesCodeFreeMaxGraphQLV2(t *testing.T) {
	t.Parallel()

	client := &http.Client{
		Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			if got := req.URL.String(); got != warpGraphQLV2URL+"?op=GetRequestLimitInfo" {
				t.Fatalf("unexpected graphql url: %s", got)
			}
			if got := req.Header.Get("X-Warp-Client-ID"); got != "warp-app" {
				t.Fatalf("unexpected client id header: %q", got)
			}
			body := `{"data":{"user":{"__typename":"UserOutput","user":{"workspaces":[{"uid":"ws-1","bonusGrantsInfo":{"grants":[]}}],"requestLimitInfo":{"isUnlimited":false,"nextRefreshTime":"2026-03-15T00:00:00Z","requestLimit":100,"requestsUsedSinceLastRefresh":25,"requestLimitRefreshDuration":"MONTHLY"},"bonusGrants":[{"requestCreditsGranted":7,"requestCreditsRemaining":7,"expiration":"2026-03-20T00:00:00Z","reason":"bonus","userFacingMessage":"bonus"}]}}},"errors":[]}`
			return &http.Response{
				StatusCode: http.StatusOK,
				Body:       io.NopCloser(bytes.NewBufferString(body)),
				Header:     make(http.Header),
			}, nil
		}),
	}

	info, bonuses, err := fetchRequestLimitInfo(context.Background(), client, "jwt")
	if err != nil {
		t.Fatalf("fetchRequestLimitInfo() error = %v", err)
	}
	if info == nil {
		t.Fatal("expected request limit info")
	}
	if info.RequestLimit != 100 || info.Remaining != 82 || info.RequestsUsedSinceLastRefresh != 25 {
		t.Fatalf("unexpected limit info: %+v", info)
	}
	if len(bonuses) != 1 || bonuses[0].RequestCreditsRemaining != 7 {
		t.Fatalf("unexpected bonuses: %+v", bonuses)
	}
}
