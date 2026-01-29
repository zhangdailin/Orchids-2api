package debug

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

// Logger 调试日志记录器
type Logger struct {
	enabled    bool
	sseEnabled bool
	dir        string
	rawFile    *os.File
	outFile    *os.File
	mu         sync.Mutex
	startTime  time.Time
}

// New 创建新的调试日志记录器
func New(enabled bool, sseEnabled bool) *Logger {
	if !enabled {
		return &Logger{enabled: false}
	}

	timestamp := time.Now().Format("2006-01-02_15-04-05")
	dir := filepath.Join("debug-logs", timestamp)
	os.MkdirAll(dir, 0755)

	return &Logger{
		enabled:    true,
		sseEnabled: sseEnabled,
		dir:        dir,
		startTime:  time.Now(),
	}
}

// CleanupAllLogs 清理所有调试日志（启动时调用）
func CleanupAllLogs() {
	os.RemoveAll("debug-logs")
	os.MkdirAll("debug-logs", 0755)
}

// Dir 返回日志目录
func (l *Logger) Dir() string {
	if !l.enabled {
		return ""
	}
	return l.dir
}

// LogIncomingRequest 记录 1. 进入的 Claude API 请求
func (l *Logger) LogIncomingRequest(req interface{}) {
	if !l.enabled {
		return
	}
	l.writeJSON("1_claude_request.json", req)
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

	data := map[string]interface{}{
		"url":     url,
		"headers": headers,
		"body":    body,
	}
	l.writeJSON("3_upstream_request.json", data)
}

// LogUpstreamSSE 记录 4. 上游返回的原始 SSE（追加写入）
func (l *Logger) LogUpstreamSSE(eventType string, data string) {
	if !l.enabled || !l.sseEnabled {
		return
	}

	l.mu.Lock()
	defer l.mu.Unlock()

	if l.rawFile == nil {
		f, err := os.OpenFile(filepath.Join(l.dir, "4_upstream_sse.jsonl"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
		if err != nil {
			return
		}
		l.rawFile = f
	}

	elapsed := time.Since(l.startTime).Milliseconds()
	fmt.Fprintf(l.rawFile, "[%dms] %s: %s\n", elapsed, eventType, data)
}

// LogOutputSSE 记录 5. 转换给客户端的 SSE（追加写入）
func (l *Logger) LogOutputSSE(event string, data string) {
	if !l.enabled || !l.sseEnabled {
		return
	}

	l.mu.Lock()
	defer l.mu.Unlock()

	if l.outFile == nil {
		f, err := os.OpenFile(filepath.Join(l.dir, "5_client_sse.jsonl"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
		if err != nil {
			return
		}
		l.outFile = f
	}

	elapsed := time.Since(l.startTime).Milliseconds()
	fmt.Fprintf(l.outFile, "[%dms] event: %s\ndata: %s\n\n", elapsed, event, data)
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
	if !l.enabled {
		return
	}
	jsonData, err := json.MarshalIndent(data, "", "  ")
	if err != nil {
		return
	}
	os.WriteFile(filepath.Join(l.dir, filename), jsonData, 0644)
}

func (l *Logger) writeFile(filename string, content string) {
	if !l.enabled {
		return
	}
	os.WriteFile(filepath.Join(l.dir, filename), []byte(content), 0644)
}

func cleanupOldDirs(basePath string, maxKeep int) {
	entries, err := os.ReadDir(basePath)
	if err != nil {
		return
	}

	var dirs []os.DirEntry
	for _, e := range entries {
		if e.IsDir() {
			dirs = append(dirs, e)
		}
	}

	if len(dirs) <= maxKeep {
		return
	}

	// 按名称排序（时间戳格式，越新越大）
	sort.Slice(dirs, func(i, j int) bool {
		return dirs[i].Name() > dirs[j].Name()
	})

	// 删除旧的
	for i := maxKeep; i < len(dirs); i++ {
		os.RemoveAll(filepath.Join(basePath, dirs[i].Name()))
	}
}
