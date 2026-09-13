package debug

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"github.com/goccy/go-json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const (
	maxDebugStreamLogBytes int64 = 8 << 20
	maxDebugLogDirectories       = 512
)

var debugLoggerCreateCount atomic.Uint64

// Logger 调试日志记录器
type Logger struct {
	capture    *Capture
	attempt    *UpstreamAttempt
	enabled    bool
	sseEnabled bool
	dir        string
	rawFile    *os.File
	outFile    *os.File
	rawBytes   int64
	outBytes   int64
	mu         sync.Mutex
	startTime  time.Time
}

// New 创建新的调试日志记录器
func New(enabled bool, sseEnabled bool) *Logger {
	if !enabled {
		return &Logger{enabled: false}
	}

	now := time.Now()
	timestamp := now.Format("2006-01-02_15-04-05.000")
	suffix := "0000"
	var randBytes [2]byte
	if _, err := rand.Read(randBytes[:]); err == nil {
		suffix = hex.EncodeToString(randBytes[:])
	}
	dir := filepath.Join("debug-logs", fmt.Sprintf("%s_%s", timestamp, suffix))
	if err := os.MkdirAll(dir, 0700); err != nil {
		return &Logger{enabled: false}
	}
	if count := debugLoggerCreateCount.Add(1); count%64 == 0 {
		pruneDebugLogDirectories("debug-logs", maxDebugLogDirectories)
	}

	return &Logger{
		enabled:    true,
		sseEnabled: sseEnabled,
		dir:        dir,
		startTime:  time.Now(),
	}
}

func pruneDebugLogDirectories(root string, keep int) {
	if keep < 1 {
		return
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		return
	}
	dirs := make([]string, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() {
			dirs = append(dirs, entry.Name())
		}
	}
	if len(dirs) <= keep {
		return
	}
	sort.Strings(dirs)
	for _, name := range dirs[:len(dirs)-keep] {
		_ = os.RemoveAll(filepath.Join(root, name))
	}
}

// CleanupAllLogs 清空所有调试日志（启动时调用）
func CleanupAllLogs() error {
	if err := os.RemoveAll("debug-logs"); err != nil {
		return err
	}
	return os.MkdirAll("debug-logs", 0700)
}

// LogIncomingRequest 记录 1. 进入的 Claude API 请求
func (l *Logger) LogIncomingRequest(req interface{}) {
	if !l.enabled {
		return
	}
	l.writeJSON("1_claude_request.json", req)
}

// LogEarlyExit 记录提前返回的原因
func (l *Logger) LogEarlyExit(reason string, details map[string]interface{}) {
	if !l.enabled {
		return
	}
	payload := map[string]interface{}{
		"reason":     reason,
		"elapsed_ms": time.Since(l.startTime).Milliseconds(),
	}
	if details != nil {
		payload["details"] = details
	}
	l.writeJSON("1_early_exit.json", payload)
}

// LogConvertedPrompt 记录 2. 转换后的 prompt
func (l *Logger) LogConvertedPrompt(prompt string) {
	if !l.enabled {
		return
	}
	l.writeFile("2_converted_prompt.md", prompt)
}

// LogUpstreamRequest 记录 3. 发送给上游的请求
func (l *Logger) LogUpstreamRequest(url string, headers map[string]string, body interface{}) {
	if !l.enabled {
		return
	}

	safeHeaders := make(map[string]string, len(headers))
	for key, value := range headers {
		switch strings.ToLower(strings.TrimSpace(key)) {
		case "authorization", "proxy-authorization", "cookie", "set-cookie", "x-api-key", "api-key":
			safeHeaders[key] = "[REDACTED]"
		default:
			safeHeaders[key] = value
		}
	}
	if raw, ok := body.([]byte); ok {
		if json.Valid(raw) {
			body = json.RawMessage(raw)
		} else {
			body = string(raw)
		}
	}
	data := map[string]interface{}{
		"url":     url,
		"headers": safeHeaders,
		"body":    body,
	}
	if l.capture != nil {
		h := http.Header{}
		for k, v := range safeHeaders {
			h.Set(k, v)
		}
		l.UseUpstreamAttempt(beginUpstream(l.capture, "", url, h, body))
		return
	}
	l.writeJSON("3_upstream_request.json", data)
}

// LogUpstreamHTTPError 记录上游 HTTP 错误（请求失败或返回非 200）
func (l *Logger) LogUpstreamHTTPError(url string, status int, body string, err error) {
	if !l.enabled {
		return
	}
	payload := map[string]interface{}{
		"url":        url,
		"status":     status,
		"body":       body,
		"elapsed_ms": time.Since(l.startTime).Milliseconds(),
	}
	if err != nil {
		payload["error"] = err.Error()
	}
	if l.capture != nil {
		l.mu.Lock()
		a := l.attempt
		l.mu.Unlock()
		if a != nil {
			a.writeJSON("error.json", payload)
			return
		}
		raw, _ := json.Marshal(payload)
		l.capture.Append("3_upstream_http_error.json", string(raw)+"\n")
		return
	}
	l.writeJSON("3_upstream_http_error.json", payload)
}

// LogUpstreamSSE 记录 4. 上游返回的原始 SSE（追加写入）
func (l *Logger) LogUpstreamSSE(eventType string, data string) {
	if !l.enabled || !l.sseEnabled {
		return
	}
	elapsed := time.Since(l.startTime).Milliseconds()
	l.mu.Lock()
	a := l.attempt
	l.mu.Unlock()
	if a != nil {
		a.Append(fmt.Sprintf("[%dms] %s: %s\n", elapsed, eventType, data))
		return
	}
	l.appendStream(&l.rawFile, &l.rawBytes, "4_upstream_sse.jsonl", fmt.Sprintf("[%dms] %s: %s\n", elapsed, eventType, data))
}

// LogOutputSSE 记录 5. 转换给客户端的 SSE（追加写入）
func (l *Logger) LogOutputSSE(event string, data string) {
	if !l.enabled || !l.sseEnabled {
		return
	}
	elapsed := time.Since(l.startTime).Milliseconds()
	l.appendStream(&l.outFile, &l.outBytes, "5_client_sse.jsonl", fmt.Sprintf("[%dms] event: %s\ndata: %s\n\n", elapsed, event, data))
}

func (l *Logger) appendStream(file **os.File, written *int64, name, data string) {
	if l.capture != nil {
		l.capture.Append(name, data)
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()

	if *file == nil {
		opened, err := os.OpenFile(filepath.Join(l.dir, name), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0600)
		if err != nil {
			return
		}
		*file = opened
	}
	l.writeLimitedStream(*file, written, data)
}

func (l *Logger) writeLimitedStream(file *os.File, written *int64, data string) {
	if file == nil || written == nil || *written >= maxDebugStreamLogBytes {
		return
	}
	remaining := maxDebugStreamLogBytes - *written
	if int64(len(data)) > remaining {
		data = data[:remaining]
	}
	n, _ := io.WriteString(file, data)
	*written += int64(n)
}

// LogInputTokenBreakdown 记录输入 token 分解
func (l *Logger) LogInputTokenBreakdown(profile string, basePromptTokens, systemContextTokens, historyTokens, toolsTokens, total int) {
	if !l.enabled {
		return
	}

	payload := map[string]interface{}{
		"prompt_profile":        profile,
		"base_prompt_tokens":    basePromptTokens,
		"system_context_tokens": systemContextTokens,
		"history_tokens":        historyTokens,
		"tools_tokens":          toolsTokens,
		"estimated_total":       total,
	}
	l.writeJSON("6_input_token_breakdown.json", payload)
}

// LogSummary 记录请求摘要
func (l *Logger) LogSummary(inputTokens, outputTokens int, duration time.Duration, stopReason string) {
	if !l.enabled {
		return
	}

	summary := map[string]interface{}{
		"input_tokens":  inputTokens,
		"output_tokens": outputTokens,
		"total_tokens":  inputTokens + outputTokens,
		"duration_ms":   duration.Milliseconds(),
		"stop_reason":   stopReason,
	}
	l.writeJSON("6_summary.json", summary)
}

// Close 关闭日志文件
func (l *Logger) Close() {
	if !l.enabled {
		return
	}

	l.mu.Lock()
	defer l.mu.Unlock()

	if l.rawFile != nil {
		l.rawFile.Close()
		l.rawFile = nil
	}
	if l.outFile != nil {
		l.outFile.Close()
		l.outFile = nil
	}
}

func (l *Logger) writeJSON(filename string, data interface{}) {
	jsonData, err := json.MarshalIndent(data, "", "  ")
	if err != nil {
		return
	}
	l.writeFile(filename, string(jsonData))
}

func (l *Logger) writeFile(filename string, content string) {
	if l.capture != nil {
		l.capture.Set(filename, content)
		return
	}
	os.WriteFile(filepath.Join(l.dir, filename), []byte(content), 0600)
}

func (l *Logger) Capturing() bool { return l != nil && l.capture != nil }
func (l *Logger) UseUpstreamAttempt(a *UpstreamAttempt) {
	if l == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.attempt = a
}

func (l *Logger) SSEEnabled() bool { return l != nil && l.enabled && l.sseEnabled }
