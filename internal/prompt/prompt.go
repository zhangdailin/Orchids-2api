package prompt

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"hash"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"orchids-api/internal/tiktoken"
	"orchids-api/internal/util"
)

// hasherPool 复用 SHA256 hasher，避免频繁内存分配
var hasherPool = sync.Pool{
	New: func() interface{} {
		return sha256.New()
	},
}

// ImageSource 表示图片来源
type ImageSource struct {
	Type      string `json:"type"`
	MediaType string `json:"media_type"`
	Data      string `json:"data"`
	URL       string `json:"url,omitempty"`
}

// CacheControl 缓存控制
type CacheControl struct {
	Type string `json:"type"`
}

// ContentBlock 表示消息内容中的一个块
type ContentBlock struct {
	Type   string       `json:"type"`
	Text   string       `json:"text,omitempty"`
	Source *ImageSource `json:"source,omitempty"`
	URL    string       `json:"url,omitempty"`

	// tool_use 字段
	ID       string      `json:"id,omitempty"`
	Name     string      `json:"name,omitempty"`
	Input    interface{} `json:"input,omitempty"`
	Thinking string      `json:"thinking,omitempty"`

	// tool_result 字段
	ToolUseID    string        `json:"tool_use_id,omitempty"`
	Content      interface{}   `json:"content,omitempty"`
	IsError      bool          `json:"is_error,omitempty"`
	CacheControl *CacheControl `json:"cache_control,omitempty"`
}

// MessageContent 联合类型
type MessageContent struct {
	Text   string
	Blocks []ContentBlock
}

func (mc *MessageContent) UnmarshalJSON(data []byte) error {
	var text string
	if err := json.Unmarshal(data, &text); err == nil {
		mc.Text = text
		mc.Blocks = nil
		return nil
	}

	var blocks []ContentBlock
	if err := json.Unmarshal(data, &blocks); err == nil {
		mc.Text = ""
		mc.Blocks = blocks
		return nil
	}

	return fmt.Errorf("content must be string or array of content blocks")
}

func (mc MessageContent) MarshalJSON() ([]byte, error) {
	if mc.Blocks != nil {
		return json.Marshal(mc.Blocks)
	}
	return json.Marshal(mc.Text)
}

func (mc *MessageContent) IsString() bool {
	return mc.Blocks == nil
}

func (mc *MessageContent) GetText() string {
	return mc.Text
}

func (mc *MessageContent) GetBlocks() []ContentBlock {
	return mc.Blocks
}

// Message 消息结构
type Message struct {
	Role    string         `json:"role"`
	Content MessageContent `json:"content"`
}

// SystemItem 系统提示词项
type SystemItem struct {
	Type         string        `json:"type"`
	Text         string        `json:"text"`
	CacheControl *CacheControl `json:"cache_control,omitempty"`
}

// ClaudeAPIRequest Claude API 请求结构
type ClaudeAPIRequest struct {
	Model    string        `json:"model"`
	Messages []Message     `json:"messages"`
	System   []SystemItem  `json:"system"`
	Tools    []interface{} `json:"tools"`
	Stream   bool          `json:"stream"`
}

type PromptOptions struct {
	Context          context.Context
	MaxTokens        int
	SummaryMaxTokens int
	KeepTurns        int
	ConversationID   string
	ProjectContext   string // Summary of project structure (e.g. file tree)
	ProjectRoot      string // Absolute path to project root for filtering
	SummaryCache     SummaryCache
}

type SummaryCacheEntry struct {
	Summary   string
	Lines     []string
	Hashes    []string
	Budget    int
	UpdatedAt time.Time
}

type SummaryCache interface {
	Get(ctx context.Context, key string) (SummaryCacheEntry, bool)
	Put(ctx context.Context, key string, entry SummaryCacheEntry)
	GetStats(ctx context.Context) (int64, int64, error)
	Clear(ctx context.Context) error
}

// 系统预设提示词
const systemPreset = `<model>Claude</model>
<rules>
You are an AI assistant for the user's current project.
1. Act as a senior engineer. Be concise and accurate.
2. If context is unclear, ask for clarification.
</rules>

## Conversation Format
- <turn index="N" role="user|assistant"> marks each turn
- <tool_use id="..." name="..."> for tool calls
- <tool_result id="..."> for results
`

// FormatMessagesAsMarkdown 将 Claude messages 转换为结构化的对话历史
// 对于大量消息使用并行处理
func FormatMessagesAsMarkdown(messages []Message, projectRoot string) string {
	if len(messages) == 0 {
		return ""
	}

	historyMessages := messages
	if len(messages) > 0 && messages[len(messages)-1].Role == "user" && !isToolResultOnly(messages[len(messages)-1].Content) {
		historyMessages = messages[:len(messages)-1]
	}

	if len(historyMessages) == 0 {
		return ""
	}

	// 并发阈值：少于 8 条消息时串行处理更高效
	const parallelThreshold = 8

	// 顺序组装结果
	var sb strings.Builder
	// Estimating capacity: typical 4KB or proportional to message count
	sb.Grow(len(historyMessages) * 512)

	turnIndex := 1
	if len(historyMessages) >= parallelThreshold {
		// 并行格式化消息
		formattedContents := make([]string, len(historyMessages))
		util.ParallelFor(len(historyMessages), func(idx int) {
			msg := historyMessages[idx]
			var content string
			switch msg.Role {
			case "user":
				content = formatUserMessage(msg.Content)
			case "assistant":
				content = formatAssistantMessage(msg.Content, projectRoot)
			}
			formattedContents[idx] = content
		})

		for i, content := range formattedContents {
			if content == "" {
				continue
			}
			if turnIndex > 1 {
				sb.WriteString("\n\n")
			}
			// Optimized: <turn index="%d" role="%s">\n%s\n</turn>
			sb.WriteString("<turn index=\"")
			sb.WriteString(strconv.Itoa(turnIndex))
			sb.WriteString("\" role=\"")
			sb.WriteString(historyMessages[i].Role)
			sb.WriteString("\">\n")
			sb.WriteString(content)
			sb.WriteString("\n</turn>")
			turnIndex++
		}
		return sb.String()
	}

	// 串行处理小批量消息并直接写入 builder
	for _, msg := range historyMessages {
		var content string
		switch msg.Role {
		case "user":
			content = formatUserMessage(msg.Content)
		case "assistant":
			content = formatAssistantMessage(msg.Content, projectRoot)
		}
		if content == "" {
			continue
		}
		if turnIndex > 1 {
			sb.WriteString("\n\n")
		}
		sb.WriteString("<turn index=\"")
		sb.WriteString(strconv.Itoa(turnIndex))
		sb.WriteString("\" role=\"")
		sb.WriteString(msg.Role)
		sb.WriteString("\">\n")
		sb.WriteString(content)
		sb.WriteString("\n</turn>")
		turnIndex++
	}

	return sb.String()
}

func isToolResultOnly(content MessageContent) bool {
	if content.IsString() {
		return false
	}
	blocks := content.GetBlocks()
	if len(blocks) == 0 {
		return false
	}
	for _, block := range blocks {
		if block.Type != "tool_result" {
			return false
		}
	}
	return true
}

// formatUserMessage 格式化用户消息
func formatUserMessage(content MessageContent) string {
	if content.IsString() {
		text := strings.TrimSpace(content.GetText())
		if text == "" {
			return ""
		}
		return text
	}

	blocks := content.GetBlocks()
	if len(blocks) == 0 {
		return ""
	}

	var sb strings.Builder
	sb.Grow(len(blocks) * 128)

	for i, block := range blocks {
		if i > 0 {
			sb.WriteByte('\n')
		}

		switch block.Type {
		case "text":
			text := strings.TrimSpace(block.Text)
			if text != "" {
				sb.WriteString(text)
			}
		case "image":
			if block.Source != nil {
				sb.WriteString("[Image: ")
				sb.WriteString(block.Source.MediaType)
				sb.WriteByte(']')
			}
		case "tool_result":
			if block.IsError {
				sb.WriteString("TOOL_RESULT_ERROR: The tool failed. Do not infer file contents. Ask for the correct path or list files with LS/Glob.\n")
			}
			resultStr := formatToolResultContent(block.Content)
			sb.WriteString("<tool_result tool_use_id=\"")
			sb.WriteString(block.ToolUseID)
			sb.WriteByte('"')
			if block.IsError {
				sb.WriteString(` is_error="true"`)
			}
			sb.WriteString(">\n")
			sb.WriteString(resultStr)
			sb.WriteString("\n</tool_result>")
		}
	}

	return sb.String()
}

func formatUserMessageNoToolResult(content MessageContent) string {
	if content.IsString() {
		return strings.TrimSpace(content.GetText())
	}

	blocks := content.GetBlocks()
	if len(blocks) == 0 {
		return ""
	}

	var sb strings.Builder
	sb.Grow(len(blocks) * 64)
	first := true
	for _, block := range blocks {
		switch block.Type {
		case "text":
			text := strings.TrimSpace(block.Text)
			if text != "" {
				if !first {
					sb.WriteByte('\n')
				}
				sb.WriteString(text)
				first = false
			}
		case "image":
			if block.Source != nil {
				if !first {
					sb.WriteByte('\n')
				}
				sb.WriteString("[Image: ")
				sb.WriteString(block.Source.MediaType)
				sb.WriteByte(']')
				first = false
			}
		}
	}

	return sb.String()
}

// formatAssistantMessage 格式化 assistant 消息
func formatAssistantMessage(content MessageContent, projectRoot string) string {
	if content.IsString() {
		text := strings.TrimSpace(content.GetText())
		if text == "" {
			return ""
		}
		return filterLogLines(text, projectRoot)
	}

	blocks := content.GetBlocks()
	if len(blocks) == 0 {
		return ""
	}

	var sb strings.Builder
	sb.Grow(len(blocks) * 128)
	first := true

	for _, block := range blocks {
		switch block.Type {
		case "text":
			text := strings.TrimSpace(block.Text)
			if text != "" {
				filtered := filterLogLines(text, projectRoot)
				if filtered == "" {
					continue
				}
				if !first {
					sb.WriteByte('\n')
				}
				sb.WriteString(filtered)
				first = false
			}
		case "thinking":
			continue
		case "tool_use":
			if !first {
				sb.WriteByte('\n')
			}
			inputJSON, _ := json.Marshal(block.Input)
			sb.WriteString("<tool_use id=\"")
			sb.WriteString(block.ID)
			sb.WriteString("\" name=\"")
			sb.WriteString(block.Name)
			sb.WriteString("\">\n")
			sb.Write(inputJSON)
			sb.WriteString("\n</tool_use>")
			first = false
		}
	}

	return sb.String()
}

func filterLogLines(text, projectRoot string) string {
	if projectRoot == "" {
		return text
	}
	if !strings.Contains(text, "[Scanning:") && !strings.Contains(text, "[System:") {
		return text
	}

	lines := strings.Split(text, "\n")
	var filtered []string
	rootBase := filepath.Base(projectRoot)

	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "[Scanning:") {
			// Check if line contains project path
			// We accept if it contains either the full root path OR the base name (e.g. Orchids-2api)
			if !strings.Contains(line, projectRoot) && !strings.Contains(line, rootBase) {
				continue // Skip this line
			}
		}
		filtered = append(filtered, line)
	}
	return strings.Join(filtered, "\n")
}

// formatToolResultContent 格式化工具结果内容
func formatToolResultContent(content interface{}) string {
	switch v := content.(type) {
	case string:
		return stripSystemReminders(v)
	case []interface{}:
		var sb strings.Builder
		sb.Grow(len(v) * 32)
		first := true
		for _, item := range v {
			if itemMap, ok := item.(map[string]interface{}); ok {
				if text, ok := itemMap["text"].(string); ok {
					clean := stripSystemReminders(text)
					if clean == "" {
						continue
					}
					if !first {
						sb.WriteByte('\n')
					}
					sb.WriteString(clean)
					first = false
				}
			}
		}
		if !first {
			return sb.String()
		}
		jsonBytes, _ := json.Marshal(v)
		return string(jsonBytes)
	default:
		jsonBytes, _ := json.Marshal(v)
		return string(jsonBytes)
	}
}

// stripSystemReminders 使用单次遍历移除所有 <system-reminder>...</system-reminder> 标签
// 优化：避免多次 Index 调用和字符串拼接
func stripSystemReminders(text string) string {
	const startTag = "<system-reminder>"
	const endTag = "</system-reminder>"

	// 快速路径：没有标签直接返回
	if !strings.Contains(text, startTag) {
		return trimSpaceIfNeeded(text)
	}

	var sb strings.Builder
	sb.Grow(len(text))

	i := 0
	for i < len(text) {
		// 查找下一个 startTag
		start := strings.Index(text[i:], startTag)
		if start == -1 {
			// 没有更多标签，写入剩余内容
			sb.WriteString(text[i:])
			break
		}

		// 写入标签之前的内容
		sb.WriteString(text[i : i+start])

		// 查找对应的 endTag
		endStart := i + start + len(startTag)
		end := strings.Index(text[endStart:], endTag)
		if end == -1 {
			// 没有结束标签，写入剩余内容
			sb.WriteString(text[i+start:])
			break
		}

		// 跳过整个标签块
		i = endStart + end + len(endTag)
	}

	return trimSpaceIfNeeded(sb.String())
}

func trimSpaceIfNeeded(text string) string {
	if text == "" {
		return text
	}
	first, _ := utf8.DecodeRuneInString(text)
	if !unicode.IsSpace(first) {
		last, _ := utf8.DecodeLastRuneInString(text)
		if !unicode.IsSpace(last) {
			return text
		}
	}
	return strings.TrimSpace(text)
}

func wrapSection(tag, content string) string {
	if content == "" {
		return ""
	}
	var sb strings.Builder
	sb.Grow(len(tag)*2 + len(content) + 5)
	sb.WriteByte('<')
	sb.WriteString(tag)
	sb.WriteString(">\n")
	sb.WriteString(content)
	sb.WriteString("\n</")
	sb.WriteString(tag)
	sb.WriteByte('>')
	return sb.String()
}

func wrapUserRequest(content string) string {
	var sb strings.Builder
	sb.Grow(len(content) + 32)
	sb.WriteString("<user_request>\n")
	sb.WriteString(content)
	sb.WriteString("\n</user_request>")
	return sb.String()
}

// BuildPromptV2 构建优化的 prompt
func BuildPromptV2(req ClaudeAPIRequest) string {
	return BuildPromptV2WithOptions(req, PromptOptions{})
}

// BuildPromptV2WithOptions 构建优化的 prompt
func BuildPromptV2WithOptions(req ClaudeAPIRequest, opts PromptOptions) string {
	var baseSections []string

	// 1. 原始系统提示词（来自客户端）
	var clientSystem []string
	for _, s := range req.System {
		if s.Type == "text" && s.Text != "" {
			clientSystem = append(clientSystem, s.Text)
		}
	}
	if len(clientSystem) > 0 {
		baseSections = append(baseSections, wrapSection("client_system", strings.Join(clientSystem, "\n\n")))
	}

	// 2. 代理系统预设
	baseSections = append(baseSections, wrapSection("proxy_instructions", systemPreset))

	// 2.1 项目上下文（快照）
	if opts.ProjectContext != "" {
		baseSections = append(baseSections, wrapSection("project_context", opts.ProjectContext))
	}

	// 3. 可用工具列表
	if len(req.Tools) > 0 {
		var toolBuilder strings.Builder
		firstTool := true
		for _, t := range req.Tools {
			if tm, ok := t.(map[string]interface{}); ok {
				if name, ok := tm["name"].(string); ok {
					if name == "" {
						continue
					}
					if !firstTool {
						toolBuilder.WriteString(", ")
					}
					firstTool = false
					toolBuilder.WriteString(name)
				}
			}
		}
		if toolBuilder.Len() > 0 {
			baseSections = append(baseSections, wrapSection("available_tools", toolBuilder.String()))
		}

	}

	historyMessages := req.Messages
	if len(historyMessages) > 0 && historyMessages[len(historyMessages)-1].Role == "user" && !isToolResultOnly(historyMessages[len(historyMessages)-1].Content) {
		historyMessages = historyMessages[:len(historyMessages)-1]
	}

	// Intelligent Context Compression
	historyMessages = CompressToolResults(historyMessages, 6, 2000)
	historyMessages = CollapseRepeatedErrors(historyMessages)

	// 5. 当前用户请求
	var currentRequest string
	for i := len(req.Messages) - 1; i >= 0; i-- {
		msg := req.Messages[i]
		if msg.Role != "user" {
			continue
		}
		currentRequest = formatUserMessageNoToolResult(msg.Content)
		if strings.TrimSpace(currentRequest) != "" {
			break
		}
	}
	if strings.TrimSpace(currentRequest) == "" {
		currentRequest = "继续"
	}

	buildSections := func(summary string, history string) string {
		var sb strings.Builder
		sb.Grow(1024)
		writeSection := func(s string) {
			if sb.Len() > 0 {
				sb.WriteString("\n\n")
			}
			sb.WriteString(s)
		}
		for _, section := range baseSections {
			if section != "" {
				writeSection(section)
			}
		}
		if summary != "" {
			writeSection(wrapSection("conversation_summary", summary))
		}
		if history != "" {
			writeSection(wrapSection("conversation_history", history))
		}
		writeSection(wrapUserRequest(currentRequest))
		return sb.String()
	}

	if opts.MaxTokens <= 0 {
		fullHistory := FormatMessagesAsMarkdown(historyMessages, opts.ProjectRoot)
		return buildSections("", fullHistory)
	}

	// Optimization: Token-First History Selection
	// If context is too large, we strictly prioritize recent messages that fit in the budget.
	// 1. Calculate non-history usage
	baseText := buildSections("", "")
	baseTokens := tiktoken.EstimateTextTokens(baseText)

	if len(historyMessages) == 0 {
		return baseText
	}

	var tokenCounts []int
	const estimateThreshold = 16
	if len(historyMessages) < estimateThreshold {
		fullHistory := FormatMessagesAsMarkdown(historyMessages, opts.ProjectRoot)
		promptText := buildSections("", fullHistory)
		if tiktoken.EstimateTextTokens(promptText) <= opts.MaxTokens {
			return promptText
		}
	} else {
		tokenCounts = calculateMessageTokensParallel(historyMessages, opts.ProjectRoot)
		estimatedHistoryTokens := 0
		for _, t := range tokenCounts {
			estimatedHistoryTokens += t
		}
		const wrapperOverhead = 20
		estimatedTotal := baseTokens + estimatedHistoryTokens + wrapperOverhead
		if estimatedTotal <= opts.MaxTokens {
			fullHistory := FormatMessagesAsMarkdown(historyMessages, opts.ProjectRoot)
			promptText := buildSections("", fullHistory)
			if tiktoken.EstimateTextTokens(promptText) <= opts.MaxTokens {
				return promptText
			}
		}
	}

	// Reserve some tokens for summary if possible (e.g. 500 tokens)
	reservedForSummary := opts.SummaryMaxTokens
	if reservedForSummary <= 0 {
		reservedForSummary = 800
	}

	historyBudget := opts.MaxTokens - baseTokens - reservedForSummary
	if historyBudget < 0 {
		historyBudget = 0 // Tight squeeze, prioritize base + request
	}

	var recent []Message
	var older []Message

	// 1. Calculate tokens for all history messages in parallel
	if tokenCounts == nil {
		tokenCounts = calculateMessageTokensParallel(historyMessages, opts.ProjectRoot)
	}

	// 2. Select optimal history window using Suffix Sum + Binary Search
	older, recent = selectHistoryWindow(historyMessages, tokenCounts, historyBudget, baseTokens, opts.MaxTokens)

	// Generate summary for older messages
	summary := ""
	if len(older) > 0 {
		// Use Recursive Summarization (Divide & Conquer)
		summary = summarizeMessagesRecursive(older, reservedForSummary)
	}

	historyText := FormatMessagesAsMarkdown(recent, opts.ProjectRoot)
	return buildSections(summary, historyText)
}

func summaryBudgetFor(maxTokens int, baseSections []string, recent []Message, currentRequest string, projectRoot string) int {
	if maxTokens <= 0 {
		return 0
	}
	var sb strings.Builder
	sb.Grow(1024)
	writeSection := func(s string) {
		if sb.Len() > 0 {
			sb.WriteString("\n\n")
		}
		sb.WriteString(s)
	}
	for _, section := range baseSections {
		if section != "" {
			writeSection(section)
		}
	}
	if len(recent) > 0 {
		history := FormatMessagesAsMarkdown(recent, projectRoot)
		if history != "" {
			writeSection(wrapSection("conversation_history", history))
		}
	}
	writeSection(wrapUserRequest(currentRequest))
	promptText := sb.String()
	usedTokens := tiktoken.EstimateTextTokens(promptText)
	budget := maxTokens - usedTokens
	if budget < 0 {
		return 0
	}
	return budget
}

func summarizeMessagesWithCache(ctx context.Context, opts PromptOptions, messages []Message, maxTokens int) string {
	if maxTokens <= 0 {
		return ""
	}
	cache := opts.SummaryCache
	key := strings.TrimSpace(opts.ConversationID)
	if cache == nil || key == "" {
		if len(messages) == 0 {
			return ""
		}
		summary := summarizeMessages(messages, maxTokens)
		if opts.ProjectRoot != "" {
			summary = filterLogLines(summary, opts.ProjectRoot)
		}
		return summary
	}

	entry, ok := cache.Get(ctx, key)
	if len(messages) == 0 {
		if ok && entry.Summary != "" {
			summary := trimSummaryToBudget(entry.Summary, maxTokens)
			if opts.ProjectRoot != "" {
				summary = filterLogLines(summary, opts.ProjectRoot)
			}
			return summary
		}
		return ""
	}

	hashes := hashMessages(messages)
	if ok && isPrefix(entry.Hashes, hashes) {
		if len(entry.Hashes) == len(hashes) {
			if entry.Summary != "" && tiktoken.EstimateTextTokens(entry.Summary) <= maxTokens {
				if entry.Budget != maxTokens {
					entry.Budget = maxTokens
					entry.UpdatedAt = time.Now()
					cache.Put(ctx, key, entry)
				}
				if opts.ProjectRoot != "" {
					return filterLogLines(entry.Summary, opts.ProjectRoot)
				}
				return entry.Summary
			}
		} else {
			perLineTokens := maxTokens / len(messages)
			if perLineTokens < 8 {
				perLineTokens = 8
			}
			newLines := buildSummaryLines(messages[len(entry.Hashes):], perLineTokens)
			lines := make([]string, 0, len(entry.Lines)+len(newLines))
			lines = append(lines, entry.Lines...)
			lines = append(lines, newLines...)
			var sb strings.Builder
			for i, line := range lines {
				if i > 0 {
					sb.WriteByte('\n')
				}
				sb.WriteString(line)
			}
			summary := sb.String()
			if tiktoken.EstimateTextTokens(summary) > maxTokens {
				summary = summarizeMessages(messages, maxTokens)
				lines = splitSummaryLines(summary)
			}
			cache.Put(ctx, key, SummaryCacheEntry{
				Summary:   summary,
				Lines:     lines,
				Hashes:    hashes,
				Budget:    maxTokens,
				UpdatedAt: time.Now(),
			})
			if opts.ProjectRoot != "" {
				return filterLogLines(summary, opts.ProjectRoot)
			}
			return summary
		}
	}

	summary := summarizeMessages(messages, maxTokens)
	cache.Put(ctx, key, SummaryCacheEntry{
		Summary:   summary,
		Lines:     splitSummaryLines(summary),
		Hashes:    hashes,
		Budget:    maxTokens,
		UpdatedAt: time.Now(),
	})
	if opts.ProjectRoot != "" {
		return filterLogLines(summary, opts.ProjectRoot)
	}
	return summary
}

func splitSummaryLines(summary string) []string {
	if summary == "" {
		return nil
	}
	lines := strings.Split(summary, "\n")
	trimmed := make([]string, 0, len(lines))
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line != "" {
			trimmed = append(trimmed, line)
		}
	}
	return trimmed
}

func joinLines(lines []string) string {
	if len(lines) == 0 {
		return ""
	}
	var sb strings.Builder
	for i, line := range lines {
		if i > 0 {
			sb.WriteByte('\n')
		}
		sb.WriteString(line)
	}
	return sb.String()
}

func trimSummaryToBudget(summary string, maxTokens int) string {
	if summary == "" || maxTokens <= 0 {
		return ""
	}
	if tiktoken.EstimateTextTokens(summary) <= maxTokens {
		return summary
	}
	return truncateToTokens(summary, maxTokens)
}

// hashMessages 并行计算消息哈希
func hashMessages(messages []Message) []string {
	if len(messages) == 0 {
		return nil
	}

	hashes := make([]string, len(messages))

	// 并发阈值：少于 8 条消息时串行处理更高效
	const parallelThreshold = 8

	if len(messages) >= parallelThreshold {
		util.ParallelFor(len(messages), func(idx int) {
			hashes[idx] = messageHash(messages[idx])
		})
	} else {
		for i, msg := range messages {
			hashes[i] = messageHash(msg)
		}
	}

	return hashes
}

// messageHash 计算消息的 SHA256 哈希，使用 hasherPool 复用 hasher
func messageHash(msg Message) string {
	// 从 pool 获取 hasher
	hasher := hasherPool.Get().(hash.Hash)
	hasher.Reset()
	defer hasherPool.Put(hasher)

	hasher.Write([]byte(msg.Role))
	hasher.Write([]byte{0})

	if msg.Content.IsString() {
		hasher.Write([]byte(strings.TrimSpace(msg.Content.GetText())))
		return hex.EncodeToString(hasher.Sum(nil))
	}

	for _, block := range msg.Content.GetBlocks() {
		hasher.Write([]byte(block.Type))
		hasher.Write([]byte{0})
		switch block.Type {
		case "text":
			hasher.Write([]byte(strings.TrimSpace(block.Text)))
		case "image":
			if block.Source != nil {
				hasher.Write([]byte(block.Source.MediaType))
			}
		case "tool_use":
			hasher.Write([]byte(block.Name))
			if block.Input != nil {
				if inputBytes, err := json.Marshal(block.Input); err == nil {
					hasher.Write(inputBytes)
				}
			}
		case "tool_result":
			writeToolResultHash(hasher, block.Content)
			if block.IsError {
				hasher.Write([]byte("error"))
			}
		}
		hasher.Write([]byte{0})
	}

	return hex.EncodeToString(hasher.Sum(nil))
}

func writeToolResultHash(hasher hash.Hash, content interface{}) {
	switch v := content.(type) {
	case string:
		hasher.Write([]byte(stripSystemReminders(v)))
	case []interface{}:
		wrote := false
		for _, item := range v {
			itemMap, ok := item.(map[string]interface{})
			if !ok {
				continue
			}
			text, ok := itemMap["text"].(string)
			if !ok {
				continue
			}
			if wrote {
				hasher.Write([]byte{'\n'})
			}
			hasher.Write([]byte(stripSystemReminders(text)))
			wrote = true
		}
		if wrote {
			return
		}
		if jsonBytes, err := json.Marshal(v); err == nil {
			hasher.Write(jsonBytes)
		}
	default:
		if jsonBytes, err := json.Marshal(v); err == nil {
			hasher.Write(jsonBytes)
		}
	}
}

func isPrefix(prefix []string, full []string) bool {
	if len(prefix) > len(full) {
		return false
	}
	for i := range prefix {
		if prefix[i] != full[i] {
			return false
		}
	}
	return true
}

func splitHistory(messages []Message, keepTurns int) (older []Message, recent []Message) {
	if len(messages) == 0 {
		return messages, nil
	}
	if keepTurns <= 0 {
		return messages, nil
	}

	count := 0
	splitIndex := 0
	for i := len(messages) - 1; i >= 0; i-- {
		if messages[i].Role == "user" {
			count++
			if count == keepTurns {
				splitIndex = i
				break
			}
		}
	}
	if count < keepTurns {
		return []Message{}, messages
	}
	return messages[:splitIndex], messages[splitIndex:]
}

func summarizeMessages(messages []Message, maxTokens int) string {
	if len(messages) == 0 || maxTokens <= 0 {
		return ""
	}

	perLineTokens := maxTokens / len(messages)
	if perLineTokens < 8 {
		perLineTokens = 8
	}

	for perLineTokens >= 4 {
		lines := buildSummaryLines(messages, perLineTokens)
		joined := joinLines(lines)
		if tiktoken.EstimateTextTokens(joined) <= maxTokens {
			return joined
		}
		perLineTokens = int(float64(perLineTokens) * 0.7)
	}

	lines := buildSummaryLines(messages, 4)
	return joinLines(lines)
}

// buildSummaryLines 并行构建摘要行
func buildSummaryLines(messages []Message, perLineTokens int) []string {
	if len(messages) == 0 {
		return nil
	}

	// 并发阈值：少于 8 条消息时串行处理更高效
	const parallelThreshold = 8

	type indexedLine struct {
		index int
		line  string
	}

	if len(messages) >= parallelThreshold {
		results := make([]string, len(messages))
		util.ParallelFor(len(messages), func(idx int) {
			msg := messages[idx]
			summary := summarizeMessageWithLimit(msg, perLineTokens)
			if summary != "" {
				results[idx] = fmt.Sprintf("- %s: %s", msg.Role, summary)
			}
		})

		// 过滤空行，保持顺序
		lines := make([]string, 0, len(messages))
		for _, line := range results {
			if line != "" {
				lines = append(lines, line)
			}
		}
		return lines
	}

	// 串行处理小批量
	lines := make([]string, 0, len(messages))
	for _, msg := range messages {
		summary := summarizeMessageWithLimit(msg, perLineTokens)
		if summary == "" {
			continue
		}
		lines = append(lines, fmt.Sprintf("- %s: %s", msg.Role, summary))
	}
	return lines
}

func summarizeMessageWithLimit(msg Message, maxTokens int) string {
	if msg.Content.IsString() {
		return truncateToTokens(stripSystemReminders(strings.TrimSpace(msg.Content.GetText())), maxTokens)
	}

	blocks := msg.Content.GetBlocks()
	var sb strings.Builder
	sb.Grow(len(blocks) * 48)
	first := true
	for _, block := range blocks {
		switch block.Type {
		case "text":
			text := stripSystemReminders(strings.TrimSpace(block.Text))
			if text != "" {
				if !first {
					sb.WriteString(" | ")
				}
				sb.WriteString(text)
				first = false
			}
		case "tool_use":
			if block.Name != "" {
				if !first {
					sb.WriteString(" | ")
				}
				sb.WriteString("tool_use:")
				sb.WriteString(block.Name)
				if block.Input != nil {
					if b, err := json.Marshal(block.Input); err == nil {
						sb.WriteByte('(')
						sb.Write(b)
						sb.WriteByte(')')
					}
				}
				first = false
			}
		case "tool_result":
			resultText := formatToolResultContent(block.Content)
			if block.IsError {
				if !first {
					sb.WriteString(" | ")
				}
				sb.WriteString("tool_result:error: ")
				sb.WriteString(resultText)
				first = false
			} else {
				if !first {
					sb.WriteString(" | ")
				}
				sb.WriteString("tool_result: ")
				sb.WriteString(resultText)
				first = false
			}
		}
	}

	if first {
		return ""
	}
	return truncateToTokens(sb.String(), maxTokens)
}

// calculateMessageTokensParallel 并行计算消息 token
func calculateMessageTokensParallel(messages []Message, projectRoot string) []int {
	historyLen := len(messages)
	tokenCounts := make([]int, historyLen)
	if historyLen == 0 {
		return tokenCounts
	}

	const parallelThreshold = 8
	if historyLen >= parallelThreshold {
		util.ParallelFor(historyLen, func(idx int) {
			msg := messages[idx]
			msgContent := ""
			if msg.Role == "user" {
				msgContent = formatUserMessage(msg.Content)
			} else {
				msgContent = formatAssistantMessage(msg.Content, projectRoot)
			}
			tokenCounts[idx] = tiktoken.EstimateTextTokens(msgContent) + 15
		})
	} else {
		// Serial for small history
		for i, msg := range messages {
			msgContent := ""
			if msg.Role == "user" {
				msgContent = formatUserMessage(msg.Content)
			} else {
				msgContent = formatAssistantMessage(msg.Content, projectRoot)
			}
			tokenCounts[i] = tiktoken.EstimateTextTokens(msgContent) + 15
		}
	}
	return tokenCounts
}

// selectHistoryWindow 使用后缀和+二分查找选择最优历史窗口
func selectHistoryWindow(messages []Message, tokenCounts []int, budget int, baseTokens int, maxTokens int) (older []Message, recent []Message) {
	historyLen := len(messages)
	if historyLen == 0 {
		return nil, nil
	}

	// Optimization: Suffix Sum (DP) + Binary Search
	suffixSum := make([]int, historyLen+1)
	for i := historyLen - 1; i >= 0; i-- {
		suffixSum[i] = suffixSum[i+1] + tokenCounts[i]
	}

	// Use Binary Search to find the optimal split index
	splitIdx := sort.Search(historyLen, func(i int) bool {
		return suffixSum[i] <= budget
	})

	older = messages[:splitIdx]
	recent = messages[splitIdx:]

	if len(recent) == 0 && len(messages) > 0 {
		// Try to add at least one if it fits in MaxTokens ignoring summary reservation
		lastMsg := messages[len(messages)-1]
		lastTokens := tokenCounts[len(tokenCounts)-1]
		if lastTokens+baseTokens <= maxTokens {
			recent = []Message{lastMsg}
			older = messages[:len(messages)-1]
		}
	}
	return older, recent
}

// summarizeMessagesRecursive uses Divide & Conquer to summarize messages
func summarizeMessagesRecursive(messages []Message, maxTokens int) string {
	if len(messages) == 0 {
		return ""
	}

	// Base case: if estimated tokens are within budget, perform simple formatting
	// We use a quick estimation here.
	totalEstimated := 0
	for _, m := range messages {
		if m.Content.IsString() {
			totalEstimated += tiktoken.EstimateTextTokens(m.Content.GetText())
		} else {
			totalEstimated += 100 // Rough estimate for blocks
		}
	}

	if totalEstimated <= maxTokens {
		// Just format them all, but maybe still need to truncate individual large ones?
		// For simplicity, we just use the simple line builder for small enough chunks
		lines := buildSummaryLines(messages, maxTokens) // Reuse existing logic which formats well
		return joinLines(lines)
	}

	// Base case 2: If we only have one message and it's still too large,
	// use the standard summarization which includes truncation.
	if len(messages) == 1 {
		return summarizeMessages(messages, maxTokens)
	}

	// Recursive step: Split into two halves
	mid := len(messages) / 2
	leftBudget := maxTokens / 2
	rightBudget := maxTokens - leftBudget

	leftSummary := summarizeMessagesRecursive(messages[:mid], leftBudget)
	rightSummary := summarizeMessagesRecursive(messages[mid:], rightBudget)

	if leftSummary == "" {
		return rightSummary
	}
	if rightSummary == "" {
		return leftSummary
	}
	return leftSummary + "\n" + rightSummary
}

// truncateToTokens 使用二分查找优化 token 截断，减少重复 token 估算
func truncateToTokens(text string, maxTokens int) string {
	if text == "" || maxTokens <= 0 {
		return ""
	}

	// 转换一次 runes，之后复用
	runes := []rune(text)

	// 快速路径：文本足够短
	estimated := tiktoken.EstimateTextTokens(text)
	if estimated <= maxTokens {
		return text
	}

	// 估算初始上界：假设每个 token 约 3 个 rune
	maxRunes := maxTokens * 3
	if maxRunes > len(runes) {
		maxRunes = len(runes)
	}
	if maxRunes < 1 {
		maxRunes = 1
	}

	// 先截断到估算的最大长度
	if len(runes) > maxRunes {
		runes = runes[:maxRunes]
	}

	// 二分查找最优截断点
	low, high := 1, len(runes)
	bestLen := 0

	for low <= high {
		mid := (low + high) / 2
		tokens := tiktoken.EstimateTextTokens(string(runes[:mid]))
		if tokens <= maxTokens {
			bestLen = mid
			low = mid + 1
		} else {
			high = mid - 1
		}
	}

	if bestLen == 0 {
		return ""
	}

	result := trimSpaceIfNeeded(string(runes[:bestLen]))
	if result == "" {
		return ""
	}
	return result + "…"
}

func removeSection(sections []string, sectionName string) []string {
	prefix := "<" + sectionName + ">"
	result := make([]string, 0, len(sections))
	for _, section := range sections {
		if strings.HasPrefix(section, prefix) {
			continue
		}
		result = append(result, section)
	}
	return result
}

func insertSectionBefore(sections []string, sectionName string, newSection string) []string {
	prefix := "<" + sectionName + ">"
	for i, section := range sections {
		if strings.HasPrefix(section, prefix) {
			return append(append(sections[:i], newSection), sections[i:]...)
		}
	}
	return append(sections, newSection)
}
