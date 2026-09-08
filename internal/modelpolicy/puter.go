package modelpolicy

import (
	"slices"
	"strings"
)

const DefaultPuterModelID = "claude-opus-5"

// latestPuterModelIDs is deliberately small. Puter's public catalog contains
// historical aliases and hundreds of routed models; the gateway only exposes
// the current generation from each directly supported provider.
var latestPuterModelIDs = []string{
	"claude-opus-5",
	"claude-sonnet-5",
	"claude-fable-5",
	"gpt-5.6-sol",
	"gpt-5.6-terra",
	"gpt-5.6-luna",
	"gemini-3.5-flash",
	"grok-4.5",
	"deepseek-v4-pro",
	"deepseek-v4-flash",
	"mistral-small-2603",
}

var latestPuterModelAllowlist = stringSet(latestPuterModelIDs)

func LatestPuterModelIDs() []string {
	return slices.Clone(latestPuterModelIDs)
}

func IsLatestPuterModelID(modelID string) bool {
	id := strings.ToLower(strings.TrimSpace(modelID))
	_, ok := latestPuterModelAllowlist[id]
	return ok
}
