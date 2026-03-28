package bolt

import (
	"strings"

	"orchids-api/internal/orchids"
)

var coreToolSet = map[string]struct{}{
	"Read":  {},
	"Write": {},
	"Edit":  {},
	"Bash":  {},
	"Glob":  {},
	"Grep":  {},
}

var coreToolHints = map[string]string{
	"Read":  "Read(file_path, limit?, offset?)",
	"Write": "Write(file_path, content)",
	"Edit":  "Edit(file_path, old_string, new_string, replace_all?)",
	"Bash":  "Bash(command, description?, timeout?, run_in_background?)",
	"Glob":  "Glob(path, pattern)",
	"Grep":  "Grep(path, pattern, glob?, output_mode?, -n?, -C?)",
}

var boltSupportedToolOrder = []string{"Read", "Write", "Edit", "Bash", "Glob", "Grep", "Task", "Skill"}

var boltSupportedToolSet = map[string]struct{}{
	"Read":  {},
	"Write": {},
	"Edit":  {},
	"Bash":  {},
	"Glob":  {},
	"Grep":  {},
	"Task":  {},
	"Skill": {},
}

var boltSupportedToolHints = map[string]string{
	"Task":  "Task(description, prompt, subagent_type?)",
	"Skill": "Skill(skill, args)",
}

var continuationOnlyTextSet = map[string]struct{}{
	"继续":        {},
	"继续吧":       {},
	"继续处理":      {},
	"继续上传":      {},
	"继续提交":      {},
	"继续推送":      {},
	"继续修复":      {},
	"继续分析":      {},
	"继续深挖":      {},
	"继续看":       {},
	"接着":        {},
	"接着来":       {},
	"下一步":       {},
	"然后呢":       {},
	"再来":        {},
	"ok":        {},
	"okay":      {},
	"好的":        {},
	"可以":        {},
	"行":         {},
	"continue":  {},
	"goon":      {},
	"proceed":   {},
	"next":      {},
	"nextstep":  {},
	"keepgoing": {},
}

func CanonicalSupportedToolName(name string) string {
	mappedName := orchids.NormalizeToolNameFallback(strings.TrimSpace(name))
	switch strings.ToLower(strings.TrimSpace(mappedName)) {
	case "agent", "task":
		return "Task"
	case "skill":
		return "Skill"
	default:
		return mappedName
	}
}

func FilterSupportedToolNames(names []string) []string {
	if len(names) == 0 {
		return nil
	}

	seen := make(map[string]struct{}, len(boltSupportedToolOrder))
	for _, name := range names {
		mappedName := CanonicalSupportedToolName(name)
		if !IsSupportedTool(mappedName) {
			continue
		}
		seen[strings.ToLower(strings.TrimSpace(mappedName))] = struct{}{}
	}
	if len(seen) == 0 {
		return nil
	}

	out := make([]string, 0, len(seen))
	for _, name := range boltSupportedToolOrder {
		if _, ok := seen[strings.ToLower(name)]; ok {
			out = append(out, name)
		}
	}
	return out
}

func MinimalSupportedToolSpecs(names []string) []map[string]interface{} {
	normalized := FilterSupportedToolNames(names)
	if len(normalized) == 0 {
		return nil
	}

	specs := make([]map[string]interface{}, 0, len(normalized))
	for _, name := range normalized {
		specs = append(specs, map[string]interface{}{"name": name})
	}
	return specs
}

func LooksLikeContinuationOnlyText(text string) bool {
	normalized := normalizeContinuationOnlyText(text)
	if normalized == "" {
		return false
	}

	if _, ok := continuationOnlyTextSet[normalized]; ok {
		return true
	}

	if len([]rune(normalized)) <= 12 {
		if strings.HasPrefix(normalized, "继续") || strings.HasPrefix(normalized, "接着") {
			return true
		}
	}

	return false
}

func normalizeContinuationOnlyText(text string) string {
	text = strings.TrimSpace(strings.ToLower(text))
	if text == "" {
		return ""
	}

	replacer := strings.NewReplacer(
		" ", "",
		"\n", "",
		"\r", "",
		"\t", "",
	)
	return replacer.Replace(text)
}

func IsCoreTool(name string) bool {
	_, ok := coreToolSet[strings.TrimSpace(name)]
	return ok
}

func IsSupportedTool(name string) bool {
	_, ok := boltSupportedToolSet[strings.TrimSpace(CanonicalSupportedToolName(name))]
	return ok
}

func coreToolHint(name string) string {
	if hint, ok := coreToolHints[strings.TrimSpace(name)]; ok {
		return hint
	}
	return name
}

func supportedToolHint(name string) string {
	canonicalName := CanonicalSupportedToolName(name)
	if hint, ok := boltSupportedToolHints[strings.TrimSpace(canonicalName)]; ok {
		return hint
	}
	return coreToolHint(canonicalName)
}
