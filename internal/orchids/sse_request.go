package orchids

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/goccy/go-json"

	"orchids-api/internal/debug"
	"orchids-api/internal/perf"
	"orchids-api/internal/upstream"
)

const orchidsSSEDataPrefix = "data: "

var orchidsSSEDataPrefixBytes = []byte(orchidsSSEDataPrefix)

func (c *Client) sendRequestSSE(ctx context.Context, req upstream.UpstreamRequest, onMessage func(upstream.SSEMessage), logger *debug.Logger) error {
	if c == nil {
		return errors.New("orchids client is nil")
	}
	cfg := c.config
	chatSessionID := orchidsChatSessionID(req)
	debugEnabled := cfg != nil && cfg.DebugEnabled
	timeout := 120 * time.Second
	if cfg != nil && cfg.RequestTimeout > 0 {
		timeout = time.Duration(cfg.RequestTimeout) * time.Second
	}

	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	token, err := c.GetToken()
	if err != nil {
		return fmt.Errorf("failed to get token: %w", err)
	}

	if projectID, err := c.createProject(ctx); err != nil {
		if !errors.Is(err, errOrchidsProjectBootstrapUnavailable) {
			existingProjectID := orchidsProjectID(cfg, req)
			if existingProjectID != "" {
				slog.Warn("Orchids SSE createProject preflight failed; using existing project", "trace_id", traceIDForLog(req), "attempt", attemptForLog(req), "project_id", existingProjectID, "error", err)
			} else {
				slog.Warn("Orchids SSE createProject preflight failed; continuing without project", "trace_id", traceIDForLog(req), "attempt", attemptForLog(req), "error", err)
			}
		}
	} else if projectID != "" {
		req.ProjectID = projectID
	}

	projectID := orchidsProjectID(cfg, req)
	orchidsReq := c.buildSSEAgentRequest(req)
	breakerKey := ""
	if cfg != nil {
		breakerKey = cfg.Email
	}

	buf := perf.AcquireByteBuffer()
	defer perf.ReleaseByteBuffer(buf)

	if err := json.NewEncoder(buf).Encode(orchidsReq); err != nil {
		return err
	}

	url := c.upstreamURL()
	slog.Info("Orchids SSE request start", "trace_id", traceIDForLog(req), "attempt", attemptForLog(req), "chat_session_id", chatSessionID, "project_id", projectID, "message_items", len(orchidsReq.Messages), "system_present", orchidsReq.System != "")

	breaker := upstream.GetAccountBreaker(breakerKey)
	start := time.Now()

	result, err := breaker.Execute(func() (interface{}, error) {
		httpReq, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(buf.Bytes()))
		if err != nil {
			return nil, err
		}

		httpReq.Header.Set("Accept", "text/event-stream")
		httpReq.Header.Set("Authorization", "Bearer "+token)
		httpReq.Header.Set("Content-Type", "application/json")
		httpReq.Header.Set("User-Agent", orchidsWSUserAgent)
		httpReq.Header.Set("X-Requested-With", "XMLHttpRequest")

		if logger != nil {
			headers := map[string]string{
				"Accept":           "text/event-stream",
				"Authorization":    "Bearer [REDACTED]",
				"Content-Type":     "application/json",
				"User-Agent":       orchidsWSUserAgent,
				"X-Requested-With": "XMLHttpRequest",
			}
			logger.LogUpstreamRequest(url, headers, orchidsReq)
		}

		return c.httpClient.Do(httpReq)
	})

	if err != nil {
		if logger != nil {
			logger.LogUpstreamHTTPError(url, 0, "", err)
		}
		slog.Warn("Orchids SSE request failed before response", "trace_id", traceIDForLog(req), "attempt", attemptForLog(req), "chat_session_id", chatSessionID, "error", err)
		if debugEnabled {
			slog.Info("[Performance] Upstream Request Failed", "duration", time.Since(start), "error", err)
		}
		return err
	}
	if debugEnabled {
		slog.Info("[Performance] Upstream Request Headers Received", "duration", time.Since(start))
	}

	resp := result.(*http.Response)
	defer resp.Body.Close()
	slog.Info("Orchids SSE response headers received", "trace_id", traceIDForLog(req), "attempt", attemptForLog(req), "chat_session_id", chatSessionID, "status_code", resp.StatusCode)

	if resp.StatusCode != http.StatusOK {
		body, err := io.ReadAll(resp.Body)
		if err != nil {
			return fmt.Errorf("upstream request failed with status %d (failed to read error body: %v)", resp.StatusCode, err)
		}
		slog.Warn("Orchids SSE non-200 response", "trace_id", traceIDForLog(req), "attempt", attemptForLog(req), "chat_session_id", chatSessionID, "status_code", resp.StatusCode)
		return fmt.Errorf("upstream request failed with status %d: %s", resp.StatusCode, string(body))
	}

	state, err := c.streamSSEBody(ctx, resp.Body, req, onMessage, logger)
	if err != nil {
		return err
	}

	slog.Info("Orchids SSE request completed", "trace_id", traceIDForLog(req), "attempt", attemptForLog(req), "chat_session_id", chatSessionID, "saw_tool_call", state.sawToolCall, "response_started", state.responseStarted)
	return nil
}

func (c *Client) streamSSEBody(
	ctx context.Context,
	body io.Reader,
	req upstream.UpstreamRequest,
	onMessage func(upstream.SSEMessage),
	logger *debug.Logger,
) (requestState, error) {
	reader := perf.AcquireBufioReader(body)
	defer perf.ReleaseBufioReader(reader)

	runtime := newOrchidsRequestRuntime(req, onMessage)
	state := &runtime.state
	var lineScratch []byte

	for {
		select {
		case <-ctx.Done():
			return *state, ctx.Err()
		default:
		}

		line, nextScratch, err := readLineBytes(reader, lineScratch)
		lineScratch = nextScratch[:0]
		if err != nil && err != io.EOF {
			return *state, err
		}
		if err == io.EOF && len(line) == 0 {
			break
		}

		rawBytes, ok := orchidsSSEDataPayloadBytes(line)
		if !ok {
			if err == io.EOF {
				break
			}
			continue
		}
		if handled, shouldBreak := runtime.handleRawMessage(rawBytes, logger, req.Tools); handled {
			if shouldBreak {
				goto done
			}
			if err == io.EOF {
				break
			}
			continue
		}

		var msg map[string]interface{}
		if err := json.Unmarshal(rawBytes, &msg); err != nil {
			if err == io.EOF {
				break
			}
			continue
		}

		if shouldBreak := runtime.handleDecodedMessage(msg, rawBytes, logger, req.Tools); shouldBreak {
			goto done
		}
		if err == io.EOF {
			break
		}
	}

done:
	if err := runtime.finalize(ctx); err != nil {
		if state.errorMsg != "" {
			slog.Warn("Orchids SSE stream ended with upstream error", "trace_id", traceIDForLog(req), "attempt", attemptForLog(req), "chat_session_id", orchidsChatSessionID(req), "error", state.errorMsg)
		}
		return *state, err
	}
	return *state, nil
}

func orchidsSSEDataPayloadBytes(line []byte) ([]byte, bool) {
	if !bytes.HasPrefix(line, orchidsSSEDataPrefixBytes) {
		return nil, false
	}
	raw := trimTrailingLineBreakBytes(line[len(orchidsSSEDataPrefixBytes):])
	if len(raw) == 0 {
		return nil, false
	}
	return raw, true
}
