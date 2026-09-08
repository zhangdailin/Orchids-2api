package handler

import (
	"net/http"
	"regexp"
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

// mapModel 将请求的 model 名称映射为上游实际支持的规范化模型 ID。
func mapModel(requestModel string) string {
	normalized := strings.ToLower(strings.TrimSpace(requestModel))
	if strings.HasPrefix(normalized, "claude-") {
		normalized = strings.ReplaceAll(normalized, "4.6", "4-6")
		normalized = strings.ReplaceAll(normalized, "4.5", "4-5")
	}
	if normalized == "" {
		return "claude-sonnet-4-6"
	}
	if mapped, ok := modelMap[normalized]; ok {
		return mapped
	}
	return "claude-sonnet-4-6"
}

// modelMap 维护跨通道共享的模型别名到上游模型 ID 的规范化映射。
var modelMap = map[string]string{
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

func warpRequestRequiresCloudAgent(messages []prompt.Message, tools []interface{}) bool {
	if len(tools) > 0 {
		return true
	}
	if messagesContainToolExchange(messages) {
		return true
	}
	return looksLikeWarpAgentIntent(lastNonSuggestionUserText(messages))
}

func messagesContainToolExchange(messages []prompt.Message) bool {
	for _, msg := range messages {
		if msg.Content.IsString() {
			continue
		}
		for _, block := range msg.Content.GetBlocks() {
			switch strings.TrimSpace(block.Type) {
			case "tool_use", "tool_result":
				return true
			}
		}
	}
	return false
}

func looksLikeWarpAgentIntent(text string) bool {
	lower := strings.ToLower(strings.TrimSpace(stripSystemRemindersForMode(text)))
	if lower == "" {
		return false
	}

	for _, marker := range []string{
		"创建文件", "生成文件", "修改文件", "编辑文件", "保存到", "写入",
		"执行命令", "运行命令", "跑一下", "跑测试", "运行测试", "编译", "构建",
		"修复代码", "改代码", "写代码", "代码实现", "项目里", "仓库里",
		"update the file", "create file", "write file", "save to",
		"run command", "execute command", "compile", "run tests", "fix the code", "write code",
	} {
		if strings.Contains(lower, marker) {
			return true
		}
	}

	if looksLikeSourceOrCommandSubject(lower) {
		for _, action := range []string{
			"帮我", "写", "生成", "创建", "实现", "修改", "修复", "运行", "执行", "编译", "构建",
			"write", "create", "generate", "build", "implement", "modify", "edit", "fix", "run", "execute", "compile",
		} {
			if strings.Contains(lower, action) {
				return true
			}
		}
	}

	return false
}

func looksLikeSourceOrCommandSubject(lower string) bool {
	for _, marker := range []string{
		"python", "golang", " go ", "javascript", "typescript", "node", "react", "vue", "java", "rust", "php", "ruby",
		"代码", "源码", "函数", "类", "接口", "脚本", "计算器", "文件", "项目", "仓库", "应用",
		"code", "function", "class", "api", "calculator", "file", "project", "repo", "repository", " app", "application",
		".py", ".go", ".js", ".ts", ".tsx", ".jsx", ".java", ".rs", ".php", ".rb", "package.json", "go.mod",
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

const clientEmptyOutputRecoveryMarker = "[Your previous response had no visible output. Please continue and produce a user-visible response.]"

func isClientEmptyOutputRecoveryText(text string) bool {
	return strings.TrimSpace(text) == clientEmptyOutputRecoveryMarker
}

func emptyOutputRecoveryPrefix(messages []prompt.Message) ([]prompt.Message, bool) {
	if len(messages) < 3 {
		return messages, false
	}
	last := messages[len(messages)-1]
	if !strings.EqualFold(strings.TrimSpace(last.Role), "user") ||
		!last.Content.IsString() ||
		!isClientEmptyOutputRecoveryText(last.Content.GetText()) {
		return messages, false
	}
	previous := messages[len(messages)-2]
	if !strings.EqualFold(strings.TrimSpace(previous.Role), "assistant") || strings.TrimSpace(previous.ExtractText()) != "" {
		return messages, false
	}
	if !previous.Content.IsString() {
		for _, block := range previous.Content.GetBlocks() {
			if block.Type != "text" || strings.TrimSpace(block.Text) != "" {
				return messages, false
			}
		}
	}
	return messages[:len(messages)-2], true
}

func successfulFileMutationToolResultFallback(messages []prompt.Message) string {
	effective, _ := emptyOutputRecoveryPrefix(messages)
	if len(effective) == 0 {
		return ""
	}

	toolNames := make(map[string]string)
	for _, msg := range effective {
		if !strings.EqualFold(strings.TrimSpace(msg.Role), "assistant") || msg.Content.IsString() {
			continue
		}
		for _, block := range msg.Content.GetBlocks() {
			if block.Type == "tool_use" && strings.TrimSpace(block.ID) != "" {
				toolNames[block.ID] = normalizeToolNameKey(block.Name)
			}
		}
	}

	last := effective[len(effective)-1]
	if !strings.EqualFold(strings.TrimSpace(last.Role), "user") || last.Content.IsString() {
		return ""
	}
	count := 0
	for _, block := range last.Content.GetBlocks() {
		switch block.Type {
		case "text":
			if strings.TrimSpace(block.Text) != "" {
				return ""
			}
		case "tool_result":
			if block.IsError || looksLikeToolResultFailure(extractToolResultContent(block.Content)) {
				return ""
			}
			switch toolNames[block.ToolUseID] {
			case "write", "edit", "notebookedit":
				count++
			default:
				return ""
			}
		default:
			return ""
		}
	}
	if count == 0 {
		return ""
	}
	if count == 1 {
		return "File operation completed successfully."
	}
	return "Requested file operations completed successfully."
}

func buildEmptyOutputRecoveryPrompt(messages []prompt.Message) string {
	if _, ok := emptyOutputRecoveryPrefix(messages); !ok {
		return ""
	}
	confirmation := successfulFileMutationToolResultFallback(messages)
	if confirmation == "" {
		return ""
	}
	return confirmation + " Confirm this result to the user in one concise sentence. Do not call tools and do not repeat the file operation."
}

func lastNonToolResultUserText(messages []prompt.Message) string {
	for i := len(messages) - 1; i >= 0; i-- {
		msg := messages[i]
		if !strings.EqualFold(strings.TrimSpace(msg.Role), "user") {
			continue
		}
		if msg.Content.IsString() {
			text := strings.TrimSpace(stripSystemRemindersForMode(msg.Content.GetText()))
			if text != "" && !containsSuggestionMode(text) && !isClientEmptyOutputRecoveryText(text) {
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

func containsNormalizedRequestText(text string, markers ...string) bool {
	lower := strings.ToLower(strings.TrimSpace(stripSystemRemindersForMode(text)))
	for _, marker := range markers {
		if strings.Contains(lower, marker) {
			return true
		}
	}
	return false
}

func looksLikeOptimizationRequest(text string) bool {
	return containsNormalizedRequestText(text,
		"怎么优化", "如何优化", "优化建议", "性能怎么优化", "重构建议", "改进建议",
		"帮我优化", "优化这个项目", "项目优化", "优化下这个项目", "帮我改进这个项目",
		"优化这个方案", "帮我优化这个方案", "优化这个设计", "帮我优化这个设计", "优化这个实现", "帮我优化这个实现",
		"how to optimize", "optimization advice", "performance optimization", "refactor suggestions", "improvement suggestions",
		"optimize this plan", "optimize this design", "optimize this implementation",
	)
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
	text = stripModeTaggedBlock(text, "system-reminder", true)
	for _, tag := range []string{
		"local-command-caveat",
		"command-name",
		"command-message",
		"command-args",
		"local-command-stdout",
		"local-command-stderr",
		"local-command-exit-code",
	} {
		text = stripModeTaggedBlock(text, tag, false)
	}
	return text
}

func stripModeTaggedBlock(text string, tag string, nested bool) string {
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
		if nested {
			end = strings.LastIndex(text[endStart:], endTag)
		}
		if end == -1 {
			sb.WriteString(text[blockStart:])
			break
		}
		i = endStart + end + len(endTag)
	}
	return sb.String()
}
