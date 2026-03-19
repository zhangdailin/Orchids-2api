package handler

import (
	"fmt"
	"net/http"
	"regexp"
	"sort"
	"strings"
	"unicode"

	"orchids-api/internal/prompt"
	"orchids-api/internal/util"
)

var explicitEnvWorkdirRegex = regexp.MustCompile(`(?im)^\s*(?:cwd|working directory)\s*:\s*([^\n\r]+)\s*$`)
var isolatedPrimaryEnvWorkdirRegex = regexp.MustCompile(`(?im)^\s*primary\s+working\s+directory\s*:\s*([^\n\r]+)\s*$`)
var primaryEnvWorkdirRegex = regexp.MustCompile(`(?im)^\s*(?:[-*]\s*)?primary\s+working\s+directory\s*:\s*([^\n\r]+)\s*$`)

func extractWorkdirFromSystem(system []prompt.SystemItem) string {
	for _, item := range system {
		if item.Type == "text" {
			text := strings.TrimSpace(item.Text)
			if text == "" {
				continue
			}
			if matches := explicitEnvWorkdirRegex.FindStringSubmatch(text); len(matches) > 1 {
				return strings.TrimSpace(matches[1])
			}
			if looksLikeClaudeEnvironmentBlock(text) {
				if wd := extractWorkdirFromEnvironmentText(text); wd != "" {
					return wd
				}
				continue
			}
			if matches := isolatedPrimaryEnvWorkdirRegex.FindStringSubmatch(text); len(matches) > 1 && countNonEmptyLines(text) <= 2 {
				return strings.TrimSpace(matches[1])
			}
		}
	}
	return ""
}

func extractWorkdirFromMessages(messages []prompt.Message) string {
	for i := len(messages) - 1; i >= 0; i-- {
		msg := messages[i]
		if msg.Content.IsString() {
			if wd := extractWorkdirFromEnvironmentText(msg.Content.GetText()); wd != "" {
				return wd
			}
			continue
		}
		for _, block := range msg.Content.GetBlocks() {
			if block.Type != "text" {
				continue
			}
			if wd := extractWorkdirFromEnvironmentText(block.Text); wd != "" {
				return wd
			}
		}
	}
	return ""
}

func extractWorkdirFromEnvironmentText(text string) string {
	text = strings.TrimSpace(text)
	if text == "" {
		return ""
	}
	if !looksLikeClaudeEnvironmentBlock(text) {
		return ""
	}
	if matches := explicitEnvWorkdirRegex.FindStringSubmatch(text); len(matches) > 1 {
		return strings.TrimSpace(matches[1])
	}
	if matches := primaryEnvWorkdirRegex.FindStringSubmatch(text); len(matches) > 1 {
		return strings.TrimSpace(matches[1])
	}
	if matches := isolatedPrimaryEnvWorkdirRegex.FindStringSubmatch(text); len(matches) > 1 {
		return strings.TrimSpace(matches[1])
	}
	return ""
}

func extractWorkdirFromRequest(r *http.Request, req ClaudeRequest) (string, string) {
	if req.Metadata != nil {
		if wd := metadataString(req.Metadata,
			"workdir", "working_directory", "workingDirectory", "cwd",
			"workspace", "workspace_path", "workspacePath",
			"project_root", "projectRoot",
		); wd != "" {
			return strings.TrimSpace(wd), "metadata"
		}
	}

	if wd := headerValue(r,
		"X-Workdir", "X-Working-Directory", "X-Cwd", "X-Workspace", "X-Project-Root",
	); wd != "" {
		return strings.TrimSpace(wd), "header"
	}

	if wd := extractWorkdirFromSystem(req.System); wd != "" {
		return strings.TrimSpace(wd), "system"
	}

	if wd := extractWorkdirFromMessages(req.Messages); wd != "" {
		return strings.TrimSpace(wd), "messages"
	}

	return "", ""
}

func channelFromPath(path string) string {
	if strings.HasPrefix(path, "/orchids/") {
		return "orchids"
	}
	if strings.HasPrefix(path, "/warp/") {
		return "warp"
	}
	if strings.HasPrefix(path, "/bolt/") {
		return "bolt"
	}
	if strings.HasPrefix(path, "/puter/") {
		return "puter"
	}
	if strings.HasPrefix(path, "/grok/v1/") {
		return "grok"
	}
	return ""
}

// mapModel 根据请求的 model 名称映射到 orchids 上游实际支持的模型
// 以当前 Orchids 公共模型为准（会随上游更新）：claude-sonnet-4-6 / claude-opus-4.6 / claude-haiku-4-5 等。
func mapModel(requestModel string) string {
	normalized := normalizeOrchidsModelKey(requestModel)
	if normalized == "" {
		return "claude-sonnet-4-6"
	}
	if mapped, ok := orchidsModelMap[normalized]; ok {
		return mapped
	}
	return "claude-sonnet-4-6"
}

func normalizeOrchidsModelKey(model string) string {
	normalized := strings.ToLower(strings.TrimSpace(model))
	if strings.HasPrefix(normalized, "claude-") {
		normalized = strings.ReplaceAll(normalized, "4.6", "4-6")
		normalized = strings.ReplaceAll(normalized, "4.5", "4-5")
	}
	return normalized
}

var orchidsModelMap = map[string]string{
	"claude-sonnet-4-5":          "claude-sonnet-4-6",
	"claude-sonnet-4-6":          "claude-sonnet-4-6",
	"claude-sonnet-4-5-thinking": "claude-sonnet-4-5-thinking",
	"claude-sonnet-4-6-thinking": "claude-sonnet-4-6",
	"claude-opus-4-6":            "claude-opus-4-6",
	"claude-opus-4-5":            "claude-opus-4-6",
	"claude-opus-4-5-thinking":   "claude-opus-4-5-thinking",
	"claude-opus-4-6-thinking":   "claude-opus-4-6",
	"claude-haiku-4-5":           "claude-haiku-4-5",
	"claude-sonnet-4-20250514":   "claude-sonnet-4-20250514",
	"claude-3-7-sonnet-20250219": "claude-3-7-sonnet-20250219",
	"gemini-3-flash":             "gemini-3-flash",
	"gemini-3-pro":               "gemini-3-pro",
	"gpt-5.3-codex":              "gpt-5.3-codex",
	"gpt-5.2-codex":              "gpt-5.2-codex",
	"gpt-5.2":                    "gpt-5.2",
	"grok-4.1-fast":              "grok-4.1-fast",
	"glm-5":                      "glm-5",
	"kimi-k2.5":                  "kimi-k2.5",
}

func conversationKeyForRequest(r *http.Request, req ClaudeRequest) string {
	if req.ConversationID != "" {
		return req.ConversationID
	}
	if req.Metadata != nil {
		if key := metadataString(req.Metadata, "conversation_id", "conversationId", "session_id", "sessionId", "thread_id", "threadId", "chat_id", "chatId"); key != "" {
			return key
		}
	}
	if key := headerValue(r, "X-Conversation-Id", "X-Session-Id", "X-Thread-Id", "X-Chat-Id"); key != "" {
		return key
	}
	if req.Metadata != nil {
		if key := metadataString(req.Metadata, "user_id", "userId"); key != "" {
			return key
		}
	}
	return ""
}

func metadataString(metadata map[string]interface{}, keys ...string) string {
	for _, key := range keys {
		if value, ok := metadata[key]; ok {
			if str, ok := value.(string); ok {
				str = strings.TrimSpace(str)
				if str != "" {
					return str
				}
			}
		}
	}
	return ""
}

func headerValue(r *http.Request, keys ...string) string {
	for _, key := range keys {
		if value := strings.TrimSpace(r.Header.Get(key)); value != "" {
			return value
		}
	}
	return ""
}

func extractUserText(messages []prompt.Message) string {
	for i := len(messages) - 1; i >= 0; i-- {
		msg := messages[i]
		if msg.Role == "user" {
			if text := msg.ExtractText(); text != "" {
				return text
			}
		}
	}
	return ""
}

func lastUserIsToolResultFollowup(messages []prompt.Message) bool {
	for i := len(messages) - 1; i >= 0; i-- {
		msg := messages[i]
		if msg.Role != "user" {
			continue
		}
		if msg.Content.IsString() {
			return false
		}
		blocks := msg.Content.GetBlocks()
		hasToolResult := false
		for _, block := range blocks {
			switch block.Type {
			case "tool_result":
				hasToolResult = true
			case "text":
				continue
			default:
				if strings.TrimSpace(block.Type) != "" {
					return false
				}
			}
		}
		return hasToolResult
	}
	return false
}

func shouldKeepToolsForWarpToolResultFollowup(messages []prompt.Message) bool {
	if !lastUserIsToolResultFollowup(messages) {
		return false
	}
	original := lastNonToolResultUserText(messages)
	if original == "" {
		return false
	}
	toolResult := lastToolResultText(messages)
	if looksLikeToolResultFailure(toolResult) {
		return shouldKeepToolsForRecoverableWarpToolFailure(messages, original)
	}

	if looksLikeOptimizationRequest(original) {
		if looksLikeExploratoryAssistantPreface(lastAssistantText(messages)) {
			return true
		}
		if looksLikeWarpExplorationSeed(toolResult) {
			return true
		}
		return !hasSufficientOptimizationEvidence(messages, explicitlyRequestsDeepAnalysis(original))
	}

	if looksLikeTechStackRequest(original) ||
		looksLikeProjectPurposeRequest(original) ||
		looksLikeBackendImplementationRequest(original) ||
		looksLikeWebImplementationRequest(original) ||
		looksLikeDataLayerRequest(original) ||
		looksLikeTestingRequest(original) ||
		(looksLikeDeploymentRequest(original) && !looksLikeReleaseRiskRequest(original)) ||
		looksLikeSecurityRiskRequest(original) ||
		looksLikePermissionRiskRequest(original) ||
		looksLikeDependencyRiskRequest(original) {
		return false
	}
	if !looksLikeWarpExplorationSeed(toolResult) {
		return false
	}
	return looksLikeWarpExploratoryRequest(original)
}

func shouldKeepToolsForRecoverableWarpToolFailure(messages []prompt.Message, original string) bool {
	if !looksLikeWarpExploratoryRequest(original) {
		return false
	}

	malformedReads := countMalformedReadToolUsesInLatestAssistant(messages)
	if malformedReads == 0 {
		return false
	}

	totalResults, failedResults := latestToolResultTurnFailureCount(messages)
	if totalResults == 0 || failedResults == 0 || failedResults != totalResults {
		return false
	}
	if failedResults < malformedReads {
		return false
	}

	for _, item := range collectSuccessfulToolResultEvidence(messages) {
		if looksLikeWarpExplorationSeed(item.Content) || looksLikeImplementationReadEvidence(item) {
			return true
		}
	}
	return false
}

type toolResultEvidence struct {
	ToolName string
	FilePath string
	Command  string
	Content  string
}

func hasSufficientOptimizationEvidence(messages []prompt.Message, allowDeeperExploration bool) bool {
	evidence := collectSuccessfulToolResultEvidence(messages)
	if len(evidence) == 0 {
		return false
	}

	corpus := buildToolResultFallbackCorpus(messages)
	signals := inspectTechStackSignals(corpus)
	hasSignals := !signals.isEmpty()

	hasDirectoryOverview := false
	hasReadme := false
	hasDependencyManifest := false
	hasCoreFile := false
	implementationReadPaths := make(map[string]struct{})
	readCounts := make(map[string]int)
	totalReadResults := 0

	for _, item := range evidence {
		if looksLikeWarpExplorationSeed(item.Content) {
			hasDirectoryOverview = true
		}
		if !strings.EqualFold(strings.TrimSpace(item.ToolName), "Read") {
			continue
		}

		totalReadResults++
		pathKey := normalizeToolInputPath(item.FilePath)
		if pathKey == "" {
			pathKey = fmt.Sprintf("unnamed-read-%d", totalReadResults)
		}
		readCounts[pathKey]++
		hasReadme = hasReadme || looksLikeReadmePath(pathKey)
		hasDependencyManifest = hasDependencyManifest || looksLikeDependencyManifestPath(pathKey)
		hasCoreFile = hasCoreFile || looksLikeCoreImplementationPath(pathKey)
		if looksLikeImplementationReadEvidence(item) {
			implementationReadPaths[pathKey] = struct{}{}
		}
	}

	uniqueReadCount := len(readCounts)
	if uniqueReadCount == 0 {
		return false
	}
	uniqueImplementationReadCount := len(implementationReadPaths)

	latestRepeatedRead := false
	last := evidence[len(evidence)-1]
	if strings.EqualFold(strings.TrimSpace(last.ToolName), "Read") {
		if lastPath := normalizeToolInputPath(last.FilePath); lastPath != "" && readCounts[lastPath] > 1 {
			latestRepeatedRead = true
		}
	}

	if allowDeeperExploration {
		if latestRepeatedRead && uniqueImplementationReadCount >= 2 && hasCoreFile && hasSignals {
			return true
		}
		if hasReadme && hasDependencyManifest && hasCoreFile && uniqueImplementationReadCount >= 2 {
			return true
		}
		if hasDirectoryOverview && uniqueImplementationReadCount >= 3 && hasSignals {
			return true
		}
		return uniqueImplementationReadCount >= 3 && hasSignals
	}

	if latestRepeatedRead && uniqueImplementationReadCount >= 2 && hasCoreFile && hasSignals &&
		(hasReadme || hasDependencyManifest || hasDirectoryOverview) {
		return true
	}
	if hasCoreFile && uniqueImplementationReadCount >= 2 && hasSignals &&
		(hasReadme || hasDependencyManifest) {
		return true
	}
	return uniqueImplementationReadCount >= 2 && hasSignals &&
		(hasReadme || hasDependencyManifest || hasDirectoryOverview)
}

func collectSuccessfulToolResultEvidence(messages []prompt.Message) []toolResultEvidence {
	toolUses := make(map[string]toolResultEvidence)
	var evidence []toolResultEvidence

	for _, msg := range messages {
		role := strings.TrimSpace(msg.Role)
		if strings.EqualFold(role, "assistant") && !msg.Content.IsString() {
			for _, block := range msg.Content.GetBlocks() {
				if block.Type != "tool_use" {
					continue
				}
				toolUses[block.ID] = toolResultEvidence{
					ToolName: strings.TrimSpace(block.Name),
					FilePath: extractToolUseInputString(block.Input, "file_path", "path"),
					Command:  extractToolUseInputString(block.Input, "command", "cmd"),
				}
			}
			continue
		}

		if !strings.EqualFold(role, "user") || msg.Content.IsString() {
			continue
		}
		for _, block := range msg.Content.GetBlocks() {
			if block.Type != "tool_result" {
				continue
			}
			text := strings.TrimSpace(extractToolResultContent(block.Content))
			if text == "" || looksLikeToolResultFailure(text) {
				continue
			}
			item := toolUses[block.ToolUseID]
			item.Content = text
			evidence = append(evidence, item)
		}
	}

	return evidence
}

func countMalformedReadToolUsesInLatestAssistant(messages []prompt.Message) int {
	for i := len(messages) - 1; i >= 0; i-- {
		msg := messages[i]
		if !strings.EqualFold(strings.TrimSpace(msg.Role), "assistant") || msg.Content.IsString() {
			continue
		}
		count := 0
		for _, block := range msg.Content.GetBlocks() {
			if block.Type != "tool_use" || !strings.EqualFold(strings.TrimSpace(block.Name), "Read") {
				continue
			}
			path := extractToolUseInputString(block.Input, "file_path", "path")
			if isRecoverableMalformedReadPath(path) {
				count++
			}
		}
		return count
	}
	return 0
}

func countProjectDriftToolUsesInLatestAssistant(messages []prompt.Message) int {
	for i := len(messages) - 1; i >= 0; i-- {
		msg := messages[i]
		if !strings.EqualFold(strings.TrimSpace(msg.Role), "assistant") || msg.Content.IsString() {
			continue
		}
		count := 0
		for _, block := range msg.Content.GetBlocks() {
			if block.Type != "tool_use" {
				continue
			}
			name := strings.ToLower(strings.TrimSpace(block.Name))
			switch name {
			case "read", "write", "edit", "glob", "grep":
				path := extractToolUseInputString(block.Input, "file_path", "path", "directory", "dir")
				if looksLikeProjectDriftPath(path) {
					count++
				}
			case "bash":
				command := extractToolUseInputString(block.Input, "command", "cmd")
				if looksLikeProjectDriftCommand(command) {
					count++
				}
			}
		}
		return count
	}
	return 0
}

func latestToolResultTurnFailureCount(messages []prompt.Message) (total int, failures int) {
	for i := len(messages) - 1; i >= 0; i-- {
		msg := messages[i]
		if !strings.EqualFold(strings.TrimSpace(msg.Role), "user") || msg.Content.IsString() {
			continue
		}
		for _, block := range msg.Content.GetBlocks() {
			if block.Type != "tool_result" {
				continue
			}
			total++
			if looksLikeToolResultFailure(extractToolResultContent(block.Content)) {
				failures++
			}
		}
		if total > 0 {
			return total, failures
		}
	}
	return 0, 0
}

func isRecoverableMalformedReadPath(path string) bool {
	recovered := recoverMalformedReadPath(path)
	return recovered != "" && recovered != strings.TrimSpace(path)
}

func looksLikeProjectDriftPath(path string) bool {
	path = strings.TrimSpace(strings.Trim(path, "\"'"))
	if path == "" {
		return false
	}
	if isRecoverableMalformedReadPath(path) {
		return true
	}
	lower := strings.ToLower(strings.ReplaceAll(path, "\\", "/"))
	for _, marker := range []string{
		"/tmp/cc-agent/",
		"/home/user/app",
		"/mnt/",
	} {
		if strings.Contains(lower, marker) {
			return true
		}
	}
	return false
}

func looksLikeProjectDriftCommand(command string) bool {
	command = strings.TrimSpace(command)
	if command == "" {
		return false
	}
	lower := strings.ToLower(command)
	for _, marker := range []string{
		"/tmp/cc-agent/",
		"/home/user/app",
		"/mnt/",
		"cannot access windows path",
		"~/",
	} {
		if strings.Contains(lower, marker) {
			return true
		}
	}
	return findWindowsToolPathStart(command) >= 0
}

func recoverMalformedReadPath(path string) string {
	path = strings.TrimSpace(strings.Trim(path, "\"'"))
	if path == "" || looksLikeNormalToolPath(path) {
		return ""
	}
	if idx := strings.Index(path, "/"); idx > 0 {
		candidate := strings.TrimSpace(path[idx:])
		if looksLikeNormalToolPath(candidate) {
			return candidate
		}
	}
	if idx := findWindowsToolPathStart(path); idx > 0 {
		candidate := strings.TrimSpace(path[idx:])
		if looksLikeNormalToolPath(candidate) {
			return candidate
		}
	}
	return ""
}

func looksLikeNormalToolPath(path string) bool {
	path = strings.TrimSpace(path)
	if path == "" {
		return false
	}
	if strings.HasPrefix(path, "/") || strings.HasPrefix(path, "./") || strings.HasPrefix(path, "../") {
		return true
	}
	return findWindowsToolPathStart(path) == 0
}

func findWindowsToolPathStart(s string) int {
	for i := 0; i+2 < len(s); i++ {
		ch := s[i]
		if ((ch >= 'A' && ch <= 'Z') || (ch >= 'a' && ch <= 'z')) && s[i+1] == ':' && (s[i+2] == '\\' || s[i+2] == '/') {
			return i
		}
	}
	return -1
}

func buildToolResultFallbackCorpus(messages []prompt.Message) string {
	evidence := collectSuccessfulToolResultEvidence(messages)
	if len(evidence) == 0 {
		return ""
	}

	const maxEntries = 6
	const maxChars = 12000

	seen := make(map[string]struct{}, len(evidence))
	parts := make([]string, 0, len(evidence))
	totalChars := 0

	for _, item := range evidence {
		text := strings.TrimSpace(item.Content)
		if text == "" {
			continue
		}
		key := strings.ToLower(text)
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}

		part := text
		if base := toolInputBaseName(item.FilePath); base != "" && !strings.Contains(strings.ToLower(text), base) {
			part = base + "\n" + text
		}
		if totalChars+len(part) > maxChars {
			break
		}
		parts = append(parts, part)
		totalChars += len(part)
		if len(parts) >= maxEntries {
			break
		}
	}

	return strings.TrimSpace(strings.Join(parts, "\n\n"))
}

func extractToolUseInputString(input interface{}, keys ...string) string {
	switch v := input.(type) {
	case map[string]interface{}:
		for _, key := range keys {
			if raw, ok := v[key]; ok {
				if text, ok := raw.(string); ok {
					return strings.TrimSpace(text)
				}
			}
		}
	case map[string]string:
		for _, key := range keys {
			if text := strings.TrimSpace(v[key]); text != "" {
				return text
			}
		}
	}
	return ""
}

func normalizeToolInputPath(path string) string {
	path = strings.TrimSpace(strings.ReplaceAll(path, "\\", "/"))
	path = strings.TrimSuffix(path, "/")
	return strings.ToLower(path)
}

func toolInputBaseName(path string) string {
	path = normalizeToolInputPath(path)
	if path == "" {
		return ""
	}
	if idx := strings.LastIndex(path, "/"); idx >= 0 {
		return path[idx+1:]
	}
	return path
}

func looksLikeReadmePath(path string) bool {
	base := toolInputBaseName(path)
	return strings.HasPrefix(base, "readme")
}

func looksLikeDependencyManifestPath(path string) bool {
	switch toolInputBaseName(path) {
	case "requirements.txt", "pyproject.toml", "poetry.lock", "pipfile", "pipfile.lock",
		"package.json", "package-lock.json", "pnpm-lock.yaml", "yarn.lock",
		"go.mod", "go.sum", "cargo.toml", "cargo.lock", "composer.json", "gemfile":
		return true
	default:
		return false
	}
}

func looksLikeCoreImplementationPath(path string) bool {
	switch toolInputBaseName(path) {
	case "main.py", "app.py", "api.py", "server.py", "dashboard.py",
		"main.go", "server.go", "main.ts", "index.ts", "index.tsx", "index.js", "index.jsx":
		return true
	default:
		return false
	}
}

func looksLikeImplementationReadEvidence(item toolResultEvidence) bool {
	if !strings.EqualFold(strings.TrimSpace(item.ToolName), "Read") {
		return false
	}
	if looksLikeCoreImplementationPath(item.FilePath) || looksLikeSourceFilePath(item.FilePath) {
		return true
	}
	return looksLikeSourceLikeContent(item.Content)
}

func looksLikeSourceFilePath(path string) bool {
	switch {
	case strings.HasSuffix(toolInputBaseName(path), ".py"),
		strings.HasSuffix(toolInputBaseName(path), ".go"),
		strings.HasSuffix(toolInputBaseName(path), ".js"),
		strings.HasSuffix(toolInputBaseName(path), ".jsx"),
		strings.HasSuffix(toolInputBaseName(path), ".ts"),
		strings.HasSuffix(toolInputBaseName(path), ".tsx"),
		strings.HasSuffix(toolInputBaseName(path), ".java"),
		strings.HasSuffix(toolInputBaseName(path), ".kt"),
		strings.HasSuffix(toolInputBaseName(path), ".rs"),
		strings.HasSuffix(toolInputBaseName(path), ".rb"),
		strings.HasSuffix(toolInputBaseName(path), ".php"),
		strings.HasSuffix(toolInputBaseName(path), ".cs"),
		strings.HasSuffix(toolInputBaseName(path), ".cpp"),
		strings.HasSuffix(toolInputBaseName(path), ".c"),
		strings.HasSuffix(toolInputBaseName(path), ".h"):
		return true
	default:
		return false
	}
}

func looksLikeSourceLikeContent(text string) bool {
	lower := strings.ToLower(text)
	for _, marker := range []string{
		"\ndef ", "\nclass ", "\nfunc ", "\nconst ", "\nlet ", "\nvar ",
		"import ", "from ", "return ", "if __name__ ==",
		"app = fastapi()", "router =",
	} {
		if strings.Contains(lower, marker) {
			return true
		}
	}
	return false
}

func hasImplementationReadEvidence(messages []prompt.Message) bool {
	return countImplementationReadEvidence(messages) > 0
}

func countImplementationReadEvidence(messages []prompt.Message) int {
	count := 0
	for _, item := range collectSuccessfulToolResultEvidence(messages) {
		if looksLikeImplementationReadEvidence(item) {
			count++
		}
	}
	return count
}

func countSuccessfulWarpFileToolResults(messages []prompt.Message) int {
	count := 0
	for _, msg := range messages {
		if !strings.EqualFold(strings.TrimSpace(msg.Role), "user") {
			continue
		}
		if msg.Content.IsString() {
			continue
		}
		for _, block := range msg.Content.GetBlocks() {
			if block.Type != "tool_result" {
				continue
			}
			text := strings.TrimSpace(extractToolResultContent(block.Content))
			if text == "" || looksLikeToolResultFailure(text) || looksLikeWarpExplorationSeed(text) {
				continue
			}
			count++
		}
	}
	return count
}

func buildToolResultNoToolsFallback(messages []prompt.Message) string {
	original := strings.TrimSpace(lastNonToolResultUserText(messages))
	latestToolResult := strings.TrimSpace(lastToolResultText(messages))
	toolResult, recoveredFromFailure := selectToolResultForFallback(messages)
	if toolResult == "" {
		toolResult = latestToolResult
	}
	preferChinese := containsHan(original) || containsHan(latestToolResult) || containsHan(toolResult)

	finalize := func(answer string) string {
		answer = strings.TrimSpace(answer)
		if answer == "" {
			return ""
		}
		if recoveredFromFailure && looksLikeToolResultFailure(latestToolResult) {
			if preferChinese {
				return "刚才那次读取已经偏到当前项目之外的路径；" + answer
			}
			return "The previous read drifted outside the current project; " + answer
		}
		return answer
	}

	optimizationRequest := looksLikeOptimizationRequest(original)
	deepAnalysisRequest := explicitlyRequestsDeepAnalysis(original)
	if deepAnalysisRequest && !optimizationRequest {
		return ""
	}

	if looksLikeSecurityRiskRequest(original) {
		if answer := buildLocalSecurityRiskFallback(toolResult, preferChinese); answer != "" {
			return finalize(answer)
		}
	}
	if looksLikePermissionRiskRequest(original) {
		if answer := buildLocalPermissionRiskFallback(toolResult, preferChinese); answer != "" {
			return finalize(answer)
		}
	}
	if looksLikeDependencyRiskRequest(original) {
		if answer := buildLocalDependencyRiskFallback(toolResult, preferChinese); answer != "" {
			return finalize(answer)
		}
	}
	if looksLikeConfigRiskRequest(original) {
		if answer := buildLocalConfigRiskFallback(toolResult, preferChinese); answer != "" {
			return finalize(answer)
		}
	}
	if looksLikeObservabilityGapRequest(original) {
		if answer := buildLocalObservabilityGapFallback(toolResult, preferChinese); answer != "" {
			return finalize(answer)
		}
	}
	if looksLikeReleaseRiskRequest(original) {
		if answer := buildLocalReleaseRiskFallback(toolResult, preferChinese); answer != "" {
			return finalize(answer)
		}
	}
	if looksLikeCompatibilityRiskRequest(original) {
		if answer := buildLocalCompatibilityRiskFallback(toolResult, preferChinese); answer != "" {
			return finalize(answer)
		}
	}
	if looksLikeOperationalRiskRequest(original) {
		if answer := buildLocalOperationalRiskFallback(toolResult, preferChinese); answer != "" {
			return finalize(answer)
		}
	}
	if looksLikeRecoveryRollbackRiskRequest(original) {
		if answer := buildLocalRecoveryRollbackRiskFallback(toolResult, preferChinese); answer != "" {
			return finalize(answer)
		}
	}
	if looksLikePerformanceBottleneckRequest(original) {
		if answer := buildLocalPerformanceBottleneckFallback(toolResult, preferChinese); answer != "" {
			return finalize(answer)
		}
	}
	if looksLikeCodeSmellRequest(original) {
		if answer := buildLocalCodeSmellFallback(toolResult, preferChinese); answer != "" {
			return finalize(answer)
		}
	}
	if looksLikeMaintainabilityRiskRequest(original) {
		if answer := buildLocalMaintainabilityRiskFallback(toolResult, preferChinese); answer != "" {
			return finalize(answer)
		}
	}
	if looksLikeTechStackRequest(original) {
		if answer := buildLocalTechStackFallback(original, toolResult, preferChinese); answer != "" {
			return finalize(answer)
		}
	}
	if looksLikeProjectPurposeRequest(original) {
		if answer := buildLocalProjectPurposeFallback(toolResult, preferChinese); answer != "" {
			return finalize(answer)
		}
	}
	if looksLikeBackendImplementationRequest(original) {
		if answer := buildLocalBackendImplementationFallback(toolResult, preferChinese); answer != "" {
			return finalize(answer)
		}
	}
	if looksLikeDataLayerRequest(original) {
		if answer := buildLocalDataLayerFallback(toolResult, preferChinese); answer != "" {
			return finalize(answer)
		}
	}
	if looksLikeTestingRequest(original) {
		if answer := buildLocalTestingFallback(toolResult, preferChinese); answer != "" {
			return finalize(answer)
		}
	}
	if looksLikeDeploymentRequest(original) {
		if answer := buildLocalDeploymentFallback(toolResult, preferChinese); answer != "" {
			return finalize(answer)
		}
	}
	if optimizationRequest {
		optimizationReady := hasSufficientOptimizationEvidence(messages, deepAnalysisRequest)
		if optimizationReady {
			if answer := buildProjectSpecificOptimizationFallback(messages, preferChinese); answer != "" {
				return finalize(answer)
			}
			optimizationContext := toolResult
			if corpus := buildToolResultFallbackCorpus(messages); corpus != "" {
				optimizationContext = corpus
			}
			if answer := buildLocalOptimizationFallback(optimizationContext, preferChinese); answer != "" {
				return finalize(answer)
			}
		}
		implementationReads := countImplementationReadEvidence(messages)
		if implementationReads == 0 {
			if preferChinese {
				return finalize("当前只读到了目录、README 或依赖清单，还没读到核心源码。请继续查看主入口或关键实现文件（例如 api.py、monitor_trump.py、dashboard.py）后再给优化建议。")
			}
			return finalize("So far I only have the directory, README, or dependency manifest, but not the core source files yet. Read the entrypoint or key implementation files first (for example api.py, monitor_trump.py, or dashboard.py) before giving optimization advice.")
		}
		if !optimizationReady {
			if preferChinese {
				return finalize("当前只读到了少量核心源码，还不足以给出可靠的项目级优化建议。请继续查看其他关键实现文件，例如主流程、共享工具层、后台任务或 dashboard 相关代码，然后再收口分析。")
			}
			return finalize("So far I only have a small slice of the core source, which is not enough for reliable project-wide optimization advice. Continue reading other key implementation files such as the main workflow, shared utilities, background tasks, or dashboard code before closing with recommendations.")
		}
		if answer := buildLocalOptimizationFallback(toolResult, preferChinese); answer != "" {
			return finalize(answer)
		}
	}
	if looksLikeWebImplementationRequest(original) {
		if answer := buildLocalWebImplementationFallback(toolResult, preferChinese); answer != "" {
			return finalize(answer)
		}
	}

	if looksLikeWarpExplorationSeed(toolResult) {
		if looksLikeWarpExploratoryRequest(original) {
			if preferChinese {
				return "当前只拿到目录概览，还不足以直接给出项目分析或优化建议。请明确指定要分析的项目目录，或让我查看 README、主入口文件或关键配置。"
			}
			return "I only have a directory overview so far, which is not enough for a solid analysis or optimization pass. Specify the project directory, or let me inspect the README, entrypoint, or key config files."
		}
		if preferChinese {
			return "当前只拿到目录概览，还不足以继续判断。请直接说明要分析的目录或文件。"
		}
		return "I only have a directory overview so far. Please specify the directory or file you want me to analyze."
	}

	if looksLikeToolResultFailure(latestToolResult) {
		if preferChinese {
			return "刚才那次读取已经偏到当前项目之外的路径，当前文件结论不可靠。请让我继续查看当前项目的 README、依赖清单或主入口文件后再分析。"
		}
		return "The previous read drifted outside the current project, so that file result is not reliable. Let me inspect the current project's README, dependency manifest, or entrypoint next."
	}

	if preferChinese {
		return "当前这轮没有得到可用的文本结论。请明确指定要查看的文件或目录后重试。"
	}
	return "This round did not produce a usable text answer. Please specify the file or directory you want me to inspect and try again."
}

func selectToolResultForFallback(messages []prompt.Message) (string, bool) {
	var latest string
	for i := len(messages) - 1; i >= 0; i-- {
		msg := messages[i]
		if !strings.EqualFold(strings.TrimSpace(msg.Role), "user") || msg.Content.IsString() {
			continue
		}
		for _, block := range msg.Content.GetBlocks() {
			if block.Type != "tool_result" {
				continue
			}
			text := strings.TrimSpace(extractToolResultContent(block.Content))
			if text == "" {
				continue
			}
			if latest == "" {
				latest = text
				if !looksLikeToolResultFailure(text) {
					return text, false
				}
				continue
			}
			if !looksLikeToolResultFailure(text) {
				return text, true
			}
		}
	}
	return latest, false
}

func extractToolResultContent(content interface{}) string {
	switch v := content.(type) {
	case string:
		return util.NormalizePersistedToolResultText(v)
	case []interface{}:
		var parts []string
		for _, item := range v {
			if s, ok := item.(string); ok {
				s = util.NormalizePersistedToolResultText(s)
				if s != "" {
					parts = append(parts, s)
				}
			}
		}
		return util.NormalizePersistedToolResultText(strings.Join(parts, "\n"))
	default:
		return ""
	}
}

func lastNonToolResultUserText(messages []prompt.Message) string {
	for i := len(messages) - 1; i >= 0; i-- {
		msg := messages[i]
		if !strings.EqualFold(strings.TrimSpace(msg.Role), "user") {
			continue
		}
		if msg.Content.IsString() {
			text := strings.TrimSpace(stripSystemRemindersForMode(msg.Content.GetText()))
			if text != "" && !containsSuggestionMode(text) {
				return text
			}
			continue
		}
		blocks := msg.Content.GetBlocks()
		var parts []string
		hasToolResult := false
		for _, block := range blocks {
			switch block.Type {
			case "tool_result":
				hasToolResult = true
			case "text":
				text := strings.TrimSpace(stripSystemRemindersForMode(block.Text))
				if text != "" && !containsSuggestionMode(text) {
					parts = append(parts, text)
				}
			}
		}
		if len(parts) > 0 {
			return strings.TrimSpace(strings.Join(parts, "\n"))
		}
		if hasToolResult {
			continue
		}
	}
	return ""
}

func looksLikeWarpExploratoryRequest(text string) bool {
	lower := strings.ToLower(strings.TrimSpace(stripSystemRemindersForMode(text)))
	if lower == "" {
		return false
	}
	keywords := []string{
		"项目", "工程", "仓库", "代码", "文件", "结构", "架构",
		"优化", "改进", "重构", "分析", "查看", "检查", "排查", "修复",
		"错误", "异常", "日志", "实现", "用途", "干什么", "做什么",
		"配置", "配置文件", "观测", "监控", "指标", "追踪", "发布", "上线", "部署",
		"兼容", "兼容性", "运维", "恢复", "回滚",
		"project", "repo", "repository", "codebase", "source", "file", "files",
		"structure", "architecture", "optimize", "optimization",
		"improve", "refactor", "analyze", "analysis", "inspect", "check",
		"review", "debug", "fix", "error", "bug", "issue", "implement",
		"purpose", "what does this project do", "what is this project",
		"config", "configuration", "observability", "monitoring", "metrics", "metric", "trace", "tracing",
		"release", "rollout", "deploy", "deployment",
		"compatibility", "compatible", "ops", "operations", "operational", "rollback", "recovery",
	}
	for _, keyword := range keywords {
		if strings.Contains(lower, keyword) {
			return true
		}
	}
	return false
}

func lastToolResultText(messages []prompt.Message) string {
	for i := len(messages) - 1; i >= 0; i-- {
		msg := messages[i]
		if !strings.EqualFold(strings.TrimSpace(msg.Role), "user") {
			continue
		}
		if msg.Content.IsString() {
			continue
		}
		for _, block := range msg.Content.GetBlocks() {
			if block.Type != "tool_result" {
				continue
			}
			if text := extractToolResultContent(block.Content); text != "" {
				return text
			}
		}
	}
	return ""
}

func looksLikeToolResultFailure(text string) bool {
	lower := strings.ToLower(strings.TrimSpace(text))
	if lower == "" {
		return false
	}
	for _, marker := range []string{
		"file does not exist",
		"no such file or directory",
		"no such file",
		"cannot find the file",
		"cannot open file",
		"permission denied",
		"is a directory",
		"current working directory is ",
		"file has not been read yet",
		"read it first before writing to it",
		"old_string not found",
		"string to replace not found",
		"could not find old_string",
	} {
		if strings.Contains(lower, marker) {
			return true
		}
	}
	return false
}

func looksLikeWarpExplorationSeed(text string) bool {
	text = strings.TrimSpace(text)
	if text == "" {
		return false
	}
	lower := strings.ToLower(text)
	if strings.Contains(lower, "[directory listing summarized:") || strings.Contains(lower, "[directory listing trimmed:") {
		return true
	}
	lines := strings.Split(text, "\n")
	if len(lines) < 3 {
		return false
	}
	entryLike := 0
	considered := 0
	for _, line := range lines {
		line = normalizeToolResultLine(line)
		if line == "" {
			continue
		}
		considered++
		if looksLikeExplorationDirectoryEntry(line) {
			entryLike++
		}
	}
	if considered < 3 {
		return false
	}
	return entryLike*100/considered >= 70
}

func normalizeToolResultLine(line string) string {
	line = strings.TrimSpace(line)
	if line == "" {
		return ""
	}
	if idx := strings.Index(line, "→"); idx >= 0 {
		line = strings.TrimSpace(line[idx+len("→"):])
	}
	if entry, ok := extractEntryFromLongLsLine(line); ok {
		line = entry
	}
	return strings.TrimSpace(line)
}

func extractEntryFromLongLsLine(line string) (string, bool) {
	fields := strings.Fields(strings.TrimSpace(line))
	if len(fields) < 9 {
		return "", false
	}
	mode := fields[0]
	if len(mode) < 10 {
		return "", false
	}
	switch mode[0] {
	case '-', 'd', 'l', 'b', 'c', 'p', 's':
	default:
		return "", false
	}
	if !strings.ContainsAny(mode, "rwx-") {
		return "", false
	}
	entry := strings.Join(fields[8:], " ")
	entry = strings.TrimSpace(entry)
	if entry == "" {
		return "", false
	}
	if idx := strings.Index(entry, " -> "); idx >= 0 {
		entry = strings.TrimSpace(entry[:idx])
	}
	return entry, entry != ""
}

func looksLikeExplorationDirectoryEntry(line string) bool {
	line = strings.TrimSpace(line)
	if line == "" || len(line) > 120 {
		return false
	}
	if strings.HasPrefix(line, "/") || strings.HasPrefix(line, "./") || strings.HasPrefix(line, "../") {
		return true
	}
	if len(line) >= 3 && ((line[1] == ':' && line[2] == '\\') || (line[1] == ':' && line[2] == '/')) {
		return true
	}
	if strings.Contains(line, ": ") || strings.ContainsAny(line, "{}<>|`") {
		return false
	}
	if strings.Count(line, " ") > 1 {
		return false
	}
	lower := strings.ToLower(line)
	for _, suffix := range []string{"/", ".md", ".txt", ".py", ".go", ".js", ".ts", ".json", ".yaml", ".yml", ".toml"} {
		if strings.HasSuffix(lower, suffix) {
			return true
		}
	}
	return strings.HasPrefix(line, ".") || strings.ContainsAny(line, "/\\")
}

func isSuggestionMode(messages []prompt.Message) bool {
	for i := len(messages) - 1; i >= 0; i-- {
		msg := messages[i]
		if msg.Role == "user" {
			text := msg.ExtractText()
			if text != "" {
				return containsSuggestionMode(text)
			}
			return false
		}
	}
	return false
}

func buildLocalSuggestion(messages []prompt.Message) string {
	lastUser := lastNonSuggestionUserText(messages)
	lastAssistant := lastAssistantText(messages)
	if lastAssistant == "" {
		return ""
	}
	if !hasExplicitNextStepOffer(lastAssistant) {
		return ""
	}
	if containsHan(lastUser) || containsHan(lastAssistant) {
		return "可以"
	}
	return "go ahead"
}

func containsSuggestionMode(text string) bool {
	clean := stripSystemRemindersForMode(text)
	return strings.Contains(strings.ToLower(clean), "suggestion mode")
}

func lastNonSuggestionUserText(messages []prompt.Message) string {
	for i := len(messages) - 1; i >= 0; i-- {
		msg := messages[i]
		if !strings.EqualFold(strings.TrimSpace(msg.Role), "user") {
			continue
		}
		text := strings.TrimSpace(stripSystemRemindersForMode(msg.ExtractText()))
		if text == "" || containsSuggestionMode(text) {
			continue
		}
		return text
	}
	return ""
}

func buildToolGateMessage(messages []prompt.Message, suggestionMode bool) string {
	if suggestionMode {
		return "This is a suggestion-mode follow-up. Answer directly without calling tools or performing any file operations."
	}
	if lastUserIsToolResultFollowup(messages) {
		original := lastNonToolResultUserText(messages)
		if looksLikeOptimizationRequest(original) {
			return "Use the provided tool results to answer the user's project optimization request directly. Tool access is unavailable for this turn, and any request to read, inspect, search, or review more files will be ignored. Stay specific to the current project and available code context. Do NOT call tools, do not describe a plan, and do not say you will first analyze or review the codebase. Give the best concrete project-specific recommendations now."
		}
		return "Use the provided tool results to answer the user's follow-up directly. Tool access is unavailable for this turn, and any request to read, inspect, search, or review more files will be ignored. Stay specific to the current project and available code context. Do NOT call tools, do not describe a plan, and answer now based only on the provided results."
	}
	return "Answer directly without calling tools or performing any file operations."
}

func lastAssistantText(messages []prompt.Message) string {
	for i := len(messages) - 1; i >= 0; i-- {
		msg := messages[i]
		if !strings.EqualFold(strings.TrimSpace(msg.Role), "assistant") {
			continue
		}
		text := strings.TrimSpace(stripSystemRemindersForMode(msg.ExtractText()))
		if text != "" {
			return text
		}
	}
	return ""
}

func looksLikeExploratoryAssistantPreface(text string) bool {
	text = strings.TrimSpace(text)
	if text == "" {
		return false
	}
	if len([]rune(text)) > 260 {
		return false
	}

	lower := strings.ToLower(strings.Join(strings.Fields(text), " "))
	intro := []string{
		"let me",
		"i'll first",
		"i will first",
		"让我先",
		"我先",
	}
	action := []string{
		"look",
		"read",
		"explore",
		"examine",
		"analyze",
		"identify",
		"understand",
		"inspect",
		"review",
		"check",
		"learn",
		"看看",
		"看一下",
		"了解",
		"阅读",
		"读取",
		"理解",
		"分析",
		"检查",
		"审查",
	}

	hasIntro := false
	for _, marker := range intro {
		if strings.Contains(lower, marker) {
			hasIntro = true
			break
		}
	}
	if !hasIntro {
		return false
	}
	for _, marker := range action {
		if strings.Contains(lower, marker) {
			return true
		}
	}
	return false
}

func hasExplicitNextStepOffer(text string) bool {
	lower := strings.ToLower(strings.TrimSpace(text))
	if lower == "" {
		return false
	}
	englishMarkers := []string{
		"if you want",
		"if you'd like",
		"if you need",
		"i can continue",
		"i can also",
		"i can help",
		"i can restart",
		"i can check",
		"i can review",
		"i can commit",
		"i can push",
	}
	for _, marker := range englishMarkers {
		if strings.Contains(lower, marker) {
			return true
		}
	}
	chineseMarkers := []string{
		"如果你要",
		"如果需要",
		"如果你愿意",
		"要的话",
		"需要的话",
		"我可以继续",
		"我可以直接",
		"我可以帮你",
		"我下一步可以",
	}
	for _, marker := range chineseMarkers {
		if strings.Contains(text, marker) {
			return true
		}
	}
	return false
}

func containsHan(text string) bool {
	for _, r := range text {
		if unicode.Is(unicode.Han, r) {
			return true
		}
	}
	return false
}

func looksLikeClaudeEnvironmentBlock(text string) bool {
	lower := strings.ToLower(text)
	markers := 0
	for _, marker := range []string{
		"# environment",
		"primary working directory:",
		"# auto memory",
		"gitstatus:",
		"you have been invoked in the following environment",
	} {
		if strings.Contains(lower, marker) {
			markers++
		}
	}
	return markers >= 2
}

func countNonEmptyLines(text string) int {
	count := 0
	for _, line := range strings.Split(text, "\n") {
		if strings.TrimSpace(line) != "" {
			count++
		}
	}
	return count
}

func looksLikeTechStackRequest(text string) bool {
	lower := strings.ToLower(strings.TrimSpace(stripSystemRemindersForMode(text)))
	if lower == "" {
		return false
	}
	for _, marker := range []string{
		"技术架构", "技术栈", "架构", "框架", "依赖", "用了哪些技术", "使用了哪些技术",
		"tech stack", "technology stack", "architecture", "framework", "frameworks", "dependencies",
	} {
		if strings.Contains(lower, marker) {
			return true
		}
	}
	return false
}

func looksLikeProjectPurposeRequest(text string) bool {
	lower := strings.ToLower(strings.TrimSpace(stripSystemRemindersForMode(text)))
	if lower == "" {
		return false
	}
	for _, marker := range []string{
		"项目是干什么的", "这个项目是干什么的", "这个项目做什么", "这个项目有什么用", "用途", "做什么",
		"what does this project do", "what is this project", "project purpose", "purpose of this project",
	} {
		if strings.Contains(lower, marker) {
			return true
		}
	}
	return false
}

func looksLikeBackendImplementationRequest(text string) bool {
	lower := strings.ToLower(strings.TrimSpace(stripSystemRemindersForMode(text)))
	if lower == "" {
		return false
	}
	for _, marker := range []string{
		"后端", "服务端", "接口", "api", "后端如何实现", "接口如何实现",
		"backend", "server side", "server-side", "api implementation", "service implementation",
	} {
		if strings.Contains(lower, marker) {
			return true
		}
	}
	return false
}

func looksLikeDataLayerRequest(text string) bool {
	lower := strings.ToLower(strings.TrimSpace(stripSystemRemindersForMode(text)))
	if lower == "" {
		return false
	}
	for _, marker := range []string{
		"数据层", "存储", "数据库", "缓存", "持久化", "数据怎么存", "数据存在哪里",
		"data layer", "storage", "database", "db", "cache", "persistence",
	} {
		if strings.Contains(lower, marker) {
			return true
		}
	}
	return false
}

func looksLikeTestingRequest(text string) bool {
	lower := strings.ToLower(strings.TrimSpace(stripSystemRemindersForMode(text)))
	if lower == "" {
		return false
	}
	for _, marker := range []string{
		"测试", "单测", "集成测试", "e2e", "怎么测试", "如何测试",
		"testing", "test strategy", "unit test", "integration test", "e2e test",
	} {
		if strings.Contains(lower, marker) {
			return true
		}
	}
	return false
}

func looksLikeDeploymentRequest(text string) bool {
	lower := strings.ToLower(strings.TrimSpace(stripSystemRemindersForMode(text)))
	if lower == "" {
		return false
	}
	for _, marker := range []string{
		"部署", "构建", "发布", "上线", "运行方式", "怎么启动",
		"deployment", "deploy", "build", "release", "runtime", "how to run", "how to start",
	} {
		if strings.Contains(lower, marker) {
			return true
		}
	}
	return false
}

func looksLikeSecurityRiskRequest(text string) bool {
	lower := strings.ToLower(strings.TrimSpace(stripSystemRemindersForMode(text)))
	if lower == "" {
		return false
	}
	for _, marker := range []string{
		"安全风险", "安全问题", "漏洞", "风险点", "不安全", "安全隐患",
		"security risk", "security risks", "security issue", "vulnerability", "vulnerabilities",
	} {
		if strings.Contains(lower, marker) {
			return true
		}
	}
	return false
}

func looksLikePermissionRiskRequest(text string) bool {
	lower := strings.ToLower(strings.TrimSpace(stripSystemRemindersForMode(text)))
	if lower == "" {
		return false
	}
	for _, marker := range []string{
		"权限风险", "权限问题", "提权", "root 权限", "高权限", "最小权限",
		"permission risk", "permissions issue", "privilege escalation", "run as root", "least privilege",
	} {
		if strings.Contains(lower, marker) {
			return true
		}
	}
	return false
}

func looksLikeDependencyRiskRequest(text string) bool {
	lower := strings.ToLower(strings.TrimSpace(stripSystemRemindersForMode(text)))
	if lower == "" {
		return false
	}
	for _, marker := range []string{
		"依赖风险", "依赖问题", "供应链风险", "依赖是否安全", "第三方依赖风险",
		"dependency risk", "dependency risks", "package risk", "supply chain risk", "third-party dependency",
	} {
		if strings.Contains(lower, marker) {
			return true
		}
	}
	return false
}

func looksLikeConfigRiskRequest(text string) bool {
	lower := strings.ToLower(strings.TrimSpace(stripSystemRemindersForMode(text)))
	if lower == "" {
		return false
	}
	for _, marker := range []string{
		"配置风险", "配置问题", "配置隐患", "错误配置", "配置是否合理",
		"configuration risk", "config risk", "misconfiguration", "configuration issue",
	} {
		if strings.Contains(lower, marker) {
			return true
		}
	}
	return false
}

func looksLikeObservabilityGapRequest(text string) bool {
	lower := strings.ToLower(strings.TrimSpace(stripSystemRemindersForMode(text)))
	if lower == "" {
		return false
	}
	for _, marker := range []string{
		"可观测性", "观测性", "监控缺口", "日志缺口", "追踪缺口", "指标缺口",
		"observability", "monitoring gap", "logging gap", "tracing gap", "metrics gap",
	} {
		if strings.Contains(lower, marker) {
			return true
		}
	}
	return false
}

func looksLikeReleaseRiskRequest(text string) bool {
	lower := strings.ToLower(strings.TrimSpace(stripSystemRemindersForMode(text)))
	if lower == "" {
		return false
	}
	for _, marker := range []string{
		"发布风险", "上线风险", "交付风险", "发布隐患", "发布问题",
		"release risk", "rollout risk", "deployment risk", "shipping risk", "release issue",
	} {
		if strings.Contains(lower, marker) {
			return true
		}
	}
	return false
}

func looksLikeCompatibilityRiskRequest(text string) bool {
	lower := strings.ToLower(strings.TrimSpace(stripSystemRemindersForMode(text)))
	if lower == "" {
		return false
	}
	for _, marker := range []string{
		"兼容性风险", "兼容问题", "兼容性问题", "兼容隐患",
		"compatibility risk", "compatibility issue", "compatibility issues", "portability risk",
	} {
		if strings.Contains(lower, marker) {
			return true
		}
	}
	return false
}

func looksLikeOperationalRiskRequest(text string) bool {
	lower := strings.ToLower(strings.TrimSpace(stripSystemRemindersForMode(text)))
	if lower == "" {
		return false
	}
	for _, marker := range []string{
		"运维风险", "运维问题", "操作风险", "线上运维风险",
		"operational risk", "operations risk", "ops risk", "operational issue",
	} {
		if strings.Contains(lower, marker) {
			return true
		}
	}
	return false
}

func looksLikeRecoveryRollbackRiskRequest(text string) bool {
	lower := strings.ToLower(strings.TrimSpace(stripSystemRemindersForMode(text)))
	if lower == "" {
		return false
	}
	for _, marker := range []string{
		"恢复风险", "回滚风险", "灾备风险", "恢复与回滚风险", "回退风险",
		"recovery risk", "rollback risk", "rollback risks", "restore risk", "disaster recovery risk",
	} {
		if strings.Contains(lower, marker) {
			return true
		}
	}
	return false
}

func looksLikePerformanceBottleneckRequest(text string) bool {
	lower := strings.ToLower(strings.TrimSpace(stripSystemRemindersForMode(text)))
	if lower == "" {
		return false
	}
	for _, marker := range []string{
		"性能瓶颈", "性能问题", "性能隐患", "哪里慢", "慢在哪", "热点路径",
		"performance bottleneck", "performance issue", "slow path", "hotspot", "latency issue",
	} {
		if strings.Contains(lower, marker) {
			return true
		}
	}
	return false
}

func looksLikeCodeSmellRequest(text string) bool {
	lower := strings.ToLower(strings.TrimSpace(stripSystemRemindersForMode(text)))
	if lower == "" {
		return false
	}
	for _, marker := range []string{
		"代码异味", "坏味道", "坏味", "设计异味", "代码质量问题",
		"code smell", "code smells", "design smell", "design smells", "quality issue",
	} {
		if strings.Contains(lower, marker) {
			return true
		}
	}
	return false
}

func looksLikeMaintainabilityRiskRequest(text string) bool {
	lower := strings.ToLower(strings.TrimSpace(stripSystemRemindersForMode(text)))
	if lower == "" {
		return false
	}
	for _, marker := range []string{
		"可维护性", "维护风险", "维护成本", "难维护", "维护性问题",
		"maintainability", "maintenance risk", "hard to maintain", "maintenance issue",
	} {
		if strings.Contains(lower, marker) {
			return true
		}
	}
	return false
}

func looksLikeOptimizationRequest(text string) bool {
	lower := strings.ToLower(strings.TrimSpace(stripSystemRemindersForMode(text)))
	if lower == "" {
		return false
	}
	for _, marker := range []string{
		"怎么优化", "如何优化", "优化建议", "性能怎么优化", "重构建议", "改进建议",
		"帮我优化", "优化这个项目", "项目优化", "优化下这个项目", "帮我改进这个项目",
		"优化这个方案", "帮我优化这个方案", "优化这个设计", "帮我优化这个设计", "优化这个实现", "帮我优化这个实现",
		"how to optimize", "optimization advice", "performance optimization", "refactor suggestions", "improvement suggestions",
		"optimize this plan", "optimize this design", "optimize this implementation",
	} {
		if strings.Contains(lower, marker) {
			return true
		}
	}
	return false
}

func explicitlyRequestsDeepAnalysis(text string) bool {
	lower := strings.ToLower(strings.TrimSpace(stripSystemRemindersForMode(text)))
	if lower == "" {
		return false
	}
	for _, marker := range []string{
		"深入分析", "详细分析", "深层分析", "全面分析", "deep analysis", "detailed analysis", "in-depth analysis",
	} {
		if strings.Contains(lower, marker) {
			return true
		}
	}
	return false
}

func looksLikeWebImplementationRequest(text string) bool {
	lower := strings.ToLower(strings.TrimSpace(stripSystemRemindersForMode(text)))
	if lower == "" {
		return false
	}
	for _, marker := range []string{
		"网页", "前端", "页面", "界面", "网站", "web ui", "web-ui", "如何实现",
		"frontend", "front-end", "web", "page", "pages", "website",
	} {
		if strings.Contains(lower, marker) {
			return true
		}
	}
	return false
}

func buildLocalTechStackFallback(original, toolResult string, preferChinese bool) string {
	signals := inspectTechStackSignals(toolResult)
	if signals.isEmpty() {
		return ""
	}

	if preferChinese {
		return signals.renderChinese()
	}
	return signals.renderEnglish()
}

func buildLocalProjectPurposeFallback(toolResult string, preferChinese bool) string {
	signals := inspectTechStackSignals(toolResult)
	lines := strings.Split(toolResult, "\n")
	var hasReadme, hasWebUI, hasPackageJSON, hasGoModule bool
	for _, raw := range lines {
		line := strings.ToLower(normalizeToolResultLine(raw))
		if line == "" {
			continue
		}
		if strings.Contains(line, "readme") {
			hasReadme = true
		}
		if strings.Contains(line, "web-ui") || strings.Contains(line, "frontend") || strings.Contains(line, "index.html") {
			hasWebUI = true
		}
		if strings.Contains(line, "package.json") {
			hasPackageJSON = true
		}
		if strings.Contains(line, "go.mod") || strings.Contains(line, "/cmd/") || strings.Contains(line, "/internal/") {
			hasGoModule = true
		}
	}

	if preferChinese {
		switch {
		case hasWebUI && len(signals.architecture) > 0:
			return "从当前目录信号看，这更像是一个带独立前端目录的应用项目，前端负责页面展示，后端或脚本层负责 API、数据处理或业务逻辑。"
		case hasGoModule:
			return "从当前目录结构看，这更像是一个 Go 服务端项目，通常会由 `cmd/` 作为入口、`internal/` 承载核心业务逻辑。"
		case signals.language == "Python" && len(signals.architecture) > 0:
			return "从当前已读取内容看，这更像是一个 Python 应用项目，主要用途偏向 " + strings.Join(limitTechStackList(signals.architecture, 2), "、") + "。"
		case signals.language == "Node.js / TypeScript" && (hasPackageJSON || hasWebUI):
			return "从当前目录信号看，这更像是一个 Node.js / 前端应用项目，主要用途偏向页面展示或 Web 交互。"
		case hasReadme:
			return "当前只能从目录判断这是一个带 README 的代码项目，但具体业务用途还需要再看 README 或主入口文件。"
		}
		return ""
	}

	switch {
	case hasWebUI && len(signals.architecture) > 0:
		return "From the current directory signals, this looks like an application with a separate frontend directory, where the frontend handles the UI and the backend or script layer handles API, data flow, or business logic."
	case hasGoModule:
		return "From the current structure, this looks like a Go service project, typically with `cmd/` as entrypoints and `internal/` for core business logic."
	case signals.language == "Python" && len(signals.architecture) > 0:
		return "From the current files, this looks like a Python application whose main role is " + strings.Join(limitTechStackList(signals.architecture, 2), ", ") + "."
	case signals.language == "Node.js / TypeScript" && (hasPackageJSON || hasWebUI):
		return "From the current directory signals, this looks like a Node.js / frontend application focused on web UI or page interaction."
	case hasReadme:
		return "I can tell this is a code project with a README, but the exact business purpose still needs the README or entrypoint to confirm."
	}
	return ""
}

func buildLocalBackendImplementationFallback(toolResult string, preferChinese bool) string {
	signals := inspectTechStackSignals(toolResult)
	lines := strings.Split(toolResult, "\n")
	var hasAPIFile, hasRoutes, hasControllers, hasRequirements bool
	for _, raw := range lines {
		line := strings.ToLower(normalizeToolResultLine(raw))
		if line == "" {
			continue
		}
		if strings.Contains(line, "api.py") || strings.Contains(line, "server.py") || strings.Contains(line, "app.py") || strings.Contains(line, "main.go") || strings.Contains(line, "main.py") {
			hasAPIFile = true
		}
		if strings.Contains(line, "route") || strings.Contains(line, "router") || strings.Contains(line, "/handlers") || strings.Contains(line, "/controllers") {
			hasRoutes = true
		}
		if strings.Contains(line, "controller") {
			hasControllers = true
		}
		if strings.Contains(line, "requirements.txt") || strings.Contains(line, "pyproject.toml") || strings.Contains(line, "go.mod") || strings.Contains(line, "package.json") {
			hasRequirements = true
		}
	}

	if !(hasAPIFile || len(signals.architecture) > 0 || len(signals.frameworks) > 0 || hasRoutes || hasControllers) {
		return ""
	}

	role := "后端服务"
	if len(signals.architecture) > 0 {
		role = strings.Join(limitTechStackList(signals.architecture, 2), "、")
	}
	if preferChinese {
		var parts []string
		if signals.language != "" {
			parts = append(parts, "从当前目录信号看，后端更像是用 "+signals.language+" 实现")
		} else {
			parts = append(parts, "从当前目录信号看，这里有一层明显的后端实现")
		}
		if len(signals.frameworks) > 0 {
			parts = append(parts, "可见的框架/运行组件包括 "+strings.Join(limitTechStackList(signals.frameworks, 3), "、"))
		}
		if role != "" {
			parts = append(parts, "职责更偏向 "+role)
		}
		var details []string
		if hasAPIFile {
			details = append(details, "存在明显的 API/服务入口文件")
		}
		if hasRoutes || hasControllers {
			details = append(details, "目录里有路由或控制器分层")
		}
		if len(signals.storage) > 0 {
			details = append(details, "数据层使用 "+strings.Join(limitTechStackList(signals.storage, 2), "、"))
		}
		if hasRequirements {
			details = append(details, "依赖通过项目配置文件管理")
		}
		if len(details) > 0 {
			parts = append(parts, "并且 "+strings.Join(details, "，"))
		}
		return strings.Join(parts, "，") + "。"
	}

	var parts []string
	if signals.language != "" {
		parts = append(parts, "The backend appears to be implemented in "+signals.language)
	} else {
		parts = append(parts, "There is a clear backend layer in the current project structure")
	}
	if len(signals.frameworks) > 0 {
		parts = append(parts, "with frameworks or runtime pieces such as "+strings.Join(limitTechStackList(signals.frameworks, 3), ", "))
	}
	if role != "" {
		parts = append(parts, "serving mainly as "+role)
	}
	var details []string
	if hasAPIFile {
		details = append(details, "there are obvious API or service entry files")
	}
	if hasRoutes || hasControllers {
		details = append(details, "the layout includes route/controller style separation")
	}
	if len(signals.storage) > 0 {
		details = append(details, "the data layer uses "+strings.Join(limitTechStackList(signals.storage, 2), ", "))
	}
	if hasRequirements {
		details = append(details, "dependencies are managed through project config files")
	}
	if len(details) > 0 {
		parts = append(parts, strings.Join(details, "; "))
	}
	return strings.Join(parts, ". ") + "."
}

func buildLocalDataLayerFallback(toolResult string, preferChinese bool) string {
	signals := inspectTechStackSignals(toolResult)
	lines := strings.Split(toolResult, "\n")
	var hasSchema, hasMigrations, hasORM, hasModels bool
	for _, raw := range lines {
		line := strings.ToLower(normalizeToolResultLine(raw))
		if line == "" {
			continue
		}
		if strings.Contains(line, "schema") || strings.Contains(line, "prisma") {
			hasSchema = true
		}
		if strings.Contains(line, "migration") || strings.Contains(line, "migrations") {
			hasMigrations = true
		}
		if strings.Contains(line, "sqlalchemy") || strings.Contains(line, "gorm") || strings.Contains(line, "prisma") || strings.Contains(line, "typeorm") {
			hasORM = true
		}
		if strings.Contains(line, "model") || strings.Contains(line, "models/") {
			hasModels = true
		}
	}

	if len(signals.storage) == 0 && !hasSchema && !hasMigrations && !hasORM && !hasModels {
		return ""
	}

	if preferChinese {
		var parts []string
		if len(signals.storage) > 0 {
			parts = append(parts, "从当前目录信号看，数据层主要依赖 "+strings.Join(limitTechStackList(signals.storage, 3), "、"))
		} else {
			parts = append(parts, "从当前目录信号看，这里有一层独立的数据存储或持久化实现")
		}
		var details []string
		if hasSchema || hasMigrations {
			details = append(details, "可以看到 schema/migrations 这类结构管理信号")
		}
		if hasORM {
			details = append(details, "存在 ORM 或数据库访问层")
		}
		if hasModels {
			details = append(details, "目录里有 models 这类数据模型分层")
		}
		if len(details) > 0 {
			parts = append(parts, "并且 "+strings.Join(details, "，"))
		}
		return strings.Join(parts, "，") + "。"
	}

	var parts []string
	if len(signals.storage) > 0 {
		parts = append(parts, "From the current directory signals, the data layer mainly relies on "+strings.Join(limitTechStackList(signals.storage, 3), ", "))
	} else {
		parts = append(parts, "There is a dedicated data storage or persistence layer in the current structure")
	}
	var details []string
	if hasSchema || hasMigrations {
		details = append(details, "there are schema or migration style files")
	}
	if hasORM {
		details = append(details, "there is an ORM or database access layer")
	}
	if hasModels {
		details = append(details, "the layout includes model-style layering")
	}
	if len(details) > 0 {
		parts = append(parts, strings.Join(details, "; "))
	}
	return strings.Join(parts, ". ") + "."
}

func buildLocalTestingFallback(toolResult string, preferChinese bool) string {
	lines := strings.Split(toolResult, "\n")
	var hasTestsDir, hasPytest, hasGoTest, hasJest, hasVitest, hasPlaywright, hasCypress, hasCI bool
	for _, raw := range lines {
		line := strings.ToLower(normalizeToolResultLine(raw))
		if line == "" {
			continue
		}
		if strings.Contains(line, "tests/") || strings.HasSuffix(line, "tests") || strings.HasSuffix(line, "_test.go") || strings.Contains(line, "test_") || strings.Contains(line, "__tests__") {
			hasTestsDir = true
		}
		if strings.Contains(line, "pytest") || strings.Contains(line, "conftest.py") {
			hasPytest = true
		}
		if strings.Contains(line, "_test.go") || strings.Contains(line, "go test") {
			hasGoTest = true
		}
		if strings.Contains(line, "jest") {
			hasJest = true
		}
		if strings.Contains(line, "vitest") {
			hasVitest = true
		}
		if strings.Contains(line, "playwright") {
			hasPlaywright = true
		}
		if strings.Contains(line, "cypress") {
			hasCypress = true
		}
		if strings.Contains(line, ".github/workflows") || strings.Contains(line, "ci") {
			hasCI = true
		}
	}

	if !(hasTestsDir || hasPytest || hasGoTest || hasJest || hasVitest || hasPlaywright || hasCypress || hasCI) {
		return ""
	}

	var frameworks []string
	if hasPytest {
		frameworks = append(frameworks, "pytest")
	}
	if hasGoTest {
		frameworks = append(frameworks, "go test")
	}
	if hasJest {
		frameworks = append(frameworks, "Jest")
	}
	if hasVitest {
		frameworks = append(frameworks, "Vitest")
	}
	if hasPlaywright {
		frameworks = append(frameworks, "Playwright")
	}
	if hasCypress {
		frameworks = append(frameworks, "Cypress")
	}

	if preferChinese {
		var parts []string
		if len(frameworks) > 0 {
			parts = append(parts, "从当前目录信号看，测试主要依赖 "+strings.Join(limitTechStackList(frameworks, 3), "、"))
		} else {
			parts = append(parts, "从当前目录信号看，这个项目有独立测试层")
		}
		var details []string
		if hasTestsDir {
			details = append(details, "存在 tests 或同类测试目录")
		}
		if hasCI {
			details = append(details, "测试很可能接进了 CI 工作流")
		}
		if len(details) > 0 {
			parts = append(parts, "并且 "+strings.Join(details, "，"))
		}
		return strings.Join(parts, "，") + "。"
	}

	var parts []string
	if len(frameworks) > 0 {
		parts = append(parts, "From the current directory signals, testing mainly relies on "+strings.Join(limitTechStackList(frameworks, 3), ", "))
	} else {
		parts = append(parts, "There is a dedicated testing layer in the project")
	}
	var details []string
	if hasTestsDir {
		details = append(details, "there are tests or similar test directories")
	}
	if hasCI {
		details = append(details, "testing is likely wired into CI workflows")
	}
	if len(details) > 0 {
		parts = append(parts, strings.Join(details, "; "))
	}
	return strings.Join(parts, ". ") + "."
}

func buildLocalDeploymentFallback(toolResult string, preferChinese bool) string {
	lines := strings.Split(toolResult, "\n")
	var hasDockerfile, hasCompose, hasWorkflow, hasProcfile, hasNginx, hasK8s, hasMakefile bool
	var hasFrontendBuild, hasPythonRuntime, hasNodeRuntime, hasGoRuntime bool
	for _, raw := range lines {
		line := strings.ToLower(normalizeToolResultLine(raw))
		if line == "" {
			continue
		}
		if strings.Contains(line, "dockerfile") {
			hasDockerfile = true
		}
		if strings.Contains(line, "docker-compose") || strings.Contains(line, "compose.yaml") || strings.Contains(line, "compose.yml") {
			hasCompose = true
		}
		if strings.Contains(line, ".github/workflows") || strings.Contains(line, "workflow") {
			hasWorkflow = true
		}
		if strings.Contains(line, "procfile") {
			hasProcfile = true
		}
		if strings.Contains(line, "nginx") {
			hasNginx = true
		}
		if strings.Contains(line, "k8s") || strings.Contains(line, "kubernetes") || strings.Contains(line, "helm") {
			hasK8s = true
		}
		if strings.Contains(line, "makefile") {
			hasMakefile = true
		}
		if strings.Contains(line, "vite.config") || strings.Contains(line, "dist") || strings.Contains(line, "package.json") {
			hasFrontendBuild = true
		}
		if strings.Contains(line, "requirements.txt") || strings.Contains(line, "pyproject.toml") || strings.Contains(line, "uvicorn") || strings.Contains(line, "gunicorn") {
			hasPythonRuntime = true
		}
		if strings.Contains(line, "package.json") || strings.Contains(line, "pnpm") || strings.Contains(line, "npm") || strings.Contains(line, "node") {
			hasNodeRuntime = true
		}
		if strings.Contains(line, "go.mod") || strings.Contains(line, "main.go") {
			hasGoRuntime = true
		}
	}

	if !(hasDockerfile || hasCompose || hasWorkflow || hasProcfile || hasNginx || hasK8s || hasMakefile || hasFrontendBuild || hasPythonRuntime || hasNodeRuntime || hasGoRuntime) {
		return ""
	}

	if preferChinese {
		var parts []string
		switch {
		case hasDockerfile || hasCompose:
			parts = append(parts, "从当前目录信号看，部署更像是围绕 Docker / Compose 组织")
		case hasK8s:
			parts = append(parts, "从当前目录信号看，部署更像是围绕 Kubernetes / Helm 组织")
		case hasWorkflow:
			parts = append(parts, "从当前目录信号看，构建发布流程很可能接进了 GitHub Actions 或同类工作流")
		default:
			parts = append(parts, "从当前目录信号看，这个项目有一套显式的构建或部署链路")
		}
		var details []string
		if hasFrontendBuild {
			details = append(details, "前端需要单独构建产出")
		}
		if hasWorkflow {
			details = append(details, "构建发布流程接进了工作流")
		}
		if hasPythonRuntime {
			details = append(details, "后端运行时偏 Python")
		}
		if hasNodeRuntime {
			details = append(details, "包含 Node.js 构建或运行步骤")
		}
		if hasGoRuntime {
			details = append(details, "包含 Go 服务构建或运行步骤")
		}
		if hasProcfile || hasNginx || hasMakefile {
			details = append(details, "还能看到运行入口或辅助脚本配置")
		}
		if len(details) > 0 {
			parts = append(parts, "并且 "+strings.Join(details, "，"))
		}
		return strings.Join(parts, "，") + "。"
	}

	var parts []string
	switch {
	case hasDockerfile || hasCompose:
		parts = append(parts, "From the current directory signals, deployment is organized around Docker or Compose")
	case hasK8s:
		parts = append(parts, "From the current directory signals, deployment is organized around Kubernetes or Helm")
	case hasWorkflow:
		parts = append(parts, "From the current directory signals, build and release are likely wired into GitHub Actions or similar workflows")
	default:
		parts = append(parts, "There is an explicit build or deployment pipeline in the current structure")
	}
	var details []string
	if hasFrontendBuild {
		details = append(details, "the frontend has its own build output")
	}
	if hasWorkflow {
		details = append(details, "build and release are wired into workflows")
	}
	if hasPythonRuntime {
		details = append(details, "the backend runtime leans Python")
	}
	if hasNodeRuntime {
		details = append(details, "there is a Node.js build or runtime step")
	}
	if hasGoRuntime {
		details = append(details, "there is a Go build or runtime step")
	}
	if hasProcfile || hasNginx || hasMakefile {
		details = append(details, "there are explicit runtime or helper script configs")
	}
	if len(details) > 0 {
		parts = append(parts, strings.Join(details, "; "))
	}
	return strings.Join(parts, ". ") + "."
}

func buildLocalSecurityRiskFallback(toolResult string, preferChinese bool) string {
	lines := strings.Split(toolResult, "\n")
	var findings []string
	for _, raw := range lines {
		line := strings.ToLower(normalizeToolResultLine(raw))
		if line == "" {
			continue
		}
		switch {
		case strings.Contains(line, ".env") || strings.Contains(line, "secret") || strings.Contains(line, "token") || strings.Contains(line, "private key") || strings.Contains(line, "api_key") || strings.Contains(line, "client_secret"):
			findings = append(findings, "敏感配置或密钥暴露面")
		case strings.Contains(line, "verify=false") || strings.Contains(line, "insecure") || strings.Contains(line, "ssl._create_unverified_context"):
			findings = append(findings, "关闭 TLS 校验或使用不安全网络配置")
		case strings.Contains(line, "eval(") || strings.Contains(line, "exec(") || strings.Contains(line, "shell=true") || strings.Contains(line, "child_process.exec") || strings.Contains(line, "pickle.loads") || strings.Contains(line, "yaml.load("):
			findings = append(findings, "存在高风险代码执行或不安全反序列化点")
		case strings.Contains(line, "allow_origins=['*']") || strings.Contains(line, "allow_origins = ['*']") || strings.Contains(line, "cors") && strings.Contains(line, "*"):
			findings = append(findings, "CORS 放得过宽")
		case strings.Contains(line, "debug=true") || strings.Contains(line, "flask_env=development"):
			findings = append(findings, "生产环境误开调试模式")
		}
	}
	findings = limitTechStackList(util.UniqueStrings(findings), 3)
	if len(findings) == 0 {
		return ""
	}
	if preferChinese {
		return "从当前已读取内容看，较明显的安全风险包括：" + strings.Join(findings, "；") + "。"
	}
	return "From the current files, the clearest security risks are: " + strings.Join(findings, "; ") + "."
}

func buildLocalPermissionRiskFallback(toolResult string, preferChinese bool) string {
	lines := strings.Split(toolResult, "\n")
	var findings []string
	for _, raw := range lines {
		line := strings.ToLower(normalizeToolResultLine(raw))
		if line == "" {
			continue
		}
		switch {
		case strings.Contains(line, "chmod 777") || strings.Contains(line, "0o777") || strings.Contains(line, "mode=777"):
			findings = append(findings, "文件或目录权限过宽")
		case strings.Contains(line, "user root") || strings.Contains(line, "run as root") || strings.Contains(line, "sudo ") || strings.Contains(line, "uid=0"):
			findings = append(findings, "以 root 或高权限身份运行")
		case strings.Contains(line, "--privileged") || strings.Contains(line, "cap_sys_admin") || strings.Contains(line, "setuid"):
			findings = append(findings, "容器或进程权限超出最小权限原则")
		case strings.Contains(line, "chown -r") || strings.Contains(line, "icacls") || strings.Contains(line, "grant all"):
			findings = append(findings, "权限授予范围偏大")
		}
	}
	findings = limitTechStackList(util.UniqueStrings(findings), 3)
	if len(findings) == 0 {
		return ""
	}
	if preferChinese {
		return "从当前已读取内容看，权限相关风险主要在：" + strings.Join(findings, "；") + "。"
	}
	return "From the current files, the main permission-related risks are: " + strings.Join(findings, "; ") + "."
}

func buildLocalDependencyRiskFallback(toolResult string, preferChinese bool) string {
	lines := strings.Split(toolResult, "\n")
	var findings []string
	var hasLockfile bool
	for _, raw := range lines {
		line := strings.ToLower(normalizeToolResultLine(raw))
		if line == "" {
			continue
		}
		switch {
		case strings.Contains(line, "package-lock.json") || strings.Contains(line, "pnpm-lock.yaml") || strings.Contains(line, "yarn.lock") || strings.Contains(line, "poetry.lock"):
			hasLockfile = true
		case strings.Contains(line, ">=") || strings.Contains(line, "~=") || strings.Contains(line, "^") || strings.Contains(line, "~") || strings.Contains(line, "*"):
			findings = append(findings, "依赖版本未严格锁定")
		case strings.Contains(line, "git+") || strings.Contains(line, "http://") || strings.Contains(line, "https://") && strings.Contains(line, "#egg="):
			findings = append(findings, "存在直接拉取远程源的依赖")
		case strings.Contains(line, "latest"):
			findings = append(findings, "使用 latest 这类不稳定版本标签")
		case strings.Contains(line, "package.json") || strings.Contains(line, "requirements.txt") || strings.Contains(line, "pyproject.toml") || strings.Contains(line, "go.mod"):
			findings = append(findings, "依赖集中在清单文件里管理")
		}
	}
	findings = util.UniqueStrings(findings)
	if hasLockfile {
		findings = append(findings, "已存在锁文件，供应链漂移风险相对可控")
	}
	findings = limitTechStackList(util.UniqueStrings(findings), 3)
	if len(findings) == 0 {
		return ""
	}
	if preferChinese {
		return "从当前已读取内容看，依赖层面的主要风险/特征是：" + strings.Join(findings, "；") + "。"
	}
	return "From the current files, the main dependency-level risks or signals are: " + strings.Join(findings, "; ") + "."
}

func buildLocalConfigRiskFallback(toolResult string, preferChinese bool) string {
	lines := strings.Split(toolResult, "\n")
	var findings []string
	for _, raw := range lines {
		line := strings.ToLower(normalizeToolResultLine(raw))
		if line == "" {
			continue
		}
		switch {
		case strings.Contains(line, "debug=true") || strings.Contains(line, "debug = true") || strings.Contains(line, "flask_env=development") || strings.Contains(line, "env=development"):
			findings = append(findings, "开发模式或调试配置可能泄漏到生产环境")
		case strings.Contains(line, "verify=false") || strings.Contains(line, "insecure") || strings.Contains(line, "skip_verify") || strings.Contains(line, "ssl._create_unverified_context"):
			findings = append(findings, "网络或 TLS 相关配置偏不安全")
		case strings.Contains(line, "api_key") || strings.Contains(line, "token") || strings.Contains(line, "password") || strings.Contains(line, "client_secret") || strings.Contains(line, "secret_key"):
			findings = append(findings, "敏感配置和业务代码/配置文件耦合过近")
		case strings.Contains(line, "base_url") || strings.Contains(line, "target_url") || strings.Contains(line, "localhost") || strings.Contains(line, "127.0.0.1"):
			findings = append(findings, "环境地址或外部依赖配置存在硬编码倾向")
		case strings.Contains(line, "allow_origins=['*']") || strings.Contains(line, "allow_origins = ['*']") || (strings.Contains(line, "cors") && strings.Contains(line, "*")):
			findings = append(findings, "跨域等运行配置放得过宽")
		}
	}
	findings = limitTechStackList(util.UniqueStrings(findings), 3)
	if len(findings) == 0 {
		return ""
	}
	if preferChinese {
		return "从当前已读取内容看，配置层面的主要风险在：" + strings.Join(findings, "；") + "。"
	}
	return "From the current files, the main configuration risks are: " + strings.Join(findings, "; ") + "."
}

func buildLocalObservabilityGapFallback(toolResult string, preferChinese bool) string {
	lines := strings.Split(toolResult, "\n")
	var findings []string
	var hasMetrics, hasTracing, hasHealthcheck, hasRuntimeSignal bool
	for _, raw := range lines {
		line := strings.ToLower(normalizeToolResultLine(raw))
		if line == "" {
			continue
		}
		if strings.Contains(line, "fastapi") || strings.Contains(line, "flask") || strings.Contains(line, "django") ||
			strings.Contains(line, "express") || strings.Contains(line, "gin") || strings.Contains(line, "uvicorn") ||
			strings.Contains(line, "requests.") || strings.Contains(line, "httpx.") || strings.Contains(line, "urllib.request") ||
			strings.Contains(line, "print(") || strings.Contains(line, "console.log(") || strings.Contains(line, "fmt.println(") ||
			strings.Contains(line, "logger.") || strings.Contains(line, "logging.") || strings.Contains(line, "zap.") || strings.Contains(line, "slog.") {
			hasRuntimeSignal = true
		}
		switch {
		case strings.Contains(line, "print(") || strings.Contains(line, "console.log(") || strings.Contains(line, "fmt.println("):
			findings = append(findings, "日志仍主要依赖 print/console 这类临时输出")
		case strings.Contains(line, "logger.") || strings.Contains(line, "logging.") || strings.Contains(line, "zap.") || strings.Contains(line, "slog."):
			// structured logging signal exists; do not add a gap for logging
		case strings.Contains(line, "prometheus") || strings.Contains(line, "metrics") || strings.Contains(line, "/metrics"):
			hasMetrics = true
		case strings.Contains(line, "opentelemetry") || strings.Contains(line, "trace") || strings.Contains(line, "tracing"):
			hasTracing = true
		case strings.Contains(line, "health") || strings.Contains(line, "ready") || strings.Contains(line, "liveness"):
			hasHealthcheck = true
		}
	}
	if !hasRuntimeSignal {
		return ""
	}
	if !hasMetrics {
		findings = append(findings, "当前片段里看不到指标采集或 `/metrics` 这类信号")
	}
	if !hasTracing {
		findings = append(findings, "当前片段里看不到 trace / tracing 相关埋点")
	}
	if !hasHealthcheck {
		findings = append(findings, "当前片段里看不到 health/ready 这类探针入口")
	}
	findings = limitTechStackList(util.UniqueStrings(findings), 3)
	if len(findings) == 0 {
		return ""
	}
	if preferChinese {
		return "从当前已读取内容看，较明显的可观测性缺口有：" + strings.Join(findings, "；") + "。"
	}
	return "From the current files, the clearest observability gaps are: " + strings.Join(findings, "; ") + "."
}

func buildLocalReleaseRiskFallback(toolResult string, preferChinese bool) string {
	lines := strings.Split(toolResult, "\n")
	var findings []string
	for _, raw := range lines {
		line := strings.ToLower(normalizeToolResultLine(raw))
		if line == "" {
			continue
		}
		switch {
		case strings.Contains(line, ":latest") || strings.Contains(line, "latest"):
			findings = append(findings, "构建或镜像版本依赖可变标签，发布结果不够可复现")
		case strings.Contains(line, "on: push") || strings.Contains(line, "branches: [main]") || strings.Contains(line, "branches: [master]") || strings.Contains(line, "push:"):
			findings = append(findings, "发布链路看起来会直接跟随主干提交触发")
		case strings.Contains(line, "kubectl apply") || strings.Contains(line, "docker-compose up") || strings.Contains(line, "compose up") || strings.Contains(line, "rsync ") || strings.Contains(line, "scp "):
			findings = append(findings, "发布步骤偏直接，回滚和发布保护信号不明显")
		case strings.Contains(line, "curl | bash") || strings.Contains(line, "curl|bash"):
			findings = append(findings, "发布过程中存在直接执行远程脚本的步骤")
		}
	}
	findings = limitTechStackList(util.UniqueStrings(findings), 3)
	if len(findings) == 0 {
		return ""
	}
	if preferChinese {
		return "从当前已读取内容看，发布层面的主要风险有：" + strings.Join(findings, "；") + "。"
	}
	return "From the current files, the main release risks are: " + strings.Join(findings, "; ") + "."
}

func buildLocalCompatibilityRiskFallback(toolResult string, preferChinese bool) string {
	lines := strings.Split(toolResult, "\n")
	var findings []string
	for _, raw := range lines {
		line := strings.ToLower(normalizeToolResultLine(raw))
		if line == "" {
			continue
		}
		switch {
		case strings.Contains(line, "/users/") || strings.Contains(line, "c:\\") || strings.Contains(line, "\\\\"):
			findings = append(findings, "路径或文件处理明显带平台假设")
		case strings.Contains(line, "python3.7") || strings.Contains(line, "python3.8") || strings.Contains(line, "node14") || strings.Contains(line, "node16") || strings.Contains(line, "go1.20") || strings.Contains(line, "go 1.20"):
			findings = append(findings, "运行时版本绑定较死，跨环境兼容空间较小")
		case strings.Contains(line, "brew ") || strings.Contains(line, "apt-get ") || strings.Contains(line, "powershell") || strings.Contains(line, ".bat") || strings.Contains(line, ".ps1") || strings.Contains(line, "#!/bin/bash"):
			findings = append(findings, "安装或脚本流程对特定系统/命令环境耦合较重")
		case strings.Contains(line, ".dylib") || strings.Contains(line, ".so") || strings.Contains(line, ".dll"):
			findings = append(findings, "依赖平台相关二进制，跨系统兼容性要额外验证")
		}
	}
	findings = limitTechStackList(util.UniqueStrings(findings), 3)
	if len(findings) == 0 {
		return ""
	}
	if preferChinese {
		return "从当前已读取内容看，兼容性方面的主要风险有：" + strings.Join(findings, "；") + "。"
	}
	return "From the current files, the main compatibility risks are: " + strings.Join(findings, "; ") + "."
}

func buildLocalOperationalRiskFallback(toolResult string, preferChinese bool) string {
	lines := strings.Split(toolResult, "\n")
	var findings []string
	for _, raw := range lines {
		line := strings.ToLower(normalizeToolResultLine(raw))
		if line == "" {
			continue
		}
		switch {
		case strings.Contains(line, "nohup ") || strings.Contains(line, "screen ") || strings.Contains(line, "tmux "):
			findings = append(findings, "进程管理依赖临时会话或 nohup 这类人工方式")
		case strings.Contains(line, "ssh ") || strings.Contains(line, "scp ") || strings.Contains(line, "rsync "):
			findings = append(findings, "上线或运维步骤依赖人工远程命令/文件拷贝")
		case strings.Contains(line, "kill -9") || strings.Contains(line, "pkill") || strings.Contains(line, "restart.sh"):
			findings = append(findings, "故障处置更像人工脚本驱动，标准化程度不高")
		case strings.Contains(line, "crontab") || strings.Contains(line, "cron") || strings.Contains(line, "systemd") || strings.Contains(line, "supervisor") || strings.Contains(line, "pm2"):
			findings = append(findings, "运行调度依赖宿主机进程/计划任务配置，环境漂移风险较高")
		}
	}
	findings = limitTechStackList(util.UniqueStrings(findings), 3)
	if len(findings) == 0 {
		return ""
	}
	if preferChinese {
		return "从当前已读取内容看，运维层面的主要风险有：" + strings.Join(findings, "；") + "。"
	}
	return "From the current files, the main operational risks are: " + strings.Join(findings, "; ") + "."
}

func buildLocalRecoveryRollbackRiskFallback(toolResult string, preferChinese bool) string {
	lines := strings.Split(toolResult, "\n")
	var findings []string
	var hasReleaseSignal bool
	var hasRollbackSignal bool
	var hasBackupSignal bool
	for _, raw := range lines {
		line := strings.ToLower(normalizeToolResultLine(raw))
		if line == "" {
			continue
		}
		if strings.Contains(line, "kubectl apply") || strings.Contains(line, "docker-compose up") || strings.Contains(line, "compose up") ||
			strings.Contains(line, "alembic upgrade") || strings.Contains(line, "migrate deploy") || strings.Contains(line, "terraform apply") ||
			strings.Contains(line, "rsync ") || strings.Contains(line, "scp ") {
			hasReleaseSignal = true
		}
		if strings.Contains(line, "rollback") || strings.Contains(line, "roll back") || strings.Contains(line, "restore") || strings.Contains(line, "downgrade") {
			hasRollbackSignal = true
		}
		if strings.Contains(line, "backup") || strings.Contains(line, "snapshot") || strings.Contains(line, "dump ") {
			hasBackupSignal = true
		}
		switch {
		case strings.Contains(line, "drop table") || strings.Contains(line, "truncate ") || strings.Contains(line, "delete from") || strings.Contains(line, "terraform apply"):
			findings = append(findings, "存在破坏性变更或一次性 apply，回滚成本偏高")
		case strings.Contains(line, "kubectl apply") || strings.Contains(line, "docker-compose up") || strings.Contains(line, "compose up"):
			findings = append(findings, "发布步骤偏覆盖式，显式回滚策略不明显")
		case strings.Contains(line, "alembic upgrade") || strings.Contains(line, "migrate deploy") || strings.Contains(line, "prisma migrate deploy"):
			findings = append(findings, "数据库迁移看起来偏向前滚，回退路径要单独验证")
		}
	}
	if hasReleaseSignal && !hasRollbackSignal {
		findings = append(findings, "当前片段里看不到明确的 rollback/restore 步骤")
	}
	if hasReleaseSignal && !hasBackupSignal {
		findings = append(findings, "当前片段里看不到备份或快照信号")
	}
	findings = limitTechStackList(util.UniqueStrings(findings), 3)
	if len(findings) == 0 {
		return ""
	}
	if preferChinese {
		return "从当前已读取内容看，恢复与回滚层面的主要风险有：" + strings.Join(findings, "；") + "。"
	}
	return "From the current files, the main recovery or rollback risks are: " + strings.Join(findings, "; ") + "."
}

func buildLocalPerformanceBottleneckFallback(toolResult string, preferChinese bool) string {
	lines := strings.Split(toolResult, "\n")
	var hasLoop, hasSyncNetwork, hasFileIO, hasJSONIO, hasDBAccess, hasSleep bool
	for _, raw := range lines {
		line := strings.ToLower(normalizeToolResultLine(raw))
		if line == "" {
			continue
		}
		if strings.Contains(line, "for ") || strings.Contains(line, "while ") || strings.Contains(line, "range(") || strings.Contains(line, "foreach") {
			hasLoop = true
		}
		if strings.Contains(line, "requests.") || strings.Contains(line, "urllib.request") || strings.Contains(line, "httpx.") || strings.Contains(line, "axios.") || strings.Contains(line, "fetch(") {
			hasSyncNetwork = true
		}
		if strings.Contains(line, "open(") || strings.Contains(line, "readfile(") || strings.Contains(line, "writefile(") || strings.Contains(line, "read_text(") || strings.Contains(line, "write_text(") {
			hasFileIO = true
		}
		if strings.Contains(line, "json.load(") || strings.Contains(line, "json.dump(") || strings.Contains(line, "json.loads(") || strings.Contains(line, "json.dumps(") {
			hasJSONIO = true
		}
		if strings.Contains(line, "select ") || strings.Contains(line, "insert into") || strings.Contains(line, "update ") || strings.Contains(line, "delete from") || strings.Contains(line, "execute(") || strings.Contains(line, "query(") {
			hasDBAccess = true
		}
		if strings.Contains(line, "time.sleep(") || strings.Contains(line, "sleep(") {
			hasSleep = true
		}
	}

	findings := make([]string, 0, 4)
	switch {
	case hasLoop && hasSyncNetwork:
		findings = append(findings, "循环里夹杂同步网络请求，容易形成串行热点")
	case hasSyncNetwork:
		findings = append(findings, "外部请求路径看起来是同步阻塞式")
	}
	if hasLoop && (hasFileIO || hasJSONIO || hasDBAccess) {
		findings = append(findings, "循环里混入文件或数据库 I/O，容易放大热点")
	}
	if hasJSONIO && hasFileIO {
		findings = append(findings, "频繁 JSON 文件读写可能成为性能瓶颈")
	}
	if hasSleep {
		findings = append(findings, "存在显式 sleep 或阻塞等待")
	}
	if len(findings) == 0 {
		return ""
	}
	findings = limitTechStackList(util.UniqueStrings(findings), 3)
	if preferChinese {
		return "从当前已读取内容看，比较明显的性能瓶颈信号有：" + strings.Join(findings, "；") + "。"
	}
	return "From the current files, the clearest performance bottleneck signals are: " + strings.Join(findings, "; ") + "."
}

func buildLocalCodeSmellFallback(toolResult string, preferChinese bool) string {
	lines := strings.Split(toolResult, "\n")
	var findings []string
	var hasNetwork, hasFileIO, hasDebugPrint bool
	for _, raw := range lines {
		line := strings.ToLower(normalizeToolResultLine(raw))
		if line == "" {
			continue
		}
		if strings.Contains(line, "requests.") || strings.Contains(line, "urllib.request") || strings.Contains(line, "httpx.") || strings.Contains(line, "axios.") || strings.Contains(line, "fetch(") {
			hasNetwork = true
		}
		if strings.Contains(line, "open(") || strings.Contains(line, "json.load(") || strings.Contains(line, "json.dump(") {
			hasFileIO = true
		}
		if strings.Contains(line, "print(") || strings.Contains(line, "console.log(") || strings.Contains(line, "fmt.println(") {
			hasDebugPrint = true
		}
		switch {
		case strings.Contains(line, "except:") || strings.Contains(line, "except exception") || strings.Contains(line, "catch (e)") || strings.Contains(line, "catch(error)") || strings.Contains(line, "catch (error)"):
			findings = append(findings, "错误处理过宽，容易吞掉真实异常")
		case strings.Contains(line, "todo") || strings.Contains(line, "fixme") || strings.Contains(line, "xxx"):
			findings = append(findings, "代码里残留 TODO/FIXME 这类未收口标记")
		case strings.Contains(line, "http://") || strings.Contains(line, "/users/") || strings.Contains(line, "c:\\") || strings.Contains(line, "target_url") || strings.Contains(line, "base_url"):
			findings = append(findings, "存在硬编码路径或地址")
		}
	}
	if hasNetwork && hasFileIO {
		findings = append(findings, "单文件混合网络、文件 I/O 和业务逻辑，职责耦合偏重")
	}
	if hasDebugPrint {
		findings = append(findings, "调试输出仍散落在业务路径里")
	}
	findings = limitTechStackList(util.UniqueStrings(findings), 3)
	if len(findings) == 0 {
		return ""
	}
	if preferChinese {
		return "从当前已读取内容看，比较明显的代码异味有：" + strings.Join(findings, "；") + "。"
	}
	return "From the current files, the clearest code smells are: " + strings.Join(findings, "; ") + "."
}

func buildLocalMaintainabilityRiskFallback(toolResult string, preferChinese bool) string {
	lines := strings.Split(toolResult, "\n")
	var findings []string
	var hasNetwork, hasStorage, hasGlobals bool
	for _, raw := range lines {
		line := strings.ToLower(normalizeToolResultLine(raw))
		if line == "" {
			continue
		}
		if strings.Contains(line, "requests.") || strings.Contains(line, "urllib.request") || strings.Contains(line, "httpx.") || strings.Contains(line, "axios.") || strings.Contains(line, "fetch(") {
			hasNetwork = true
		}
		if strings.Contains(line, "open(") || strings.Contains(line, "json.load(") || strings.Contains(line, "json.dump(") || strings.Contains(line, "redis") || strings.Contains(line, "sqlalchemy") || strings.Contains(line, "postgres") || strings.Contains(line, "mysql") {
			hasStorage = true
		}
		if strings.Contains(line, "global ") || strings.Contains(line, "singleton") || strings.Contains(line, "shared_state") || strings.Contains(line, "module-level state") {
			hasGlobals = true
		}
		switch {
		case strings.Contains(line, ".env") || strings.Contains(line, "api_key") || strings.Contains(line, "token") || strings.Contains(line, "secret") || strings.Contains(line, "password"):
			findings = append(findings, "配置或敏感信息和业务代码靠得过近")
		case strings.Contains(line, "except:") || strings.Contains(line, "except exception") || strings.Contains(line, "catch (e)") || strings.Contains(line, "catch(error)") || strings.Contains(line, "catch (error)"):
			findings = append(findings, "异常处理过宽，会抬高定位和维护成本")
		case strings.Contains(line, "/users/") || strings.Contains(line, "c:\\") || strings.Contains(line, "target_url") || strings.Contains(line, "base_url") || strings.Contains(line, "hardcoded"):
			findings = append(findings, "硬编码路径或环境配置会增加迁移成本")
		}
	}
	if hasNetwork && hasStorage {
		findings = append(findings, "同一模块同时承担网络、存储和业务职责，分层边界不清")
	}
	if hasGlobals {
		findings = append(findings, "存在共享可变状态，后续扩展和测试成本会升高")
	}
	findings = limitTechStackList(util.UniqueStrings(findings), 3)
	if len(findings) == 0 {
		return ""
	}
	if preferChinese {
		return "从当前已读取内容看，可维护性风险主要在：" + strings.Join(findings, "；") + "。"
	}
	return "From the current files, the main maintainability risks are: " + strings.Join(findings, "; ") + "."
}

func buildLocalOptimizationFallback(toolResult string, preferChinese bool) string {
	if looksLikeWarpExplorationSeed(toolResult) {
		signals := inspectTechStackSignals(toolResult)
		if signals.isEmpty() {
			return ""
		}
		var suggestions []string
		if signals.language == "Python" {
			suggestions = append(suggestions, "先围绕主入口脚本和模块边界拆分职责，减少脚本式耦合")
		}
		if signals.language == "Node.js / TypeScript" {
			suggestions = append(suggestions, "优先理清前端工程入口、构建产物和页面代码的分层")
		}
		if len(signals.architecture) > 1 {
			suggestions = append(suggestions, "把 API、页面层和数据处理层的边界收紧，避免跨层直接耦合")
		}
		if len(signals.storage) > 0 {
			suggestions = append(suggestions, "把数据读写和校验抽成独立层，避免 JSON/文件存取散落在业务代码里")
		}
		if len(signals.frameworks) > 0 || len(signals.libraries) > 0 {
			suggestions = append(suggestions, "先补 README、依赖清单和主入口的最小约定，再继续做性能或结构优化")
		}
		suggestions = limitTechStackList(util.UniqueStrings(suggestions), 3)
		if len(suggestions) == 0 {
			return ""
		}
		if preferChinese {
			return "基于当前目录概览，优先可做这几项优化：" + strings.Join(suggestions, "；") + "。"
		}
		return "From the current directory overview, the highest-value optimizations are: " + strings.Join(suggestions, "; ") + "."
	}

	if looksLikeToolResultFailure(toolResult) {
		return ""
	}

	signals := inspectTechStackSignals(toolResult)
	var suggestions []string
	lower := strings.ToLower(toolResult)

	if strings.Contains(lower, "if __name__ == \"__main__\"") || strings.Contains(lower, "if __name__ == '__main__'") {
		suggestions = append(suggestions, "把脚本入口和可复用逻辑拆开，CLI/测试入口单独放到薄包装层")
	}
	if strings.Contains(lower, `:\\`) || strings.Contains(lower, `/users/`) || strings.Contains(lower, `\github`) {
		suggestions = append(suggestions, "去掉硬编码本地路径，改成命令行参数、配置文件或环境变量")
	}
	if strings.Contains(lower, "print(") {
		suggestions = append(suggestions, "把临时 print 输出改成结构化日志或明确返回值，便于测试和排障")
	}
	if strings.Contains(lower, "from ") && strings.Contains(lower, " import ") && strings.Contains(lower, "timeout=") {
		suggestions = append(suggestions, "把外部依赖调用包进单独适配层，超时和错误处理统一收口")
	}

	if signals.language == "Python" {
		suggestions = append(suggestions, "把 I/O、网络请求和业务逻辑拆开，减少脚本式耦合")
	}
	if signals.language == "Node.js / TypeScript" {
		suggestions = append(suggestions, "把构建配置、页面逻辑和数据访问分层，避免前端工程继续堆在单一入口")
	}
	if len(signals.networking) > 0 {
		suggestions = append(suggestions, "统一超时、重试和错误处理，避免网络调用散落在业务代码里")
	}
	if len(signals.storage) > 0 {
		suggestions = append(suggestions, "把数据读写抽成独立的数据访问层，并补上 schema/输入校验")
	}
	if len(signals.architecture) > 0 {
		suggestions = append(suggestions, "明确 API、页面层和数据处理层边界，减少跨层直接调用")
	}
	if len(signals.frameworks) > 0 {
		suggestions = append(suggestions, "围绕当前框架补最小测试和启动/配置约定，先提升可维护性再谈微优化")
	}
	if len(suggestions) == 0 {
		return ""
	}

	suggestions = limitTechStackList(suggestions, 3)
	if preferChinese {
		return "基于当前已读取内容，优先可做这几项优化：" + strings.Join(suggestions, "；") + "。"
	}
	return "Based on the files already read, the highest-value optimizations are: " + strings.Join(suggestions, "; ") + "."
}

func buildProjectSpecificOptimizationFallback(messages []prompt.Message, preferChinese bool) string {
	evidence := collectSuccessfulToolResultEvidence(messages)
	if len(evidence) == 0 {
		return ""
	}

	fileContents := make(map[string]string)
	keys := make([]string, 0, len(evidence))
	var corpusParts []string

	for _, item := range evidence {
		if !strings.EqualFold(strings.TrimSpace(item.ToolName), "Read") {
			continue
		}
		base := toolInputBaseName(item.FilePath)
		if base == "" {
			continue
		}
		key := strings.ToLower(base)
		text := strings.TrimSpace(item.Content)
		if text == "" {
			continue
		}
		if _, ok := fileContents[key]; ok {
			continue
		}
		fileContents[key] = text
		keys = append(keys, key)
	}

	if len(fileContents) == 0 {
		return ""
	}

	sort.Strings(keys)
	for _, key := range keys {
		corpusParts = append(corpusParts, strings.ToLower(fileContents[key]))
	}
	corpus := strings.Join(corpusParts, "\n\n")

	var suggestions []string

	entryFiles := collectOptimizationFiles(fileContents, "api.py", "monitor_trump.py", "dashboard.py")
	if len(entryFiles) >= 2 && containsAnyLower(corpus, "fastapi", "streamlit", "openai", "huggingface", "@app.get", "st.components.v1.html") {
		if preferChinese {
			suggestions = append(suggestions, "把 "+strings.Join(entryFiles, "、")+" 这些入口文件按 API 路由、抓取/AI、页面渲染拆成更小模块，避免单文件同时承担接口、媒体处理和 UI 逻辑")
		} else {
			suggestions = append(suggestions, "Split "+strings.Join(entryFiles, ", ")+" into smaller API, ingestion/AI, and UI modules so one file no longer owns routes, media handling, and presentation logic at the same time")
		}
	}

	utilsContent := strings.ToLower(fileContents["utils.py"])
	if utilsContent != "" && containsAnyLower(utilsContent, "alerts_file", "media_mapping_file", "dashboard_json_file", "json.load(", "json.dump(", "with open(") {
		if preferChinese {
			suggestions = append(suggestions, "把 `utils.py` 里围绕 `ALERTS_FILE`、`MEDIA_MAPPING_FILE`、`dashboard_data.json` 的 JSON 读写收口成仓储层，并补原子写、文件锁和 schema 校验")
		} else {
			suggestions = append(suggestions, "Turn the JSON reads and writes around `ALERTS_FILE`, `MEDIA_MAPPING_FILE`, and `dashboard_data.json` in `utils.py` into a repository layer with atomic writes, file locking, and schema validation")
		}
	}

	networkingCorpus := strings.ToLower(fileContents["monitor_trump.py"] + "\n" + fileContents["api.py"])
	if containsAnyLower(networkingCorpus, "openai", "huggingface", "siliconflow", "requests.post", "urlopen(", "except exception", "print(") {
		if preferChinese {
			suggestions = append(suggestions, "把 `monitor_trump.py` 和 `api.py` 里的 Truth Social、HuggingFace、SiliconFlow、OCR/下载调用抽成独立 client 或 adapter，统一 timeout、retry、错误分类和结构化日志，替换散落的 `print`/宽泛异常处理")
		} else {
			suggestions = append(suggestions, "Extract the Truth Social, HuggingFace, SiliconFlow, OCR, and download calls in `monitor_trump.py` and `api.py` into dedicated clients or adapters with shared timeout, retry, error classification, and structured logging instead of scattered prints and broad exception handling")
		}
	}

	dashboardContent := strings.ToLower(fileContents["dashboard.py"])
	if dashboardContent != "" && containsAnyLower(dashboardContent, "streamlit as st", "st.components.v1.html", "sessionstorage", "api_base_url", "<script>") {
		if preferChinese {
			suggestions = append(suggestions, "把 `dashboard.py` 里的内联 HTML/JavaScript 和 API 地址探测逻辑迁到 `web-ui` 或独立前端资源，让 Streamlit 层只保留状态管理和渲染")
		} else {
			suggestions = append(suggestions, "Move the inline HTML/JavaScript and API URL detection in `dashboard.py` into `web-ui` or dedicated frontend assets so the Streamlit layer only handles state and rendering")
		}
	}

	testContent := strings.ToLower(fileContents["test_caption_cloud.py"])
	if testContent != "" && containsAnyLower(testContent, `d:\github`, `media\\images`, "if __name__ == \"__main__\"", "print(") {
		if preferChinese {
			suggestions = append(suggestions, "把 `test_caption_cloud.py` 里的硬编码本地图片路径改成命令行参数、fixture 或临时文件，避免测试依赖单机目录")
		} else {
			suggestions = append(suggestions, "Replace the hard-coded local image path in `test_caption_cloud.py` with CLI arguments, fixtures, or temp files so the test does not depend on one machine-specific directory")
		}
	}

	suggestions = util.UniqueStrings(suggestions)
	if len(suggestions) < 2 {
		return ""
	}
	suggestions = limitTechStackList(suggestions, 4)
	if len(suggestions) == 0 {
		return ""
	}

	if preferChinese {
		return "基于当前已读取的关键文件，优先可做这几项优化：" + strings.Join(suggestions, "；") + "。"
	}
	return "Based on the key files already read, the highest-value improvements are: " + strings.Join(suggestions, "; ") + "."
}

func collectOptimizationFiles(fileContents map[string]string, keys ...string) []string {
	files := make([]string, 0, len(keys))
	for _, key := range keys {
		if strings.TrimSpace(fileContents[strings.ToLower(key)]) == "" {
			continue
		}
		files = append(files, "`"+key+"`")
	}
	return files
}

func containsAnyLower(text string, markers ...string) bool {
	for _, marker := range markers {
		if marker != "" && strings.Contains(text, marker) {
			return true
		}
	}
	return false
}

func buildLocalWebImplementationFallback(toolResult string, preferChinese bool) string {
	lines := strings.Split(toolResult, "\n")
	var hasWebUIDir, hasSrc, hasPublic, hasDist bool
	var hasIndexHTML, hasPackageJSON, hasVite, hasTailwind, hasPostCSS bool
	var hasReact, hasVue, hasNext, hasNuxt bool
	var hasPythonBackend bool

	for _, raw := range lines {
		line := strings.ToLower(normalizeToolResultLine(raw))
		if line == "" {
			continue
		}
		if strings.Contains(line, "web-ui") || strings.Contains(line, "frontend") {
			hasWebUIDir = true
		}
		if strings.HasSuffix(line, "src") || strings.HasSuffix(line, "src/") || strings.Contains(line, "/src") {
			hasSrc = true
		}
		if strings.HasSuffix(line, "public") || strings.HasSuffix(line, "public/") || strings.Contains(line, "/public") {
			hasPublic = true
		}
		if strings.HasSuffix(line, "dist") || strings.HasSuffix(line, "dist/") || strings.Contains(line, "/dist") {
			hasDist = true
		}
		if strings.Contains(line, "index.html") {
			hasIndexHTML = true
		}
		if strings.Contains(line, "package.json") {
			hasPackageJSON = true
		}
		if strings.Contains(line, "vite.config") {
			hasVite = true
		}
		if strings.Contains(line, "tailwind.config") {
			hasTailwind = true
		}
		if strings.Contains(line, "postcss.config") {
			hasPostCSS = true
		}
		if strings.Contains(line, "react") {
			hasReact = true
		}
		if strings.Contains(line, "vue") {
			hasVue = true
		}
		if strings.Contains(line, "next.config") || strings.Contains(line, "next/") {
			hasNext = true
		}
		if strings.Contains(line, "nuxt") {
			hasNuxt = true
		}

		if strings.HasSuffix(line, ".py") || strings.Contains(line, "api.py") || strings.Contains(line, "dashboard.py") || strings.Contains(line, "flask") || strings.Contains(line, "fastapi") {
			hasPythonBackend = true
		}
	}

	if !(hasWebUIDir || hasSrc || hasPackageJSON || hasIndexHTML) {
		return ""
	}

	framework := ""
	switch {
	case hasNext:
		framework = "Next.js"
	case hasNuxt:
		framework = "Nuxt"
	case hasReact:
		framework = "React"
	case hasVue:
		framework = "Vue"
	case hasVite:
		framework = "Vite"
	}

	if preferChinese {
		var parts []string
		if hasWebUIDir {
			parts = append(parts, "网页前端看起来是单独放在 `web-ui/` 目录里")
		} else {
			parts = append(parts, "网页前端看起来是一个独立的前端工程")
		}
		switch {
		case framework != "":
			parts = append(parts, "当前目录信号更像是基于 "+framework+" 构建")
		case hasPackageJSON && hasIndexHTML:
			parts = append(parts, "当前目录形态更像是基于 Node 工具链的静态前端")
		}
		var details []string
		if hasSrc {
			details = append(details, "`src/` 作为源码目录")
		}
		if hasPublic {
			details = append(details, "`public/` 放静态资源")
		}
		if hasDist {
			details = append(details, "`dist/` 放构建产物")
		}
		if hasTailwind {
			details = append(details, "Tailwind CSS")
		}
		if hasPostCSS {
			details = append(details, "PostCSS")
		}
		if len(details) > 0 {
			parts = append(parts, "并且能看到 "+strings.Join(limitTechStackList(details, 4), "、"))
		}
		if hasPythonBackend {
			parts = append(parts, "项目整体更像是“Python 后端/脚本 + 单独前端目录”的组合")
		}
		return strings.Join(parts, "，") + "。"
	}

	var parts []string
	if hasWebUIDir {
		parts = append(parts, "The web UI appears to live in a dedicated `web-ui/` directory")
	} else {
		parts = append(parts, "The web UI looks like a separate frontend project")
	}
	if framework != "" {
		parts = append(parts, "the layout is most consistent with "+framework)
	}
	var details []string
	if hasSrc {
		details = append(details, "`src/` for source code")
	}
	if hasPublic {
		details = append(details, "`public/` for static assets")
	}
	if hasDist {
		details = append(details, "`dist/` for build output")
	}
	if hasTailwind {
		details = append(details, "Tailwind CSS")
	}
	if hasPostCSS {
		details = append(details, "PostCSS")
	}
	if len(details) > 0 {
		parts = append(parts, "with "+strings.Join(limitTechStackList(details, 4), ", "))
	}
	if hasPythonBackend {
		parts = append(parts, "The overall app also looks like a Python backend or script layer paired with that frontend")
	}
	return strings.Join(parts, ". ") + "."
}

type techStackSignals struct {
	language     string
	frameworks   []string
	libraries    []string
	storage      []string
	networking   []string
	architecture []string
}

func (s techStackSignals) isEmpty() bool {
	return s.language == "" &&
		len(s.frameworks) == 0 &&
		len(s.libraries) == 0 &&
		len(s.storage) == 0 &&
		len(s.networking) == 0 &&
		len(s.architecture) == 0
}

func inspectTechStackSignals(text string) techStackSignals {
	lines := strings.Split(text, "\n")
	signalSet := func(values ...string) []string {
		seen := make(map[string]struct{}, len(values))
		out := make([]string, 0, len(values))
		for _, value := range values {
			value = strings.TrimSpace(value)
			if value == "" {
				continue
			}
			if _, ok := seen[value]; ok {
				continue
			}
			seen[value] = struct{}{}
			out = append(out, value)
		}
		sort.Strings(out)
		return out
	}

	var (
		frameworks   []string
		libraries    []string
		storage      []string
		networking   []string
		architecture []string
		language     string
	)

	addMatches := func(lower string, rules map[string]string, dst *[]string) {
		for pattern, label := range rules {
			if strings.Contains(lower, pattern) {
				*dst = append(*dst, label)
			}
		}
	}

	for _, raw := range lines {
		line := normalizeToolResultLine(raw)
		if line == "" {
			continue
		}
		lower := strings.ToLower(line)

		switch {
		case strings.HasPrefix(lower, "import "), strings.HasPrefix(lower, "from "), strings.Contains(lower, "def "), strings.Contains(lower, ".py"):
			if language == "" {
				language = "Python"
			}
		case strings.Contains(lower, "package.json"), strings.Contains(lower, ".ts"), strings.Contains(lower, ".tsx"), strings.Contains(lower, ".js"), strings.Contains(lower, ".jsx"), strings.Contains(lower, "npm "), strings.Contains(lower, "pnpm "), strings.Contains(lower, "yarn "):
			if language == "" {
				language = "Node.js / TypeScript"
			}
		case strings.Contains(lower, "go.mod"), strings.Contains(lower, "package main"), strings.Contains(lower, "func "), strings.Contains(lower, ".go"):
			if language == "" {
				language = "Go"
			}
		}

		addMatches(lower, map[string]string{
			"fastapi":   "FastAPI",
			"flask":     "Flask",
			"django":    "Django",
			"streamlit": "Streamlit",
			"uvicorn":   "Uvicorn",
			"gin ":      "Gin",
			"echo.":     "Echo",
			"fiber":     "Fiber",
			"react":     "React",
			"next.js":   "Next.js",
			"next/":     "Next.js",
			"vue":       "Vue",
			"express":   "Express",
		}, &frameworks)

		addMatches(lower, map[string]string{
			"requests":                "requests",
			"httpx":                   "httpx",
			"aiohttp":                 "aiohttp",
			"urllib.request":          "urllib",
			"socks":                   "socks",
			"sqlalchemy":              "SQLAlchemy",
			"redis":                   "Redis",
			"pandas":                  "pandas",
			"numpy":                   "numpy",
			"beautifulsoup":           "BeautifulSoup",
			"bs4":                     "BeautifulSoup",
			"playwright":              "Playwright",
			"selenium":                "Selenium",
			"undetected_chromedriver": "undetected_chromedriver",
		}, &libraries)

		if strings.Contains(lower, "json.load") || strings.Contains(lower, "json.dump") || strings.Contains(lower, ".json") {
			storage = append(storage, "本地 JSON 文件")
		}
		if strings.Contains(lower, "sqlite") {
			storage = append(storage, "SQLite")
		}
		if strings.Contains(lower, "postgres") || strings.Contains(lower, "postgresql") {
			storage = append(storage, "PostgreSQL")
		}
		if strings.Contains(lower, "mysql") {
			storage = append(storage, "MySQL")
		}
		if strings.Contains(lower, "redis") {
			storage = append(storage, "Redis")
		}
		if strings.Contains(lower, "urllib.request") || strings.Contains(lower, "requests") || strings.Contains(lower, "httpx") || strings.Contains(lower, "aiohttp") {
			networking = append(networking, "HTTP 抓取 / API 调用")
		}
		if strings.Contains(lower, "socks") || strings.Contains(lower, "proxy") {
			networking = append(networking, "代理支持")
		}
		if strings.Contains(lower, "api.py") || strings.Contains(lower, "fastapi") || strings.Contains(lower, "uvicorn") {
			architecture = append(architecture, "API 服务")
		}
		if strings.Contains(lower, "dashboard.py") || strings.Contains(lower, "streamlit") {
			architecture = append(architecture, "仪表盘 / Web 界面")
		}
		if strings.Contains(lower, "media_mapping.json") || strings.Contains(lower, "alerts") || strings.Contains(lower, "local_media_paths") {
			architecture = append(architecture, "文件系统 + JSON 数据流")
		}
	}

	return techStackSignals{
		language:     language,
		frameworks:   signalSet(frameworks...),
		libraries:    signalSet(libraries...),
		storage:      signalSet(storage...),
		networking:   signalSet(networking...),
		architecture: signalSet(architecture...),
	}
}

func (s techStackSignals) renderChinese() string {
	var segments []string
	if s.language != "" {
		segments = append(segments, fmt.Sprintf("从当前已读取内容看，这是一个 %s 项目", s.language))
	}
	if len(s.frameworks) > 0 {
		segments = append(segments, "主要框架/运行组件包括 "+strings.Join(s.frameworks, "、"))
	}
	if len(s.libraries) > 0 {
		segments = append(segments, "还能看到 "+strings.Join(limitTechStackList(s.libraries, 4), "、")+" 等库")
	}
	if len(s.storage) > 0 {
		segments = append(segments, "数据层更像是 "+strings.Join(limitTechStackList(s.storage, 3), "、"))
	}
	if len(s.networking) > 0 {
		segments = append(segments, "同时包含 "+strings.Join(limitTechStackList(s.networking, 2), "、"))
	}
	if len(s.architecture) > 0 {
		segments = append(segments, "整体形态接近 "+strings.Join(limitTechStackList(s.architecture, 3), " + "))
	}
	if len(segments) == 0 {
		return ""
	}
	return strings.Join(segments, "，") + "。当前结论只基于这次已读取的文件，完整技术栈还需要再看 README、requirements.txt、package.json 或 go.mod。"
}

func (s techStackSignals) renderEnglish() string {
	var segments []string
	if s.language != "" {
		segments = append(segments, fmt.Sprintf("From the file I have so far, this looks like a %s project", s.language))
	}
	if len(s.frameworks) > 0 {
		segments = append(segments, "the main framework/runtime pieces include "+strings.Join(s.frameworks, ", "))
	}
	if len(s.libraries) > 0 {
		segments = append(segments, "with libraries like "+strings.Join(limitTechStackList(s.libraries, 4), ", "))
	}
	if len(s.storage) > 0 {
		segments = append(segments, "and storage leaning on "+strings.Join(limitTechStackList(s.storage, 3), ", "))
	}
	if len(s.networking) > 0 {
		segments = append(segments, "plus "+strings.Join(limitTechStackList(s.networking, 2), ", "))
	}
	if len(s.architecture) > 0 {
		segments = append(segments, "so the shape is closer to "+strings.Join(limitTechStackList(s.architecture, 3), " + "))
	}
	if len(segments) == 0 {
		return ""
	}
	return strings.Join(segments, ", ") + ". This is still based on a partial file read, so I would need README, requirements.txt, package.json, or go.mod for a complete stack summary."
}

func limitTechStackList(values []string, max int) []string {
	if len(values) <= max || max <= 0 {
		return values
	}
	return values[:max]
}

func isTopicClassifierRequest(req ClaudeRequest) bool {
	for _, item := range req.System {
		if strings.ToLower(strings.TrimSpace(item.Type)) != "text" {
			continue
		}
		lower := strings.ToLower(stripSystemRemindersForMode(item.Text))
		if strings.Contains(lower, "new conversation topic") &&
			strings.Contains(lower, "isnewtopic") &&
			strings.Contains(lower, "json object") &&
			strings.Contains(lower, "title") {
			return true
		}
	}
	return false
}

func classifyTopicRequest(req ClaudeRequest) (bool, string) {
	userTexts := extractUserTexts(req.Messages)
	if len(userTexts) == 0 {
		return false, ""
	}

	latest := strings.TrimSpace(userTexts[len(userTexts)-1])
	if latest == "" {
		return false, ""
	}

	prev := ""
	if len(userTexts) >= 2 {
		prev = strings.TrimSpace(userTexts[len(userTexts)-2])
	}

	if prev == "" {
		return true, generateTopicTitle(latest)
	}

	if isGreetingText(latest) {
		return false, ""
	}

	latestNorm := normalizeTopicText(latest)
	prevNorm := normalizeTopicText(prev)
	if latestNorm == "" || prevNorm == "" {
		return latest != prev, generateTopicTitle(latest)
	}
	if latestNorm == prevNorm || strings.Contains(latestNorm, prevNorm) || strings.Contains(prevNorm, latestNorm) {
		return false, ""
	}
	return true, generateTopicTitle(latest)
}

func extractUserTexts(messages []prompt.Message) []string {
	texts := make([]string, 0, len(messages))
	for _, msg := range messages {
		if strings.ToLower(strings.TrimSpace(msg.Role)) != "user" {
			continue
		}
		text := strings.TrimSpace(stripSystemRemindersForMode(msg.ExtractText()))
		if text != "" {
			texts = append(texts, text)
		}
	}
	return texts
}

func isGreetingText(text string) bool {
	lower := strings.ToLower(strings.TrimSpace(text))
	switch lower {
	case "hi", "hello", "hey", "你好", "您好", "嗨", "在吗":
		return true
	default:
		return false
	}
}

func normalizeTopicText(text string) string {
	text = strings.ToLower(strings.TrimSpace(text))
	if text == "" {
		return ""
	}
	var b strings.Builder
	b.Grow(len(text))
	for _, r := range text {
		if unicode.IsSpace(r) || unicode.IsPunct(r) {
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

func generateTopicTitle(text string) string {
	trimmed := strings.TrimSpace(text)
	if trimmed == "" {
		return "New Topic"
	}
	words := strings.Fields(trimmed)
	if len(words) >= 2 {
		if len(words) > 3 {
			words = words[:3]
		}
		return strings.Join(words, " ")
	}
	runes := []rune(trimmed)
	if len(runes) > 10 {
		runes = runes[:10]
	}
	return strings.TrimSpace(string(runes))
}

// stripSystemRemindersForMode 移除 <system-reminder>...</system-reminder>，避免误判 plan/suggestion 模式
// 使用 LastIndex 查找结束标签，正确处理嵌套的字面量标签
func stripSystemRemindersForMode(text string) string {
	text = stripNestedModeTaggedBlock(text, "system-reminder")
	for _, tag := range []string{
		"local-command-caveat",
		"command-name",
		"command-message",
		"command-args",
		"local-command-stdout",
		"local-command-stderr",
		"local-command-exit-code",
	} {
		text = stripSimpleModeTaggedBlock(text, tag)
	}
	return text
}

func stripNestedModeTaggedBlock(text string, tag string) string {
	startTag := "<" + tag + ">"
	endTag := "</" + tag + ">"
	if !strings.Contains(text, startTag) {
		return text
	}
	var sb strings.Builder
	sb.Grow(len(text))
	i := 0
	for i < len(text) {
		start := strings.Index(text[i:], startTag)
		if start == -1 {
			sb.WriteString(text[i:])
			break
		}
		sb.WriteString(text[i : i+start])
		blockStart := i + start
		endStart := blockStart + len(startTag)
		end := strings.LastIndex(text[endStart:], endTag)
		if end == -1 {
			sb.WriteString(text[blockStart:])
			break
		}
		i = endStart + end + len(endTag)
	}
	return sb.String()
}

func stripSimpleModeTaggedBlock(text string, tag string) string {
	startTag := "<" + tag + ">"
	endTag := "</" + tag + ">"
	if !strings.Contains(text, startTag) {
		return text
	}
	var sb strings.Builder
	sb.Grow(len(text))
	i := 0
	for i < len(text) {
		start := strings.Index(text[i:], startTag)
		if start == -1 {
			sb.WriteString(text[i:])
			break
		}
		sb.WriteString(text[i : i+start])
		blockStart := i + start
		endStart := blockStart + len(startTag)
		end := strings.Index(text[endStart:], endTag)
		if end == -1 {
			sb.WriteString(text[blockStart:])
			break
		}
		i = endStart + end + len(endTag)
	}
	return sb.String()
}
