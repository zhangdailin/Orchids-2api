package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/kballard/go-shellquote"
)

const (
	safeToolTimeout       = 3 * time.Second
	safeToolMaxOutputSize = 32 * 1024
	safeToolMaxLines      = 200
	safeToolMaxFindDepth  = 5
)

type safeToolResult struct {
	call    toolCall
	input   interface{}
	output  string
	isError bool
}

func executeSafeTool(call toolCall) safeToolResult {
	result := safeToolResult{
		call:  call,
		input: parseToolInputValue(call.input),
	}

	command, err := extractToolCommand(call.input)
	if err != nil {
		result.isError = true
		result.output = err.Error()
		return result
	}

	output, err := runSafeCommand(command)
	if err != nil {
		result.isError = true
		result.output = err.Error()
		return result
	}

	result.output = output
	return result
}

func parseToolInputValue(inputJSON string) interface{} {
	if strings.TrimSpace(inputJSON) == "" {
		return map[string]interface{}{}
	}
	fixed := fixToolInput(inputJSON)
	var value interface{}
	if err := json.Unmarshal([]byte(fixed), &value); err != nil {
		return map[string]interface{}{}
	}
	return value
}

func extractToolCommand(inputJSON string) (string, error) {
	fixed := fixToolInput(inputJSON)
	var payload struct {
		Command string `json:"command"`
	}
	if err := json.Unmarshal([]byte(fixed), &payload); err != nil {
		return "", fmt.Errorf("invalid tool input: %w", err)
	}
	if strings.TrimSpace(payload.Command) == "" {
		return "", errors.New("tool input missing command")
	}
	return payload.Command, nil
}

func runSafeCommand(command string) (string, error) {
	command = strings.TrimSpace(command)
	if command == "" {
		return "", errors.New("empty command")
	}
	if strings.Contains(command, ";") || strings.Contains(command, "||") {
		return "", errors.New("unsupported command syntax")
	}

	baseDir, err := os.Getwd()
	if err != nil {
		return "", fmt.Errorf("failed to resolve working directory: %w", err)
	}

	segments := splitByAndAnd(command)
	var outputs []string
	var lastErr error

	for _, segment := range segments {
		segment = strings.TrimSpace(segment)
		if segment == "" {
			continue
		}
		out, err := runSafeSegment(baseDir, segment)
		if err != nil {
			lastErr = err
			continue
		}
		if out != "" {
			outputs = append(outputs, out)
		}
	}

	if len(outputs) == 0 && lastErr != nil {
		return "", lastErr
	}
	if lastErr != nil {
		outputs = append(outputs, "[warning] "+lastErr.Error())
	}
	return strings.Join(outputs, "\n"), nil
}

func splitByAndAnd(command string) []string {
	return strings.Split(command, "&&")
}

func runSafeSegment(baseDir, segment string) (string, error) {
	parts := strings.Split(segment, "|")
	if len(parts) > 2 {
		return "", errors.New("unsupported pipe usage")
	}
	left := strings.TrimSpace(parts[0])
	if left == "" {
		return "", errors.New("empty command segment")
	}

	output, err := runSafeSimple(baseDir, left)
	if err != nil {
		return "", err
	}

	if len(parts) == 2 {
		right := strings.TrimSpace(parts[1])
		if right == "" {
			return "", errors.New("invalid pipe segment")
		}
		output, err = applyHead(right, output)
		if err != nil {
			return "", err
		}
	}

	return output, nil
}

func runSafeSimple(baseDir, command string) (string, error) {
	tokens, err := shellquote.Split(command)
	if err != nil || len(tokens) == 0 {
		return "", errors.New("invalid command format")
	}

	switch tokens[0] {
	case "pwd":
		return baseDir, nil
	case "ls":
		return runSafeLS(baseDir, tokens[1:])
	case "find":
		return runSafeFind(baseDir, tokens[1:])
	default:
		return "", fmt.Errorf("command not allowed: %s", tokens[0])
	}
}

func applyHead(segment string, input string) (string, error) {
	tokens, err := shellquote.Split(segment)
	if err != nil || len(tokens) == 0 {
		return "", errors.New("invalid head segment")
	}
	if tokens[0] != "head" {
		return "", errors.New("only head pipe is supported")
	}
	lines := strings.Split(strings.ReplaceAll(input, "\r\n", "\n"), "\n")
	count := 10
	if len(tokens) > 1 {
		if strings.HasPrefix(tokens[1], "-") {
			switch tokens[1] {
			case "-n":
				if len(tokens) < 3 {
					return "", errors.New("missing head -n value")
				}
				value, err := strconv.Atoi(tokens[2])
				if err != nil || value < 1 {
					return "", errors.New("invalid head -n value")
				}
				count = value
			default:
				value, err := strconv.Atoi(strings.TrimPrefix(tokens[1], "-"))
				if err != nil || value < 1 {
					return "", errors.New("invalid head value")
				}
				count = value
			}
		} else {
			value, err := strconv.Atoi(tokens[1])
			if err != nil || value < 1 {
				return "", errors.New("invalid head value")
			}
			count = value
		}
	}
	if count > safeToolMaxLines {
		count = safeToolMaxLines
	}
	if len(lines) > count {
		lines = lines[:count]
	}
	return strings.TrimSpace(strings.Join(lines, "\n")), nil
}

func runSafeLS(baseDir string, args []string) (string, error) {
	var flags []string
	var pathArg string
	for _, arg := range args {
		if strings.HasPrefix(arg, "-") {
			switch arg {
			case "-a", "-l", "-la", "-al":
				flags = append(flags, arg)
			default:
				return "", fmt.Errorf("ls flag not allowed: %s", arg)
			}
			continue
		}
		if pathArg != "" {
			return "", errors.New("ls supports a single path argument")
		}
		pathArg = arg
	}

	targetDir := baseDir
	if pathArg != "" {
		clean, err := safeRelPath(baseDir, pathArg)
		if err != nil {
			return "", err
		}
		targetDir = clean
	}

	ctx, cancel := context.WithTimeout(context.Background(), safeToolTimeout)
	defer cancel()

	lsArgs := append([]string{}, flags...)
	lsArgs = append(lsArgs, targetDir)
	cmd := exec.CommandContext(ctx, "ls", lsArgs...)
	cmd.Dir = baseDir

	output, err := captureLimitedOutput(cmd)
	if err != nil {
		return "", err
	}
	return output, nil
}

func runSafeFind(baseDir string, args []string) (string, error) {
	startPath := "."
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		startPath = args[0]
		args = args[1:]
	}

	startAbs, err := safeRelPath(baseDir, startPath)
	if err != nil {
		return "", err
	}

	maxDepth := safeToolMaxFindDepth
	filterTypes := map[string]bool{}
	namePatterns := []string{}
	excludeHidden := false

	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "-maxdepth":
			if i+1 >= len(args) {
				return "", errors.New("missing value for -maxdepth")
			}
			val, err := strconv.Atoi(args[i+1])
			if err != nil || val < 0 {
				return "", errors.New("invalid -maxdepth")
			}
			maxDepth = val
			i++
		case "-type":
			if i+1 >= len(args) {
				return "", errors.New("missing value for -type")
			}
			val := args[i+1]
			if val != "f" && val != "d" {
				return "", errors.New("only -type f/d supported")
			}
			filterTypes[val] = true
			i++
		case "-name":
			if i+1 >= len(args) {
				return "", errors.New("missing value for -name")
			}
			pattern := strings.TrimSpace(args[i+1])
			if pattern == "" {
				return "", errors.New("invalid -name pattern")
			}
			if strings.Contains(pattern, string(filepath.Separator)) {
				return "", errors.New("invalid -name pattern")
			}
			namePatterns = append(namePatterns, pattern)
			i++
		case "-o":
			// allow OR between supported predicates
		case "-not":
			if i+2 >= len(args) {
				return "", errors.New("invalid -not syntax")
			}
			if args[i+1] != "-path" {
				return "", errors.New("only -not -path supported")
			}
			pattern := args[i+2]
			if pattern == "*/.*" || pattern == "*/\\.*" {
				excludeHidden = true
			} else {
				return "", errors.New("unsupported -not -path pattern")
			}
			i += 2
		default:
			return "", fmt.Errorf("unsupported find option: %s", args[i])
		}
	}

	lines := make([]string, 0, 128)
	err = filepath.WalkDir(startAbs, func(path string, d os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if excludeHidden && isHiddenPath(startAbs, path) {
			if d.IsDir() && path != startAbs {
				return filepath.SkipDir
			}
			return nil
		}

		rel, err := filepath.Rel(startAbs, path)
		if err != nil {
			return err
		}
		if rel == "." {
			rel = "."
		} else {
			rel = "./" + filepath.ToSlash(rel)
		}

		depth := 0
		if rel != "." {
			depth = strings.Count(rel, "/")
		}
		if depth > maxDepth {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}

		if len(filterTypes) > 0 {
			if d.IsDir() && !filterTypes["d"] {
				return nil
			}
			if !d.IsDir() && !filterTypes["f"] {
				return nil
			}
		}
		if len(namePatterns) > 0 {
			matched := false
			name := d.Name()
			for _, pattern := range namePatterns {
				if ok, _ := filepath.Match(pattern, name); ok {
					matched = true
					break
				}
			}
			if !matched {
				return nil
			}
		}

		lines = append(lines, rel)
		if len(lines) >= safeToolMaxLines {
			return errors.New("output limit reached")
		}
		return nil
	})

	if err != nil && err.Error() != "output limit reached" {
		return "", err
	}

	return strings.Join(lines, "\n"), nil
}

func isHiddenPath(root, path string) bool {
	rel, err := filepath.Rel(root, path)
	if err != nil || rel == "." {
		return false
	}
	parts := strings.Split(rel, string(filepath.Separator))
	for _, part := range parts {
		if strings.HasPrefix(part, ".") {
			return true
		}
	}
	return false
}

func safeRelPath(baseDir, path string) (string, error) {
	if filepath.IsAbs(path) {
		return "", errors.New("absolute paths are not allowed")
	}
	clean := filepath.Clean(path)
	if clean == "." {
		return baseDir, nil
	}
	if strings.HasPrefix(clean, "..") {
		return "", errors.New("path traversal is not allowed")
	}
	full := filepath.Join(baseDir, clean)
	return full, nil
}

func captureLimitedOutput(cmd *exec.Cmd) (string, error) {
	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = &buf

	if err := cmd.Run(); err != nil {
		return "", err
	}

	out := buf.String()
	if len(out) > safeToolMaxOutputSize {
		out = out[:safeToolMaxOutputSize]
	}
	out = strings.TrimSpace(out)
	return out, nil
}
