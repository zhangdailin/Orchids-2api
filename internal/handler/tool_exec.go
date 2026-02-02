package handler

import (
	"bufio"
	"bytes"
	"context"
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
	"time"

	"orchids-api/internal/config"
	"orchids-api/internal/orchids"

	"github.com/kballard/go-shellquote"
)

const (
	toolMaxOutputSize = 0
	toolMaxLines      = 0
	toolMaxFileSize   = 0
	toolMaxFiles      = 0
)

func executeToolCall(call toolCall, cfg *config.Config) safeToolResult {
	result := safeToolResult{
		call:  call,
		input: parseToolInputValue(call.input),
	}

	baseDir := resolveLocalWorkdir(cfg)
	if baseDir == "" {
		result.isError = true
		result.output = "base directory is empty"
		return result
	}

	inputMap := parseToolInputMap(call.input)
	toolName := strings.ToLower(strings.TrimSpace(call.name))
	toolName = strings.ToLower(orchids.NormalizeToolName(toolName))
	ignore := cfg.OrchidsFSIgnore

	switch toolName {
	case "read":
		path := toolInputString(inputMap, "file_path", "path", "file")
		if path == "" {
			result.isError = true
			result.output = "missing file_path for Read"
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
		data, err := readFileLimited(abs)
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
		pattern := toolInputString(inputMap, "pattern", "glob")
		if pattern == "" {
			pattern = "*"
		}
		root := toolInputString(inputMap, "path", "root")
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
		pattern := toolInputString(inputMap, "pattern", "query")
		if pattern == "" {
			result.isError = true
			result.output = "missing pattern for Grep"
			return result
		}
		root := toolInputString(inputMap, "path", "root")
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
		output, err := grepSearch(baseDir, absRoot, pattern, ignore)
		if err != nil {
			result.isError = true
			result.output = err.Error()
			return result
		}
		result.output = output
		return result

	case "bash":
		if !cfg.OrchidsAllowRunCommand {
			result.isError = true
			result.output = "run_command is disabled by server config"
			return result
		}
		command := toolInputString(inputMap, "command", "cmd")
		if strings.TrimSpace(command) == "" {
			result.isError = true
			result.output = "missing command for Bash"
			return result
		}
		output, err := runAllowedCommand(baseDir, command, cfg.OrchidsRunAllowlist)
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

func validateToolPathIgnore(baseDir, target string, ignore []string) error {
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
					if isToolIgnoredRelPath(rel, ignore) {
						return filepath.SkipDir
					}
				}
			}
			return nil
		}
		if (maxResults > 0 && count >= maxResults) || (toolMaxFiles > 0 && count >= toolMaxFiles) {
			return filepath.SkipDir
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
					if isToolIgnoredRelPath(rel, ignore) {
						return filepath.SkipDir
					}
				}
			}
			return nil
		}
		if (toolMaxLines > 0 && count >= toolMaxLines) || (toolMaxFiles > 0 && count >= toolMaxFiles) {
			return filepath.SkipDir
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
				if toolMaxLines > 0 && count >= toolMaxLines {
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
	if toolMaxOutputSize > 0 && len(output) > toolMaxOutputSize {
		output = output[:toolMaxOutputSize]
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
	if toolMaxOutputSize > 0 && len(out) > toolMaxOutputSize {
		out = out[:toolMaxOutputSize]
	}
	return strings.TrimSpace(out), nil
}

func runExecCommand(baseDir string, tokens []string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, tokens[0], tokens[1:]...)
	cmd.Dir = baseDir
	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	if err := cmd.Run(); err != nil {
		if ctx.Err() == context.DeadlineExceeded {
			return buf.String(), fmt.Errorf("command timed out: %w", err)
		}
		return buf.String(), err
	}
	return buf.String(), nil
}

func runShellCommand(baseDir, command string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "bash", "-lc", command)
	cmd.Dir = baseDir
	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	if err := cmd.Run(); err != nil {
		if ctx.Err() == context.DeadlineExceeded {
			return buf.String(), fmt.Errorf("command timed out: %w", err)
		}
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
