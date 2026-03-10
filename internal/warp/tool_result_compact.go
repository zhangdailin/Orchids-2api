package warp

import (
	"fmt"
	"strings"
)

const (
	warpCurrentToolResultLimit = 900
	warpHistoryToolResultLimit = 500
)

func compactWarpToolResultContent(text string, historyMode bool) string {
	text = strings.TrimSpace(text)
	if text == "" {
		return ""
	}
	if looksLikeWarpDirectoryListing(text) {
		return compactWarpDirectoryListing(text, historyMode)
	}
	if historyMode {
		return compactHistoricalWarpToolResult(text)
	}
	return compactCurrentWarpToolResult(text)
}

func looksLikeWarpDirectoryListing(text string) bool {
	lines := nonEmptyWarpLines(text)
	if len(lines) < 4 {
		return false
	}
	entryLike := 0
	strongSignals := 0
	for _, line := range lines {
		switch {
		case looksLikeWarpPathLine(line):
			entryLike++
			strongSignals++
		case looksLikeWarpBareDirectoryEntryLine(line):
			entryLike++
			if hasWarpDirectoryEntrySignal(line) {
				strongSignals++
			}
		}
	}
	return entryLike*100/len(lines) >= 70 && strongSignals > 0
}

func looksLikeWarpPathLine(line string) bool {
	line = strings.TrimSpace(line)
	if line == "" {
		return false
	}
	if strings.HasPrefix(line, "/") || strings.HasPrefix(line, "./") || strings.HasPrefix(line, "../") {
		return true
	}
	return len(line) >= 3 && ((line[1] == ':' && line[2] == '\\') || (line[1] == ':' && line[2] == '/'))
}

func looksLikeWarpBareDirectoryEntryLine(line string) bool {
	line = strings.TrimSpace(line)
	if line == "" {
		return false
	}
	if strings.ContainsAny(line, "\r\n\t") {
		return false
	}
	lower := strings.ToLower(line)
	if strings.Contains(lower, "results are truncated") {
		return false
	}
	if strings.HasPrefix(line, "[") || strings.HasPrefix(line, "<") {
		return false
	}
	if strings.HasPrefix(line, "- ") || strings.HasPrefix(line, "* ") || strings.HasPrefix(line, "• ") {
		return false
	}
	if strings.Contains(line, ": ") || strings.ContainsAny(line, "{}<>|`") {
		return false
	}
	if strings.HasSuffix(line, ".") || strings.HasSuffix(line, "。") || strings.HasSuffix(line, ":") {
		return false
	}
	if strings.Count(line, " ") > 2 {
		return false
	}
	return hasWarpDirectoryEntrySignal(line)
}

func hasWarpDirectoryEntrySignal(line string) bool {
	line = strings.TrimSpace(line)
	if line == "" {
		return false
	}
	if strings.HasPrefix(line, ".") {
		return true
	}
	return strings.ContainsAny(line, "._-/\\")
}

func compactWarpDirectoryListing(text string, historyMode bool) string {
	lines := nonEmptyWarpLines(text)
	if len(lines) == 0 {
		return ""
	}
	total := len(lines)
	pathLines, nonPathCount := splitWarpDirectoryListingLines(lines)
	prefix := sharedWarpDirectoryPrefix(pathLines)

	filtered := make([]string, 0, len(pathLines))
	omittedNoise := 0
	for _, line := range pathLines {
		if shouldDropWarpDirectoryLine(line) {
			omittedNoise++
			continue
		}
		filtered = append(filtered, shortenWarpDirectoryLine(line, prefix))
	}
	if len(filtered) == 0 {
		return fmt.Sprintf("[directory listing trimmed: omitted %d metadata entries and %d non-path lines]", total, nonPathCount)
	}

	if shouldSummarizeWarpDirectoryTopLevel(filtered) {
		summarized, summarizedRoots, omittedRoots := summarizeWarpDirectoryTopLevel(filtered, historyMode)
		if omittedNoise > 0 || nonPathCount > 0 || omittedRoots > 0 {
			summarized = append(summarized, fmt.Sprintf("[directory listing summarized: %d root entries from %d lines; omitted %d metadata entries, %d non-path lines, and %d additional root entries]", summarizedRoots, total, omittedNoise, nonPathCount, omittedRoots))
		}
		result := strings.Join(summarized, "\n")
		if historyMode {
			return truncateWarpResultWithEllipsis(result, warpHistoryToolResultLimit)
		}
		return truncateWarpResultWithEllipsis(result, warpCurrentToolResultLimit)
	}

	limit := 16
	if historyMode {
		limit = 8
	}
	kept := len(filtered)
	extra := 0
	if kept > limit {
		extra = kept - limit
		filtered = filtered[:limit]
	}
	if omittedNoise > 0 || nonPathCount > 0 || extra > 0 {
		filtered = append(filtered, fmt.Sprintf("[directory listing trimmed: kept %d of %d entries; omitted %d metadata entries, %d non-path lines, and %d extra entries]", kept-extra, total, omittedNoise, nonPathCount, extra))
	}

	result := strings.Join(filtered, "\n")
	if historyMode {
		return truncateWarpResultWithEllipsis(result, warpHistoryToolResultLimit)
	}
	return truncateWarpResultWithEllipsis(result, warpCurrentToolResultLimit)
}

func splitWarpDirectoryListingLines(lines []string) ([]string, int) {
	pathLines := make([]string, 0, len(lines))
	nonPathCount := 0
	for _, line := range lines {
		if looksLikeWarpPathLine(line) || looksLikeWarpBareDirectoryEntryLine(line) {
			pathLines = append(pathLines, line)
			continue
		}
		nonPathCount++
	}
	return pathLines, nonPathCount
}

func shouldDropWarpDirectoryLine(line string) bool {
	line = strings.TrimSpace(line)
	if line == "" {
		return false
	}
	if strings.Contains(line, "/.git/") || strings.HasSuffix(line, "/.git") {
		return true
	}
	base := line
	if idx := strings.LastIndexAny(base, `/\`); idx >= 0 {
		base = base[idx+1:]
	}
	switch base {
	case ".DS_Store", "Thumbs.db", "desktop.ini":
		return true
	default:
		return false
	}
}

func sharedWarpDirectoryPrefix(lines []string) string {
	if len(lines) == 0 {
		return ""
	}
	pathLines, _ := splitWarpDirectoryListingLines(lines)
	if len(pathLines) == 0 {
		return ""
	}
	prefix := strings.TrimSpace(pathLines[0])
	for _, raw := range pathLines[1:] {
		line := strings.TrimSpace(raw)
		for prefix != "" && !strings.HasPrefix(line, prefix) {
			prefix = prefix[:len(prefix)-1]
		}
		if prefix == "" {
			return ""
		}
	}
	idx := strings.LastIndex(prefix, "/")
	if idx < 0 {
		return ""
	}
	return prefix[:idx+1]
}

func shortenWarpDirectoryLine(line, prefix string) string {
	line = strings.TrimSpace(line)
	if line == "" {
		return ""
	}
	if prefix != "" && strings.HasPrefix(line, prefix) {
		trimmed := strings.TrimPrefix(line, prefix)
		if trimmed == "" {
			return "./"
		}
		return "./" + trimmed
	}
	return line
}

func shouldSummarizeWarpDirectoryTopLevel(lines []string) bool {
	if len(lines) <= 10 {
		return false
	}
	nested := 0
	for _, line := range lines {
		trimmed := strings.TrimPrefix(strings.TrimSpace(line), "./")
		if trimmed == "" {
			continue
		}
		if strings.Contains(trimmed, "/") {
			nested++
		}
	}
	return nested*100/len(lines) >= 60
}

func summarizeWarpDirectoryTopLevel(lines []string, historyMode bool) ([]string, int, int) {
	type rootSummary struct {
		label   string
		samples []string
	}

	maxRoots := 8
	maxSamples := 0
	if historyMode {
		maxRoots = 5
		maxSamples = 1
	}

	order := make([]string, 0, len(lines))
	roots := make(map[string]*rootSummary)
	for _, line := range lines {
		trimmed := strings.TrimPrefix(strings.TrimSpace(line), "./")
		if trimmed == "" {
			continue
		}
		parts := strings.Split(trimmed, "/")
		key := "./" + parts[0]
		sample := ""
		if len(parts) > 1 {
			key += "/"
			sample = strings.Join(parts[1:], "/")
		}
		summary, ok := roots[key]
		if !ok {
			summary = &rootSummary{label: key}
			roots[key] = summary
			order = append(order, key)
		}
		if sample != "" && len(summary.samples) < maxSamples && !containsWarpString(summary.samples, sample) {
			summary.samples = append(summary.samples, sample)
		}
	}

	out := make([]string, 0, minWarpInt(len(order), maxRoots))
	omitted := 0
	for idx, key := range order {
		if idx >= maxRoots {
			omitted++
			continue
		}
		summary := roots[key]
		if len(summary.samples) == 0 {
			out = append(out, summary.label)
			continue
		}
		out = append(out, fmt.Sprintf("%s (sample: %s)", summary.label, strings.Join(summary.samples, ", ")))
	}
	return out, minWarpInt(len(order), maxRoots), omitted
}

func compactCurrentWarpToolResult(text string) string {
	return compactWarpToolResultLines(text, warpCurrentToolResultLimit, 10, 3)
}

func compactHistoricalWarpToolResult(text string) string {
	return compactWarpToolResultLines(text, warpHistoryToolResultLimit, 6, 2)
}

func compactWarpToolResultLines(text string, maxChars int, head, tail int) string {
	text = strings.TrimSpace(text)
	if text == "" {
		return ""
	}
	lines := nonEmptyWarpLines(text)
	if len(lines) == 0 {
		return truncateWarpResultWithEllipsis(text, maxChars)
	}
	if len(lines) > head+tail+1 {
		compacted := append([]string{}, lines[:head]...)
		compacted = append(compacted, fmt.Sprintf("[tool_result summary: omitted %d middle lines]", len(lines)-head-tail))
		compacted = append(compacted, lines[len(lines)-tail:]...)
		return truncateWarpResultWithEllipsis(strings.Join(compacted, "\n"), maxChars)
	}
	if warpResultRuneLen(text) > maxChars {
		return truncateWarpResultWithEllipsis(compactWarpResultText(text, maxChars-32), maxChars)
	}
	return strings.Join(lines, "\n")
}

func nonEmptyWarpLines(text string) []string {
	rawLines := strings.Split(text, "\n")
	lines := make([]string, 0, len(rawLines))
	for _, raw := range rawLines {
		line := strings.TrimSpace(raw)
		if line == "" {
			continue
		}
		lines = append(lines, line)
	}
	return lines
}

func containsWarpString(items []string, target string) bool {
	for _, item := range items {
		if item == target {
			return true
		}
	}
	return false
}

func minWarpInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func compactWarpResultText(text string, targetChars int) string {
	text = strings.TrimSpace(text)
	if text == "" {
		return ""
	}
	if targetChars <= 0 || warpResultRuneLen(text) <= targetChars {
		return text
	}

	lines := strings.Split(text, "\n")
	keywords := []string{
		"error", "failed", "todo", "fix", "bug", "constraint", "must", "important",
		"错误", "失败", "修复", "约束", "必须", "结论", "决定", "下一步", "风险",
		"tool", "read", "write", "edit", "bash", "path", "file",
	}

	selected := make([]string, 0, 6)
	seen := make(map[string]struct{})
	add := func(line string) {
		line = strings.TrimSpace(line)
		if line == "" {
			return
		}
		line = strings.Join(strings.Fields(line), " ")
		line = truncateWarpResultWithEllipsis(line, 180)
		if _, ok := seen[line]; ok {
			return
		}
		seen[line] = struct{}{}
		selected = append(selected, line)
	}

	for _, line := range lines {
		lower := strings.ToLower(line)
		for _, kw := range keywords {
			if strings.Contains(lower, kw) {
				add(line)
				break
			}
		}
		if len(selected) >= 6 {
			break
		}
	}
	for _, line := range lines {
		if len(selected) >= 6 {
			break
		}
		add(line)
	}
	if len(lines) > 0 {
		add(lines[len(lines)-1])
	}
	if len(selected) == 0 {
		return truncateWarpResultWithEllipsis(text, targetChars)
	}
	joined := strings.Join(selected, " | ")
	joined = truncateWarpResultWithEllipsis(joined, targetChars-32)
	return fmt.Sprintf("[compressed %d chars] %s", warpResultRuneLen(text), joined)
}

func warpResultRuneLen(text string) int {
	return len([]rune(text))
}

func truncateWarpResultWithEllipsis(text string, maxLen int) string {
	if maxLen <= 0 {
		return ""
	}
	runes := []rune(text)
	if len(runes) <= maxLen {
		return text
	}
	return string(runes[:maxLen]) + "…[truncated]"
}
