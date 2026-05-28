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

func hasInterruptedRetryMarker(messages []prompt.Message) bool {
	for _, msg := range messages {
		if strings.ToLower(strings.TrimSpace(msg.Role)) != "user" {
			continue
		}
		text := strings.TrimSpace(stripSystemRemindersForMode(msg.ExtractText()))
		if strings.Contains(text, "[Request interrupted by user]") {
			return true
		}
	}
	return false
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

func isTitleGenerationRequest(req ClaudeRequest) bool {
	hasTitleInstruction := false
	hasJSONInstruction := false

	for _, item := range req.System {
		if strings.ToLower(strings.TrimSpace(item.Type)) != "text" {
			continue
		}
		lower := strings.ToLower(stripSystemRemindersForMode(item.Text))
		if strings.Contains(lower, "generate a concise, sentence-case title") ||
			(strings.Contains(lower, "sentence-case title") && strings.Contains(lower, "coding session")) {
			hasTitleInstruction = true
		}
		if strings.Contains(lower, "return json with a single \"title\" field") ||
			(strings.Contains(lower, "return json") && strings.Contains(lower, "single") && strings.Contains(lower, "\"title\"")) {
			hasJSONInstruction = true
		}
	}

	return hasTitleInstruction && hasJSONInstruction
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
