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
	"orchids-api/internal/tiktoken"
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
var supportedBoltToolSet = map[string]struct{}{
	"Read":      {},
	"Write":     {},
	"Edit":      {},
	"Bash":      {},
	"Glob":      {},
	"Grep":      {},
	"TodoWrite": {},
}
var boltStripTaggedNames = []string{
	"local-command-caveat",
	"command-name",
	"command-message",
	"command-args",
	"local-command-stdout",
	"local-command-stderr",
	"local-command-exit-code",
	"ide_opened_file",
	"ide_selection",
}
var boltInvalidPathMarkers = []string{"/tmp/cc-agent/", "/mnt/", "d:\\", "c:\\", "~/"}
var boltEnvironmentBlockMarkers = []string{
	"# environment",
	"primary working directory:",
	"# auto memory",
	"gitstatus:",
	"you have been invoked in the following environment",
}
var boltStructuredNestedKeys = []string{"parts", "content", "delta", "data", "messages"}
var boltStructuredStringKeys = []string{"text", "message", "content", "delta", "output", "response"}
var boltToolCallCollectionKeys = []string{"tool_calls", "toolCalls", "calls", "tool_call", "toolCall"}
var boltEmptyParts = make([]Part, 0)
var boltEmptyInterfaces = make([]interface{}, 0)

type InputTokenEstimate struct {
	BasePromptTokens    int
	SystemContextTokens int
	HistoryTokens       int
	ToolsTokens         int
	Total               int
}

type builtMessages struct {
	Items         []Message
	HistoryTokens int
}

type systemPromptParts struct {
	BasePrompt   string
	ToolPrompt   string
	SystemPrompt string
	FullPrompt   string
}

type Client struct {
	httpClient       *http.Client
	sessionToken     string
	projectID        string
	sharedHTTPClient bool
}

func NewFromAccount(acc *store.Account, cfg *config.Config) *Client {
	timeout := 30 * time.Second
	if cfg != nil && cfg.RequestTimeout > 0 {
		timeout = time.Duration(cfg.RequestTimeout) * time.Second
	}

	proxyFunc := http.ProxyFromEnvironment
	proxyKey := "direct"
	if cfg != nil {
		proxyFunc = util.ProxyFunc(cfg.ProxyHTTP, cfg.ProxyHTTPS, cfg.ProxyUser, cfg.ProxyPass, cfg.ProxyBypass)
		proxyKey = util.GenerateProxyKey(cfg.ProxyHTTP, cfg.ProxyHTTPS, cfg.ProxyUser)
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
		httpClient:       util.GetSharedHTTPClient(proxyKey, timeout, proxyFunc),
		sessionToken:     sessionToken,
		projectID:        projectID,
		sharedHTTPClient: true,
	}
}

func (c *Client) Close() {
	if c == nil || c.sharedHTTPClient || c.httpClient == nil || c.httpClient.Transport == nil {
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

	boltReq, inputEstimate := prepareRequest(req, projectID)
	body, err := json.Marshal(boltReq)
	if err != nil {
		return fmt.Errorf("failed to marshal bolt request: %w", err)
	}
	if logger != nil {
		logger.LogUpstreamRequest(boltAPIURL, map[string]string{"provider": "bolt"}, body)
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

	converter := newOutboundConverter(req.Model, inputEstimate.Total)
	return converter.ProcessStream(resp.Body, func(msg upstream.SSEMessage) error {
		if onMessage != nil {
			onMessage(msg)
		}
		return nil
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

func EstimateInputTokens(req upstream.UpstreamRequest) InputTokenEstimate {
	_, estimate := prepareRequest(req, strings.TrimSpace(req.ProjectID))
	return estimate
}

func prepareRequest(req upstream.UpstreamRequest, projectID string) (*Request, InputTokenEstimate) {
	promptParts := buildSystemPromptParts(req.System, req.Workdir, req.Tools, req.NoTools, req.Messages)
	messages := buildBoltMessages(req.Messages)

	boltReq := &Request{
		ID:                   generateRandomID(16),
		SelectedModel:        strings.TrimSpace(req.Model),
		IsFirstPrompt:        false,
		PromptMode:           "build",
		EffortLevel:          "high",
		ProjectID:            projectID,
		GlobalSystemPrompt:   promptParts.FullPrompt,
		ProjectPrompt:        promptParts.FullPrompt,
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
			Visible: boltEmptyInterfaces,
			Hidden:  boltEmptyInterfaces,
		},
		RunningCommands: boltEmptyInterfaces,
		Dependencies:    boltEmptyInterfaces,
		Problems:        "",
		Messages:        messages.Items,
	}
	return boltReq, estimatePreparedRequestInput(promptParts, messages.HistoryTokens)
}

func shouldSkipBoltMessage(role string) bool {
	return strings.EqualFold(strings.TrimSpace(role), "tool")
}

func buildSystemPromptParts(system []prompt.SystemItem, workdir string, tools []interface{}, noTools bool, messages []prompt.Message) systemPromptParts {
	parts := systemPromptParts{
		BasePrompt: defaultAdapterPrompt,
		ToolPrompt: buildBoltToolPrompt(workdir, tools, noTools, messages),
	}

	custom := make([]string, 0, len(system))
	for _, item := range system {
		text := sanitizeBoltSystemText(item.Text)
		if strings.TrimSpace(text) != "" {
			custom = append(custom, text)
		}
	}
	parts.SystemPrompt = strings.Join(custom, "\n\n")

	combined := make([]string, 0, 3)
	for _, part := range []string{parts.BasePrompt, parts.ToolPrompt, parts.SystemPrompt} {
		if strings.TrimSpace(part) != "" {
			combined = append(combined, part)
		}
	}
	parts.FullPrompt = strings.Join(combined, "\n\n")
	return parts
}

func buildBoltMessages(messages []prompt.Message) builtMessages {
	built := builtMessages{
		Items: make([]Message, 0, len(messages)),
	}
	var lastUserMsgID string
	for _, msg := range messages {
		blocks := normalizeBlocks(msg)
		if shouldSkipBoltMessage(msg.Role) {
			continue
		}

		boltMsg := Message{
			ID:    generateRandomID(16),
			Role:  msg.Role,
			Cache: hasEphemeralCache(blocks),
			Parts: boltEmptyParts,
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
		built.Items = append(built.Items, boltMsg)
		built.HistoryTokens += estimateBoltMessageTokens(boltMsg)
	}
	return built
}

func estimatePreparedRequestInput(parts systemPromptParts, historyTokens int) InputTokenEstimate {
	estimate := InputTokenEstimate{
		BasePromptTokens:    estimateBoltTextTokens(parts.BasePrompt),
		SystemContextTokens: estimateBoltTextTokens(parts.SystemPrompt),
		ToolsTokens:         estimateBoltTextTokens(parts.ToolPrompt),
		HistoryTokens:       historyTokens,
	}
	estimate.Total = estimate.BasePromptTokens + estimate.SystemContextTokens + estimate.ToolsTokens + estimate.HistoryTokens
	return estimate
}

func estimateBoltTextTokens(text string) int {
	text = strings.TrimSpace(text)
	if text == "" {
		return 0
	}
	return tiktoken.EstimateTextTokens(text)
}

func estimateBoltMessageTokens(msg Message) int {
	text := strings.TrimSpace(msg.Content)
	if text == "" {
		return 0
	}

	overhead := 15
	if len(msg.Annotations) > 0 {
		overhead += 5 * len(msg.Annotations)
	}
	return tiktoken.EstimateTextTokens(text) + overhead
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
	if strings.Contains(workdir, "\\") {
		windowsPath := strings.ReplaceAll(workdir, "\\", "/")
		if base := filepath.Base(filepath.Clean(windowsPath)); strings.TrimSpace(base) != "" {
			projectName = base
		}
	}
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
	for _, marker := range boltInvalidPathMarkers {
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
	_, ok := supportedBoltToolSet[strings.TrimSpace(name)]
	return ok
}

func sanitizeBoltSystemText(text string) string {
	text = stripTaggedBoltText(text)
	text = stripCCEntrypointLines(text)
	text = stripBoltEnvironmentLines(text)
	return condenseBoltSystemText(text)
}

func condenseBoltSystemText(text string) string {
	if strings.TrimSpace(text) == "" {
		return ""
	}
	lines := strings.Split(text, "\n")
	kept := make([]string, 0, len(lines))
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			continue
		}
		if shouldDropBoltSystemLine(trimmed) {
			continue
		}
		kept = append(kept, trimmed)
	}
	return strings.Join(kept, "\n")
}

func shouldDropBoltSystemLine(line string) bool {
	lower := strings.ToLower(strings.TrimSpace(line))
	switch {
	case lower == "":
		return true
	case strings.Contains(lower, "anthropic's official cli for claude"):
		return true
	case strings.Contains(lower, "you are claude code"):
		return true
	case strings.Contains(lower, "you are an interactive cli tool"):
		return true
	case strings.Contains(lower, "claude agent sdk"):
		return true
	case strings.Contains(lower, "claude code system prompt"):
		return true
	}
	return false
}

func sanitizeBoltMessageText(text string) string {
	return strings.TrimSpace(stripTaggedBoltText(text))
}

func sanitizeBoltAssistantText(text string) string {
	if strings.TrimSpace(text) == "" {
		return ""
	}
	text = stripNestedTaggedBlock(text, "thinking")
	text = stripTaggedBoltText(text)
	text = stripCCEntrypointLines(text)
	text = stripBoltEnvironmentLines(text)
	text = stripBoltToolTranscript(text)
	text = collapseBoltBlankLines(text)
	if strings.TrimSpace(text) == "" {
		return ""
	}
	return text
}

func stripTaggedBoltText(text string) string {
	text = stripNestedTaggedBlock(text, "system-reminder")
	for _, tag := range boltStripTaggedNames {
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
	for _, marker := range boltEnvironmentBlockMarkers {
		if strings.Contains(lower, marker) {
			markers++
		}
	}
	return markers >= 2
}

func stripBoltToolTranscript(text string) string {
	if strings.TrimSpace(text) == "" {
		return ""
	}

	lines := strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n")
	filtered := make([]string, 0, len(lines))
	inTranscript := false
	justExitedTranscript := false

	for _, line := range lines {
		trimmed := strings.TrimSpace(strings.ReplaceAll(line, "\u00a0", " "))
		if isBoltToolTranscriptHeader(trimmed) {
			inTranscript = true
			justExitedTranscript = false
			continue
		}
		if inTranscript {
			if trimmed == "" || isBoltIndentedTranscriptLine(line) || strings.HasPrefix(trimmed, "⎿") || strings.HasPrefix(trimmed, "... +") || strings.HasPrefix(trimmed, "… +") {
				continue
			}
			inTranscript = false
			justExitedTranscript = true
		}

		if justExitedTranscript {
			if cleaned, ok := stripBoltTranscriptBullet(trimmed); ok {
				line = cleaned
				trimmed = cleaned
			}
			justExitedTranscript = false
		}

		filtered = append(filtered, strings.TrimRight(line, " \t"))
	}

	return strings.Join(filtered, "\n")
}

func isBoltToolTranscriptHeader(line string) bool {
	if line == "" {
		return false
	}
	lower := strings.ToLower(line)
	if strings.Contains(lower, "ctrl+o to expand") {
		return true
	}
	if !strings.HasPrefix(line, "● ") {
		return false
	}
	rest := strings.TrimSpace(strings.TrimPrefix(line, "● "))
	for _, name := range supportedBoltToolOrder {
		if strings.HasPrefix(rest, name+"(") {
			return true
		}
	}
	return false
}

func isBoltIndentedTranscriptLine(line string) bool {
	if line == "" {
		return false
	}
	switch line[0] {
	case ' ', '\t':
		return true
	default:
		return false
	}
}

func stripBoltTranscriptBullet(line string) (string, bool) {
	if !strings.HasPrefix(line, "● ") {
		return "", false
	}
	return strings.TrimSpace(strings.TrimPrefix(line, "● ")), true
}

func collapseBoltBlankLines(text string) string {
	lines := strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n")
	out := make([]string, 0, len(lines))
	prevBlank := false
	for _, line := range lines {
		if strings.TrimSpace(line) == "" {
			if prevBlank {
				continue
			}
			prevBlank = true
			out = append(out, "")
			continue
		}
		prevBlank = false
		out = append(out, line)
	}
	return strings.Join(out, "\n")
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
	var sb strings.Builder
	first := true
	for _, block := range blocks {
		if block.Type == "text" && block.Text != "" {
			if text := sanitizeBoltMessageText(block.Text); text != "" {
				if !first {
					sb.WriteByte('\n')
				}
				sb.WriteString(text)
				first = false
			}
		}
	}
	return sb.String()
}

func extractBoltUserContent(blocks []prompt.ContentBlock) string {
	var sb strings.Builder
	first := true
	for _, block := range blocks {
		switch block.Type {
		case "text":
			if text := sanitizeBoltMessageText(block.Text); text != "" {
				if !first {
					sb.WriteString("\n\n")
				}
				sb.WriteString(text)
				first = false
			}
		case "tool_result":
			if text := strings.TrimSpace(stringifyContent(block.Content)); text != "" {
				if !first {
					sb.WriteString("\n\n")
				}
				sb.WriteString("Tool result:\n")
				sb.WriteString(text)
				first = false
			}
		}
	}
	return sb.String()
}

func stringifyContent(v interface{}) string {
	switch x := v.(type) {
	case nil:
		return ""
	case string:
		return x
	case []prompt.ContentBlock:
		var sb strings.Builder
		first := true
		for _, block := range x {
			if block.Type == "text" && strings.TrimSpace(block.Text) != "" {
				if !first {
					sb.WriteByte('\n')
				}
				sb.WriteString(block.Text)
				first = false
			}
		}
		if !first {
			return sb.String()
		}
	case []interface{}:
		var sb strings.Builder
		first := true
		for _, item := range x {
			if text, ok := item.(string); ok && strings.TrimSpace(text) != "" {
				if !first {
					sb.WriteByte('\n')
				}
				sb.WriteString(text)
				first = false
			}
		}
		if !first {
			return sb.String()
		}
	}
	data, err := json.Marshal(v)
	if err != nil {
		return ""
	}
	return string(data)
}

func generateRandomID(length int) string {
	bytes := make([]byte, length/2+1)
	if _, err := rand.Read(bytes); err != nil {
		return fmt.Sprintf("%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(bytes)[:length]
}

type outboundConverter struct {
	model                   string
	inCodeBlock             bool
	codeBuffer              strings.Builder
	codeFenceHeader         strings.Builder
	awaitingCodeFenceHeader bool
	inputTokens             int
	outputTokens            int
	emittedToolUse          bool
	seenToolCalls           map[string]struct{}
	suppressText            bool
}

func newOutboundConverter(model string, inputTokens int) *outboundConverter {
	return &outboundConverter{
		model:         model,
		inputTokens:   inputTokens,
		seenToolCalls: make(map[string]struct{}),
	}
}

func (c *outboundConverter) ProcessStream(reader io.Reader, writer func(upstream.SSEMessage) error) error {
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

func (c *outboundConverter) processChunkData(data string, textBuffer *strings.Builder, writer func(upstream.SSEMessage) error, allowRawFallback bool) error {
	switch firstNonSpaceByte(data) {
	case '"':
		var text string
		if err := json.Unmarshal([]byte(data), &text); err != nil {
			if allowRawFallback {
				return c.processTextContent(strings.TrimSpace(data), textBuffer, writer)
			}
			return nil
		}
		text = strings.ReplaceAll(text, "626f6c742d63632d6167656e74", "")
		return c.processTextContent(text, textBuffer, writer)
	case '{', '[':
		var value interface{}
		if err := json.Unmarshal([]byte(data), &value); err != nil {
			if allowRawFallback {
				return c.processTextContent(strings.TrimSpace(data), textBuffer, writer)
			}
			return nil
		}
		return c.processStructuredValue(value, textBuffer, writer, allowRawFallback)
	default:
		if allowRawFallback {
			return c.processTextContent(strings.TrimSpace(data), textBuffer, writer)
		}
		return nil
	}
}

func (c *outboundConverter) processStructuredValue(value interface{}, textBuffer *strings.Builder, writer func(upstream.SSEMessage) error, allowRawFallback bool) error {
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
		for _, key := range boltStructuredNestedKeys {
			if nested, ok := v[key]; ok {
				switch nested.(type) {
				case []interface{}, map[string]interface{}:
					if err := c.processStructuredValue(nested, textBuffer, writer, false); err != nil {
						return err
					}
				}
			}
		}
		for _, key := range boltStructuredStringKeys {
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

func (c *outboundConverter) processTextContent(text string, textBuffer *strings.Builder, writer func(upstream.SSEMessage) error) error {
	if c.suppressText {
		return nil
	}
	trimmed := strings.TrimSpace(text)
	if strings.HasPrefix(trimmed, "{") && strings.HasSuffix(trimmed, "}") {
		if toolCalls := extractToolCallsFromJSON([]byte(trimmed)); len(toolCalls) > 0 {
			return c.flushTextAndSendToolCalls(toolCalls, textBuffer, writer)
		}
	}

	if !c.inCodeBlock {
		idx := strings.Index(text, "```")
		if idx >= 0 {
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
			c.awaitingCodeFenceHeader = true
			c.codeFenceHeader.Reset()
			c.codeBuffer.Reset()
			text = afterMarker
		}
	}

	if c.inCodeBlock {
		if idx := strings.Index(text, "```"); idx >= 0 {
			beforeEnd := text[:idx]
			afterEnd := text[idx+3:]
			c.appendCodeBlockContent(beforeEnd)
			codeContent := strings.Trim(c.codeBuffer.String(), "\r\n")
			fenceHeader := strings.TrimSpace(c.codeFenceHeader.String())
			c.codeBuffer.Reset()
			c.codeFenceHeader.Reset()
			c.inCodeBlock = false
			c.awaitingCodeFenceHeader = false
			if err := c.processCodeBlock(fenceHeader, codeContent, textBuffer, writer); err != nil {
				return err
			}
			if afterEnd != "" {
				textBuffer.WriteString(afterEnd)
			}
		} else {
			c.appendCodeBlockContent(text)
		}
		return nil
	}

	textBuffer.WriteString(text)
	return nil
}

func (c *outboundConverter) appendCodeBlockContent(text string) {
	if !c.awaitingCodeFenceHeader {
		c.codeBuffer.WriteString(text)
		return
	}
	if text == "" {
		return
	}

	if idx := strings.IndexAny(text, "\r\n"); idx >= 0 {
		c.codeFenceHeader.WriteString(text[:idx])
		c.awaitingCodeFenceHeader = false
		next := idx + 1
		if text[idx] == '\r' && next < len(text) && text[next] == '\n' {
			next++
		}
		if next < len(text) {
			c.codeBuffer.WriteString(text[next:])
		}
		return
	}

	c.codeFenceHeader.WriteString(text)
}

func (c *outboundConverter) processCodeBlock(fenceHeader string, content string, textBuffer *strings.Builder, writer func(upstream.SSEMessage) error) error {
	if toolCalls := extractToolCallsFromJSON([]byte(content)); len(toolCalls) > 0 {
		return c.flushTextAndSendToolCalls(toolCalls, textBuffer, writer)
	}

	textBuffer.WriteString("```")
	if fenceHeader != "" {
		textBuffer.WriteString(fenceHeader)
	}
	textBuffer.WriteByte('\n')
	textBuffer.WriteString(content)
	textBuffer.WriteString("\n```")
	return nil
}

func (c *outboundConverter) sendTextDelta(text string, writer func(upstream.SSEMessage) error) error {
	text = sanitizeBoltAssistantText(text)
	if text == "" {
		return nil
	}
	return writeBoltStreamMessage(writer, "model.text-delta", map[string]interface{}{
		"delta": text,
	})
}

func (c *outboundConverter) sendToolUse(toolCall *ToolCall, writer func(upstream.SSEMessage) error) error {
	if toolCall == nil || strings.TrimSpace(toolCall.Function) == "" {
		return nil
	}
	params := normalizeToolCallParameters(toolCall.Parameters)
	key := toolCall.Function + "\x00" + string(params)
	if _, exists := c.seenToolCalls[key]; exists {
		return nil
	}
	c.seenToolCalls[key] = struct{}{}
	c.emittedToolUse = true
	c.suppressText = true
	return writeBoltStreamMessage(writer, "model.tool-call", map[string]interface{}{
		"toolCallId": "toolu_" + generateRandomID(20),
		"toolName":   strings.TrimSpace(toolCall.Function),
		"input":      string(params),
	})
}

func (c *outboundConverter) flushTextAndSendToolCalls(toolCalls []ToolCall, textBuffer *strings.Builder, writer func(upstream.SSEMessage) error) error {
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

func (c *outboundConverter) flushTextBuffer(textBuffer *strings.Builder, writer func(upstream.SSEMessage) error) error {
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
	var calls []ToolCall
	appendToolCallsFromValue(&calls, value)
	return calls
}

func appendToolCallsFromValue(calls *[]ToolCall, value interface{}) {
	switch v := value.(type) {
	case []interface{}:
		for _, item := range v {
			appendToolCallsFromValue(calls, item)
		}
	case map[string]interface{}:
		for _, key := range boltToolCallCollectionKeys {
			if nested, ok := v[key]; ok {
				appendToolCallsFromValue(calls, nested)
				if len(*calls) > 0 {
					return
				}
			}
		}
		if call, ok := parseToolCallValue(v); ok {
			*calls = append(*calls, call)
		}
	}
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

func (c *outboundConverter) sendEndEvents(endEvent *EndEvent, writer func(upstream.SSEMessage) error) error {
	c.outputTokens = endEvent.Usage.CompletionTokens
	finishReason := "end_turn"
	if c.emittedToolUse || strings.EqualFold(strings.TrimSpace(endEvent.FinishReason), "tool_use") {
		finishReason = "tool_use"
	}

	return writeBoltStreamMessage(writer, "model.finish", map[string]interface{}{
		"finishReason": finishReason,
		"usage": map[string]interface{}{
			"inputTokens":   c.inputTokens,
			"outputTokens":  c.outputTokens,
			"input_tokens":  c.inputTokens,
			"output_tokens": c.outputTokens,
		},
	})
}

func (c *outboundConverter) flushAndFinish(endEvent *EndEvent, textBuffer *strings.Builder, writer func(upstream.SSEMessage) error) error {
	if c.inCodeBlock && c.codeBuffer.Len() > 0 {
		codeContent := strings.Trim(c.codeBuffer.String(), "\r\n")
		fenceHeader := strings.TrimSpace(c.codeFenceHeader.String())
		c.codeBuffer.Reset()
		c.codeFenceHeader.Reset()
		c.inCodeBlock = false
		c.awaitingCodeFenceHeader = false
		if err := c.processCodeBlock(fenceHeader, codeContent, textBuffer, writer); err != nil {
			return err
		}
	}
	if err := c.flushTextBuffer(textBuffer, writer); err != nil {
		return err
	}
	return c.sendEndEvents(endEvent, writer)
}

func writeBoltStreamMessage(writer func(upstream.SSEMessage) error, eventType string, eventMap map[string]interface{}) error {
	if writer == nil {
		return nil
	}
	raw, err := json.Marshal(eventMap)
	if err != nil {
		return err
	}
	return writer(upstream.SSEMessage{
		Type:    eventType,
		Event:   eventMap,
		RawJSON: raw,
	})
}

func firstNonSpaceByte(text string) byte {
	for i := 0; i < len(text); i++ {
		switch text[i] {
		case ' ', '\t', '\r', '\n':
			continue
		default:
			return text[i]
		}
	}
	return 0
}
