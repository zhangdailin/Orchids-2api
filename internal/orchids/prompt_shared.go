package orchids

import (
	"fmt"
	"strings"

	"orchids-api/internal/prompt"
	"orchids-api/internal/util"
)

func hasUserPlainText(msg prompt.Message) bool {
	if msg.Role != "user" {
		return false
	}
	if msg.Content.IsString() {
		return strings.TrimSpace(msg.Content.GetText()) != ""
	}
	for _, block := range msg.Content.GetBlocks() {
		if block.Type != "text" {
			continue
		}
		if strings.TrimSpace(block.Text) != "" {
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
			text := strings.TrimSpace(msg.Content.GetText())
			if text != "" {
				return text
			}
		} else {
			var parts []string
			for _, block := range msg.Content.GetBlocks() {
				if block.Type != "text" {
					continue
				}
				text := strings.TrimSpace(block.Text)
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
			text := strings.TrimSpace(msg.Content.GetText())
			if text != "" {
				parts = append(parts, text)
			}
			continue
		}
		for _, block := range msg.Content.GetBlocks() {
			if block.Type != "text" {
				continue
			}
			text := strings.TrimSpace(block.Text)
			if text != "" {
				parts = append(parts, text)
			}
		}
	}
	return strings.Join(parts, "\n\n")
}

func resolveCurrentUserTurnText(messages []prompt.Message, currentUserIdx int, userText string) string {
	userText = strings.TrimSpace(userText)
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

	toolUses := make(map[string]orchidsToolResultEvidence)
	for i := 0; i < currentUserIdx; i++ {
		if !strings.EqualFold(strings.TrimSpace(messages[i].Role), "assistant") || messages[i].Content.IsString() {
			continue
		}
		for _, block := range messages[i].Content.GetBlocks() {
			if block.Type != "tool_use" {
				continue
			}
			toolUses[block.ID] = orchidsToolResultEvidence{
				ToolName: strings.TrimSpace(block.Name),
				FilePath: extractOrchidsToolUsePath(block.Input),
				Content:  extractOrchidsToolUseCommand(block.Input),
			}
		}
	}

	var parts []string
	for _, block := range msg.Content.GetBlocks() {
		switch block.Type {
		case "text":
			text := strings.TrimSpace(block.Text)
			if text != "" {
				parts = append(parts, text)
			}
		case "tool_result":
			text := strings.TrimSpace(util.NormalizePersistedToolResultText(extractToolResultText(block.Content)))
			text = strings.ReplaceAll(text, "<tool_use_error>", "")
			text = strings.ReplaceAll(text, "</tool_use_error>", "")
			text = strings.TrimSpace(text)
			if text == "" {
				continue
			}
			if label := formatAttributedToolResultLabel(toolUses[block.ToolUseID]); label != "" {
				parts = append(parts, fmt.Sprintf("[%s]\n%s", label, text))
				continue
			}
			parts = append(parts, text)
		}
	}

	if len(parts) == 0 {
		return strings.TrimSpace(fallback)
	}
	return strings.TrimSpace(strings.Join(parts, "\n\n"))
}

type orchidsToolResultEvidence struct {
	ToolName string
	FilePath string
	Content  string
}

func formatAttributedToolResultLabel(item orchidsToolResultEvidence) string {
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

func extractOrchidsToolUseCommand(input interface{}) string {
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

	evidence := collectOrchidsToolResultEvidence(messages)
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
			base := orchidsToolInputBaseName(item.FilePath)
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

func collectOrchidsToolResultEvidence(messages []prompt.Message) []orchidsToolResultEvidence {
	toolUses := make(map[string]orchidsToolResultEvidence)
	var evidence []orchidsToolResultEvidence

	for _, msg := range messages {
		role := strings.TrimSpace(msg.Role)
		if strings.EqualFold(role, "assistant") && !msg.Content.IsString() {
			for _, block := range msg.Content.GetBlocks() {
				if block.Type != "tool_use" {
					continue
				}
				toolUses[block.ID] = orchidsToolResultEvidence{
					ToolName: strings.TrimSpace(block.Name),
					FilePath: extractOrchidsToolUsePath(block.Input),
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

func extractOrchidsToolUsePath(input interface{}) string {
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

func orchidsToolInputBaseName(path string) string {
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
		"refactor suggestions", "improvement suggestions",
		"深入分析", "深层分析", "deep analysis", "in-depth analysis",
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
				if strings.TrimSpace(block.Text) != "" {
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
			if strings.TrimSpace(block.Text) != "" {
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

func runeLen(text string) int {
	return len([]rune(text))
}
