package handler

import (
	"sort"
	"strings"

	"github.com/goccy/go-json"

	"orchids-api/internal/tiktoken"
	"orchids-api/internal/toolname"
)

func supportedToolNames(tools []interface{}) []string {
	return filterSupportedToolNames(collectIncomingToolNames(tools))
}

func collectIncomingToolNames(tools []interface{}) []string {
	if len(tools) == 0 {
		return nil
	}

	rawNames := make([]string, 0, len(tools))
	for _, tool := range tools {
		name, _, _ := toolname.ExtractToolSpecFields(tool)
		if name == "" {
			continue
		}
		rawNames = append(rawNames, name)
	}
	return rawNames
}

func declaredToolNames(tools []interface{}) []string {
	if len(tools) == 0 {
		return nil
	}

	seen := make(map[string]struct{}, len(tools)*2)
	out := make([]string, 0, len(tools)*2)
	add := func(name string) {
		name = strings.TrimSpace(name)
		if name == "" {
			return
		}
		key := strings.ToLower(name)
		if _, ok := seen[key]; ok {
			return
		}
		seen[key] = struct{}{}
		out = append(out, name)
	}

	for _, tool := range tools {
		name, _, _ := toolname.ExtractToolSpecFields(tool)
		if name == "" {
			continue
		}
		add(name)
		mappedName := toolname.NormalizeToolNameFallback(name)
		if !strings.EqualFold(strings.TrimSpace(name), strings.TrimSpace(mappedName)) {
			add(mappedName)
		}
	}

	if len(out) == 0 {
		return nil
	}
	return out
}

func passthroughAllowedToolNames(tools []interface{}, supportedOnly bool) []string {
	if supportedOnly {
		return supportedToolNames(tools)
	}
	return declaredToolNames(tools)
}

func validationAllowedToolNames(effectiveTools []interface{}, originalTools []interface{}, supportedOnly bool) []string {
	if supportedOnly && len(originalTools) > 0 {
		if declared := declaredToolNames(originalTools); len(declared) > 0 {
			return declared
		}
	}
	return passthroughAllowedToolNames(effectiveTools, supportedOnly)
}

// estimateToolsTokens reports how many tokens the tool definitions contribute.
//
// It measures the tools exactly as they are forwarded. The previous version
// measured a compacted projection of them — an allowlist of at most 24 tools,
// every description cut to 128 characters and every schema to 4 KiB — which
// described a request this gateway never sends. The number is what a client
// budgets against (directly through /v1/messages/count_tokens), so under-
// reporting it let a client believe it had room it did not have.
func estimateToolsTokens(tools []interface{}) int {
	if len(tools) == 0 {
		return 0
	}
	raw, err := json.Marshal(tools)
	if err != nil {
		return 0
	}
	var estimator tiktoken.Estimator
	estimator.AddBytes(raw)
	return estimator.Count()
}

func filterSupportedToolNames(raw []string) []string {
	if len(raw) == 0 {
		return nil
	}
	order := map[string]int{
		"Read":  0,
		"Write": 1,
		"Edit":  2,
		"Bash":  3,
		"Glob":  4,
		"Grep":  5,
		"Task":  6,
		"Skill": 7,
	}
	seen := make(map[string]struct{}, len(raw))
	out := make([]string, 0, len(raw))
	for _, name := range raw {
		mapped := toolname.NormalizeToolNameFallback(name)
		if !isCoreTool(mapped) {
			continue
		}
		if _, ok := seen[mapped]; ok {
			continue
		}
		seen[mapped] = struct{}{}
		out = append(out, mapped)
	}
	sort.SliceStable(out, func(i, j int) bool {
		return order[out[i]] < order[out[j]]
	})
	if len(out) == 0 {
		return nil
	}
	return out
}

func isCoreTool(name string) bool {
	switch strings.TrimSpace(name) {
	case "Read", "Write", "Edit", "Bash", "Glob", "Grep", "Task", "Skill":
		return true
	default:
		return false
	}
}
