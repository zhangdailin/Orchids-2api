package warp

import (
	"fmt"
	"net/url"
	"os"
	"strings"
)

const (
	warpFirebaseAPIKeyEnv           = "ORCHIDS_WARP_FIREBASE_API_KEY"
	warpFirebaseTokenEndpoint       = "https://securetoken.googleapis.com/v1/token"
	warpFirebaseCustomTokenEndpoint = "https://identitytoolkit.googleapis.com/v1/accounts:signInWithCustomToken"
)

func warpFirebaseTokenURL() (string, error) {
	return warpFirebaseURL(warpFirebaseTokenEndpoint)
}

func warpFirebaseCustomTokenURL() (string, error) {
	return warpFirebaseURL(warpFirebaseCustomTokenEndpoint)
}

func warpFirebaseURL(endpoint string) (string, error) {
	apiKey := strings.TrimSpace(os.Getenv(warpFirebaseAPIKeyEnv))
	if apiKey == "" {
		return "", fmt.Errorf("%s is not configured", warpFirebaseAPIKeyEnv)
	}
	parsed, err := url.Parse(endpoint)
	if err != nil {
		return "", fmt.Errorf("parse Warp Firebase endpoint: %w", err)
	}
	query := parsed.Query()
	query.Set("key", apiKey)
	parsed.RawQuery = query.Encode()
	return parsed.String(), nil
}
