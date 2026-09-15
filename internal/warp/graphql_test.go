package warp

import (
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
)

func TestDoGraphQL_DecodesGzipResponse(t *testing.T) {
	t.Parallel()

	var compressed bytes.Buffer
	zw := gzip.NewWriter(&compressed)
	if _, err := zw.Write([]byte(`{"data":{"ok":true}}`)); err != nil {
		t.Fatalf("gzip response: %v", err)
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("close gzip response: %v", err)
	}

	client := &http.Client{
		Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
			return &http.Response{
				StatusCode: http.StatusOK,
				Body:       io.NopCloser(bytes.NewReader(compressed.Bytes())),
				Header:     http.Header{"Content-Encoding": []string{"gzip"}},
			}, nil
		}),
	}
	var response struct {
		Data struct {
			OK bool `json:"ok"`
		} `json:"data"`
	}
	if err := doGraphQL(context.Background(), client, warpGraphQLURL, "jwt", "Test", map[string]any{}, &response); err != nil {
		t.Fatalf("doGraphQL() error = %v", err)
	}
	if !response.Data.OK {
		t.Fatal("expected decoded gzip response")
	}
}

func TestDoGraphQL_SendsRegisteredExperimentHeaders(t *testing.T) {
	jwt := "registered-jwt"
	registerJWTExperimentHeaders(jwt, "experiment-id", "experiment-bucket")
	client := &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if got := req.Header.Get("X-Warp-Experiment-Id"); got != "experiment-id" {
			t.Fatalf("experiment id=%q", got)
		}
		if got := req.Header.Get("X-Warp-Experiment-Bucket"); got != "experiment-bucket" {
			t.Fatalf("experiment bucket=%q", got)
		}
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(`{"data":{}}`)), Header: make(http.Header)}, nil
	})}
	if err := doGraphQL(context.Background(), client, warpGraphQLURL, jwt, "Test", map[string]any{}, &struct{}{}); err != nil {
		t.Fatal(err)
	}
}

func TestFetchRequestLimitInfo_UsesOfficialWarpGraphQLHeaders(t *testing.T) {
	t.Parallel()

	client := &http.Client{
		Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			if got := req.URL.String(); got != warpGraphQLV2URL+"?op=GetRequestLimitInfo" {
				t.Fatalf("unexpected graphql url: %s", got)
			}
			if got := req.Header.Get("X-Warp-Client-ID"); got != "warp-app" {
				t.Fatalf("unexpected client id header: %q", got)
			}
			if got := req.Header.Get("X-Warp-OS-Category"); got != warpOSCategory() {
				t.Fatalf("unexpected os category header: %q", got)
			}
			if got := req.Header.Get("User-Agent"); got != "" {
				t.Fatalf("unexpected user agent header: %q", got)
			}
			if got := req.Header.Get("Accept-Encoding"); got != "gzip" {
				t.Fatalf("unexpected accept encoding header: %q", got)
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
	if info.RequestLimit != 100 || info.RequestsUsedSinceLastRefresh != 25 {
		t.Fatalf("unexpected limit info: %+v", info)
	}
	if len(bonuses) != 1 || bonuses[0].RequestCreditsRemaining != 7 {
		t.Fatalf("unexpected bonuses: %+v", bonuses)
	}
}

func TestDoGraphQL_PreservesWarpErrorCode(t *testing.T) {
	t.Parallel()
	client := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusTooManyRequests,
			Body:       io.NopCloser(strings.NewReader(`{"error":"limit"}`)),
			Header:     http.Header{"X-Warp-Error-Code": []string{"REQUEST_LIMIT_EXCEEDED"}},
		}, nil
	})}
	err := doGraphQL(context.Background(), client, warpGraphQLV2URL, "jwt", "Test", map[string]any{}, &struct{}{})
	var statusErr *HTTPStatusError
	if !errors.As(err, &statusErr) || statusErr.ErrorCode != "REQUEST_LIMIT_EXCEEDED" || !strings.Contains(err.Error(), "REQUEST_LIMIT_EXCEEDED") {
		t.Fatalf("error code not preserved: %v", err)
	}
}
