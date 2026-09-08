// Derived from chenyme/grok2api, commit 44a390b890e7a3e0dd209b95b8c29a9f2b1be8dd.
// Copyright (c) 2026 Chenyme. MIT license: ../../licenses/grok2api-MIT.txt.
// Source: backend/internal/infra/provider/cli/responses_response.go (SSE codec).
package grok

import (
	"bufio"
	"fmt"
	"io"
	"strings"
)

const upstreamMaxEventBytes = 8 << 20

type compatibleSSEEvent struct {
	Event    string
	ID       string
	Retry    string
	Comments []string
	Other    []string
	data     []string
}

func (e compatibleSSEEvent) Data() []byte {
	return []byte(strings.Join(e.data, "\n"))
}

func (e compatibleSSEEvent) HasData() bool { return len(e.data) > 0 }

func (e compatibleSSEEvent) writeTo(writer io.Writer) error {
	for _, comment := range e.Comments {
		if _, err := fmt.Fprintln(writer, comment); err != nil {
			return err
		}
	}
	if e.Event != "" {
		if _, err := fmt.Fprintf(writer, "event: %s\n", e.Event); err != nil {
			return err
		}
	}
	if e.ID != "" {
		if _, err := fmt.Fprintf(writer, "id: %s\n", e.ID); err != nil {
			return err
		}
	}
	if e.Retry != "" {
		if _, err := fmt.Fprintf(writer, "retry: %s\n", e.Retry); err != nil {
			return err
		}
	}
	for _, field := range e.Other {
		if _, err := fmt.Fprintln(writer, field); err != nil {
			return err
		}
	}
	for _, line := range e.data {
		if _, err := fmt.Fprintf(writer, "data: %s\n", line); err != nil {
			return err
		}
	}
	_, err := fmt.Fprintln(writer)
	return err
}

func consumeCompatibleSSE(source io.Reader, handle func(compatibleSSEEvent) error) error {
	scanner := bufio.NewScanner(source)
	scanner.Buffer(make([]byte, 64<<10), upstreamMaxEventBytes)
	event := compatibleSSEEvent{}
	eventBytes := 0
	firstLine := true
	flush := func() error {
		if len(event.data) == 0 && len(event.Comments) == 0 && len(event.Other) == 0 && event.Event == "" && event.ID == "" && event.Retry == "" {
			return nil
		}
		current := event
		event = compatibleSSEEvent{}
		eventBytes = 0
		return handle(current)
	}
	for scanner.Scan() {
		line := strings.TrimSuffix(scanner.Text(), "\r")
		if firstLine {
			line = strings.TrimPrefix(line, "\uFEFF")
			firstLine = false
		}
		if line == "" {
			if err := flush(); err != nil {
				return err
			}
			continue
		}
		eventBytes += len(line)
		if eventBytes > upstreamMaxEventBytes {
			return fmt.Errorf("Grok Build Responses SSE 单事件超过 %d MiB", upstreamMaxEventBytes>>20)
		}
		field, value, found := strings.Cut(line, ":")
		if found && strings.HasPrefix(value, " ") {
			value = value[1:]
		}
		switch {
		case strings.HasPrefix(line, ":"):
			event.Comments = append(event.Comments, line)
		case !found:
			event.Other = append(event.Other, line)
		case field == "event":
			event.Event = value
		case field == "data":
			event.data = append(event.data, value)
		case field == "id":
			event.ID = value
		case field == "retry":
			event.Retry = value
		default:
			event.Other = append(event.Other, line)
		}
	}
	if err := scanner.Err(); err != nil {
		return err
	}
	return flush()
}
