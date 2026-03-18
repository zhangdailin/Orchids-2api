package bolt

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/goccy/go-json"

	"orchids-api/internal/config"
	"orchids-api/internal/debug"
	"orchids-api/internal/orchids"
	"orchids-api/internal/prompt"
	"orchids-api/internal/store"
	"orchids-api/internal/upstream"
	"orchids-api/internal/util"
)

const (
	defaultAPIURL        = "https://bolt.new/api/chat/v2"
	defaultRootDataURL   = "https://bolt.new/?_data=root"
	defaultRateLimitsURL = "https://bolt.new/api/rate-limits/user"
	defaultTeamsRateURL  = "https://bolt.new/api/rate-limits/teams"
	defaultAdapterPrompt = "你正在通过 Orchids 的 Bolt 适配层处理一个代码代理对话。能直接回答时直接回答；需要查看代码、目录或执行命令时，不要先解释计划，不要说你接下来要去看文件，而是立刻返回严格 JSON 的工具调用。"
)

var boltAPIURL = defaultAPIURL
var boltRootDataURL = defaultRootDataURL
var boltRateLimitsURL = defaultRateLimitsURL
var boltTeamsRateLimitsURL = defaultTeamsRateURL
var supportedBoltToolOrder = []string{"Read", "Write", "Edit", "Bash", "Glob", "Grep", "TodoWrite"}

type Client struct {
	httpClient   *http.Client
	sessionToken string
	projectID    string
}

func NewFromAccount(acc *store.Account, cfg *config.Config) *Client {
	timeout := 30 * time.Second
	if cfg != nil && cfg.RequestTimeout > 0 {
		timeout = time.Duration(cfg.RequestTimeout) * time.Second
	}

	transport := http.DefaultTransport.(*http.Transport).Clone()
	if cfg != nil {
		transport.Proxy = util.ProxyFunc(cfg.ProxyHTTP, cfg.ProxyHTTPS, cfg.ProxyUser, cfg.ProxyPass, cfg.ProxyBypass)
	}

	sessionToken := ""
	projectID := ""
	if acc != nil {
		sessionToken = strings.TrimSpace(acc.SessionCookie)
		if sessionToken == "" {
			sessionToken = strings.TrimSpace(acc.ClientCookie)
		}
		projectID = strings.TrimSpace(acc.ProjectID)
	}

	return &Client{
		httpClient:   &http.Client{Timeout: timeout, Transport: transport},
		sessionToken: sessionToken,
		projectID:    projectID,
	}
}

func (c *Client) Close() {
	if c == nil || c.httpClient == nil || c.httpClient.Transport == nil {
		return
	}
	if closer, ok := c.httpClient.Transport.(interface{ CloseIdleConnections() }); ok {
		closer.CloseIdleConnections()
	}
}

func (c *Client) FetchRootData(ctx context.Context) (*RootData, error) {
	if c == nil {
		return nil, fmt.Errorf("bolt client is nil")
	}
	if strings.TrimSpace(c.sessionToken) == "" {
		return nil, fmt.Errorf("missing bolt session token")
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, boltRootDataURL, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create bolt root request: %w", err)
	}
	c.applyCommonHeaders(req)
	req.Header.Set("Accept", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to fetch bolt root data: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 8192))
		return nil, fmt.Errorf("bolt root data error: status=%d, body=%s", resp.StatusCode, strings.TrimSpace(string(body)))
	}

	var data RootData
	if err := json.NewDecoder(resp.Body).Decode(&data); err != nil {
		return nil, fmt.Errorf("failed to decode bolt root data: %w", err)
	}
	return &data, nil
}

func (c *Client) FetchRateLimits(ctx context.Context, organizationID int64) (*RateLimits, error) {
	if c == nil {
		return nil, fmt.Errorf("bolt client is nil")
	}
	if strings.TrimSpace(c.sessionToken) == "" {
		return nil, fmt.Errorf("missing bolt session token")
	}

	targetURL := boltRateLimitsURL
	if organizationID > 0 {
		targetURL = strings.TrimRight(boltTeamsRateLimitsURL, "/") + "/" + strconv.FormatInt(organizationID, 10)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, targetURL, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create bolt rate-limit request: %w", err)
	}
	c.applyCommonHeaders(req)
	req.Header.Set("Accept", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to fetch bolt rate limits: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 8192))
		return nil, fmt.Errorf("bolt rate-limit error: status=%d, body=%s", resp.StatusCode, strings.TrimSpace(string(body)))
	}

	var data RateLimits
	if err := json.NewDecoder(resp.Body).Decode(&data); err != nil {
		return nil, fmt.Errorf("failed to decode bolt rate limits: %w", err)
	}
	return &data, nil
}

func (c *Client) SendRequest(ctx context.Context, _ string, _ []interface{}, model string, onMessage func(upstream.SSEMessage), logger *debug.Logger) error {
	req := upstream.UpstreamRequest{Model: model}
	return c.SendRequestWithPayload(ctx, req, onMessage, logger)
}

func (c *Client) SendRequestWithPayload(ctx context.Context, req upstream.UpstreamRequest, onMessage func(upstream.SSEMessage), logger *debug.Logger) error {
	if c == nil {
		return fmt.Errorf("bolt client is nil")
	}
	if strings.TrimSpace(c.sessionToken) == "" {
		return fmt.Errorf("missing bolt session token")
	}
	projectID := strings.TrimSpace(req.ProjectID)
	if projectID == "" {
		projectID = strings.TrimSpace(c.projectID)
	}
	if projectID == "" {
		return fmt.Errorf("missing bolt project id")
	}

	boltReq := c.buildRequest(req, projectID)
	if logger != nil {
		if raw, err := json.Marshal(boltReq); err == nil {
			logger.LogUpstreamRequest(boltAPIURL, map[string]string{"provider": "bolt"}, raw)
		}
	}

	body, err := json.Marshal(boltReq)
	if err != nil {
		return fmt.Errorf("failed to marshal bolt request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, boltAPIURL, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("failed to create bolt request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Referer", "https://bolt.new/~/sb1-"+projectID)
	c.applyCommonHeaders(httpReq)

	resp, err := c.httpClient.Do(httpReq)
	if err != nil {
		return fmt.Errorf("failed to send bolt request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 8192))
		return fmt.Errorf("bolt API error: status=%d, body=%s", resp.StatusCode, strings.TrimSpace(string(body)))
	}

	converter := newOutboundConverter(req.Model, estimateInputTokens(req.Messages, req.System))
	return converter.ProcessStream(resp.Body, func(event string, payload []byte) error {
		return emitSSEMessage(onMessage, event, payload)
	})
}

func (c *Client) applyCommonHeaders(req *http.Request) {
	if req == nil {
		return
	}
	req.Header.Set("Accept", "*/*")
	req.Header.Set("Accept-Language", "zh-CN,zh;q=0.9")
	req.Header.Set("Cache-Control", "no-cache")
	req.Header.Set("Origin", "https://bolt.new")
	req.Header.Set("Pragma", "no-cache")
	req.Header.Set("User-Agent", "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36")
	req.Header.Set("Cookie", "__session="+c.sessionToken)
}

func (c *Client) buildRequest(req upstream.UpstreamRequest, projectID string) *Request {
	systemPrompt := buildSystemPrompt(req.System, req.Workdir, req.Tools, req.NoTools, req.Messages)
	boltReq := &Request{
		ID:                   generateRandomID(16),
		SelectedModel:        strings.TrimSpace(req.Model),
		IsFirstPrompt:        false,
		PromptMode:           "build",
		EffortLevel:          "high",
		ProjectID:            projectID,
		GlobalSystemPrompt:   systemPrompt,
		ProjectPrompt:        systemPrompt,
		StripeStatus:         "not-configured",
		HostingProvider:      "bolt",
		SupportIntegrations:  true,
		UsesInspectedElement: false,
		ErrorReasoning:       nil,
		FeaturePreviews: FeaturePreviews{
			Reasoning: false,
			Diffs:     false,
		},
		ProjectFiles: ProjectFiles{
			Visible: []interface{}{},
			Hidden:  []interface{}{},
		},
		RunningCommands: []interface{}{},
		Dependencies:    []interface{}{},
		Problems:        "",
	}

	var lastUserMsgID string
	for _, msg := range req.Messages {
		blocks := normalizeBlocks(msg)

		if shouldSkipBoltMessage(msg.Role, blocks) {
			continue
		}

		boltMsg := Message{
			ID:    generateRandomID(16),
			Role:  msg.Role,
			Cache: hasEphemeralCache(blocks),
			Parts: []Part{},
		}

		switch msg.Role {
		case "user":
			lastUserMsgID = boltMsg.ID
			boltMsg.Content = extractBoltUserContent(blocks)
			boltMsg.RawContent = boltMsg.Content
		case "assistant":
			if lastUserMsgID != "" {
				boltMsg.Annotations = []Annotation{{Type: "metadata", UserMessageID: lastUserMsgID}}
			}
			boltMsg.Content = extractTextContent(blocks)
			if strings.TrimSpace(boltMsg.Content) != "" {
				boltMsg.Parts = []Part{{Type: "text", Text: boltMsg.Content}}
			}
		default:
			boltMsg.Content = extractTextContent(blocks)
			boltMsg.RawContent = boltMsg.Content
		}

		if strings.TrimSpace(boltMsg.Content) == "" && len(boltMsg.Parts) == 0 {
			continue
		}

		boltReq.Messages = append(boltReq.Messages, boltMsg)
	}

	return boltReq
}

func shouldSkipBoltMessage(role string, blocks []prompt.ContentBlock) bool {
	switch strings.TrimSpace(strings.ToLower(role)) {
	case "tool":
		text := strings.TrimSpace(extractTextContent(blocks))
		if text == "" {
			return true
		}
		return true
	default:
		return false
	}
}

func buildSystemPrompt(system []prompt.SystemItem, workdir string, tools []interface{}, noTools bool, messages []prompt.Message) string {
	parts := []string{defaultAdapterPrompt}
	if toolPrompt := buildBoltToolPrompt(workdir, tools, noTools, messages); toolPrompt != "" {
		parts = append(parts, toolPrompt)
	}
	for _, item := range system {
		text := sanitizeBoltSystemText(item.Text)
		if strings.TrimSpace(text) != "" {
			parts = append(parts, text)
		}
	}
	return strings.Join(parts, "\n\n")
}

func buildBoltToolPrompt(workdir string, tools []interface{}, noTools bool, messages []prompt.Message) string {
	workdir = strings.TrimSpace(workdir)
	parts := append([]string{}, buildBoltWorkspacePrompt(workdir)...)

	if noTools {
		parts = append(parts, "这次回合不要发起任何工具调用，只基于已有上下文和已返回的工具结果直接回答。")
		return strings.Join(parts, "\n")
	}

	toolNames := supportedBoltToolNames(tools)
	if len(toolNames) == 0 {
		parts = append(parts, "如果上下文已经足够，请直接回答。")
		return strings.Join(parts, "\n")
	}

	parts = append(parts, buildBoltToolUsagePrompt(toolNames)...)
	parts = append(parts, buildBoltHistoryRecoveryPrompt(workdir, messages)...)
	parts = append(parts, "单个工具调用格式: {\"tool\":\"Read\",\"parameters\":{\"file_path\":\"README.md\"}}")
	parts = append(parts, "多个工具调用格式: {\"tool_calls\":[{\"function\":\"Glob\",\"parameters\":{\"path\":\".\",\"pattern\":\"*.go\"}},{\"function\":\"Read\",\"parameters\":{\"file_path\":\"README.md\"}}]}")
	parts = append(parts, "拿到工具结果后，继续基于结果回答或发起下一步工具调用，不要重复已经完成的同一调用。")

	return strings.Join(parts, "\n")
}

func buildBoltWorkspacePrompt(workdir string) []string {
	if strings.TrimSpace(workdir) == "" {
		return nil
	}

	parts := make([]string, 0, 5)
	projectName := filepath.Base(filepath.Clean(workdir))
	if projectName != "" && projectName != "." && projectName != string(filepath.Separator) {
		parts = append(parts, "当前项目目录名: "+projectName)
	}
	parts = append(parts, "当前项目真实工作目录(仅用于回答用户询问“项目目录地址/当前路径/workspace 在哪里”这类问题): `"+workdir+"`")
	parts = append(parts, "如果用户问项目目录地址、当前项目路径或 workspace 路径，直接回答上面的真实工作目录；不要回答 `/tmp/cc-agent/...`、`/mnt/...`、`~/...` 这类沙箱占位路径。")
	parts = append(parts, "把项目根目录视为 `.`。Read/Write/Edit/Glob/Grep 的路径统一优先使用相对路径；如果要看项目根目录，直接用 `.`。")
	parts = append(parts, "Bash 默认就在项目根目录执行，只写项目内的相对路径，不要拼接 `d:\\...`、`C:\\...`、`/mnt/...`、`~/...` 或 `/tmp/cc-agent/...`。")
	return parts
}

func buildBoltToolUsagePrompt(toolNames []string) []string {
	toolHints := make([]string, 0, len(toolNames))
	for _, name := range toolNames {
		toolHints = append(toolHints, boltToolHint(name))
	}

	return []string{
		"可用工具: " + strings.Join(toolHints, "; "),
		"需要工具时，输出纯 JSON，不要加解释、不要加前后缀、不要说“让我先看看项目文件”。",
		"不要解释当前运行在什么系统或沙箱；如果需要确认目录或文件，直接调用工具。",
		"如果某次工具结果提示路径不存在，不要据此断言项目为空；应优先改用 `.`、README.md、go.mod、package.json 等项目内相对路径继续调用工具。",
	}
}

func buildBoltHistoryRecoveryPrompt(workdir string, messages []prompt.Message) []string {
	invalidPath := detectRecentInvalidBoltHistoryPath(messages)
	if invalidPath == "" {
		return nil
	}

	parts := []string{
		"检测到历史里刚刚有一次无效的外部路径工具调用 `" + invalidPath + "`，它不是当前项目目录；把那次失败视为错误示例，不要复用这个路径，也不要基于它做同路径变体。",
	}
	if strings.TrimSpace(workdir) != "" {
		parts = append(parts, "真实项目目录是 `"+workdir+"`；如果用户追问项目目录地址，直接回答这个真实工作目录。")
	}
	parts = append(parts, "如果需要重新检查项目，下一次必须改用项目根目录 `.` 或 README.md、go.mod、package.json 这类项目内相对路径。")
	parts = append(parts, "在至少成功查看一次 `.`、README.md、go.mod、package.json 等项目内路径之前，不要回答“项目为空”“没有文件”或“目录是空的”。")
	return parts
}

func detectRecentInvalidBoltHistoryPath(messages []prompt.Message) string {
	if len(messages) == 0 {
		return ""
	}

	toolPaths := make(map[string]string)
	for _, msg := range messages {
		for _, block := range normalizeBlocks(msg) {
			if block.Type != "tool_use" || strings.TrimSpace(block.ID) == "" {
				continue
			}
			if path := extractInvalidBoltPathFromValue(block.Input); path != "" {
				toolPaths[block.ID] = path
			}
		}
	}

	for i := len(messages) - 1; i >= 0; i-- {
		for _, block := range normalizeBlocks(messages[i]) {
			if block.Type != "tool_result" || !isBoltMissingPathResult(block.Content) {
				continue
			}
			if path := toolPaths[strings.TrimSpace(block.ToolUseID)]; path != "" {
				return path
			}
			if path := extractInvalidBoltPathFromValue(block.Content); path != "" {
				return path
			}
		}
	}

	return ""
}

func isBoltMissingPathResult(content interface{}) bool {
	lower := strings.ToLower(strings.TrimSpace(stringifyContent(content)))
	if lower == "" {
		return false
	}
	for _, marker := range []string{
		"no such file or directory",
		"cannot access",
		"does not exist",
		"path does not exist",
		"系统找不到指定的路径",
		"找不到指定的路径",
		"enoent",
		"not found",
	} {
		if strings.Contains(lower, marker) {
			return true
		}
	}
	return false
}

func extractInvalidBoltPathFromValue(value interface{}) string {
	switch v := value.(type) {
	case nil:
		return ""
	case string:
		return extractInvalidBoltPathFromString(v)
	case map[string]interface{}:
		for _, nested := range v {
			if path := extractInvalidBoltPathFromValue(nested); path != "" {
				return path
			}
		}
	case []interface{}:
		for _, nested := range v {
			if path := extractInvalidBoltPathFromValue(nested); path != "" {
				return path
			}
		}
	case []prompt.ContentBlock:
		for _, nested := range v {
			if path := extractInvalidBoltPathFromValue(nested.Content); path != "" {
				return path
			}
			if path := extractInvalidBoltPathFromString(nested.Text); path != "" {
				return path
			}
		}
	default:
		if data, err := json.Marshal(v); err == nil {
			return extractInvalidBoltPathFromString(string(data))
		}
	}
	return ""
}

func extractInvalidBoltPathFromString(text string) string {
	if strings.TrimSpace(text) == "" {
		return ""
	}
	for _, field := range strings.Fields(text) {
		candidate := strings.Trim(field, "\"'`,;()[]{}")
		if looksLikeInvalidBoltPath(candidate) {
			return candidate
		}
	}

	lower := strings.ToLower(text)
	for _, marker := range []string{"/tmp/cc-agent/", "/mnt/", "d:\\", "c:\\", "~/"} {
		idx := strings.Index(lower, marker)
		if idx < 0 {
			continue
		}
		end := len(text)
		for i := idx; i < len(text); i++ {
			switch text[i] {
			case ' ', '\n', '\r', '\t', '"', '\'', '`':
				end = i
				goto found
			}
		}
	found:
		candidate := strings.Trim(text[idx:end], "\"'`,;()[]{}")
		if looksLikeInvalidBoltPath(candidate) {
			return candidate
		}
	}
	return ""
}

func looksLikeInvalidBoltPath(path string) bool {
	lower := strings.ToLower(strings.TrimSpace(path))
	if lower == "" {
		return false
	}
	return strings.Contains(lower, "/tmp/cc-agent/") ||
		strings.Contains(lower, "/mnt/") ||
		strings.HasPrefix(lower, "~/") ||
		strings.Contains(lower, "d:\\") ||
		strings.Contains(lower, "c:\\")
}

func supportedBoltToolNames(tools []interface{}) []string {
	if len(tools) == 0 {
		return nil
	}

	seen := make(map[string]struct{}, len(supportedBoltToolOrder))
	for _, raw := range tools {
		name := strings.TrimSpace(extractBoltToolName(raw))
		if name == "" {
			continue
		}
		mappedName := orchids.NormalizeToolNameFallback(name)
		if !isPromptToolSupported(mappedName) {
			continue
		}
		seen[strings.ToLower(mappedName)] = struct{}{}
	}

	if len(seen) == 0 {
		return nil
	}

	out := make([]string, 0, len(seen))
	for _, name := range supportedBoltToolOrder {
		if _, ok := seen[strings.ToLower(name)]; ok {
			out = append(out, name)
		}
	}
	return out
}

func extractBoltToolName(raw interface{}) string {
	rawMap, ok := raw.(map[string]interface{})
	if !ok {
		return ""
	}
	if function, ok := rawMap["function"].(map[string]interface{}); ok {
		if name, ok := function["name"].(string); ok {
			return strings.TrimSpace(name)
		}
	}
	if name, ok := rawMap["name"].(string); ok {
		return strings.TrimSpace(name)
	}
	return ""
}

func boltToolHint(name string) string {
	switch name {
	case "Read":
		return "Read(file_path, limit?, offset?)"
	case "Write":
		return "Write(file_path, content)"
	case "Edit":
		return "Edit(file_path, old_string, new_string, replace_all?)"
	case "Bash":
		return "Bash(command, description?, timeout?, run_in_background?)"
	case "Glob":
		return "Glob(path, pattern)"
	case "Grep":
		return "Grep(path, pattern, glob?, output_mode?, -n?, -C?)"
	case "TodoWrite":
		return "TodoWrite(content)"
	default:
		return name
	}
}

func isPromptToolSupported(name string) bool {
	switch strings.TrimSpace(name) {
	case "Read", "Write", "Edit", "Bash", "Glob", "Grep", "TodoWrite":
		return true
	default:
		return false
	}
}

func sanitizeBoltSystemText(text string) string {
	text = stripTaggedBoltText(text)
	text = stripCCEntrypointLines(text)
	text = stripBoltEnvironmentLines(text)
	if looksLikeClaudeCodeSystem(text) {
		return ""
	}
	return strings.TrimSpace(text)
}

func sanitizeBoltMessageText(text string) string {
	return strings.TrimSpace(stripTaggedBoltText(text))
}

func stripTaggedBoltText(text string) string {
	text = stripNestedTaggedBlock(text, "system-reminder")
	for _, tag := range []string{
		"local-command-caveat",
		"command-name",
		"command-message",
		"command-args",
		"local-command-stdout",
		"local-command-stderr",
		"local-command-exit-code",
		"ide_opened_file",
		"ide_selection",
	} {
		text = stripSimpleTaggedBlock(text, tag)
	}
	return text
}

func stripNestedTaggedBlock(text, tag string) string {
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

func stripSimpleTaggedBlock(text, tag string) string {
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

func stripCCEntrypointLines(text string) string {
	if !strings.Contains(strings.ToLower(text), "cc_entrypoint=") {
		return text
	}
	lines := strings.Split(text, "\n")
	filtered := lines[:0]
	for _, line := range lines {
		if strings.Contains(strings.ToLower(line), "cc_entrypoint=") {
			continue
		}
		filtered = append(filtered, line)
	}
	return strings.Join(filtered, "\n")
}

func stripBoltEnvironmentLines(text string) string {
	if !looksLikeBoltEnvironmentBlock(text) {
		return text
	}
	lines := strings.Split(text, "\n")
	filtered := lines[:0]
	for _, line := range lines {
		lower := strings.ToLower(strings.TrimSpace(line))
		switch {
		case lower == "# environment":
			continue
		case lower == "# auto memory":
			continue
		case strings.HasPrefix(lower, "- primary working directory:"):
			continue
		case strings.HasPrefix(lower, "primary working directory:"):
			continue
		case strings.HasPrefix(lower, "working directory:"):
			continue
		case strings.HasPrefix(lower, "cwd:"):
			continue
		case strings.HasPrefix(lower, "gitstatus:"):
			continue
		case strings.HasPrefix(lower, "current branch:"):
			continue
		case strings.HasPrefix(lower, "recent commits:"):
			continue
		case strings.HasPrefix(lower, "you have been invoked in the following environment"):
			continue
		}
		filtered = append(filtered, line)
	}
	return strings.Join(filtered, "\n")
}

func looksLikeBoltEnvironmentBlock(text string) bool {
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

func looksLikeClaudeCodeSystem(text string) bool {
	lower := strings.ToLower(text)
	for _, sig := range []string{
		"claude code",
		"claude agent sdk",
		"you are an interactive cli tool",
		"todowrite tool",
		"skill tool",
	} {
		if strings.Contains(lower, sig) {
			return true
		}
	}
	hits := 0
	for _, sig := range []string{
		"task tool",
		"vscode",
		"system-reminder",
		"claude-sonnet",
		"claude-opus",
		"enterplanmode",
		"exitplanmode",
		"auto memory",
		"# mcp server",
		"# vscode extension context",
	} {
		if strings.Contains(lower, sig) {
			hits++
			if hits >= 2 {
				return true
			}
		}
	}
	return false
}

func normalizeBlocks(msg prompt.Message) []prompt.ContentBlock {
	if msg.Content.IsString() {
		text := msg.Content.GetText()
		if strings.TrimSpace(text) == "" {
			return nil
		}
		return []prompt.ContentBlock{{Type: "text", Text: text}}
	}
	return msg.Content.GetBlocks()
}

func hasEphemeralCache(blocks []prompt.ContentBlock) bool {
	for _, block := range blocks {
		if block.CacheControl != nil && block.CacheControl.Type == "ephemeral" {
			return true
		}
	}
	return false
}

func extractTextContent(blocks []prompt.ContentBlock) string {
	var texts []string
	for _, block := range blocks {
		if block.Type == "text" && block.Text != "" {
			if text := sanitizeBoltMessageText(block.Text); text != "" {
				texts = append(texts, text)
			}
		}
	}
	return strings.Join(texts, "\n")
}

func extractBoltUserContent(blocks []prompt.ContentBlock) string {
	var parts []string
	for _, block := range blocks {
		switch block.Type {
		case "text":
			if text := sanitizeBoltMessageText(block.Text); text != "" {
				parts = append(parts, text)
			}
		case "tool_result":
			if text := strings.TrimSpace(stringifyContent(block.Content)); text != "" {
				parts = append(parts, "Tool result:\n"+text)
			}
		}
	}
	return strings.Join(parts, "\n\n")
}

func stringifyContent(v interface{}) string {
	switch x := v.(type) {
	case nil:
		return ""
	case string:
		return x
	case []prompt.ContentBlock:
		var parts []string
		for _, block := range x {
			if block.Type == "text" && strings.TrimSpace(block.Text) != "" {
				parts = append(parts, block.Text)
			}
		}
		if len(parts) > 0 {
			return strings.Join(parts, "\n")
		}
	case []interface{}:
		var parts []string
		for _, item := range x {
			if text, ok := item.(string); ok && strings.TrimSpace(text) != "" {
				parts = append(parts, text)
			}
		}
		if len(parts) > 0 {
			return strings.Join(parts, "\n")
		}
	}
	data, err := json.Marshal(v)
	if err != nil {
		return ""
	}
	return string(data)
}

func emitSSEMessage(onMessage func(upstream.SSEMessage), event string, payload []byte) error {
	if onMessage == nil {
		return nil
	}
	var eventMap map[string]interface{}
	if err := json.Unmarshal(payload, &eventMap); err != nil {
		return err
	}
	onMessage(upstream.SSEMessage{
		Type:    event,
		Event:   eventMap,
		RawJSON: append(json.RawMessage(nil), payload...),
	})
	return nil
}

func estimateInputTokens(messages []prompt.Message, system []prompt.SystemItem) int {
	totalChars := 0
	for _, item := range system {
		totalChars += len(item.Text)
	}
	for _, msg := range messages {
		totalChars += len(msg.ExtractText())
	}
	if totalChars <= 0 {
		return 0
	}
	return totalChars / 4
}

func generateRandomID(length int) string {
	bytes := make([]byte, length/2+1)
	if _, err := rand.Read(bytes); err != nil {
		return fmt.Sprintf("%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(bytes)[:length]
}

type outboundConverter struct {
	model          string
	inCodeBlock    bool
	codeBuffer     strings.Builder
	inputTokens    int
	outputTokens   int
	emittedToolUse bool
	seenToolCalls  map[string]struct{}
	suppressText   bool
}

func newOutboundConverter(model string, inputTokens int) *outboundConverter {
	return &outboundConverter{
		model:         model,
		inputTokens:   inputTokens,
		seenToolCalls: make(map[string]struct{}),
	}
}

func (c *outboundConverter) ProcessStream(reader io.Reader, writer func(event string, payload []byte) error) error {
	scanner := bufio.NewScanner(reader)
	buf := make([]byte, 0, 64*1024)
	scanner.Buffer(buf, 1024*1024)

	var textBuffer strings.Builder
	var pendingEndEvent *EndEvent
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			continue
		}
		colonIdx := strings.Index(line, ":")
		if colonIdx == -1 {
			continue
		}
		eventType := line[:colonIdx]
		eventData := line[colonIdx+1:]

		switch eventType {
		case "0":
			if err := c.processChunkData(eventData, &textBuffer, writer, true); err != nil {
				return err
			}
		case "8":
			continue
		case "9", "a":
			if err := c.processChunkData(eventData, &textBuffer, writer, false); err != nil {
				return err
			}
		case "e":
			endEvent, err := c.parseEndEvent(eventData)
			if err != nil {
				continue
			}
			pendingEndEvent = endEvent
			continue
		case "d":
			endEvent, err := c.parseEndEvent(eventData)
			if err != nil {
				endEvent = pendingEndEvent
			}
			if endEvent == nil {
				endEvent = pendingEndEvent
			}
			if endEvent == nil {
				continue
			}
			return c.flushAndFinish(endEvent, &textBuffer, writer)
		}
	}

	if err := scanner.Err(); err != nil {
		return err
	}
	if pendingEndEvent != nil {
		return c.flushAndFinish(pendingEndEvent, &textBuffer, writer)
	}
	return nil
}

func (c *outboundConverter) parseEndEvent(data string) (*EndEvent, error) {
	var event EndEvent
	if err := json.Unmarshal([]byte(data), &event); err != nil {
		return nil, err
	}
	return &event, nil
}

func (c *outboundConverter) processChunkData(data string, textBuffer *strings.Builder, writer func(event string, payload []byte) error, allowRawFallback bool) error {
	var text string
	if err := json.Unmarshal([]byte(data), &text); err == nil {
		text = strings.ReplaceAll(text, "626f6c742d63632d6167656e74", "")
		return c.processTextContent(text, textBuffer, writer)
	}

	var value interface{}
	if err := json.Unmarshal([]byte(data), &value); err != nil {
		if allowRawFallback {
			return c.processTextContent(strings.TrimSpace(data), textBuffer, writer)
		}
		return nil
	}

	return c.processStructuredValue(value, textBuffer, writer, allowRawFallback)
}

func (c *outboundConverter) processStructuredValue(value interface{}, textBuffer *strings.Builder, writer func(event string, payload []byte) error, allowRawFallback bool) error {
	if toolCalls := extractToolCallsFromValue(value); len(toolCalls) > 0 {
		return c.flushTextAndSendToolCalls(toolCalls, textBuffer, writer)
	}

	switch v := value.(type) {
	case string:
		return c.processTextContent(v, textBuffer, writer)
	case []interface{}:
		for _, item := range v {
			if err := c.processStructuredValue(item, textBuffer, writer, allowRawFallback); err != nil {
				return err
			}
		}
		return nil
	case map[string]interface{}:
		for _, key := range []string{"parts", "content", "delta", "data", "messages"} {
			if nested, ok := v[key]; ok {
				switch nested.(type) {
				case []interface{}, map[string]interface{}:
					if err := c.processStructuredValue(nested, textBuffer, writer, false); err != nil {
						return err
					}
				}
			}
		}
		for _, key := range []string{"text", "message", "content", "delta", "output", "response"} {
			if raw, ok := v[key].(string); ok && strings.TrimSpace(raw) != "" {
				return c.processTextContent(raw, textBuffer, writer)
			}
		}
	}

	if !allowRawFallback {
		return nil
	}
	raw, err := json.Marshal(value)
	if err != nil {
		return nil
	}
	return c.processTextContent(string(raw), textBuffer, writer)
}

func (c *outboundConverter) processTextContent(text string, textBuffer *strings.Builder, writer func(event string, payload []byte) error) error {
	if c.suppressText {
		return nil
	}
	trimmed := strings.TrimSpace(text)
	if strings.HasPrefix(trimmed, "{") && strings.HasSuffix(trimmed, "}") {
		if toolCalls := extractToolCallsFromJSON([]byte(trimmed)); len(toolCalls) > 0 {
			return c.flushTextAndSendToolCalls(toolCalls, textBuffer, writer)
		}
	}

	if !c.inCodeBlock && strings.Contains(text, "```") {
		idx := strings.Index(text, "```")
		beforeBlock := text[:idx]
		afterMarker := text[idx+3:]
		textBuffer.WriteString(beforeBlock)
		if textBuffer.Len() > 0 {
			if err := c.sendTextDelta(textBuffer.String(), writer); err != nil {
				return err
			}
			textBuffer.Reset()
		}
		c.inCodeBlock = true
		afterMarker = strings.TrimPrefix(afterMarker, "json")
		c.codeBuffer.WriteString(afterMarker)
		return nil
	}

	if c.inCodeBlock {
		if c.codeBuffer.Len() == 0 {
			text = strings.TrimPrefix(text, "json")
			text = strings.TrimPrefix(text, "JSON")
			text = strings.TrimPrefix(text, "\n")
		}
		if strings.Contains(text, "```") {
			idx := strings.Index(text, "```")
			beforeEnd := text[:idx]
			afterEnd := text[idx+3:]
			c.codeBuffer.WriteString(beforeEnd)
			codeContent := strings.TrimSpace(c.codeBuffer.String())
			c.codeBuffer.Reset()
			c.inCodeBlock = false
			if err := c.processCodeBlock(codeContent, textBuffer, writer); err != nil {
				return err
			}
			if afterEnd != "" {
				textBuffer.WriteString(afterEnd)
			}
		} else {
			c.codeBuffer.WriteString(text)
		}
		return nil
	}

	textBuffer.WriteString(text)
	return nil
}

func (c *outboundConverter) processCodeBlock(content string, textBuffer *strings.Builder, writer func(event string, payload []byte) error) error {
	if toolCalls := extractToolCallsFromJSON([]byte(content)); len(toolCalls) > 0 {
		return c.flushTextAndSendToolCalls(toolCalls, textBuffer, writer)
	}

	textBuffer.WriteString("```json\n")
	textBuffer.WriteString(content)
	textBuffer.WriteString("\n```")
	return nil
}

func (c *outboundConverter) sendTextDelta(text string, writer func(event string, payload []byte) error) error {
	text = strings.TrimSpace(text)
	if text == "" {
		return nil
	}
	event := map[string]interface{}{
		"delta": text,
	}
	raw, err := json.Marshal(event)
	if err != nil {
		return err
	}
	return writer("model.text-delta", raw)
}

func (c *outboundConverter) sendToolUse(toolCall *ToolCall, writer func(event string, payload []byte) error) error {
	if toolCall == nil || strings.TrimSpace(toolCall.Function) == "" {
		return nil
	}
	params := normalizeToolCallParameters(toolCall.Parameters)
	key := toolCall.Function + "\x00" + string(params)
	if _, exists := c.seenToolCalls[key]; exists {
		return nil
	}
	c.seenToolCalls[key] = struct{}{}
	event := map[string]interface{}{
		"toolCallId": "toolu_" + generateRandomID(20),
		"toolName":   strings.TrimSpace(toolCall.Function),
		"input":      string(params),
	}
	raw, err := json.Marshal(event)
	if err != nil {
		return err
	}
	c.emittedToolUse = true
	c.suppressText = true
	return writer("model.tool-call", raw)
}

func (c *outboundConverter) flushTextAndSendToolCalls(toolCalls []ToolCall, textBuffer *strings.Builder, writer func(event string, payload []byte) error) error {
	if err := c.flushTextBuffer(textBuffer, writer); err != nil {
		return err
	}
	for i := range toolCalls {
		if err := c.sendToolUse(&toolCalls[i], writer); err != nil {
			return err
		}
	}
	return nil
}

func (c *outboundConverter) flushTextBuffer(textBuffer *strings.Builder, writer func(event string, payload []byte) error) error {
	if textBuffer == nil || textBuffer.Len() == 0 {
		return nil
	}
	if err := c.sendTextDelta(textBuffer.String(), writer); err != nil {
		return err
	}
	textBuffer.Reset()
	return nil
}

func extractToolCallsFromJSON(data []byte) []ToolCall {
	var value interface{}
	if err := json.Unmarshal(data, &value); err != nil {
		return nil
	}
	return extractToolCallsFromValue(value)
}

func extractToolCallsFromValue(value interface{}) []ToolCall {
	switch v := value.(type) {
	case []interface{}:
		var calls []ToolCall
		for _, item := range v {
			calls = append(calls, extractToolCallsFromValue(item)...)
		}
		return calls
	case map[string]interface{}:
		for _, key := range []string{"tool_calls", "toolCalls", "calls", "tool_call", "toolCall"} {
			if nested, ok := v[key]; ok {
				if calls := extractToolCallsFromValue(nested); len(calls) > 0 {
					return calls
				}
			}
		}
		if call, ok := parseToolCallValue(v); ok {
			return []ToolCall{call}
		}
	}
	return nil
}

func parseToolCallValue(v map[string]interface{}) (ToolCall, bool) {
	typeHint := strings.ToLower(strings.TrimSpace(stringValue(v["type"])))

	if toolName := strings.TrimSpace(stringValue(v["tool"])); toolName != "" {
		return ToolCall{Function: toolName, Parameters: normalizeToolCallParameters(firstNonNil(v["parameters"], v["params"], v["arguments"], v["args"], v["input"]))}, true
	}

	if functionValue, ok := v["function"].(map[string]interface{}); ok {
		if toolName := strings.TrimSpace(stringValue(functionValue["name"])); toolName != "" {
			args := firstNonNil(functionValue["parameters"], functionValue["arguments"], v["parameters"], v["arguments"], v["args"], v["input"])
			return ToolCall{Function: toolName, Parameters: normalizeToolCallParameters(args)}, true
		}
	}

	name := strings.TrimSpace(stringValue(v["name"]))
	if name == "" {
		name = strings.TrimSpace(stringValue(v["function"]))
	}
	if name == "" {
		name = strings.TrimSpace(stringValue(v["toolName"]))
	}
	if name == "" {
		return ToolCall{}, false
	}

	args := firstNonNil(v["parameters"], v["params"], v["arguments"], v["args"], v["input"])
	if args == nil && typeHint != "tool_use" && typeHint != "tool_call" && typeHint != "function" {
		return ToolCall{}, false
	}

	return ToolCall{Function: name, Parameters: normalizeToolCallParameters(args)}, true
}

func firstNonNil(values ...interface{}) interface{} {
	for _, value := range values {
		if value != nil {
			return value
		}
	}
	return nil
}

func stringValue(value interface{}) string {
	switch v := value.(type) {
	case string:
		return v
	case fmt.Stringer:
		return v.String()
	default:
		return ""
	}
}

func normalizeToolCallParameters(value interface{}) json.RawMessage {
	switch v := value.(type) {
	case nil:
		return json.RawMessage("{}")
	case json.RawMessage:
		if len(v) == 0 {
			return json.RawMessage("{}")
		}
		return v
	case string:
		trimmed := strings.TrimSpace(v)
		if trimmed == "" {
			return json.RawMessage("{}")
		}
		if json.Valid([]byte(trimmed)) {
			return json.RawMessage(trimmed)
		}
	}

	data, err := json.Marshal(value)
	if err != nil || len(data) == 0 || string(data) == "null" {
		return json.RawMessage("{}")
	}
	return json.RawMessage(data)
}

func (c *outboundConverter) sendEndEvents(endEvent *EndEvent, writer func(event string, payload []byte) error) error {
	c.outputTokens = endEvent.Usage.CompletionTokens
	finishReason := "end_turn"
	if c.emittedToolUse || strings.EqualFold(strings.TrimSpace(endEvent.FinishReason), "tool_use") {
		finishReason = "tool_use"
	}

	event := map[string]interface{}{
		"finishReason": finishReason,
		"usage": map[string]interface{}{
			"inputTokens":   c.inputTokens,
			"outputTokens":  c.outputTokens,
			"input_tokens":  c.inputTokens,
			"output_tokens": c.outputTokens,
		},
	}
	raw, err := json.Marshal(event)
	if err != nil {
		return err
	}
	return writer("model.finish", raw)
}

func (c *outboundConverter) flushAndFinish(endEvent *EndEvent, textBuffer *strings.Builder, writer func(event string, payload []byte) error) error {
	if c.inCodeBlock && c.codeBuffer.Len() > 0 {
		codeContent := strings.TrimSpace(c.codeBuffer.String())
		c.codeBuffer.Reset()
		c.inCodeBlock = false
		if err := c.processCodeBlock(codeContent, textBuffer, writer); err != nil {
			return err
		}
	}
	if err := c.flushTextBuffer(textBuffer, writer); err != nil {
		return err
	}
	return c.sendEndEvents(endEvent, writer)
}
