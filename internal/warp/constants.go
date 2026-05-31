package warp

import "strings"

const (
	warpAPIBaseURL     = "https://app.warp.dev"
	warpGraphQLURL     = warpAPIBaseURL + "/graphql"
	warpGraphQLV2URL   = warpAPIBaseURL + "/graphql/v2"
	warpLegacyAIURL    = warpAPIBaseURL + "/ai/multi-agent"
	warpLegacyLoginURL = warpAPIBaseURL + "/client/login"
	// Verified on 2026-03-14 with a real Warp refresh token:
	// this key exchanges refresh_token -> id_token successfully.
	warpFirebaseKey   = "AIzaSyBdy3O3S9hrdayLJxJ7mriBR4qgUaUygAs"
	warpFirebaseURL   = "https://securetoken.googleapis.com/v1/token?key=" + warpFirebaseKey
	warpTokenProxyURL = warpAPIBaseURL + "/proxy/token?key=" + warpFirebaseKey
	clientVersion     = "v0.2026.05.06.15.42.stable_03"
	userAgent         = "Warp/" + clientVersion
	clientID          = "warp-app"
	clientOSCategory  = "Windows"
	clientOSName      = "Windows"
	clientOSVersion   = "10.0.26200"
	identifier        = "cli-agent-auto"
)

const defaultModel = "auto-open"

var warpToClientToolMap = map[string]string{
	"grep":           "Grep",
	"subagent":       "Task",
	"file_glob":      "Glob",
	"read_files":     "Read",
	"edit_file":      "Edit",
	"write_file":     "Write",
	"run_command":    "Bash",
	"list_directory": "ListDirectory",
	"search_files":   "Search",
	"create_file":    "Write",
}

func canonicalModelID(model string) string {
	key := strings.ToLower(strings.TrimSpace(model))
	if key == "" {
		return ""
	}
	return key
}

func NormalizeModelID(model string) string {
	return canonicalModelID(model)
}
