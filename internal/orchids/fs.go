package orchids

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/gorilla/websocket"

	"orchids-api/internal/perf"
)

const (
	fsMaxOutputSize = 512 * 1024       // 512KB max output size
	fsMaxLines      = 10000            // max lines for directory listing
	fsMaxFileSize   = int64(10 << 20)  // 10MB max file size for read
	fsMaxFiles      = 5000             // max files for glob/grep results
	fsCmdTimeout    = 30 * time.Second // shell command timeout
)

type fsOperation struct {
	ID             string                 `json:"id"`
	Operation      string                 `json:"operation"`
	Path           string                 `json:"path"`
	Content        interface{}            `json:"content"`
	Command        string                 `json:"command"`
	Pattern        string                 `json:"pattern"`
	IsBackground   bool                   `json:"is_background"`
	BashID         string                 `json:"bash_id"`
	GlobParameters map[string]interface{} `json:"globParameters"`
	RipgrepParams  map[string]interface{} `json:"ripgrepParameters"`
}

func (c *Client) handleFSOperation(conn *websocket.Conn, msg map[string]interface{}, onResult func(success bool, data interface{}, errMsg string), overrideWorkdir string) error {
	operation, _ := msg["operation"].(string)
	path, _ := msg["path"].(string)
	slog.Debug("Orchids FS request", "op", operation, "path", path, "overrideWorkdir", overrideWorkdir)
	start := time.Now()
	raw, err := json.Marshal(msg)
	if err != nil {
		return err
	}

	var op fsOperation
	if err := json.Unmarshal(raw, &op); err != nil {
		return err
	}

	respond := func(success bool, data interface{}, errMsg string) error {
		if c.config.DebugEnabled {
			log.Printf("[Performance] FS Operation '%s' (path: %s) took %v", op.Operation, op.Path, time.Since(start))
		}
		if onResult != nil {
			onResult(success, data, errMsg)
		}
		payload := map[string]interface{}{
			"type":    "fs_operation_response",
			"id":      op.ID,
			"success": success,
			"data":    data,
		}
		if errMsg != "" {
			payload["error"] = errMsg
		}
		if conn == nil {
			return nil
		}
		c.wsWriteMu.Lock()
		defer c.wsWriteMu.Unlock()
		return conn.WriteJSON(payload)
	}

	if c.fsExecutor != nil {
		success, data, errMsg := c.fsExecutor(msg, overrideWorkdir)
		return respond(success, data, errMsg)
	}

	operation = strings.ToLower(strings.TrimSpace(operation))
	if operation == "" {
		return respond(false, nil, "missing operation")
	}

	if c.config == nil {
		return respond(false, nil, "server config unavailable")
	}
	if strings.TrimSpace(overrideWorkdir) == "" {
		return respond(false, nil, "workdir is required")
	}
	baseDir := overrideWorkdir
	// 从配置加载 ignore 列表，并自动排除 .git
	ignore := append([]string{}, c.config.OrchidsFSIgnore...)
	hasGit := false
	for _, item := range ignore {
		if strings.TrimSpace(item) == ".git" {
			hasGit = true
			break
		}
	}
	if !hasGit {
		ignore = append(ignore, ".git")
	}

	switch operation {
	case "edit":
		// 'edit' is often an internal Orchids operation used for coordination.
		// We should ACK it but NOT execute a local write here, as it might contain
		// only partial snippets that would overwrite the entire file.
		// Standard edits are handled via the model's tool calls in handler/tool_exec.go.
		return respond(true, map[string]interface{}{"replacements": 1}, "")
	case "read":
		if op.Path == "" {
			return respond(false, nil, "path is required for read")
		}
		path, err := resolvePath(baseDir, op.Path)
		if err != nil {
			return respond(false, nil, err.Error())
		}
		if err := validatePathIgnore(baseDir, path, ignore); err != nil {
			return respond(false, nil, err.Error())
		}

		if c.fsCache != nil {
			if val, errMsg, ok := c.fsCache.Get("read:" + path); ok {
				if errMsg != "" {
					return respond(false, nil, errMsg)
				}
				return respond(true, val, "")
			}
		}

		data, err := readFileLimited(path)
		if err != nil {
			if c.fsCache != nil {
				c.fsCache.SetError("read:"+path, err.Error())
			}
			return respond(false, nil, err.Error())
		}

		if c.fsCache != nil {
			c.fsCache.Set("read:"+path, data)
		}
		return respond(true, data, "")
	case "write":
		if c.fsCache != nil {
			c.fsCache.Clear() // Invalidate cache on write
		}
		if op.Path == "" {
			return respond(false, nil, "path is required for write")
		}
		path, err := resolvePath(baseDir, op.Path)
		if err != nil {
			return respond(false, nil, err.Error())
		}
		if err := validatePathIgnore(baseDir, path, ignore); err != nil {
			return respond(false, nil, err.Error())
		}
		content := normalizeContent(op.Content)
		if err := writeFile(path, content); err != nil {
			return respond(false, nil, err.Error())
		}
		return respond(true, nil, "")
	case "delete":
		if c.fsCache != nil {
			c.fsCache.Clear() // Invalidate cache on write
		}
		if op.Path == "" {
			return respond(false, nil, "path is required for delete")
		}
		path, err := resolvePath(baseDir, op.Path)
		if err != nil {
			return respond(false, nil, err.Error())
		}
		if err := validatePathIgnore(baseDir, path, ignore); err != nil {
			return respond(false, nil, err.Error())
		}
		// 安全保护：禁止删除 baseDir 本身
		absPath, _ := filepath.Abs(path)
		absBase, _ := filepath.Abs(baseDir)
		if absPath == absBase {
			return respond(false, nil, "refusing to delete project root directory")
		}
		if err := os.RemoveAll(path); err != nil {
			return respond(false, nil, err.Error())
		}
		return respond(true, nil, "")
	case "list":
		target := op.Path
		if target == "" {
			target = "."
		}
		path, err := resolvePath(baseDir, target)
		if err != nil {
			return respond(false, nil, err.Error())
		}
		if err := validatePathIgnore(baseDir, path, ignore); err != nil {
			return respond(false, nil, err.Error())
		}

		if c.fsCache != nil {
			if val, errMsg, ok := c.fsCache.Get("list:" + path); ok {
				if errMsg != "" {
					return respond(false, nil, errMsg)
				}
				return respond(true, val, "")
			}
		}

		entries, err := listDir(baseDir, path, ignore)
		if err != nil {
			if c.fsCache != nil {
				c.fsCache.SetError("list:"+path, err.Error())
			}
			return respond(false, nil, err.Error())
		}

		if c.fsCache != nil {
			c.fsCache.Set("list:"+path, entries)
		}
		return respond(true, entries, "")
	case "glob":
		params := op.GlobParameters
		pattern := op.Pattern
		if params != nil {
			if v, ok := params["pattern"].(string); ok && v != "" {
				pattern = v
			}
		}
		if pattern == "" {
			pattern = "*"
		}
		root := baseDir
		if params != nil {
			if v, ok := params["path"].(string); ok && v != "" {
				if resolved, err := resolvePath(baseDir, v); err == nil {
					root = resolved
				}
			}
		}
		if err := validatePathIgnore(baseDir, root, ignore); err != nil {
			return respond(false, nil, err.Error())
		}

		maxResults := 0
		if params != nil {
			if v, ok := asInt(params["maxResults"]); ok {
				maxResults = v
			}
		}
		matches, err := globSearch(baseDir, root, pattern, maxResults, ignore)
		if err != nil {
			return respond(false, nil, err.Error())
		}
		output := fmt.Sprintf("Found %d file(s) for pattern: %s\n%s", len(matches), pattern, strings.Join(matches, "\n"))
		return respond(true, strings.TrimSpace(output), "")
	case "ripgrep", "grep":
		params := op.RipgrepParams
		pattern := op.Pattern
		searchRoot := baseDir
		if params != nil {
			if v, ok := params["pattern"].(string); ok && v != "" {
				pattern = v
			}
			if v, ok := params["path"].(string); ok && v != "" {
				if resolved, err := resolvePath(baseDir, v); err == nil {
					searchRoot = resolved
				}
			}
		}
		if pattern == "" {
			return respond(false, nil, "pattern is required for grep")
		}
		if err := validatePathIgnore(baseDir, searchRoot, ignore); err != nil {
			return respond(false, nil, err.Error())
		}
		output, err := grepSearch(baseDir, searchRoot, pattern, ignore)
		if err != nil {
			return respond(false, nil, err.Error())
		}
		return respond(true, output, "")
	case "run_command":
		if c.fsCache != nil {
			c.fsCache.Clear() // Invalidate cache on command execution
		}
		if op.Command == "" {
			return respond(false, nil, "command is required for run_command")
		}
		output, err := runShellCommand(baseDir, op.Command)
		if err != nil {
			return respond(false, output, err.Error())
		}
		return respond(true, output, "")
	case "get_background_output":
		return respond(false, nil, "background commands are not supported")
	case "kill_background_process":
		return respond(false, nil, "background commands are not supported")
	case "get_terminal_logs", "get_browser_logs", "update_startup_commands":
		return respond(true, "", "")
	default:
		return respond(false, nil, fmt.Sprintf("unknown operation: %s", operation))
	}
}

func resolvePath(baseDir, input string) (string, error) {
	if baseDir == "" {
		return "", errors.New("base directory is empty")
	}
	clean := filepath.Clean(input)
	if filepath.IsAbs(clean) {
		return clean, nil
	}
	// Fix for common agent error: providing absolute path without leading slash
	if len(baseDir) > 1 {
		separator := string(filepath.Separator)
		if !strings.HasPrefix(clean, separator) {
			potentialAbs := separator + clean
			if strings.HasPrefix(potentialAbs, baseDir) {
				return potentialAbs, nil
			}
		}
	}
	joined := filepath.Join(baseDir, clean)
	// Security: ensure resolved path stays within baseDir
	absJoined, err := filepath.Abs(joined)
	if err != nil {
		return "", fmt.Errorf("failed to resolve path: %w", err)
	}
	absBase, err := filepath.Abs(baseDir)
	if err != nil {
		return "", fmt.Errorf("failed to resolve base: %w", err)
	}
	if absJoined != absBase && !strings.HasPrefix(absJoined, absBase+string(filepath.Separator)) {
		return "", fmt.Errorf("path escapes base directory: %s", input)
	}
	return absJoined, nil
}

func validatePathIgnore(baseDir, target string, ignore []string) error {
	if len(ignore) == 0 {
		return nil
	}
	rel, err := filepath.Rel(baseDir, target)
	if err != nil {
		// If we can't determine relative path (e.g. different drive), we assume it's not ignored
		// since ignore patterns are typically relative to project root.
		return nil
	}
	rel = filepath.ToSlash(rel)
	if rel == "." {
		return nil
	}
	if isIgnoredRelPath(rel, ignore) {
		return errors.New("path is ignored by server config")
	}
	return nil
}

func isIgnoredRelPath(rel string, ignore []string) bool {
	rel = strings.TrimPrefix(rel, "./")
	rel = strings.TrimSpace(rel)
	rel = strings.Trim(rel, "/")
	if rel == "" || rel == "." {
		return false
	}

	relParts := strings.Split(filepath.ToSlash(rel), "/")

	for _, item := range ignore {
		name := strings.TrimSpace(item)
		if name == "" {
			continue
		}
		name = filepath.ToSlash(strings.Trim(name, "/"))
		nameParts := strings.Split(name, "/")

		if len(relParts) >= len(nameParts) {
			match := true
			for i := range nameParts {
				if relParts[i] != nameParts[i] {
					match = false
					break
				}
			}
			if match {
				return true
			}
		}
	}
	return false
}

func listDir(baseDir, path string, ignore []string) ([]string, error) {
	entries, err := os.ReadDir(path)
	if err != nil {
		return nil, err
	}
	lines := make([]string, 0, len(entries))
	for _, entry := range entries {
		if len(ignore) > 0 {
			fullPath := filepath.Join(path, entry.Name())
			if rel, err := filepath.Rel(baseDir, fullPath); err == nil {
				rel = filepath.ToSlash(rel)
				if isIgnoredRelPath(rel, ignore) {
					continue
				}
			}
		}
		lines = append(lines, entry.Name())
		if fsMaxLines > 0 && len(lines) >= fsMaxLines {
			break
		}
	}
	return lines, nil
}

func readFileLimited(path string) (string, error) {
	info, err := os.Stat(path)
	if err != nil {
		return "", err
	}
	if info.IsDir() {
		return "", errors.New("path is a directory")
	}
	if fsMaxFileSize > 0 && info.Size() > fsMaxFileSize {
		return "", errors.New("file too large")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	return string(data), nil
}

func writeFile(path string, content string) error {
	snippet := content
	if len(snippet) > 200 {
		snippet = snippet[:200]
	}
	slog.Info("Orchids FS: writeFile", "path", path, "content_len", len(content), "snippet", snippet)

	// Safeguard: Don't overwrite an existing non-empty file with empty content
	if content == "" {
		if info, err := os.Stat(path); err == nil && info.Size() > 0 {
			slog.Warn("Prevented accidental file wipe (empty content)", "path", path)
			return fmt.Errorf("refused to overwrite non-empty file %s with empty content", path)
		}
	}

	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return err
	}

	// Atomic write: create temp file, write, sync, close, rename
	// Use .tmp prefix and hidden file to avoid showing up in default listings
	tmpFile, err := os.CreateTemp(dir, ".orchids_tmp_*")
	if err != nil {
		return err
	}
	tmpPath := tmpFile.Name()

	// Ensure cleanup
	defer func() {
		_ = tmpFile.Close()
		_ = os.Remove(tmpPath) // Will fail if rename succeeded, which is fine
	}()

	if _, err := tmpFile.Write([]byte(content)); err != nil {
		return err
	}
	if err := tmpFile.Sync(); err != nil { // Unlikely to fail, but good practice for durability
		return err
	}
	if err := tmpFile.Close(); err != nil {
		return err
	}

	return os.Rename(tmpPath, path)
}

func normalizeContent(content interface{}) string {
	switch v := content.(type) {
	case string:
		return v
	case []byte:
		return string(v)
	default:
		raw, _ := json.Marshal(v)
		return string(raw)
	}
}

func globSearch(baseDir, root, pattern string, maxResults int, ignore []string) ([]string, error) {
	re, err := globToRegex(pattern)
	if err != nil {
		return nil, err
	}
	var results []string
	count := 0
	err = filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			if len(ignore) > 0 {
				if rel, err := filepath.Rel(baseDir, path); err == nil {
					rel = filepath.ToSlash(rel)
					if isIgnoredRelPath(rel, ignore) {
						return filepath.SkipDir
					}
				}
			}
			return nil
		}
		if (maxResults > 0 && count >= maxResults) || (fsMaxFiles > 0 && count >= fsMaxFiles) {
			return filepath.SkipDir
		}
		if len(ignore) > 0 {
			if rel, err := filepath.Rel(baseDir, path); err == nil {
				rel = filepath.ToSlash(rel)
				if isIgnoredRelPath(rel, ignore) {
					return nil
				}
			}
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return nil
		}
		rel = filepath.ToSlash(rel)
		if re.MatchString(rel) {
			results = append(results, path)
			count++
		}
		return nil
	})
	return results, err
}

func globToRegex(pattern string) (*regexp.Regexp, error) {
	pattern = strings.ReplaceAll(pattern, "\\", "/")
	var re strings.Builder
	re.WriteString("^")
	for i := 0; i < len(pattern); i++ {
		ch := pattern[i]
		switch ch {
		case '*':
			if i+1 < len(pattern) && pattern[i+1] == '*' {
				if i+2 < len(pattern) && pattern[i+2] == '/' {
					re.WriteString("(?:.*/)?")
					i += 2
				} else {
					re.WriteString(".*")
					i++
				}
			} else {
				re.WriteString("[^/]*")
			}
		case '?':
			re.WriteString(".")
		default:
			re.WriteString(regexp.QuoteMeta(string(ch)))
		}
	}
	re.WriteString("$")
	return regexp.Compile(re.String())
}

func grepSearch(baseDir, root, pattern string, ignore []string) (string, error) {
	re, err := regexp.Compile(pattern)
	if err != nil {
		re = regexp.MustCompile(regexp.QuoteMeta(pattern))
	}

	var lines []string
	count := 0
	err = filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			if len(ignore) > 0 {
				if rel, err := filepath.Rel(baseDir, path); err == nil {
					rel = filepath.ToSlash(rel)
					if isIgnoredRelPath(rel, ignore) {
						return filepath.SkipDir
					}
				}
			}
			return nil
		}
		if (fsMaxLines > 0 && count >= fsMaxLines) || (fsMaxFiles > 0 && count >= fsMaxFiles) {
			return filepath.SkipDir
		}
		if len(ignore) > 0 {
			if rel, err := filepath.Rel(baseDir, path); err == nil {
				rel = filepath.ToSlash(rel)
				if isIgnoredRelPath(rel, ignore) {
					return nil
				}
			}
		}
		info, err := d.Info()
		if err != nil || (fsMaxFileSize > 0 && info.Size() > fsMaxFileSize) {
			return nil
		}
		file, err := os.Open(path)
		if err != nil {
			return nil
		}
		defer file.Close()
		reader := perf.AcquireBufioReader(file)
		defer perf.ReleaseBufioReader(reader)
		lineNum := 0
		for {
			line, err := reader.ReadString('\n')
			if err != nil && !errors.Is(err, io.EOF) {
				break
			}
			if err == io.EOF && len(line) == 0 {
				break
			}
			lineNum++
			text := strings.TrimSuffix(line, "\n")
			text = strings.TrimSuffix(text, "\r")
			if re.MatchString(text) {
				lines = append(lines, fmt.Sprintf("%s:%d:%s", path, lineNum, text))
				count++
				if fsMaxLines > 0 && count >= fsMaxLines {
					break
				}
			}
			if err == io.EOF {
				break
			}
		}
		return nil
	})
	if err != nil {
		return "", err
	}
	if len(lines) == 0 {
		return "", nil
	}
	output := strings.Join(lines, "\n")
	if fsMaxOutputSize > 0 && len(output) > fsMaxOutputSize {
		output = output[:fsMaxOutputSize]
	}
	return output, nil
}

func runShellCommand(baseDir, command string) (string, error) {
	ctx := context.Background()
	if fsCmdTimeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, fsCmdTimeout)
		defer cancel()
	}
	cmd := exec.CommandContext(ctx, "bash", "-lc", command)
	cmd.Dir = baseDir
	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	if err := cmd.Run(); err != nil {
		return buf.String(), err
	}
	return buf.String(), nil
}

func asInt(value interface{}) (int, bool) {
	switch v := value.(type) {
	case float64:
		return int(v), true
	case int:
		return v, true
	case json.Number:
		parsed, err := v.Int64()
		return int(parsed), err == nil
	case string:
		parsed, err := strconv.Atoi(v)
		return parsed, err == nil
	default:
		return 0, false
	}
}
