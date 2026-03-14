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
	warpFirebaseKey  = "AIzaSyBdy3O3S9hrdayLJxJ7mriBR4qgUaUygAs"
	warpFirebaseURL  = "https://securetoken.googleapis.com/v1/token?key=" + warpFirebaseKey
	clientVersion    = "v0.2025.01.28.08.02.stable_05"
	userAgent        = "Warp/" + clientVersion
	clientID         = "warp-app"
	clientOSCategory = "macOS"
	clientOSName     = "macOS"
	clientOSVersion  = "15.6.1"
	identifier       = "cli-agent-auto"
)

const defaultModel = "auto"



var canonicalModelAliases = map[string]string{
	"claude-4-sonnet":            "claude-4-sonnet",
	"claude-sonnet-4":            "claude-4-sonnet",
	"claude-4-5-sonnet":          "claude-4-5-sonnet",
	"claude-4.5-sonnet":          "claude-4-5-sonnet",
	"claude-4-5-sonnet-thinking": "claude-4-5-sonnet-thinking",
	"claude-4.5-sonnet-thinking": "claude-4-5-sonnet-thinking",
	"claude-4-5-haiku":           "claude-4-5-haiku",
	"claude-4.5-haiku":           "claude-4-5-haiku",
	"claude-4-5-opus":            "claude-4-5-opus",
	"claude-4.5-opus":            "claude-4-5-opus",
	"claude-4-5-opus-thinking":   "claude-4-5-opus-thinking",
	"claude-4.5-opus-thinking":   "claude-4-5-opus-thinking",
	"claude-4-6-sonnet":          "claude-4-6-sonnet-high",
	"claude-4.6-sonnet":          "claude-4-6-sonnet-high",
	"claude-4-6-sonnet-high":     "claude-4-6-sonnet-high",
	"claude-4.6-sonnet-high":     "claude-4-6-sonnet-high",
	"claude-4-6-sonnet-max":      "claude-4-6-sonnet-max",
	"claude-4.6-sonnet-max":      "claude-4-6-sonnet-max",
	"claude-sonnet-4-6":          "claude-4-6-sonnet-high",
	"claude-4-6-opus":            "claude-4-6-opus-high",
	"claude-4.6-opus":            "claude-4-6-opus-high",
	"claude-4-6-opus-high":       "claude-4-6-opus-high",
	"claude-4.6-opus-high":       "claude-4-6-opus-high",
	"claude-4-6-opus-max":        "claude-4-6-opus-max",
	"claude-4.6-opus-max":        "claude-4-6-opus-max",
	"claude-opus-4-6":            "claude-4-6-opus-high",
	"claude-3-5-sonnet":          "claude-3-5-sonnet",
	"claude-3.5-sonnet":          "claude-3-5-sonnet",
	"claude-3-5-haiku":           "claude-3-5-haiku",
	"claude-3.5-haiku":           "claude-3-5-haiku",
	"claude-3-opus":              "claude-3-opus",
	"claude-3.0-opus":            "claude-3-opus",
	"gpt-4o":                     "gpt-4o",
	"gpt-4o-mini":                "gpt-4o-mini",
	"gpt-4-turbo":                "gpt-4-turbo",
	"gpt-5-low":                  "gpt-5-low",
	"gpt-5-medium":               "gpt-5-medium",
	"gpt-5-high":                 "gpt-5-high",
	"gpt-5-1-low":                "gpt-5-1-low",
	"gpt-5.1-low":                "gpt-5-1-low",
	"gpt-5-1-medium":             "gpt-5-1-medium",
	"gpt-5.1-medium":             "gpt-5-1-medium",
	"gpt-5-1-high":               "gpt-5-1-high",
	"gpt-5.1-high":               "gpt-5-1-high",
	"gpt-5-1-codex-low":          "gpt-5-1-codex-low",
	"gpt-5.1-codex-low":          "gpt-5-1-codex-low",
	"gpt-5-1-codex-medium":       "gpt-5-1-codex-medium",
	"gpt-5.1-codex-medium":       "gpt-5-1-codex-medium",
	"gpt-5-1-codex-high":         "gpt-5-1-codex-high",
	"gpt-5.1-codex-high":         "gpt-5-1-codex-high",
	"gpt-5-1-codex-max-low":      "gpt-5-1-codex-max-low",
	"gpt-5.1-codex-max-low":      "gpt-5-1-codex-max-low",
	"gpt-5-1-codex-max-medium":   "gpt-5-1-codex-max-medium",
	"gpt-5.1-codex-max-medium":   "gpt-5-1-codex-max-medium",
	"gpt-5-1-codex-max-high":     "gpt-5-1-codex-max-high",
	"gpt-5.1-codex-max-high":     "gpt-5-1-codex-max-high",
	"gpt-5-1-codex-max-xhigh":    "gpt-5-1-codex-max-xhigh",
	"gpt-5.1-codex-max-xhigh":    "gpt-5-1-codex-max-xhigh",
	"gpt-5-2-codex-low":          "gpt-5-2-codex-low",
	"gpt-5.2-codex-low":          "gpt-5-2-codex-low",
	"o1":                         "o1",
	"o1-mini":                    "o1-mini",
	"o1-preview":                 "o1-preview",
	"o3-mini":                    "o3-mini",
	"gemini-2-0-flash":           "gemini-2-0-flash",
	"gemini-2.0-flash":           "gemini-2-0-flash",
	"gemini-2-5-pro":             "gemini-2-5-pro",
	"gemini-2.5-pro":             "gemini-2-5-pro",
	"gemini-2-5-flash":           "gemini-2-5-flash",
	"gemini-2.5-flash":           "gemini-2-5-flash",
	"gemini-3-pro":               "gemini-3-pro",
	"deepseek-r1":                "deepseek-r1",
	"deepseek-v3":                "deepseek-v3",
	"grok-3":                     "grok-3",
	"grok-3-mini":                "grok-3-mini",
}

var upstreamModelMap = map[string]string{
	"claude-3-5-sonnet":          "claude_3_5_sonnet",
	"claude-3-5-haiku":           "claude_3_5_haiku",
	"claude-3-opus":              "claude_3_opus",
	"claude-4-sonnet":            "claude_sonnet_4",
	"claude-4-5-sonnet":          "claude_sonnet_4",
	"claude-4-5-sonnet-thinking": "claude_sonnet_4",
	"claude-4-5-haiku":           "claude_3_5_haiku",
	"claude-4-5-opus":            "claude_opus_4",
	"claude-4-5-opus-thinking":   "claude_opus_4",
	"claude-4-6-sonnet-high":     "claude_sonnet_4",
	"claude-4-6-sonnet-max":      "claude_sonnet_4",
	"claude-4-6-opus-high":       "claude_opus_4",
	"claude-4-6-opus-max":        "claude_opus_4",
	"gpt-4o":                     "gpt_4o",
	"gpt-4o-mini":                "gpt_4o_mini",
	"gpt-4-turbo":                "gpt_4_turbo",
	"o1":                         "o1",
	"o1-mini":                    "o1_mini",
	"o1-preview":                 "o1_preview",
	"o3-mini":                    "o3_mini",
	"gemini-2-0-flash":           "gemini_2_0_flash",
	"gemini-2-5-pro":             "gemini_2_5_pro",
	"gemini-2-5-flash":           "gemini_2_5_flash",
	"deepseek-r1":                "deepseek_r1",
	"deepseek-v3":                "deepseek_v3",
	"grok-3":                     "grok_3",
	"grok-3-mini":                "grok_3_mini",
}

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
	key = strings.ReplaceAll(key, ".", "-")
	if mapped, ok := canonicalModelAliases[key]; ok {
		return mapped
	}
	return key
}

func upstreamModelID(model string) string {
	canonical := canonicalModelID(model)
	if canonical == "" {
		return upstreamModelMap["claude-3-5-sonnet"]
	}
	if mapped, ok := upstreamModelMap[canonical]; ok {
		return mapped
	}
	return canonical
}
