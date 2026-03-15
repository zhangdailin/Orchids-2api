package promptbuilder

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/goccy/go-json"

	"orchids-api/internal/prompt"
	"orchids-api/internal/util"
)

var jwtLikePattern = regexp.MustCompile(`\beyJ[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}\b`)

type toolResultEvidence struct {
	ToolName string
	FilePath string
	Content  string
}

func stripSystemReminders(text string) string {
	text = stripNestedTaggedBlock(text, "system-reminder")
	for _, tag := range []string{
		"local-command-caveat",
		"command-name",
		"command-message",
		"command-args",
		"local-command-stdout",
		"local-command-stderr",
		"local-command-exit-code",
	} {
		text = stripSimpleTaggedBlock(text, tag)
	}
	return strings.TrimSpace(text)
}

func stripNestedTaggedBlock(text string, tag string) string {
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
		endStart := i + start + len(startTag)
		end := strings.LastIndex(text[endStart:], endTag)
		if end == -1 {
			sb.WriteString(text[i+start:])
			break
		}
		i = endStart + end + len(endTag)
	}
	return sb.String()
}

func stripSimpleTaggedBlock(text string, tag string) string {
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

func extractWarpUserMessage(messages []prompt.Message) string {
	for i := len(messages) - 1; i >= 0; i-- {
		msg := messages[i]
		if msg.Role != "user" {
			continue
		}
		text := extractWarpMessageText(msg.Content)
		if strings.TrimSpace(text) != "" {
			return text
		}
	}
	return ""
}

func extractWarpMessageText(content prompt.MessageContent) string {
	if content.IsString() {
		return stripSystemReminders(content.GetText())
	}
	var parts []string
	for _, block := range content.GetBlocks() {
		switch block.Type {
		case "text":
			text := stripSystemReminders(block.Text)
			if text != "" {
				parts = append(parts, text)
			}
		case "tool_result":
			text := formatToolResultContentLocal(block.Content)
			text = strings.ReplaceAll(text, "<tool_use_error>", "")
			text = strings.ReplaceAll(text, "</tool_use_error>", "")
			text = stripSystemReminders(text)
			if text != "" {
				parts = append(parts, text)
			}
		case "image", "document":
			parts = append(parts, formatMediaHint(block))
		}
	}
	return strings.TrimSpace(strings.Join(parts, "\n"))
}

func convertWarpChatHistory(messages []prompt.Message) []map[string]string {
	var history []map[string]string
	for _, msg := range messages {
		if msg.Role != "user" && msg.Role != "assistant" {
			continue
		}
		if msg.Role == "user" {
			if msg.Content.IsString() {
				text := stripSystemReminders(msg.Content.GetText())
				if text != "" {
					history = append(history, map[string]string{"role": "user", "content": text})
				}
				continue
			}
			blocks := msg.Content.GetBlocks()
			var textParts []string
			hasValidContent := false
			for _, block := range blocks {
				switch block.Type {
				case "text":
					text := stripSystemReminders(block.Text)
					if text != "" {
						textParts = append(textParts, text)
						hasValidContent = true
					}
				case "tool_result":
					contentText := formatToolResultContentLocalForHistory(block.Content)
					contentText = strings.ReplaceAll(contentText, "<tool_use_error>", "")
					contentText = strings.ReplaceAll(contentText, "</tool_use_error>", "")
					contentText = stripSystemReminders(contentText)
					if contentText != "" {
						textParts = append(textParts, contentText)
						hasValidContent = true
					}
				case "image", "document":
					textParts = append(textParts, formatMediaHint(block))
					hasValidContent = true
				}
			}
			if !hasValidContent {
				continue
			}
			text := strings.TrimSpace(strings.Join(textParts, "\n"))
			if text != "" {
				history = append(history, map[string]string{"role": "user", "content": text})
			}
			continue
		}

		if msg.Content.IsString() {
			text := stripSystemReminders(msg.Content.GetText())
			if text != "" {
				history = append(history, map[string]string{"role": "assistant", "content": text})
			}
			continue
		}
		var parts []string
		for _, block := range msg.Content.GetBlocks() {
			switch block.Type {
			case "text":
				text := stripSystemReminders(block.Text)
				if text != "" {
					parts = append(parts, text)
				}
			case "tool_use":
				inputJSON, _ := json.Marshal(block.Input)
				parts = append(parts, fmt.Sprintf("[Used tool: %s with input: %s]", block.Name, string(inputJSON)))
			case "image", "document":
				parts = append(parts, formatMediaHint(block))
			}
		}
		text := strings.TrimSpace(strings.Join(parts, "\n"))
		if text != "" {
			history = append(history, map[string]string{"role": "assistant", "content": text})
		}
	}
	return history
}

func formatMediaHint(block prompt.ContentBlock) string {
	sourceType := "unknown"
	mediaType := "unknown"
	sizeHint := ""
	if block.Source != nil {
		if strings.TrimSpace(block.Source.Type) != "" {
			sourceType = block.Source.Type
		}
		if strings.TrimSpace(block.Source.MediaType) != "" {
			mediaType = block.Source.MediaType
		}
		if block.Source.Data != "" {
			approx := int(float64(len(block.Source.Data)) * 0.75)
			sizeHint = fmt.Sprintf(" bytes≈%d", approx)
		}
	}
	switch block.Type {
	case "image":
		return fmt.Sprintf("[Image %s %s%s]", mediaType, sourceType, sizeHint)
	case "document":
		return fmt.Sprintf("[Document %s%s]", sourceType, sizeHint)
	default:
		return "[Document unknown]"
	}
}

func hasUserPlainText(msg prompt.Message) bool {
	if msg.Role != "user" {
		return false
	}
	if msg.Content.IsString() {
		return stripSystemReminders(msg.Content.GetText()) != ""
	}
	for _, block := range msg.Content.GetBlocks() {
		if block.Type == "text" && stripSystemReminders(block.Text) != "" {
			return true
		}
	}
	return false
}

func findLatestUserText(messages []prompt.Message) string {
	for i := len(messages) - 1; i >= 0; i-- {
		msg := messages[i]
		if msg.Role != "user" {
			continue
		}
		if msg.Content.IsString() {
			text := stripSystemReminders(msg.Content.GetText())
			if text != "" {
				return text
			}
		} else {
			var parts []string
			for _, block := range msg.Content.GetBlocks() {
				if block.Type != "text" {
					continue
				}
				text := stripSystemReminders(block.Text)
				if text != "" {
					parts = append(parts, text)
				}
			}
			if len(parts) > 0 {
				return strings.TrimSpace(strings.Join(parts, "\n"))
			}
		}
	}
	return ""
}

func extractSystemPrompt(messages []prompt.Message) string {
	var parts []string
	for _, msg := range messages {
		if msg.Role != "system" {
			continue
		}
		if msg.Content.IsString() {
			text := stripSystemReminders(msg.Content.GetText())
			if text != "" {
				parts = append(parts, text)
			}
			continue
		}
		for _, block := range msg.Content.GetBlocks() {
			if block.Type != "text" {
				continue
			}
			text := stripSystemReminders(block.Text)
			if text != "" {
				parts = append(parts, text)
			}
		}
	}
	return strings.Join(parts, "\n\n")
}

func resolveCurrentUserTurnText(messages []prompt.Message, currentUserIdx int, userText string) string {
	userText = strings.TrimSpace(stripSystemReminders(userText))
	if currentUserIdx < 0 || currentUserIdx >= len(messages) {
		return userText
	}
	if hasUserPlainText(messages[currentUserIdx]) {
		return userText
	}
	userText = buildAttributedCurrentToolResultText(messages, currentUserIdx, userText)
	previousText := strings.TrimSpace(findLatestUserText(messages[:currentUserIdx]))
	if previousText == "" {
		return userText
	}
	if userText == "" {
		return previousText
	}
	if userText == previousText {
		return userText
	}
	followUp := buildToolResultFollowUpUserText(previousText, userText)
	if guidance := buildOptimizationToolFollowUpGuidance(messages[:currentUserIdx+1], previousText); guidance != "" {
		followUp = strings.TrimSpace(followUp + "\n\n" + guidance)
	}
	return followUp
}

func buildAttributedCurrentToolResultText(messages []prompt.Message, currentUserIdx int, fallback string) string {
	if currentUserIdx < 0 || currentUserIdx >= len(messages) {
		return strings.TrimSpace(fallback)
	}
	msg := messages[currentUserIdx]
	if msg.Content.IsString() {
		return strings.TrimSpace(fallback)
	}

	toolUses := make(map[string]toolResultEvidence)
	for i := 0; i < currentUserIdx; i++ {
		if !strings.EqualFold(strings.TrimSpace(messages[i].Role), "assistant") || messages[i].Content.IsString() {
			continue
		}
		for _, block := range messages[i].Content.GetBlocks() {
			if block.Type != "tool_use" {
				continue
			}
			toolUses[block.ID] = toolResultEvidence{
				ToolName: strings.TrimSpace(block.Name),
				FilePath: extractToolUsePath(block.Input),
				Content:  extractToolUseCommand(block.Input),
			}
		}
	}

	var parts []string
	for _, block := range msg.Content.GetBlocks() {
		switch block.Type {
		case "text":
			text := strings.TrimSpace(stripSystemReminders(block.Text))
			if text != "" {
				parts = append(parts, text)
			}
		case "tool_result":
			text := strings.TrimSpace(util.NormalizePersistedToolResultText(extractToolResultText(block.Content)))
			text = strings.ReplaceAll(text, "<tool_use_error>", "")
			text = strings.ReplaceAll(text, "</tool_use_error>", "")
			text = strings.TrimSpace(stripSystemReminders(text))
			if text == "" {
				continue
			}
			if label := formatAttributedToolResultLabel(toolUses[block.ToolUseID]); label != "" {
				parts = append(parts, fmt.Sprintf("[%s]\n%s", label, text))
			} else {
				parts = append(parts, text)
			}
		}
	}
	if len(parts) == 0 {
		return strings.TrimSpace(fallback)
	}
	return strings.TrimSpace(strings.Join(parts, "\n\n"))
}

func formatAttributedToolResultLabel(item toolResultEvidence) string {
	name := strings.TrimSpace(item.ToolName)
	if path := strings.TrimSpace(item.FilePath); path != "" {
		if name == "" {
			return path
		}
		return name + " " + path
	}
	if command := strings.TrimSpace(item.Content); command != "" {
		if name == "" {
			return command
		}
		return name + " " + command
	}
	return name
}

func extractToolUseCommand(input interface{}) string {
	m, ok := input.(map[string]interface{})
	if !ok {
		return ""
	}
	for _, key := range []string{"command", "cmd"} {
		if value, ok := m[key].(string); ok {
			value = strings.TrimSpace(value)
			if value != "" {
				return value
			}
		}
	}
	return ""
}

func buildToolResultFollowUpUserText(previousText string, toolResultText string) string {
	previousText = strings.TrimSpace(previousText)
	toolResultText = strings.TrimSpace(toolResultText)
	if previousText == "" {
		return toolResultText
	}
	if toolResultText == "" {
		return previousText
	}

	var b strings.Builder
	b.WriteString("Original user request:\n")
	b.WriteString(previousText)
	b.WriteString("\n\nTool result:\n")
	b.WriteString(toolResultText)
	if isDirectoryListingLikeText(toolResultText) {
		b.WriteString("\n\nInterpret the directory listing from the root entries first. Do not assume the largest nested subdirectory is the whole project.")
		b.WriteString(" Ignore OS metadata like .DS_Store and focus on the most meaningful project files or directories.")
	}
	if looksLikeOptimizationUserText(previousText) {
		b.WriteString("\n\nUse the tool result and your tools to conduct a thorough analysis of the project.")
		b.WriteString(" Read relevant source files and provide comprehensive optimization suggestions.")
	} else {
		b.WriteString("\n\nUse the tool result above to answer the original user request directly.")
		b.WriteString(" Keep the answer concise: at most 2-3 short sentences.")
		b.WriteString(" Do not enumerate every visible entry unless the user explicitly asked for a full listing.")
		b.WriteString(" If the visible structure is insufficient to determine the purpose confidently, say so briefly instead of guessing details.")
	}
	return b.String()
}

func buildOptimizationToolFollowUpGuidance(messages []prompt.Message, previousText string) string {
	if !looksLikeOptimizationUserText(previousText) {
		return ""
	}

	evidence := collectToolResultEvidence(messages)
	if len(evidence) == 0 {
		return ""
	}

	readCounts := make(map[string]int)
	readOrder := make([]string, 0, len(evidence))
	readSeen := make(map[string]struct{})
	rootCandidates := make([]string, 0, 8)
	rootSeen := make(map[string]struct{})

	for _, item := range evidence {
		if strings.EqualFold(strings.TrimSpace(item.ToolName), "Read") {
			base := toolInputBaseName(item.FilePath)
			if base != "" {
				readCounts[base]++
				if _, ok := readSeen[base]; !ok {
					readSeen[base] = struct{}{}
					readOrder = append(readOrder, base)
				}
			}
		}
		for _, candidate := range extractRootImplementationCandidates(item.Content) {
			if _, ok := rootSeen[candidate]; ok {
				continue
			}
			rootSeen[candidate] = struct{}{}
			rootCandidates = append(rootCandidates, candidate)
		}
	}

	if len(readCounts) == 0 {
		return ""
	}

	repeated := ""
	for _, base := range readOrder {
		if readCounts[base] > 1 && looksLikeSourceFileName(base) {
			repeated = base
			break
		}
	}

	unread := make([]string, 0, 4)
	for _, candidate := range rootCandidates {
		if _, ok := readCounts[candidate]; ok {
			continue
		}
		unread = append(unread, candidate)
		if len(unread) >= 3 {
			break
		}
	}

	uniqueImplementationReads := 0
	for base := range readCounts {
		if looksLikeSourceFileName(base) {
			uniqueImplementationReads++
		}
	}

	if repeated != "" && len(unread) > 0 {
		return fmt.Sprintf("You already read %s multiple times. Do not read it again unless you need a missing section that is not already shown. Next inspect unread key implementation files such as %s before giving project-wide optimization advice.", repeated, joinGuidanceList(unread))
	}
	if uniqueImplementationReads < 2 && len(unread) > 0 {
		return fmt.Sprintf("For project-wide optimization advice, inspect more than one implementation file. Next inspect unread key implementation files such as %s before concluding.", joinGuidanceList(unread))
	}
	return ""
}

func collectToolResultEvidence(messages []prompt.Message) []toolResultEvidence {
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
					FilePath: extractToolUsePath(block.Input),
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
			text := strings.TrimSpace(util.NormalizePersistedToolResultText(extractToolResultText(block.Content)))
			if text == "" {
				continue
			}
			item := toolUses[block.ToolUseID]
			item.Content = text
			evidence = append(evidence, item)
		}
	}
	return evidence
}

func extractToolUsePath(input interface{}) string {
	if m, ok := input.(map[string]interface{}); ok {
		if path, ok := m["file_path"].(string); ok {
			return strings.TrimSpace(path)
		}
		if path, ok := m["path"].(string); ok {
			return strings.TrimSpace(path)
		}
	}
	return ""
}

func toolInputBaseName(path string) string {
	path = strings.TrimSpace(strings.ReplaceAll(path, "\\", "/"))
	path = strings.TrimSuffix(path, "/")
	if path == "" {
		return ""
	}
	if idx := strings.LastIndex(path, "/"); idx >= 0 {
		return strings.ToLower(strings.TrimSpace(path[idx+1:]))
	}
	return strings.ToLower(path)
}

func extractRootImplementationCandidates(text string) []string {
	lines := strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n")
	var out []string
	seen := make(map[string]struct{})
	for _, rawLine := range lines {
		line := normalizeRootImplementationCandidate(rawLine)
		if line == "" || !looksLikeSourceFileName(line) || !looksLikeKeyImplementationCandidate(line) {
			continue
		}
		if _, ok := seen[line]; ok {
			continue
		}
		seen[line] = struct{}{}
		out = append(out, line)
	}
	return out
}

func normalizeRootImplementationCandidate(rawLine string) string {
	line := strings.TrimSpace(rawLine)
	if line == "" || strings.HasPrefix(line, "[") {
		return ""
	}
	if idx := strings.Index(line, "→"); idx >= 0 {
		line = strings.TrimSpace(line[idx+len("→"):])
	}
	if idx := strings.Index(line, " -> "); idx >= 0 {
		line = strings.TrimSpace(line[:idx])
	}
	line = strings.TrimSpace(strings.TrimPrefix(line, "./"))
	if fields := strings.Fields(line); len(fields) > 0 {
		last := strings.TrimSpace(fields[len(fields)-1])
		if looksLikeSourceFileName(last) || looksLikeKeyImplementationCandidate(last) {
			line = last
		}
	}
	line = strings.TrimSpace(strings.TrimRight(line, "/"))
	if idx := strings.LastIndex(line, "/"); idx >= 0 {
		line = strings.TrimSpace(line[idx+1:])
	}
	line = strings.ToLower(line)
	switch line {
	case "", ".", "..":
		return ""
	default:
		return line
	}
}

func looksLikeSourceFileName(name string) bool {
	for _, suffix := range []string{".py", ".go", ".js", ".jsx", ".ts", ".tsx", ".java", ".rb", ".rs", ".php", ".cs", ".cpp"} {
		if strings.HasSuffix(strings.ToLower(strings.TrimSpace(name)), suffix) {
			return true
		}
	}
	return false
}

func looksLikeKeyImplementationCandidate(name string) bool {
	name = strings.ToLower(strings.TrimSpace(name))
	switch name {
	case "api.py", "app.py", "server.py", "main.py", "dashboard.py", "utils.py", "monitor_trump.py",
		"main.go", "server.go", "main.ts", "index.ts", "index.js", "index.tsx", "index.jsx":
		return true
	default:
		return false
	}
}

func joinGuidanceList(items []string) string {
	if len(items) == 0 {
		return ""
	}
	if len(items) == 1 {
		return items[0]
	}
	if len(items) == 2 {
		return items[0] + " and " + items[1]
	}
	return strings.Join(items[:len(items)-1], ", ") + ", and " + items[len(items)-1]
}

func looksLikeOptimizationUserText(text string) bool {
	lower := strings.ToLower(strings.TrimSpace(text))
	if lower == "" {
		return false
	}
	for _, marker := range []string{
		"怎么优化", "如何优化", "优化建议", "性能怎么优化", "重构建议", "改进建议",
		"帮我优化", "优化这个项目", "项目优化", "优化下这个项目", "帮我改进这个项目",
		"优化这个方案", "帮我优化这个方案", "优化这个设计", "帮我优化这个设计",
		"how to optimize", "optimization advice", "performance optimization",
		"refactor suggestions", "improvement suggestions", "深入分析", "深层分析", "deep analysis", "in-depth analysis",
	} {
		if strings.Contains(lower, marker) {
			return true
		}
	}
	return false
}

func findCurrentUserMessageIndex(messages []prompt.Message) int {
	if len(messages) == 0 {
		return -1
	}
	for i := len(messages) - 1; i >= 0; i-- {
		msg := messages[i]
		if msg.Role != "user" {
			continue
		}
		content := msg.Content
		if content.IsString() {
			if strings.TrimSpace(content.GetText()) != "" {
				return i
			}
			continue
		}
		blocks := content.GetBlocks()
		if len(blocks) == 0 {
			continue
		}
		for _, block := range blocks {
			switch block.Type {
			case "tool_result", "image", "document":
				return i
			case "text":
				if stripSystemReminders(block.Text) != "" {
					return i
				}
			}
		}
	}
	return -1
}

func isToolResultOnlyUserMessage(messages []prompt.Message, idx int) bool {
	if idx < 0 || idx >= len(messages) {
		return false
	}
	msg := messages[idx]
	if msg.Role != "user" || msg.Content.IsString() {
		return false
	}
	blocks := msg.Content.GetBlocks()
	if len(blocks) == 0 {
		return false
	}
	hasToolResult := false
	for _, block := range blocks {
		switch block.Type {
		case "tool_result":
			hasToolResult = true
		case "text":
			if strings.TrimSpace(stripSystemReminders(block.Text)) != "" {
				return false
			}
		default:
			if strings.TrimSpace(block.Type) != "" {
				return false
			}
		}
	}
	return hasToolResult
}

func formatToolResultContentLocal(content interface{}) string {
	return formatToolResultContentLocalWithMode(content, false)
}

func formatToolResultContentLocalForHistory(content interface{}) string {
	return formatToolResultContentLocalWithMode(content, true)
}

func formatToolResultContentLocalWithMode(content interface{}, historyMode bool) string {
	raw := util.NormalizePersistedToolResultText(extractToolResultText(content))
	raw = redactSensitiveToolResultText(raw)
	return compactLocalToolResultText(raw, historyMode)
}

func redactSensitiveToolResultText(text string) string {
	text = strings.TrimSpace(text)
	if text == "" {
		return ""
	}
	return jwtLikePattern.ReplaceAllString(text, "[redacted_jwt]")
}

func extractToolResultText(content interface{}) string {
	switch v := content.(type) {
	case string:
		return strings.TrimSpace(v)
	case []interface{}:
		var parts []string
		for _, item := range v {
			if itemMap, ok := item.(map[string]interface{}); ok {
				if text, ok := itemMap["text"].(string); ok {
					parts = append(parts, strings.TrimSpace(text))
				}
			}
		}
		if len(parts) > 0 {
			return strings.Join(parts, "\n")
		}
		raw, _ := json.Marshal(v)
		return string(raw)
	default:
		raw, _ := json.Marshal(v)
		return string(raw)
	}
}

func compactLocalToolResultText(text string, historyMode bool) string {
	text = strings.TrimSpace(strings.ReplaceAll(text, "\r\n", "\n"))
	if text == "" {
		return ""
	}
	if looksLikeDirectoryListing(text) {
		return compactDirectoryListingResult(text, historyMode)
	}
	if historyMode {
		return compactHistoricalToolResultText(text)
	}
	return text
}

func looksLikeDirectoryListing(text string) bool {
	lines := nonEmptyTrimmedLines(text)
	if len(lines) < 4 {
		return false
	}
	entryLike := 0
	strongSignals := 0
	for _, line := range lines {
		if looksLikePathLine(line) {
			entryLike++
			strongSignals++
			continue
		}
		if looksLikeBareDirectoryEntryLine(line) {
			entryLike++
			if hasDirectoryEntrySignal(line) {
				strongSignals++
			}
		}
	}
	return entryLike*100/len(lines) >= 70 && strongSignals > 0
}

func isDirectoryListingLikeText(text string) bool {
	return looksLikeDirectoryListing(text) || strings.Contains(text, "[directory listing")
}

func looksLikePathLine(line string) bool {
	line = strings.TrimSpace(line)
	if line == "" {
		return false
	}
	if strings.HasPrefix(line, "/") || strings.HasPrefix(line, "./") || strings.HasPrefix(line, "../") {
		return true
	}
	return len(line) >= 3 && ((line[1] == ':' && line[2] == '\\') || (line[1] == ':' && line[2] == '/'))
}

func looksLikeBareDirectoryEntryLine(line string) bool {
	line = strings.TrimSpace(line)
	if line == "" || strings.ContainsAny(line, "\r\n\t") {
		return false
	}
	lower := strings.ToLower(line)
	if strings.Contains(lower, "results are truncated") ||
		strings.HasPrefix(line, "[") || strings.HasPrefix(line, "<") ||
		strings.HasPrefix(line, "- ") || strings.HasPrefix(line, "* ") || strings.HasPrefix(line, "• ") ||
		strings.Contains(line, ": ") || strings.ContainsAny(line, "{}<>|`") ||
		strings.HasSuffix(line, ".") || strings.HasSuffix(line, "。") || strings.HasSuffix(line, ":") ||
		strings.Count(line, " ") > 2 {
		return false
	}
	return hasDirectoryEntrySignal(line)
}

func hasDirectoryEntrySignal(line string) bool {
	line = strings.TrimSpace(line)
	if line == "" {
		return false
	}
	if strings.HasPrefix(line, ".") {
		return true
	}
	return strings.ContainsAny(line, "._-/\\")
}

func compactDirectoryListingResult(text string, historyMode bool) string {
	lines := nonEmptyTrimmedLines(text)
	if len(lines) == 0 {
		return ""
	}
	total := len(lines)
	pathLines, nonPathCount := splitDirectoryListingLines(lines)
	prefix := sharedDirectoryPrefix(pathLines)

	filtered := make([]string, 0, len(pathLines))
	omittedGit := 0
	for _, line := range pathLines {
		if shouldDropDirectoryListingLine(line) {
			omittedGit++
			continue
		}
		filtered = append(filtered, shortenDirectoryListingLine(line, prefix))
	}
	if len(filtered) == 0 {
		return fmt.Sprintf("[directory listing trimmed: omitted %d git metadata entries and %d non-path lines]", total, nonPathCount)
	}

	if shouldSummarizeDirectoryTopLevel(filtered) {
		summarized, summarizedRoots, omittedRootEntries := summarizeDirectoryListingTopLevel(filtered, historyMode)
		if omittedGit > 0 || nonPathCount > 0 || omittedRootEntries > 0 {
			summarized = append(summarized, fmt.Sprintf("[directory listing summarized: %d root entries from %d lines; omitted %d git metadata entries, %d non-path lines, and %d additional root entries]", summarizedRoots, total, omittedGit, nonPathCount, omittedRootEntries))
		}
		result := strings.Join(summarized, "\n")
		if historyMode {
			return truncateTextWithEllipsis(result, 700)
		}
		return truncateTextWithEllipsis(result, 2200)
	}

	limit := 40
	if historyMode {
		limit = 12
	}
	extra := 0
	keptBeforeSummary := len(filtered)
	if keptBeforeSummary > limit {
		extra = keptBeforeSummary - limit
		filtered = filtered[:limit]
	}
	if omittedGit > 0 || nonPathCount > 0 || extra > 0 {
		filtered = append(filtered, fmt.Sprintf("[directory listing trimmed: kept %d of %d entries; omitted %d git metadata entries, %d non-path lines, and %d extra entries]", keptBeforeSummary-extra, total, omittedGit, nonPathCount, extra))
	}

	result := strings.Join(filtered, "\n")
	if historyMode {
		return truncateTextWithEllipsis(result, 700)
	}
	return truncateTextWithEllipsis(result, 2200)
}

func shouldDropDirectoryListingLine(line string) bool {
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

func splitDirectoryListingLines(lines []string) ([]string, int) {
	pathLines := make([]string, 0, len(lines))
	nonPathCount := 0
	for _, line := range lines {
		if looksLikePathLine(line) || looksLikeBareDirectoryEntryLine(line) {
			pathLines = append(pathLines, line)
			continue
		}
		nonPathCount++
	}
	return pathLines, nonPathCount
}

func shouldSummarizeDirectoryTopLevel(lines []string) bool {
	if len(lines) <= 12 {
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

func summarizeDirectoryListingTopLevel(lines []string, historyMode bool) ([]string, int, int) {
	type rootSummary struct {
		label   string
		samples []string
	}

	maxRoots := 10
	maxSamples := 2
	if historyMode {
		maxRoots = 6
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
		if len(parts) == 0 {
			continue
		}
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
		if sample != "" && len(summary.samples) < maxSamples && !containsString(summary.samples, sample) {
			summary.samples = append(summary.samples, sample)
		}
	}

	out := make([]string, 0, minInt(len(order), maxRoots))
	omitted := 0
	for idx, key := range order {
		if idx >= maxRoots {
			omitted++
			continue
		}
		summary := roots[key]
		if len(summary.samples) == 0 {
			out = append(out, summary.label)
		} else {
			out = append(out, fmt.Sprintf("%s (sample: %s)", summary.label, strings.Join(summary.samples, ", ")))
		}
	}
	return out, minInt(len(order), maxRoots), omitted
}

func shortenDirectoryListingLine(line string, prefix string) string {
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

func sharedDirectoryPrefix(lines []string) string {
	if len(lines) == 0 {
		return ""
	}
	pathLines, _ := splitDirectoryListingLines(lines)
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

func compactHistoricalToolResultText(text string) string {
	text = strings.TrimSpace(text)
	if text == "" {
		return ""
	}
	lines := nonEmptyTrimmedLines(text)
	if len(lines) == 0 {
		return truncateTextWithEllipsis(text, 900)
	}
	if len(lines) > 12 {
		head := lines[:8]
		tail := lines[len(lines)-3:]
		compacted := append([]string{}, head...)
		compacted = append(compacted, fmt.Sprintf("[tool_result summary: omitted %d middle lines]", len(lines)-11))
		compacted = append(compacted, tail...)
		return truncateTextWithEllipsis(strings.Join(compacted, "\n"), 900)
	}
	return truncateTextWithEllipsis(strings.Join(lines, "\n"), 900)
}

func nonEmptyTrimmedLines(text string) []string {
	rawLines := strings.Split(text, "\n")
	lines := make([]string, 0, len(rawLines))
	for _, raw := range rawLines {
		line := strings.TrimSpace(raw)
		if line != "" {
			lines = append(lines, line)
		}
	}
	return lines
}

func truncateTextWithEllipsis(text string, maxLen int) string {
	if maxLen <= 0 {
		return ""
	}
	if len(text) <= maxLen {
		return text
	}
	runes := []rune(text)
	if len(runes) <= maxLen {
		return text
	}
	return string(runes[:maxLen]) + "…[truncated]"
}

func containsString(items []string, target string) bool {
	for _, item := range items {
		if item == target {
			return true
		}
	}
	return false
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}
