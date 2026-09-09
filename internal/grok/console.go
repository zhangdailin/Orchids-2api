package grok

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/goccy/go-json"

	"orchids-api/internal/debug"
)

func (h *Handler) consoleURL(path string) string {
	base := "https://console.x.ai/v1"
	if h != nil && h.cfg != nil {
		base = h.cfg.GrokConsoleBaseURLOrDefault()
	}
	return strings.TrimRight(base, "/") + "/" + strings.TrimLeft(path, "/")
}

func chatMessageContentText(content interface{}) string {
	switch v := content.(type) {
	case nil:
		return ""
	case string:
		return v
	case []interface{}:
		var b strings.Builder
		for _, part := range v {
			m, ok := part.(map[string]interface{})
			if !ok {
				continue
			}
			t := strings.ToLower(strings.TrimSpace(fmt.Sprint(m["type"])))
			switch t {
			case "text", "input_text":
				if s := strings.TrimSpace(fmt.Sprint(m["text"])); s != "" {
					if b.Len() > 0 {
						b.WriteByte('\n')
					}
					b.WriteString(s)
				}
			}
		}
		return b.String()
	default:
		return strings.TrimSpace(fmt.Sprint(v))
	}
}

func (c *Client) consoleHeaders(token string) http.Header {
	h := c.headers(token)
	h.Set("Origin", "https://console.x.ai")
	h.Set("Referer", "https://console.x.ai/")
	h.Set("Accept", "*/*")
	return h
}

func (h *Handler) consolePayload(spec ModelSpec, req *ChatCompletionsRequest) (map[string]interface{}, error) {
	return h.responsesPayloadFromChat(spec, req, false)
}

func consoleInputHasEncryptedReasoning(input []interface{}) bool {
	for _, raw := range input {
		item, _ := raw.(map[string]interface{})
		if item == nil || !strings.EqualFold(strings.TrimSpace(fmt.Sprint(item["type"])), "reasoning") {
			continue
		}
		if value := strings.TrimSpace(fmt.Sprint(item["encrypted_content"])); value != "" && value != "<nil>" {
			return true
		}
	}
	return false
}

func insertConsoleReplayBeforeLastUser(input []interface{}, replay map[string]interface{}) []interface{} {
	insertAt := len(input)
	for index := len(input) - 1; index >= 0; index-- {
		if item, ok := input[index].(map[string]interface{}); ok && strings.EqualFold(strings.TrimSpace(fmt.Sprint(item["role"])), "user") {
			insertAt = index
			break
		}
	}
	out := make([]interface{}, 0, len(input)+1)
	out = append(out, input[:insertAt]...)
	out = append(out, replay)
	out = append(out, input[insertAt:]...)
	return out
}

func consoleToolsFromOpenAI(tools []ToolDef) []map[string]interface{} {
	if len(tools) == 0 {
		return nil
	}
	out := make([]map[string]interface{}, 0, len(tools))
	for _, tool := range tools {
		if !strings.EqualFold(strings.TrimSpace(tool.Type), "function") {
			continue
		}
		name := strings.TrimSpace(fmt.Sprint(tool.Function["name"]))
		if name == "" {
			continue
		}
		item := map[string]interface{}{
			"type":        "function",
			"name":        name,
			"description": strings.TrimSpace(fmt.Sprint(tool.Function["description"])),
			"parameters":  map[string]interface{}{},
		}
		if params, ok := tool.Function["parameters"]; ok && params != nil {
			item["parameters"] = params
		}
		if strict, ok := tool.Function["strict"].(bool); ok {
			item["strict"] = strict
		}
		out = append(out, item)
	}
	return out
}

func consoleToolChoiceFromOpenAI(choice interface{}) interface{} {
	switch v := choice.(type) {
	case nil:
		return nil
	case string:
		c := strings.ToLower(strings.TrimSpace(v))
		if c == "" {
			return nil
		}
		return c
	case map[string]interface{}:
		if !strings.EqualFold(strings.TrimSpace(fmt.Sprint(v["type"])), "function") {
			return v
		}
		fn, _ := v["function"].(map[string]interface{})
		name := strings.TrimSpace(fmt.Sprint(fn["name"]))
		if name == "" {
			return v
		}
		return map[string]interface{}{
			"type": "function",
			"name": name,
		}
	default:
		return v
	}
}

func (h *Handler) doConsole(ctx context.Context, token string, payload map[string]interface{}) (*http.Response, error) {
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	return h.client.doConsoleDPoPRequest(ctx, token, http.MethodPost, h.consoleURL("responses"), body)
}

func requiresConsoleResponses(spec ModelSpec) bool {
	return strings.TrimSpace(spec.ConsoleModel) != ""
}

func consoleExtractText(v interface{}) string {
	switch x := v.(type) {
	case nil:
		return ""
	case string:
		return x
	case map[string]interface{}:
		if t := strings.TrimSpace(fmt.Sprint(x["type"])); t == "output_text" || t == "text" || t == "message" {
			if raw := x["text"]; raw != nil {
				if s := strings.TrimSpace(fmt.Sprint(raw)); s != "" && s != "<nil>" {
					return s
				}
			}
			if raw := x["content"]; raw != nil {
				if s := strings.TrimSpace(consoleExtractText(raw)); s != "" {
					return s
				}
			}
			if raw := x["summary"]; raw != nil {
				if s := strings.TrimSpace(consoleExtractText(raw)); s != "" {
					return s
				}
			}
		}
		if t := strings.TrimSpace(fmt.Sprint(x["type"])); t == "message" || t == "response.output_message" {
			if raw := x["content"]; raw != nil {
				if s := strings.TrimSpace(consoleExtractText(raw)); s != "" {
					return s
				}
			}
		}
		for _, key := range []string{"output_text", "content", "output", "text", "message"} {
			if raw := x[key]; raw != nil {
				if s := consoleExtractText(raw); strings.TrimSpace(s) != "" {
					return s
				}
			}
		}
	case []interface{}:
		var b strings.Builder
		for _, item := range x {
			if s := strings.TrimSpace(consoleExtractText(item)); s != "" {
				if b.Len() > 0 {
					b.WriteByte('\n')
				}
				b.WriteString(s)
			}
		}
		return b.String()
	}
	return ""
}

func consoleExtractMessageText(v interface{}) string {
	switch x := v.(type) {
	case map[string]interface{}:
		if output, ok := x["output"].([]interface{}); ok {
			var text strings.Builder
			for _, item := range output {
				m, ok := item.(map[string]interface{})
				if !ok {
					continue
				}
				t := strings.ToLower(strings.TrimSpace(fmt.Sprint(m["type"])))
				if t != "message" && t != "response.output_message" {
					continue
				}
				for _, raw := range interfaceSlice(m["content"]) {
					part, _ := raw.(map[string]interface{})
					if kind := interfaceString(part["type"]); kind == "output_text" || kind == "text" {
						text.WriteString(streamString(part["text"]))
					}
				}
			}
			return text.String()
		}
	}
	return strings.TrimSpace(consoleExtractText(v))
}

func consoleFlatAnnotations(v interface{}) []map[string]interface{} {
	seen := map[string]struct{}{}
	out := make([]map[string]interface{}, 0)
	add := func(url, title string, start, end int) {
		url = strings.TrimSpace(url)
		if url == "" {
			return
		}
		key := fmt.Sprintf("%s\x00%s\x00%d:%d", url, title, start, end)
		if _, ok := seen[key]; ok {
			return
		}
		seen[key] = struct{}{}
		out = append(out, map[string]interface{}{
			"url":         url,
			"title":       strings.TrimSpace(title),
			"start_index": start,
			"end_index":   end,
		})
	}
	var walk func(interface{})
	walk = func(raw interface{}) {
		switch x := raw.(type) {
		case map[string]interface{}:
			t := strings.ToLower(strings.TrimSpace(fmt.Sprint(x["type"])))
			if t == "url_citation" || (x["url"] != nil && (x["title"] != nil || x["start_index"] != nil || x["end_index"] != nil)) {
				add(streamString(x["url"]), streamString(x["title"]), interfaceToInt(x["start_index"]), interfaceToInt(x["end_index"]))
			}
			if t == "web_search_call" {
				if action, _ := x["action"].(map[string]interface{}); action != nil {
					for _, src := range interfaceSlice(action["sources"]) {
						if m, _ := src.(map[string]interface{}); m != nil {
							add(streamString(m["url"]), streamString(m["title"]), 0, 0)
						}
					}
					if strings.EqualFold(strings.TrimSpace(fmt.Sprint(action["type"])), "open_page") {
						add(streamString(action["url"]), "", 0, 0)
					}
				}
			}
			for _, key := range []string{"annotation", "annotations", "content", "output", "item", "response", "url_citation"} {
				if child, ok := x[key]; ok {
					walk(child)
				}
			}
		case []interface{}:
			for _, item := range x {
				walk(item)
			}
		}
	}
	walk(v)
	return out
}

func consoleChatAnnotations(flat []map[string]interface{}) []interface{} {
	if len(flat) == 0 {
		return []interface{}{}
	}
	out := make([]interface{}, 0, len(flat))
	for _, ann := range flat {
		out = append(out, map[string]interface{}{
			"type": "url_citation",
			"url_citation": map[string]interface{}{
				"url":         ann["url"],
				"title":       ann["title"],
				"start_index": ann["start_index"],
				"end_index":   ann["end_index"],
			},
		})
	}
	return out
}

func appendUniqueConsoleAnnotations(dst []map[string]interface{}, src []map[string]interface{}) []map[string]interface{} {
	if len(src) == 0 {
		return dst
	}
	seen := make(map[string]struct{}, len(dst)+len(src))
	for _, ann := range dst {
		seen[fmt.Sprintf("%v\x00%v\x00%v:%v", ann["url"], ann["title"], ann["start_index"], ann["end_index"])] = struct{}{}
	}
	for _, ann := range src {
		key := fmt.Sprintf("%v\x00%v\x00%v:%v", ann["url"], ann["title"], ann["start_index"], ann["end_index"])
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		dst = append(dst, ann)
	}
	return dst
}

func consoleUsage(v map[string]interface{}) map[string]interface{} {
	raw, ok := v["usage"].(map[string]interface{})
	if !ok {
		return nil
	}
	prompt := interfaceToInt(raw["input_tokens"])
	completion := interfaceToInt(raw["output_tokens"])
	if prompt == 0 {
		prompt = interfaceToInt(raw["prompt_tokens"])
	}
	if completion == 0 {
		completion = interfaceToInt(raw["completion_tokens"])
	}
	total := interfaceToInt(raw["total_tokens"])
	if total == 0 {
		total = prompt + completion
	}
	reasoning := 0
	if details, _ := raw["output_tokens_details"].(map[string]interface{}); details != nil {
		reasoning = interfaceToInt(details["reasoning_tokens"])
	}
	if reasoning == 0 {
		reasoning = interfaceToInt(raw["reasoning_tokens"])
	}
	if reasoning == 0 {
		if details, _ := raw["completion_tokens_details"].(map[string]interface{}); details != nil {
			reasoning = interfaceToInt(details["reasoning_tokens"])
		}
	}
	inputDetails, _ := firstDefined(raw["input_tokens_details"], raw["prompt_tokens_details"]).(map[string]interface{})
	cached := min(max(interfaceToInt(inputDetails["cached_tokens"]), 0), max(prompt, 0))
	return map[string]interface{}{
		"prompt_tokens":     prompt,
		"completion_tokens": completion,
		"total_tokens":      total,
		"prompt_tokens_details": map[string]interface{}{
			"cached_tokens": cached,
			"text_tokens":   prompt,
			"audio_tokens":  0,
			"image_tokens":  0,
		},
		"completion_tokens_details": map[string]interface{}{
			"text_tokens":      max(completion-reasoning, 0),
			"audio_tokens":     0,
			"reasoning_tokens": reasoning,
		},
	}
}

// finishUpstreamChat completes a chat response after an upstream call: error
// reporting, quota sync, then streaming or collection. Shared by the console
// and CLI chat paths. url is used for both error and request logging; headers
// is evaluated lazily so it is only built on the success path.
func (h *Handler) finishUpstreamChat(ctx context.Context, w http.ResponseWriter, req *ChatCompletionsRequest, sess *chatAccountSession, logger *debug.Logger, name, url string, headers func() http.Header, payload map[string]interface{}, resp *http.Response, err error) {
	if err != nil {
		h.auditChatOutcome(ctx, sess.acc, req, chatOutcome{Finish: "error", Err: err})
		slog.Error(name+" chat upstream failed", "url", url, "status", parseUpstreamStatus(err), "error", err)
		if logger != nil {
			logger.LogUpstreamHTTPError(url, parseUpstreamStatus(err), "", err)
		}
		if markAllGrokAccountStatuses(err) {
			h.markAccountStatus(ctx, sess.acc, err)
		}
		http.Error(w, err.Error(), upstreamHTTPResponseStatus(err))
		return
	}
	defer resp.Body.Close()
	if recovery := resp.Header.Get("X-Grok2API-Reasoning-Recovery"); recovery != "" {
		w.Header().Set("X-Grok2API-Reasoning-Recovery", recovery)
	}
	if logger != nil {
		logger.LogUpstreamRequest(url, debugHeaderMap(headers()), payload)
	}
	h.syncGrokQuota(sess.acc, resp.Header)
	if req.Stream {
		result := h.streamConsoleChat(w, req, resp.Body)
		h.auditChatOutcome(ctx, sess.acc, req, result)
		return
	}
	result := h.collectConsoleChat(w, req, resp.Body)
	h.auditChatOutcome(ctx, sess.acc, req, result)
}

func (h *Handler) serveNativeChat(ctx context.Context, w http.ResponseWriter, req *ChatCompletionsRequest, spec ModelSpec, sess *chatAccountSession, logger *debug.Logger, build bool) {
	if h == nil || sess == nil || sess.acc == nil || (build && h.cliClient == nil) || (!build && h.client == nil) {
		http.Error(w, "grok upstream client or account not configured", http.StatusServiceUnavailable)
		return
	}
	if req.startedAt.IsZero() {
		req.startedAt = time.Now()
	}
	payload, err := h.responsesPayloadFromChat(spec, req, build)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	provider, endpoint, model := ProviderConsole, h.consoleURL("responses"), req.Model
	ctx = withReasoningDiagnostics(ctx, payload)
	if build {
		provider, endpoint, model = ProviderBuild, h.cliBaseURL()+"/responses", spec.UpstreamModel
		if warnings := takeBuildCompatibilityWarnings(payload); warnings != "" {
			w.Header().Set("X-Grok2API-Compatibility-Warnings", warnings)
		}
	}
	openNext := func(excluded []int64) (*chatAccountSession, error) {
		if build {
			return h.openCLIAccountSession(ctx, excluded, model)
		}
		return h.openConsoleAccountSession(ctx, excluded, model)
	}
	request := func() (*http.Response, error) {
		if build {
			attemptStarted := time.Now()
			resp, requestErr := h.cliClient.doResponsesAt(ctx, sess.acc, "/responses", payload)
			if requestErr == nil || !req.ReasoningReplay || payloadHasCompactionInput(payload) || !isReasoningReplayDecodeError(requestErr) {
				return resp, requestErr
			}
			h.auditAttempt(ctx, sess.acc, ProviderBuild, 1, attemptStarted, requestErr, "reasoning_replay_recovery")
			h.clearReasoningReplay(ctx, req.Model, req.PromptCacheKey)
			recoveryAttempt := 1
			lastRecoveryStage := ""
			retryResp, retryErr := recoverChatReasoning(payload, requestErr, func(stage string) (*http.Response, error) {
				recoveryAttempt++
				lastRecoveryStage = stage
				started := time.Now()
				response, failure := h.cliClient.doResponsesAt(ctx, sess.acc, "/responses", payload)
				h.auditAttempt(ctx, sess.acc, ProviderBuild, recoveryAttempt, started, failure, stage)
				return response, failure
			})
			if retryErr == nil && retryResp != nil {
				retryResp.Header.Set("X-Grok2API-Reasoning-Recovery", lastRecoveryStage)
			}
			return retryResp, retryErr
		}
		return h.doConsole(withRateLimitAccount(ctx, sess.acc), sess.token, payload)
	}
	resp, err := h.retryWithAccountSwitch(ctx, sess, 1500*time.Millisecond, request, openNext, nil)
	if build && err == nil && resp != nil {
		tools := append(append([]map[string]interface{}(nil), req.ResponsesTools...), consoleToolsFromOpenAI(req.Tools)...)
		if aliases := collectBuildToolAliases(map[string]interface{}{"tools": tools}); len(aliases) > 0 {
			resp.Body = rewriteBuildToolAliasResponse(resp.Body, resp.Header.Get("Content-Type"), aliases)
		}
	}
	h.finishUpstreamChat(ctx, w, req, sess, logger, provider, endpoint, func() http.Header {
		if build {
			return h.cliHeaders(sess.acc, sess.token)
		}
		return h.client.consoleHeaders(sess.token)
	}, payload, resp, err)
}

// retryWithAccountSwitch runs a request in a bounded loop, switching to
// the next account whenever shouldSwitchGrokAccount fires. doRequest issues the
// request against the current session; openNext returns its replacement.
// onSwitch runs after each successful account swap (e.g. to rebuild the request
// payload for the new account).
func (h *Handler) retryWithAccountSwitch(ctx context.Context, sess *chatAccountSession, switchPace time.Duration, doRequest func() (*http.Response, error), openNext func(used []int64) (*chatAccountSession, error), onSwitch func() error) (*http.Response, error) {
	maxAttempts := 5
	if h != nil && h.cfg != nil && h.cfg.AccountSwitchCount > 0 {
		maxAttempts = min(h.cfg.AccountSwitchCount, 20)
	}

	used := make([]int64, 0)
	attempt := 0
	for {
		attempt++
		if sess.acc != nil && sess.acc.ID != 0 {
			used = append(used, sess.acc.ID)
		}
		started := time.Now()
		resp, err := doRequest()
		provider := "web"
		if sess.acc != nil {
			provider = ProviderForAccount(sess.acc)
		}
		h.auditAttemptDiagnostic(ctx, sess.acc, provider, attempt, started, err, "account_attempt", resp, nil, "")
		if err == nil {
			return resp, nil
		}
		if markAllGrokAccountStatuses(err) {
			h.markAccountStatus(ctx, sess.acc, err)
		}
		if !shouldSwitchGrokAccount(err) || attempt >= maxAttempts {
			return nil, err
		}

		sess.Close()
		if !sleepWithContext(ctx, switchPace) {
			return nil, ctx.Err()
		}
		next, switchErr := openNext(used)
		if switchErr != nil {
			return nil, err
		}
		sess.acc = next.acc
		sess.token = next.token
		sess.poolCandidates = next.poolCandidates
		sess.release = next.release
		if onSwitch != nil {
			if err := onSwitch(); err != nil {
				return nil, err
			}
		}
	}
}

func (h *Handler) collectConsoleChat(w http.ResponseWriter, req *ChatCompletionsRequest, body io.Reader) (outcome chatOutcome) {
	var raw map[string]interface{}
	if err := json.NewDecoder(body).Decode(&raw); err != nil {
		outcome.Err = err
		http.Error(w, "console response parse error: "+err.Error(), http.StatusBadGateway)
		return
	}
	if raw["error"] != nil || interfaceString(raw["status"]) == "failed" {
		outcome.Err = responseFailure(raw)
		http.Error(w, outcome.Err.Error(), http.StatusBadGateway)
		return
	}
	text := consoleExtractMessageText(raw)
	refusal := consoleExtractRefusal(raw)
	filter := stopFilter{sequences: req.Stop}
	text = filter.push(text, true)
	reasoning := consoleExtractReasoningText(raw)
	encryptedReasoning := consoleExtractEncryptedReasoning(raw)
	annotations := consoleChatAnnotations(consoleFlatAnnotations(raw))
	toolCalls := consoleToolCallsFromOutput(raw)
	seen := map[string]bool{}
	for _, entry := range interfaceSlice(raw["output"]) {
		item, _ := entry.(map[string]interface{})
		if interfaceString(item["type"]) != "function_call" {
			continue
		}
		id := firstNonEmpty(interfaceString(item["call_id"]), interfaceString(item["id"]))
		name := interfaceString(item["name"])
		args, validArgs := item["arguments"].(string)
		if id == "" || id == "<nil>" || name == "" || name == "<nil>" || seen[id] || !validArgs || !json.Valid([]byte(args)) {
			outcome.Err = fmt.Errorf("invalid or duplicate upstream function_call")
			http.Error(w, outcome.Err.Error(), http.StatusBadGateway)
			return
		}
		seen[id] = true
	}
	message := map[string]interface{}{
		"role":        "assistant",
		"content":     text,
		"refusal":     nil,
		"annotations": annotations,
	}
	if refusal != "" {
		message["refusal"] = refusal
	}
	var searches []interface{}
	for _, entry := range interfaceSlice(raw["output"]) {
		item, _ := entry.(map[string]interface{})
		if interfaceString(item["type"]) == "web_search_call" {
			searches = append(searches, item)
		}
	}
	if len(searches) > 0 {
		message["x_grok_searches"] = searches
	}
	if filter.matched != "" {
		message["stop_sequence"] = filter.matched
	}
	if strings.TrimSpace(reasoning) != "" {
		message["reasoning_content"] = reasoning
	}
	if encryptedReasoning != "" {
		message["reasoning_encrypted_content"] = encryptedReasoning
	}
	var reasoningItems []interface{}
	for _, entry := range interfaceSlice(raw["output"]) {
		item, _ := entry.(map[string]interface{})
		if interfaceString(item["type"]) == "reasoning" {
			reasoningItems = append(reasoningItems, item)
		}
	}
	if len(reasoningItems) > 1 {
		message["x_grok_reasoning"] = reasoningItems
	}
	finishReason := "stop"
	if len(toolCalls) > 0 {
		message["tool_calls"] = toolCalls
		finishReason = "tool_calls"
		if strings.TrimSpace(text) == "" {
			message["content"] = nil
		}
	}
	if interfaceString(raw["status"]) == "incomplete" {
		finishReason = "length"
	}
	if filter.matched != "" {
		finishReason = "stop"
	}
	if text == "" && refusal == "" && len(toolCalls) == 0 && finishReason != "length" && filter.matched == "" {
		outcome.Err = fmt.Errorf("upstream completed response with no content or tool calls")
		http.Error(w, outcome.Err.Error(), http.StatusBadGateway)
		return
	}
	outcome.Usage = firstUsage(consoleUsage(raw), addReasoningUsage(buildChatUsagePayload(req, text+refusal, toolCalls), reasoning))
	outcome.Finish = finishReason
	outcome.FirstToken = time.Now()
	resp := map[string]interface{}{
		"id":                 firstNonEmpty(interfaceString(raw["id"]), "chatcmpl_"+randomHex(8)),
		"object":             "chat.completion",
		"created":            time.Now().Unix(),
		"model":              req.Model,
		"service_tier":       nil,
		"system_fingerprint": "",
		"choices": []map[string]interface{}{{
			"index":         0,
			"message":       message,
			"finish_reason": finishReason,
		}},
		"usage": outcome.Usage,
	}
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(resp); err != nil {
		outcome.Err = err
		outcome.Finish = "error"
		return
	}
	if req.ReasoningReplay && encryptedReasoning != "" {
		h.storeReasoningReplay(req.Model, req.PromptCacheKey, encryptedReasoning)
	}
	return
}

func consoleExtractReasoningText(raw map[string]interface{}) string {
	if raw == nil {
		return ""
	}
	var result strings.Builder
	for _, value := range interfaceSlice(raw["output"]) {
		item, _ := value.(map[string]interface{})
		if item == nil || !strings.EqualFold(strings.TrimSpace(fmt.Sprint(item["type"])), "reasoning") {
			continue
		}
		var rawText, summaryText strings.Builder
		for _, value := range interfaceSlice(item["content"]) {
			part, _ := value.(map[string]interface{})
			if part == nil || !strings.EqualFold(strings.TrimSpace(fmt.Sprint(part["type"])), "reasoning_text") {
				continue
			}
			if text := fmt.Sprint(part["text"]); strings.TrimSpace(text) != "" && text != "<nil>" {
				rawText.WriteString(text)
			}
		}
		for _, value := range interfaceSlice(item["summary"]) {
			part, _ := value.(map[string]interface{})
			if part == nil {
				continue
			}
			if text := fmt.Sprint(part["text"]); strings.TrimSpace(text) != "" && text != "<nil>" {
				summaryText.WriteString(text)
			}
		}
		if rawText.Len() > 0 {
			result.WriteString(rawText.String())
		} else {
			result.WriteString(summaryText.String())
		}
	}
	return result.String()
}

func consoleExtractEncryptedReasoning(raw map[string]interface{}) string {
	if raw == nil {
		return ""
	}
	latest := ""
	for _, value := range interfaceSlice(raw["output"]) {
		item, _ := value.(map[string]interface{})
		if item == nil || !strings.EqualFold(strings.TrimSpace(fmt.Sprint(item["type"])), "reasoning") {
			continue
		}
		if encrypted := strings.TrimSpace(fmt.Sprint(item["encrypted_content"])); encrypted != "" && encrypted != "<nil>" {
			latest = encrypted
		}
	}
	return latest
}

func consoleToolCallsFromOutput(raw map[string]interface{}) []map[string]interface{} {
	if raw == nil {
		return nil
	}
	var out []map[string]interface{}
	for _, item := range interfaceSlice(raw["output"]) {
		if tc := consoleToolCallFromItem(item); tc != nil {
			out = append(out, tc)
		}
	}
	return out
}

func consoleToolCallFromItem(raw interface{}) map[string]interface{} {
	item, _ := raw.(map[string]interface{})
	if item == nil {
		return nil
	}
	if !strings.EqualFold(strings.TrimSpace(fmt.Sprint(item["type"])), "function_call") {
		return nil
	}
	name := strings.TrimSpace(fmt.Sprint(item["name"]))
	if name == "" || name == "<nil>" {
		return nil
	}
	callID := strings.TrimSpace(fmt.Sprint(item["call_id"]))
	if callID == "" || callID == "<nil>" {
		callID = strings.TrimSpace(fmt.Sprint(item["id"]))
	}
	if callID == "" || callID == "<nil>" {
		callID = "call_" + randomHex(12)
	}
	arguments := "{}"
	if rawArgs, ok := item["arguments"]; ok && rawArgs != nil {
		switch v := rawArgs.(type) {
		case string:
			if strings.TrimSpace(v) != "" {
				arguments = strings.TrimSpace(v)
			}
		default:
			if buf, err := json.Marshal(v); err == nil {
				arguments = string(buf)
			}
		}
	}
	return map[string]interface{}{
		"id":   callID,
		"type": "function",
		"function": map[string]interface{}{
			"name":      name,
			"arguments": arguments,
		},
	}
}

func firstUsage(a, b map[string]interface{}) map[string]interface{} {
	if len(a) > 0 {
		return a
	}
	return b
}

func consoleUsageFromStreamEvent(ev map[string]interface{}) map[string]interface{} {
	if ev == nil {
		return nil
	}
	if resp, _ := ev["response"].(map[string]interface{}); resp != nil {
		if usage := consoleUsage(resp); len(usage) > 0 {
			return usage
		}
	}
	return consoleUsage(ev)
}

func consoleReasoningDelta(event string, ev map[string]interface{}) string {
	event = strings.ToLower(strings.TrimSpace(event))
	if !strings.Contains(event, "reasoning") || !strings.Contains(event, "delta") {
		return ""
	}
	for _, key := range []string{"delta", "text"} {
		if value, ok := ev[key]; ok && value != nil {
			if text, ok := value.(string); ok && text != "" {
				return text
			}
		}
	}
	return ""
}
