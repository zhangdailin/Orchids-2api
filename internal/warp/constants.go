package warp

import (
	"net/http"
	"runtime"
	"strings"
	"sync"
)

type warpExperimentHeaders struct{ id, bucket string }

var jwtExperimentHeaders sync.Map

func registerJWTExperimentHeaders(jwt, id, bucket string) {
	jwt = strings.TrimSpace(jwt)
	if jwt != "" && strings.TrimSpace(id) != "" {
		jwtExperimentHeaders.Store(jwt, warpExperimentHeaders{id: id, bucket: bucket})
	}
}

func applyWarpExperimentHeaders(req *http.Request, jwt string) {
	if req == nil {
		return
	}
	if raw, ok := jwtExperimentHeaders.Load(strings.TrimSpace(jwt)); ok {
		h := raw.(warpExperimentHeaders)
		req.Header.Set("X-Warp-Experiment-Id", h.id)
		if h.bucket != "" {
			req.Header.Set("X-Warp-Experiment-Bucket", h.bucket)
		}
	}
}

const (
	warpAPIBaseURL   = "https://app.warp.dev"
	warpGraphQLURL   = warpAPIBaseURL + "/graphql"
	warpGraphQLV2URL = warpAPIBaseURL + "/graphql/v2"
	warpAIURL        = warpAPIBaseURL + "/ai/multi-agent"
	warpLoginURL     = warpAPIBaseURL + "/client/login"
	clientVersion    = "v0.2026.08.19.08.15.stable_01"
	clientID         = "warp-app"
	identifier       = "cli-agent-auto"
	computerUseModel = "computer-use-agent-auto"
)

const defaultModel = "auto-open"

func NormalizeModelID(model string) string {
	model = strings.ToLower(strings.TrimSpace(model))
	switch model {
	case "auto", "auto-efficient", "auto-genius":
		return defaultModel
	default:
		return model
	}
}

func applyWarpClientHeaders(req *http.Request) {
	if req == nil {
		return
	}
	req.Header.Set("X-Warp-Client-ID", clientID)
	// The official local-session gateway does not advertise a synthetic client
	// version. Keep the version only in GraphQL's required requestContext instead
	// of fingerprinting direct-token requests as a stale desktop build.
	req.Header.Del("X-Warp-Client-Version")
	category := warpOSCategory()
	if category != "" {
		req.Header.Set("X-Warp-OS-Category", category)
	}
	if category != "" {
		req.Header.Set("X-Warp-OS-Name", category)
	}
	req.Header.Set("User-Agent", "")
}

func warpOSCategory() string {
	switch runtime.GOOS {
	case "darwin":
		return "MacOS"
	case "windows":
		return "Windows"
	case "linux":
		return "Linux"
	default:
		return runtime.GOOS
	}
}
