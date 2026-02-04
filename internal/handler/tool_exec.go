package handler

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"orchids-api/internal/config"
	"orchids-api/internal/orchids"
)

const (
	toolMaxOutputSize = 0
	toolMaxLines      = 0
	toolMaxFileSize   = 0
	toolMaxFiles      = 0
)

func executeToolCall(call toolCall, cfg *config.Config) safeToolResult {
	return executeToolCallWithBaseDir(call, cfg, "")
}

func executeToolCallWithBaseDir(call toolCall, cfg *config.Config, baseDir string) safeToolResult {
	result := safeToolResult{
		call:  call,
		input: parseToolInputValue(call.input),
	}

	if strings.TrimSpace(baseDir) == "" {
		baseDir = resolveLocalWorkdir(cfg)
	}
	if baseDir == "" {
		result.isError = true
		result.output = "base directory is empty"
		return result
	}

	inputMap := parseToolInputMap(call.input)
	toolName := strings.ToLower(strings.TrimSpace(call.name))
	toolName = strings.ToLower(orchids.NormalizeToolName(toolName))
	var ignore []string

	switch toolName {
	case "read":
		path := toolInputString(inputMap, "file_path", "path", "file")
		if path == "" {
			path = toolInputString(inputMap, "files", "file_paths", "filePaths", "paths")
		}
		if path == "" {
			result.isError = true
			result.output = "missing file_path for Read"
			return result
		}
		limit := toolInputInt(inputMap, "limit", "line_limit", "lineLimit", "max_lines", "maxLines")
		offset := toolInputInt(inputMap, "offset", "start", "start_line", "startLine")
		if limit < 0 {
			limit = 0
		}
		if offset < 0 {
			offset = 0
		}
		abs, err := resolveToolPath(baseDir, path)
		if err != nil {
			if remapped, ok := remapToolPath(baseDir, path); ok {
				abs = remapped
			} else {
				result.isError = true
				result.output = err.Error()
				return result
			}
		}
		if err := validateToolPathIgnore(baseDir, abs, ignore); err != nil {
			result.isError = true
			result.output = err.Error()
			return result
		}
		data, err := readFileLines(abs, limit, offset)
		if err != nil {
			result.isError = true
			result.output = err.Error()
			return result
		}
		result.output = data
		return result

	case "write":
		path := toolInputString(inputMap, "file_path", "path", "file")
		if path == "" {
			result.isError = true
			result.output = "missing file_path for Write"
			return result
		}
		contentVal, ok := inputMap["content"]
		if !ok {
			result.isError = true
			result.output = "missing content for Write"
			return result
		}
		abs, err := resolveToolPath(baseDir, path)
		if err != nil {
			result.isError = true
			result.output = err.Error()
			return result
		}
		if err := validateToolPathIgnore(baseDir, abs, ignore); err != nil {
			result.isError = true
			result.output = err.Error()
			return result
		}
		content := normalizeContent(contentVal)
		if err := writeFile(abs, content); err != nil {
			result.isError = true
			result.output = err.Error()
			return result
		}
		result.output = fmt.Sprintf("Wrote %s", path)
		return result

	case "edit":
		path := toolInputString(inputMap, "file_path", "path", "file")
		if path == "" {
			result.isError = true
			result.output = "missing file_path for Edit"
			return result
		}
		abs, err := resolveToolPath(baseDir, path)
		if err != nil {
			result.isError = true
			result.output = err.Error()
			return result
		}
		if err := validateToolPathIgnore(baseDir, abs, ignore); err != nil {
			result.isError = true
			result.output = err.Error()
			return result
		}
		original, err := readFileLimited(abs)
		if err != nil {
			result.isError = true
			result.output = err.Error()
			return result
		}
		updated, count, err := applyEdits(original, inputMap)
		if err != nil {
			result.isError = true
			result.output = err.Error()
			return result
		}
		if err := writeFile(abs, updated); err != nil {
			result.isError = true
			result.output = err.Error()
			return result
		}
		result.output = fmt.Sprintf("Updated %s (%d replacement(s))", path, count)
		return result

	case "delete":
		path := toolInputString(inputMap, "file_path", "path", "file")
		if path == "" {
			result.isError = true
			result.output = "missing file_path for Delete"
			return result
		}
		abs, err := resolveToolPath(baseDir, path)
		if err != nil {
			result.isError = true
			result.output = err.Error()
			return result
		}
		if err := validateToolPathIgnore(baseDir, abs, ignore); err != nil {
			result.isError = true
			result.output = err.Error()
			return result
		}
		if err := os.RemoveAll(abs); err != nil {
			result.isError = true
			result.output = err.Error()
			return result
		}
		result.output = fmt.Sprintf("Deleted %s", path)
		return result

	case "ls", "list":
		path := toolInputString(inputMap, "path", "file_path", "dir")
		if path == "" {
			path = "."
		}
		abs, err := resolveToolPath(baseDir, path)
		if err != nil {
			result.isError = true
			result.output = err.Error()
			return result
		}
		if err := validateToolPathIgnore(baseDir, abs, ignore); err != nil {
			result.isError = true
			result.output = err.Error()
			return result
		}
		entries, err := listDir(baseDir, abs, ignore)
		if err != nil {
			result.isError = true
			result.output = err.Error()
			return result
		}
		result.output = strings.Join(entries, "\n")
		return result

	case "glob":
		pattern := toolInputString(inputMap, "pattern", "glob", "query", "search")
		if pattern == "" {
			pattern = "*"
		}
		root := toolInputString(inputMap, "path", "root", "dir", "file_path")
		if root == "" {
			root = toolInputString(inputMap, "paths", "files", "file_paths", "filePaths", "roots")
		}
		if root == "" {
			root = "."
		}
		absRoot, err := resolveToolPath(baseDir, root)
		if err != nil {
			result.isError = true
			result.output = err.Error()
			return result
		}
		if err := validateToolPathIgnore(baseDir, absRoot, ignore); err != nil {
			result.isError = true
			result.output = err.Error()
			return result
		}
		maxResults := toolInputInt(inputMap, "max_results", "maxResults")
		matches, err := globSearch(baseDir, absRoot, pattern, maxResults, ignore)
		if err != nil {
			// Fallback: Try to use 'find' command if glob fails and it's a pattern issue or similar
			// But for now, let's just make the error more helpful
			result.isError = true
			result.output = fmt.Sprintf("Glob failed: %v. Try using 'ls -R' or 'find' command instead.", err)
			return result
		}
		output := fmt.Sprintf("Found %d file(s) for pattern: %s", len(matches), pattern)
		if len(matches) > 0 {
			output = output + "\n" + strings.Join(matches, "\n")
		}
		result.output = strings.TrimSpace(output)
		return result

	case "grep", "ripgrep", "rg":
		pattern := toolInputString(inputMap, "pattern", "query", "regex", "search")
		if pattern == "" {
			result.isError = true
			result.output = "missing pattern for Grep"
			return result
		}
		root := toolInputString(inputMap, "path", "root", "dir", "file_path")
		if root == "" {
			root = toolInputString(inputMap, "paths", "files", "file_paths", "filePaths", "roots")
		}
		if root == "" {
			root = "."
		}
		absRoot, err := resolveToolPath(baseDir, root)
		if err != nil {
			result.isError = true
			result.output = err.Error()
			return result
		}
		if err := validateToolPathIgnore(baseDir, absRoot, ignore); err != nil {
			result.isError = true
			result.output = err.Error()
			return result
		}
		maxResults := toolInputInt(inputMap, "max_results", "maxResults", "max_lines", "maxLines", "limit")
		output, err := grepSearch(baseDir, absRoot, pattern, maxResults, ignore)
		if err != nil {
			result.isError = true
			result.output = err.Error()
			return result
		}
		result.output = output
		return result

	case "bash":
		command := toolInputString(inputMap, "command", "cmd")
		if strings.TrimSpace(command) == "" {
			result.isError = true
			result.output = "missing command for Bash"
			return result
		}
		output, err := runShellCommand(baseDir, command)
		if err != nil {
			result.isError = true
			if output != "" {
				result.output = output
			} else {
				result.output = err.Error()
			}
			return result
		}
		result.output = output
		return result

	case "todowrite":
		result.output = "Todos updated"
		return result

	default:
		result.isError = true
		result.output = fmt.Sprintf("unsupported tool: %s", call.name)
		return result
	}
}

func resolveLocalWorkdir(cfg *config.Config) string {
	if cwd, err := os.Getwd(); err == nil {
		return cwd
	}
	return "."
}

func parseToolInputMap(inputJSON string) map[string]interface{} {
	if strings.TrimSpace(inputJSON) == "" {
		return map[string]interface{}{}
	}
	fixed := fixToolInput(inputJSON)
	var value map[string]interface{}
	if err := json.Unmarshal([]byte(fixed), &value); err != nil {
		return map[string]interface{}{}
	}
	return value
}

func toolInputString(input map[string]interface{}, keys ...string) string {
	for _, key := range keys {
		if value, ok := input[key]; ok {
			switch v := value.(type) {
			case string:
				str := strings.TrimSpace(v)
				if str != "" {
					return str
				}
			case []string:
				for _, item := range v {
					str := strings.TrimSpace(item)
					if str != "" {
						return str
					}
				}
			case []interface{}:
				for _, item := range v {
					if s, ok := item.(string); ok {
						str := strings.TrimSpace(s)
						if str != "" {
							return str
						}
					}
				}
			}
		}
	}
	return ""
}

func toolInputInt(input map[string]interface{}, keys ...string) int {
	for _, key := range keys {
		if value, ok := input[key]; ok {
			if v, ok := asInt(value); ok {
				return v
			}
		}
	}
	return 0
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

func resolveToolPath(baseDir, input string) (string, error) {
	if baseDir == "" {
		return "", errors.New("base directory is empty")
	}
	clean := filepath.Clean(input)
	if filepath.IsAbs(clean) {
		return clean, nil
	}
	return filepath.Join(baseDir, clean), nil
}

func remapToolPath(baseDir, input string) (string, bool) {
	// With relaxed restrictions, we don't really need complex remapping for containment,
	// but we can keep it simple or just rely on resolveToolPath.
	// Preserving a simple version for compatibility with existing calls.
	if strings.TrimSpace(input) == "" {
		return "", false
	}
	// If it works as an absolute path, good.
	if _, err := os.Stat(input); err == nil {
		return input, true
	}
	return "", false
}

func validateToolPathIgnore(baseDir, target string, ignore []string) error {
	if len(ignore) == 0 {
		return nil
	}
	rel, err := filepath.Rel(baseDir, target)
	if err != nil {
		// If we can't determine relative path, assume it's NOT ignored
		return nil
	}
	rel = filepath.ToSlash(rel)
	if rel == "." {
		return nil
	}
	if isToolIgnoredRelPath(rel, ignore) {
		return errors.New("path is ignored by server config")
	}
	return nil
}

func isToolIgnoredRelPath(rel string, ignore []string) bool {
	rel = strings.TrimPrefix(rel, "./")
	rel = strings.Trim(rel, "/")
	if rel == "" || rel == "." {
		return false
	}
	for _, item := range ignore {
		name := strings.TrimSpace(item)
		if name == "" {
			continue
		}
		name = filepath.ToSlash(strings.Trim(name, "/"))
		if name == "" {
			continue
		}
		if rel == name || strings.HasPrefix(rel, name+"/") || strings.Contains(rel, "/"+name+"/") || strings.HasSuffix(rel, "/"+name) {
			return true
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
				if isToolIgnoredRelPath(rel, ignore) {
					continue
				}
			}
		}
		lines = append(lines, entry.Name())
		if toolMaxLines > 0 && len(lines) >= toolMaxLines {
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
	if toolMaxFileSize > 0 && info.Size() > toolMaxFileSize {
		return "", errors.New("file too large")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	return string(data), nil
}

func readFileLines(path string, limit, offset int) (string, error) {
	if limit <= 0 && offset <= 0 {
		return readFileLimited(path)
	}
	info, err := os.Stat(path)
	if err != nil {
		return "", err
	}
	if info.IsDir() {
		return "", errors.New("path is a directory")
	}
	if toolMaxFileSize > 0 && info.Size() > toolMaxFileSize {
		return "", errors.New("file too large")
	}

	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close()

	reader := bufio.NewReader(file)
	var builder strings.Builder
	skipped := 0
	readCount := 0

	for {
		line, err := reader.ReadString('\n')
		if err != nil && !errors.Is(err, io.EOF) {
			return "", err
		}
		if err == io.EOF && len(line) == 0 {
			break
		}
		if skipped < offset {
			skipped++
		} else {
			builder.WriteString(line)
			readCount++
			if limit > 0 && readCount >= limit {
				break
			}
		}
		if err == io.EOF {
			break
		}
	}

	output := builder.String()
	if toolMaxOutputSize > 0 && len(output) > toolMaxOutputSize {
		output = output[:toolMaxOutputSize]
	}
	return output, nil
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
	stopErr := errors.New("max results reached")
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
					if isToolIgnoredRelPath(rel, ignore) {
						return filepath.SkipDir
					}
				}
			}
			return nil
		}
		if (maxResults > 0 && count >= maxResults) || (toolMaxFiles > 0 && count >= toolMaxFiles) {
			return stopErr
		}
		if len(ignore) > 0 {
			if rel, err := filepath.Rel(baseDir, path); err == nil {
				rel = filepath.ToSlash(rel)
				if isToolIgnoredRelPath(rel, ignore) {
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
			if (maxResults > 0 && count >= maxResults) || (toolMaxFiles > 0 && count >= toolMaxFiles) {
				return stopErr
			}
		}
		return nil
	})
	if err != nil {
		if errors.Is(err, stopErr) {
			return results, nil
		}
		return results, err
	}
	return results, nil
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

func grepSearch(baseDir, root, pattern string, maxResults int, ignore []string) (string, error) {
	re, err := regexp.Compile(pattern)
	if err != nil {
		re = regexp.MustCompile(regexp.QuoteMeta(pattern))
	}

	stopErr := errors.New("max results reached")
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
					if isToolIgnoredRelPath(rel, ignore) {
						return filepath.SkipDir
					}
				}
			}
			return nil
		}
		if (maxResults > 0 && count >= maxResults) || (toolMaxLines > 0 && count >= toolMaxLines) || (toolMaxFiles > 0 && count >= toolMaxFiles) {
			return stopErr
		}
		if len(ignore) > 0 {
			if rel, err := filepath.Rel(baseDir, path); err == nil {
				rel = filepath.ToSlash(rel)
				if isToolIgnoredRelPath(rel, ignore) {
					return nil
				}
			}
		}
		info, err := d.Info()
		if err != nil || (toolMaxFileSize > 0 && info.Size() > toolMaxFileSize) {
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
				if (maxResults > 0 && count >= maxResults) || (toolMaxLines > 0 && count >= toolMaxLines) || (toolMaxFiles > 0 && count >= toolMaxFiles) {
					return stopErr
				}
			}
			if err == io.EOF {
				break
			}
		}
		return nil
	})
	if err != nil {
		if errors.Is(err, stopErr) {
			err = nil
		} else {
			return "", err
		}
	}
	if len(lines) == 0 {
		return "", nil
	}
	output := strings.Join(lines, "\n")
	if toolMaxOutputSize > 0 && len(output) > toolMaxOutputSize {
		output = output[:toolMaxOutputSize]
	}
	return output, nil
}

func runShellCommand(baseDir, command string) (string, error) {
	cmd := exec.Command("bash", "-lc", command)
	cmd.Dir = baseDir
	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	if err := cmd.Run(); err != nil {
		return buf.String(), err
	}
	return buf.String(), nil
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
			oldStr := strings.TrimSpace(toolInputString(editMap, "old_string"))
			newStr := toolInputString(editMap, "new_string")
			replaceAll := toolInputBool(editMap, "replace_all")
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
	oldStr := strings.TrimSpace(toolInputString(input, "old_string"))
	if oldStr == "" {
		return "", 0, errors.New("missing old_string for Edit")
	}
	newStr := toolInputString(input, "new_string")
	replaceAll := toolInputBool(input, "replace_all")
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
