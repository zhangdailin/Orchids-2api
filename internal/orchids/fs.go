package orchids

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/gorilla/websocket"
	"github.com/kballard/go-shellquote"
)

const (
	fsMaxOutputSize = 0
	fsMaxLines      = 0
	fsMaxFileSize   = 0
	fsMaxFiles      = 0
	fsCmdTimeout    = 0 * time.Second
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
		c.wsWriteMu.Lock()
		defer c.wsWriteMu.Unlock()
		return conn.WriteJSON(payload)
	}

	operation := strings.TrimSpace(op.Operation)
	if operation == "" {
		return respond(false, nil, "missing operation")
	}

	if c.config == nil {
		return respond(false, nil, "server config unavailable")
	}
	var baseDir string
	if overrideWorkdir != "" {
		baseDir = overrideWorkdir
	} else {
		baseDir = c.resolveLocalWorkdir()
	}
	ignore := c.config.OrchidsFSIgnore

	switch operation {
	case "edit":
		if c.fsCache != nil {
			c.fsCache.Clear() // Invalidate cache on write
		}
		if op.Path == "" {
			return respond(false, nil, "path is required for edit")
		}
		path, err := resolvePath(baseDir, op.Path)
		if err != nil {
			return respond(false, nil, err.Error())
		}
		if err := validatePathIgnore(baseDir, path, ignore); err != nil {
			return respond(false, nil, err.Error())
		}
		if op.Content == nil {
			return respond(false, nil, "content is required for edit")
		}
		switch payload := op.Content.(type) {
		case string:
			if err := writeFile(path, payload); err != nil {
				return respond(false, nil, err.Error())
			}
			return respond(true, map[string]interface{}{"replacements": 1}, "")
		case []interface{}:
			original, err := readFileLimited(path)
			if err != nil {
				return respond(false, nil, err.Error())
			}
			updated, count, err := applyEdits(original, map[string]interface{}{"edits": payload})
			if err != nil {
				return respond(false, nil, err.Error())
			}
			if err := writeFile(path, updated); err != nil {
				return respond(false, nil, err.Error())
			}
			return respond(true, map[string]interface{}{"replacements": count}, "")
		case map[string]interface{}:
			if content := toolInputString(payload, "content", "text", "new_content", "newContent"); content != "" && payload["old_string"] == nil && payload["edits"] == nil {
				if err := writeFile(path, content); err != nil {
					return respond(false, nil, err.Error())
				}
				return respond(true, map[string]interface{}{"replacements": 1}, "")
			}
			original, err := readFileLimited(path)
			if err != nil {
				return respond(false, nil, err.Error())
			}
			updated, count, err := applyEdits(original, payload)
			if err != nil {
				return respond(false, nil, err.Error())
			}
			if err := writeFile(path, updated); err != nil {
				return respond(false, nil, err.Error())
			}
			return respond(true, map[string]interface{}{"replacements": count}, "")
		default:
			content := normalizeContent(payload)
			if content == "" {
				return respond(false, nil, "unsupported edit payload")
			}
			if err := writeFile(path, content); err != nil {
				return respond(false, nil, err.Error())
			}
			go c.RefreshFSIndex()
			return respond(true, map[string]interface{}{"replacements": 1}, "")
		}
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
		go c.RefreshFSIndex()
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
		if err := os.RemoveAll(path); err != nil {
			return respond(false, nil, err.Error())
		}
		go c.RefreshFSIndex()
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

		// Fast path: use memory index
		rel, _ := filepath.Rel(baseDir, path)
		rel = filepath.ToSlash(rel)
		c.fsIndexMu.RLock()
		if c.fsIndex != nil {
			if names, ok := c.fsIndex[rel]; ok {
				c.fsIndexMu.RUnlock()
				if names == nil {
					return respond(false, nil, "readdirent: not a directory")
				}
				return respond(true, names, "")
			}
		}
		c.fsIndexMu.RUnlock()

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

		// Fast path: use memory index for glob matching
		re, err := globToRegex(pattern)
		if err == nil {
			c.fsIndexMu.RLock()
			if len(c.fsFileList) > 0 {
				var results []string
				rootRel, _ := filepath.Rel(baseDir, root)
				rootRel = filepath.ToSlash(rootRel)
				for _, f := range c.fsFileList {
					// Filter by sub-directory
					if rootRel != "." {
						if !strings.HasPrefix(f, rootRel) {
							continue
						}
					}
					// Match pattern relative to root
					matchTarget := f
					if rootRel != "." {
						if sub, err := filepath.Rel(rootRel, f); err == nil {
							matchTarget = filepath.ToSlash(sub)
						}
					}
					if re.MatchString(matchTarget) {
						results = append(results, filepath.Join(baseDir, f))
					}
				}
				c.fsIndexMu.RUnlock()
				return respond(true, results, "")
			}
			c.fsIndexMu.RUnlock()
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
		if !c.config.OrchidsAllowRunCommand {
			return respond(false, nil, "run_command is disabled by server config")
		}
		if op.Command == "" {
			return respond(false, nil, "command is required for run_command")
		}
		if op.IsBackground {
			return respond(false, nil, "background commands are not supported")
		}
		output, err := runAllowedCommand(baseDir, op.Command, c.config.OrchidsRunAllowlist)
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
	if clean == "." {
		return baseDir, nil
	}
	if filepath.IsAbs(clean) {
		rel, err := filepath.Rel(baseDir, clean)
		if err != nil || strings.HasPrefix(rel, "..") {
			return "", errors.New("path outside base directory")
		}
		return clean, nil
	}
	if strings.HasPrefix(clean, "..") {
		return "", errors.New("path traversal is not allowed")
	}
	return filepath.Join(baseDir, clean), nil
}

func validatePathIgnore(baseDir, target string, ignore []string) error {
	if len(ignore) == 0 {
		return nil
	}
	rel, err := filepath.Rel(baseDir, target)
	if err != nil {
		return err
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

func (c *Client) RefreshFSIndexSync() {
	if c == nil {
		return
	}
	baseDir := c.resolveLocalWorkdir()
	ignore := c.config.OrchidsFSIgnore

	entries, err := os.ReadDir(baseDir)
	if err != nil {
		return
	}

	c.fsIndexMu.Lock()
	// Always reset the index when syncing to avoid stale entries from previous workdir
	c.fsIndex = make(map[string][]string)
	c.fsFileList = nil

	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if !isIgnoredRelPath(e.Name(), ignore) {
			names = append(names, e.Name())
		}
	}
	c.fsIndex["."] = names
	c.fsIndexMu.Unlock()
}

func (c *Client) RefreshFSIndex() {
	if c == nil || c.config.SessionID == "" {
		return
	}
	if c.fsIndexRefresh.Swap(true) {
		c.fsIndexPending.Store(true)
		return
	}
	defer c.fsIndexRefresh.Store(false)

	for {
		c.fsIndexPending.Store(false)

		baseDir := c.resolveLocalWorkdir()
		ignore := c.config.OrchidsFSIgnore

		newIndex := make(map[string][]string)
		newFileList := make([]string, 0)
		filepath.WalkDir(baseDir, func(path string, d os.DirEntry, err error) error {
			if err != nil {
				return nil
			}

			rel, err := filepath.Rel(baseDir, path)
			if err != nil {
				return nil
			}
			rel = filepath.ToSlash(rel)

			if rel != "." && isIgnoredRelPath(rel, ignore) {
				if d.IsDir() {
					return filepath.SkipDir
				}
				return nil
			}

			// Limit depth to 3 levels
			if rel != "." && strings.Count(rel, "/") >= 3 {
				if d.IsDir() {
					return filepath.SkipDir
				}
				return nil
			}

			if d.IsDir() {
				entries, err := os.ReadDir(path)
				if err == nil {
					names := make([]string, 0, len(entries))
					for _, e := range entries {
						eRel := filepath.ToSlash(filepath.Join(rel, e.Name()))
						if !isIgnoredRelPath(eRel, ignore) {
							names = append(names, e.Name())
						}
					}
					newIndex[rel] = names
				}
			} else {
				newFileList = append(newFileList, rel)
				// Mark as file in index so list operations can fast-fail
				newIndex[rel] = nil
			}
			return nil
		})

		c.fsIndexMu.Lock()
		c.fsIndex = newIndex
		c.fsFileList = newFileList
		c.fsIndexMu.Unlock()

		if !c.fsIndexPending.Load() {
			break
		}
	}
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

func (c *Client) resolveLocalWorkdir() string {
	if cwd, err := os.Getwd(); err == nil {
		return cwd
	}
	return "."
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
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(content), 0644)
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
		reader := bufio.NewReader(file)
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

func runAllowedCommand(baseDir, command string, allowlist []string) (string, error) {
	command = strings.TrimSpace(command)
	if command == "" {
		return "", errors.New("empty command")
	}
	tokens, err := shellquote.Split(command)
	if err != nil || len(tokens) == 0 {
		return "", errors.New("invalid command")
	}
	allowed := map[string]bool{}
	allowAll := false
	for _, name := range allowlist {
		name = strings.ToLower(strings.TrimSpace(name))
		if name == "" {
			continue
		}
		if name == "*" || name == "all" {
			allowAll = true
			continue
		}
		allowed[name] = true
	}
	cmdName := strings.ToLower(tokens[0])
	if !allowAll && !allowed[cmdName] {
		return "", fmt.Errorf("command not allowed: %s", tokens[0])
	}
	useShell := allowAll || containsShellMeta(command)
	var (
		out    string
		runErr error
	)
	if useShell {
		out, runErr = runShellCommand(baseDir, command)
	} else {
		out, runErr = runExecCommand(baseDir, tokens)
	}
	if runErr != nil {
		return out, runErr
	}
	if fsMaxOutputSize > 0 && len(out) > fsMaxOutputSize {
		out = out[:fsMaxOutputSize]
	}
	return strings.TrimSpace(out), nil
}

func runExecCommand(baseDir string, tokens []string) (string, error) {
	ctx := context.Background()
	if fsCmdTimeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, fsCmdTimeout)
		defer cancel()
	}
	cmd := exec.CommandContext(ctx, tokens[0], tokens[1:]...)
	cmd.Dir = baseDir
	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	if err := cmd.Run(); err != nil {
		return buf.String(), err
	}
	return buf.String(), nil
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

func containsShellMeta(command string) bool {
	if strings.Contains(command, "|") || strings.Contains(command, ";") || strings.Contains(command, "&&") || strings.Contains(command, "||") {
		return true
	}
	if strings.Contains(command, "<") || strings.Contains(command, ">") {
		return true
	}
	if strings.Contains(command, "$(") || strings.Contains(command, "`") {
		return true
	}
	return false
}

func applyEdits(content string, input map[string]interface{}) (string, int, error) {
	if editsRaw, ok := input["edits"]; ok {
		edits, ok := editsRaw.([]interface{})
		if !ok {
			return "", 0, errors.New("invalid edits payload")
		}
		total := 0
		updated := content
		for _, item := range edits {
			editMap, ok := item.(map[string]interface{})
			if !ok {
				return "", 0, errors.New("invalid edit entry")
			}
			oldStr := strings.TrimSpace(toolInputString(editMap, "old_string", "oldString"))
			newStr := toolInputString(editMap, "new_string", "newString")
			replaceAll := toolInputBool(editMap, "replace_all", "replaceAll")
			if oldStr == "" {
				return "", 0, errors.New("edit missing old_string")
			}
			var err error
			updated, err = replaceString(updated, oldStr, newStr, replaceAll, &total)
			if err != nil {
				return "", 0, err
			}
		}
		return updated, total, nil
	}
	oldStr := strings.TrimSpace(toolInputString(input, "old_string", "oldString"))
	if oldStr == "" {
		return "", 0, errors.New("missing old_string for Edit")
	}
	newStr := toolInputString(input, "new_string", "newString")
	replaceAll := toolInputBool(input, "replace_all", "replaceAll")
	total := 0
	updated, err := replaceString(content, oldStr, newStr, replaceAll, &total)
	if err != nil {
		return "", 0, err
	}
	return updated, total, nil
}

func replaceString(content, oldStr, newStr string, replaceAll bool, total *int) (string, error) {
	if replaceAll {
		count := strings.Count(content, oldStr)
		if count == 0 {
			return "", fmt.Errorf("old_string not found")
		}
		if total != nil {
			*total += count
		}
		return strings.ReplaceAll(content, oldStr, newStr), nil
	}
	index := strings.Index(content, oldStr)
	if index == -1 {
		return "", fmt.Errorf("old_string not found")
	}
	if total != nil {
		*total++
	}
	return content[:index] + newStr + content[index+len(oldStr):], nil
}

func toolInputString(input map[string]interface{}, keys ...string) string {
	for _, key := range keys {
		if value, ok := input[key]; ok {
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

func toolInputBool(input map[string]interface{}, keys ...string) bool {
	for _, key := range keys {
		if value, ok := input[key]; ok {
			switch v := value.(type) {
			case bool:
				return v
			case string:
				parsed, err := strconv.ParseBool(strings.TrimSpace(v))
				if err == nil {
					return parsed
				}
			case float64:
				return v != 0
			case int:
				return v != 0
			}
		}
	}
	return false
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
