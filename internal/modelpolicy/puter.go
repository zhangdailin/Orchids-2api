package modelpolicy

import "strings"

// DefaultPuterModelID is the gateway-side default used when a request arrives
// without a model name. It is a routing default, not a catalog: which models
// exist is always decided by the upstream model list read during a refresh.
const DefaultPuterModelID = "claude-opus-5"

// PuterServiceForModel derives the upstream service that serves a model.
//
// The service is a property of the identifier the upstream catalog advertised,
// so it is derived from the prefix rather than looked up in a compiled-in list.
// A prefix this gateway does not know how to route is refused: publishing the
// model is the catalog's decision, but routing it is this gateway's.
func PuterServiceForModel(modelID string) (string, bool) {
	id := strings.ToLower(strings.TrimSpace(modelID))
	if namespace, _, ok := strings.Cut(id, ":"); ok {
		switch strings.TrimSpace(namespace) {
		case "openrouter", "infron", "alibaba", "togetherai", "deepinfra", "replicate":
			return strings.TrimSpace(namespace), true
		case "google", "gemini":
			return "google", true
		case "openai":
			return "openai", true
		case "anthropic", "claude":
			return "claude", true
		case "xai", "x-ai":
			return "x-ai", true
		case "deepseek", "mistral":
			return strings.TrimSpace(namespace), true
		}
	}
	switch {
	case strings.HasPrefix(id, "claude-"):
		return "claude", true
	case strings.HasPrefix(id, "gpt-"):
		return "openai", true
	case strings.HasPrefix(id, "gemini-"), strings.HasPrefix(id, "gemma-"):
		return "google", true
	case strings.HasPrefix(id, "grok-"):
		return "x-ai", true
	case strings.HasPrefix(id, "deepseek-"):
		return "deepseek", true
	case strings.HasPrefix(id, "mistral-"):
		return "mistral", true
	default:
		return "", false
	}
}
