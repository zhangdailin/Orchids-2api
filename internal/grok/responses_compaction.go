// Gateway-side Responses compaction.
//
// Derived from chenyme/grok2api, commit 906b9493. MIT license:
// ../../licenses/third-party-MIT.txt. Source files:
// backend/internal/application/gateway/responses_compaction.go and
// backend/internal/infra/provider/cli/responses_compaction.go (+ _forward.go).
//
// Why the gateway compacts at all: Codex's remote-v2 compaction asks for a
// summary turn and expects back a portable `compaction` item. A pure relay has
// to hand that turn to the upstream, which returns a blob only the upstream can
// read — so a later turn served by a different account cannot use it, and the
// conversation has to be compacted again by the client. Instead the gateway runs
// the summary turn itself, keeps the plain text in a blob only this gateway can
// open (`g2a_compact_v1.<sealed>`), and expands it back into an ordinary user
// message whenever the client replays it.
package grok

import (
	"bytes"
	"context"
	"crypto/rand"
	_ "embed"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/goccy/go-json"

	"orchids-api/internal/audit"
	"orchids-api/internal/middleware"
	"orchids-api/internal/secureblob"
)

const (
	gatewayCompactionPrefix      = "g2a_compact_v1."
	gatewayCompactionVersion     = 1
	maxGatewayCompactionSummary  = 8 << 20
	minGatewayCompactionRunes    = 500
	gatewayCompactionMaxAttempts = 3
	gatewayCompactionRetryDelay  = 3 * time.Second

	// clientCompactionPromptMarker is a distinctive line from grok-build's
	// full_replace_summary_prompt.txt and the Grok TUI compaction request. Codex
	// remote-v2 sends compaction_trigger instead; the TUI appends this prompt as
	// an ordinary last user item.
	clientCompactionPromptMarker = "it is a system-generated compaction prompt, not a real user message"
)

// Generated from xai-org/grok-build's full_replace_summary_prompt.txt using
// build_summary_prompt(None), so the optional {user_context_section} slot is
// empty. The Grok Build source appends this text as the final user item of every
// compaction attempt, so the summary is only reproducible with the same prompt.
//
//go:embed responses_compaction_prompt.txt
var gatewayCompactionPrompt string

type gatewayCompactionKind uint8

const (
	responsesCompactionNone gatewayCompactionKind = iota
	responsesCompactionTrigger
	responsesCompactionTUI
)

// Field names match grok2api's envelope. The blob is still only readable by the
// deployment that sealed it (the key is derived from that deployment's
// credential key), but the plaintext shape is the reference one, so tooling that
// inspects a blob sees the same keys on both sides.
type gatewayCompactionEnvelope struct {
	Version int    `json:"version"`
	Session string `json:"session"`
	Summary string `json:"summary"`
}

// gatewayCompactionCodec seals and opens gateway-owned compaction blobs. A
// foreign blob (no prefix) is reported as not-owned so it still reaches the
// upstream unchanged: an original upstream compaction item must keep working.
type gatewayCompactionCodec struct {
	cipher *secureblob.Cipher
}

func newGatewayCompactionCodec(cipher *secureblob.Cipher) *gatewayCompactionCodec {
	if cipher == nil || !cipher.Available() {
		return nil
	}
	return &gatewayCompactionCodec{cipher: cipher}
}

func (c *gatewayCompactionCodec) available() bool {
	return c != nil && c.cipher != nil && c.cipher.Available()
}

func (c *gatewayCompactionCodec) encode(session, summary string) (string, error) {
	if !c.available() {
		return "", fmt.Errorf("compaction codec unavailable")
	}
	if summary == "" || len(summary) > maxGatewayCompactionSummary {
		return "", fmt.Errorf("compaction summary size is invalid")
	}
	data, err := json.Marshal(gatewayCompactionEnvelope{Version: gatewayCompactionVersion, Session: session, Summary: summary})
	if err != nil {
		return "", err
	}
	sealed, err := c.cipher.Seal(string(data))
	if err != nil {
		return "", err
	}
	return gatewayCompactionPrefix + sealed, nil
}

// decode reports owned=false for a blob this gateway did not issue. Session is
// advisory: a still-decryptable summary stays usable when the client's cache key
// drifts (model switch, degrade, new TUI session). A foreign provider blob is
// never opened because it never carries this prefix, and an undecryptable
// prefixed blob is a hard error rather than a silent empty summary.
func (c *gatewayCompactionCodec) decode(session, blob string) (summary string, owned bool, sessionDrifted bool, err error) {
	if !strings.HasPrefix(blob, gatewayCompactionPrefix) {
		return "", false, false, nil
	}
	if !c.available() {
		return "", true, false, fmt.Errorf("compaction codec unavailable")
	}
	plain, err := c.cipher.Open(strings.TrimPrefix(blob, gatewayCompactionPrefix))
	if err != nil {
		return "", true, false, fmt.Errorf("decode gateway compaction blob: %w", err)
	}
	var envelope gatewayCompactionEnvelope
	if err := json.Unmarshal([]byte(plain), &envelope); err != nil {
		return "", true, false, fmt.Errorf("decode gateway compaction payload: %w", err)
	}
	if envelope.Version != gatewayCompactionVersion || envelope.Summary == "" || len(envelope.Summary) > maxGatewayCompactionSummary {
		return "", true, false, fmt.Errorf("gateway compaction payload is invalid")
	}
	return envelope.Summary, true, envelope.Session != session, nil
}

// classifyResponsesCompactionPayload decides whether this turn is a compaction
// request, without retaining or logging the body.
func classifyResponsesCompactionPayload(payload map[string]interface{}) gatewayCompactionKind {
	if payload == nil {
		return responsesCompactionNone
	}
	input := responsesPayloadItems(payload["input"])
	if hasCompactionTrigger(input) {
		return responsesCompactionTrigger
	}
	if lastItemLooksLikeCompactionPrompt(input) || lastItemLooksLikeCompactionPrompt(responsesPayloadItems(payload["messages"])) {
		return responsesCompactionTUI
	}
	return responsesCompactionNone
}

func responsesPayloadItems(value interface{}) []interface{} {
	items, _ := value.([]interface{})
	return items
}

func hasCompactionTrigger(items []interface{}) bool {
	for _, raw := range items {
		item, ok := raw.(map[string]interface{})
		if !ok {
			continue
		}
		if strings.EqualFold(strings.TrimSpace(parseLooseStringAny(item["type"])), "compaction_trigger") {
			return true
		}
	}
	return false
}

func lastItemLooksLikeCompactionPrompt(items []interface{}) bool {
	if len(items) == 0 {
		return false
	}
	item, ok := items[len(items)-1].(map[string]interface{})
	if !ok || !strings.EqualFold(strings.TrimSpace(parseLooseStringAny(item["role"])), "user") {
		return false
	}
	return looksLikeCompactionPrompt(compactionContentText(item["content"]))
}

func looksLikeCompactionPrompt(text string) bool {
	return strings.Contains(text, clientCompactionPromptMarker)
}

// compactionContentText flattens the two content shapes a user item can carry: a
// plain string, or an array of parts.
func compactionContentText(content interface{}) string {
	switch typed := content.(type) {
	case string:
		return typed
	case []interface{}:
		var builder strings.Builder
		for _, raw := range typed {
			part, ok := raw.(map[string]interface{})
			if !ok {
				continue
			}
			builder.WriteString(parseLooseStringAny(part["text"]))
		}
		return builder.String()
	case map[string]interface{}:
		return parseLooseStringAny(typed["text"])
	default:
		return ""
	}
}

// expandGatewayCompactionHistory replaces every gateway-owned compaction blob in
// the request history with the summary it carries. Non-prefixed blobs are left
// untouched (upstream originals), and an unreadable prefixed blob is an error
// carrying the exact input index, because silently dropping it would answer a
// question the client never asked.
func expandGatewayCompactionHistory(payload map[string]interface{}, codec *gatewayCompactionCodec, session string) (drifted int, err error) {
	if payload == nil || !codec.available() {
		return 0, nil
	}
	items, ok := payload["input"].([]interface{})
	if !ok {
		return 0, nil
	}
	changed := false
	for index, raw := range items {
		item, ok := raw.(map[string]interface{})
		if !ok || parseLooseStringAny(item["type"]) != "compaction" {
			continue
		}
		blob := parseLooseStringAny(item["encrypted_content"])
		summary, owned, sessionDrifted, decodeErr := codec.decode(session, blob)
		if decodeErr != nil {
			return drifted, &compactionBlobError{index: index, cause: decodeErr}
		}
		if !owned {
			continue
		}
		items[index] = gatewayCompactionSummaryMessage(summary)
		if sessionDrifted {
			drifted++
		}
		changed = true
	}
	if changed {
		payload["input"] = items
	}
	return drifted, nil
}

// compactionBlobError carries the offending input index so the client can point
// at the item it has to drop.
type compactionBlobError struct {
	index int
	cause error
}

func (e *compactionBlobError) Error() string {
	return "the gateway compaction blob could not be decoded; ensure it is unmodified and was issued by this gateway"
}

func (e *compactionBlobError) Param() string {
	return fmt.Sprintf("input[%d].encrypted_content", e.index)
}

func (e *compactionBlobError) Unwrap() error { return e.cause }

// prepareGatewayCompactionSample mirrors Grok Build full-replace sampling:
// streaming /responses, instructions=null, tools retained with
// tool_choice=auto, concise reasoning summary, and the canonical final user
// prompt.
func prepareGatewayCompactionSample(payload map[string]interface{}) map[string]interface{} {
	sample := clonePayload(payload)
	items, _ := sample["input"].([]interface{})
	items = append(items, map[string]interface{}{
		"type": "message", "role": "user", "content": gatewayCompactionPrompt,
	})
	sample["input"] = items
	sample["instructions"] = nil
	sample["stream"] = true
	sample["store"] = false
	sample["temperature"] = 1.0
	if tools, ok := sample["tools"].([]interface{}); ok && len(tools) > 0 {
		// Some deployments reject tools together with tool_choice=none.
		sample["tool_choice"] = "auto"
	} else {
		delete(sample, "tool_choice")
	}
	sample["reasoning"] = map[string]interface{}{"summary": "concise"}
	for _, field := range []string{
		"previous_response_id", "text", "response_format", "max_output_tokens", "max_completion_tokens",
	} {
		delete(sample, field)
	}
	return sample
}

func clonePayload(payload map[string]interface{}) map[string]interface{} {
	if payload == nil {
		return map[string]interface{}{}
	}
	clone := make(map[string]interface{}, len(payload))
	for key, value := range payload {
		clone[key] = value
	}
	return clone
}

// cleanGatewayCompactionSummary mirrors grok-build's summary cleaner: drop a
// leading analysis block, rewrite the <summary> wrapper as a plain "Summary:"
// heading, defuse the tags so they cannot be re-interpreted, and collapse the
// runs of blank lines the model leaves behind.
func cleanGatewayCompactionSummary(raw string) string {
	result := strings.TrimSpace(raw)
	for {
		start := strings.Index(result, "<analysis>")
		if start < 0 {
			break
		}
		summaryStart := strings.Index(result, "<summary>")
		leading := summaryStart < 0 && strings.TrimSpace(result[:start]) == ""
		if summaryStart >= 0 {
			leading = start < summaryStart || strings.TrimSpace(result[summaryStart+len("<summary>"):start]) == ""
		}
		if !leading {
			break
		}
		endRel := strings.Index(result[start:], "</analysis>")
		if endRel < 0 {
			if nextSummary := strings.Index(result[start:], "<summary>"); nextSummary >= 0 {
				result = result[:start] + result[start+nextSummary:]
			} else {
				result = result[:start]
			}
			break
		}
		end := start + endRel + len("</analysis>")
		result = result[:start] + result[end:]
	}
	if start := strings.Index(result, "<summary>"); start >= 0 {
		if end := strings.LastIndex(result, "</summary>"); end > start {
			before := result[:start]
			inner := stripLeadingGatewayCompactionScratchpad(result[start+len("<summary>") : end])
			after := result[end+len("</summary>"):]
			result = before + "Summary:\n" + inner + after
		}
	}
	result = neutralizeGatewayCompactionTags(result)
	for strings.Contains(result, "\n\n\n") {
		result = strings.ReplaceAll(result, "\n\n\n", "\n\n")
	}
	return strings.TrimSpace(result)
}

// stripLeadingGatewayCompactionScratchpad handles the common malformed shape
// where the model emits an untagged markdown analysis followed by an orphan
// </analysis> inside <summary>. A numbered summary is left intact even when it
// quotes that token later.
func stripLeadingGatewayCompactionScratchpad(inner string) string {
	result := strings.TrimSpace(inner)
	lead := strings.TrimLeft(result, "#*-> \t")
	startsWithNumber := len(lead) > 0 && lead[0] >= '0' && lead[0] <= '9'
	if !startsWithNumber {
		if end := strings.LastIndex(result, "</analysis>"); end >= 0 {
			result = strings.TrimSpace(result[end+len("</analysis>"):])
		}
	}
	if strings.HasPrefix(result, "<summary>") {
		result = strings.TrimSpace(strings.TrimPrefix(result, "<summary>"))
	}
	return result
}

func neutralizeGatewayCompactionTags(text string) string {
	for _, tag := range []string{"</summary>", "<summary>", "</analysis>", "<analysis>", "</summary_request>", "<summary_request>"} {
		text = strings.ReplaceAll(text, tag, "<\u200b"+strings.TrimPrefix(tag, "<"))
	}
	return text
}

func isDegenerateGatewayCompactionSummary(summary string) bool {
	cleaned := cleanGatewayCompactionSummary(summary)
	return cleaned == "" || utf8.RuneCountInString(cleaned) < minGatewayCompactionRunes
}

// gatewayCompactionContinuation is the preamble Grok Build puts in front of a
// replayed summary.
func gatewayCompactionContinuation(raw string) string {
	return "This session is being continued from a previous conversation that ran out of context. " +
		"The summary below covers the earlier portion of the conversation.\n\n" + cleanGatewayCompactionSummary(raw)
}

// Responses does not expose SyntheticReason, so an ordinary user input item is
// the closest wire-level representation of Grok Build's synthetic user_meta
// carrier.
func gatewayCompactionSummaryMessage(text string) map[string]interface{} {
	return map[string]interface{}{
		"type": "message", "role": "user",
		"content": []interface{}{map[string]interface{}{"type": "input_text", "text": text}},
	}
}

type gatewayCompactionSample struct {
	response map[string]interface{}
	summary  string
}

type gatewayCompactionStreamError struct {
	message   string
	transient bool
}

func (e *gatewayCompactionStreamError) Error() string { return e.message }

var (
	errGatewayCompactionStreamClosed = fmt.Errorf("compaction stream closed before response.completed")
	errGatewayCompactionDegenerate   = fmt.Errorf("compaction model returned an empty or degenerate summary")
)

// parseGatewayCompactionStream reads the summary out of the sample turn. The
// completed response is authoritative; the streamed output items are the
// fallback for a stream that ends without a usable summary object.
func parseGatewayCompactionStream(data []byte) (gatewayCompactionSample, error) {
	var completed map[string]interface{}
	var streamedParts []string
	err := consumeCompatibleSSE(bytes.NewReader(data), func(event compatibleSSEEvent) error {
		if !event.HasData() || bytes.Equal(bytes.TrimSpace(event.Data()), []byte("[DONE]")) {
			return nil
		}
		var payload map[string]interface{}
		if json.Unmarshal(event.Data(), &payload) != nil {
			return nil
		}
		kind := strings.TrimSpace(parseLooseStringAny(payload["type"]))
		if kind == "" {
			kind = strings.TrimSpace(event.Event)
		}
		switch kind {
		case "response.output_item.done":
			if item, ok := payload["item"].(map[string]interface{}); ok {
				streamedParts = append(streamedParts, gatewayCompactionItemText(item)...)
			}
		case "response.completed":
			if response, ok := payload["response"].(map[string]interface{}); ok {
				completed = clonePayload(response)
			}
		case "response.failed":
			response, _ := payload["response"].(map[string]interface{})
			errorValue, _ := response["error"].(map[string]interface{})
			return newGatewayCompactionStreamError(
				strings.TrimSpace(parseLooseStringAny(errorValue["code"])),
				strings.TrimSpace(parseLooseStringAny(errorValue["message"])),
			)
		case "error", "response.error":
			errorValue, _ := payload["error"].(map[string]interface{})
			if errorValue == nil {
				errorValue = payload
			}
			return newGatewayCompactionStreamError(
				strings.TrimSpace(parseLooseStringAny(errorValue["code"])),
				strings.TrimSpace(parseLooseStringAny(errorValue["message"])),
			)
		}
		return nil
	})
	if err != nil {
		return gatewayCompactionSample{}, err
	}
	if completed == nil {
		return gatewayCompactionSample{}, errGatewayCompactionStreamClosed
	}
	summary := extractCompactionSummary(completed)
	if summary == "" {
		summary = strings.Join(streamedParts, "\n")
	}
	return gatewayCompactionSample{response: completed, summary: summary}, nil
}

func newGatewayCompactionStreamError(code, message string) error {
	if message == "" {
		message = "upstream compaction stream failed"
	}
	detail := message
	if code != "" {
		detail = code + ": " + message
	}
	return &gatewayCompactionStreamError{message: detail, transient: gatewayCompactionEventErrorIsTransient(code, message)}
}

// gatewayCompactionEventErrorIsTransient separates "ask again" from "this
// request cannot work": a bad request or an over-long prompt must not be retried,
// because each retry costs a full summary generation.
func gatewayCompactionEventErrorIsTransient(code, message string) bool {
	lowerCode := strings.ToLower(strings.TrimSpace(code))
	lowerMessage := strings.ToLower(message)
	if lowerCode == "invalid_request_error" || strings.Contains(lowerMessage, "invalid_request_error") {
		return false
	}
	if status, err := strconv.Atoi(lowerCode); err == nil &&
		status >= 400 && status < 500 && status != http.StatusRequestTimeout && status != http.StatusTooManyRequests {
		return false
	}
	for _, marker := range []string{"prompt is too long", "maximum prompt length", "maximum context length", "context_length_exceeded", "too long for this model"} {
		if strings.Contains(lowerMessage, marker) {
			return false
		}
	}
	return true
}

func gatewayCompactionErrorIsTransient(err error) bool {
	var streamErr *gatewayCompactionStreamError
	if errors.As(err, &streamErr) {
		return streamErr.transient
	}
	return true
}

func gatewayCompactionItemText(item map[string]interface{}) []string {
	if parseLooseStringAny(item["type"]) != "message" {
		return nil
	}
	content, _ := item["content"].([]interface{})
	parts := make([]string, 0, len(content))
	for _, raw := range content {
		value, ok := raw.(map[string]interface{})
		if !ok {
			continue
		}
		switch parseLooseStringAny(value["type"]) {
		case "output_text", "text":
			if text := strings.TrimSpace(parseLooseStringAny(value["text"])); text != "" {
				parts = append(parts, text)
			}
		}
	}
	return parts
}

func extractCompactionSummary(response map[string]interface{}) string {
	if text := strings.TrimSpace(parseLooseStringAny(response["output_text"])); text != "" {
		return text
	}
	var parts []string
	output, _ := response["output"].([]interface{})
	for _, raw := range output {
		if item, ok := raw.(map[string]interface{}); ok {
			parts = append(parts, gatewayCompactionItemText(item)...)
		}
	}
	return strings.Join(parts, "\n")
}

// normalizeGatewayCompactionUsage keeps the synthetic response acceptable to
// Codex even when Grok omits one of the required usage fields. An entirely
// absent usage object stays absent rather than being fabricated.
func normalizeGatewayCompactionUsage(response map[string]interface{}) {
	usage, ok := response["usage"].(map[string]interface{})
	if !ok {
		if response["usage"] == nil {
			delete(response, "usage")
		}
		return
	}
	input := nonNegativeJSONInteger(usage["input_tokens"])
	output := nonNegativeJSONInteger(usage["output_tokens"])
	usage["input_tokens"] = input
	usage["output_tokens"] = output
	if total, valid := nonNegativeJSONIntegerOK(usage["total_tokens"]); !valid || total < input+output {
		usage["total_tokens"] = input + output
	}
}

func nonNegativeJSONInteger(value interface{}) int64 {
	number, _ := nonNegativeJSONIntegerOK(value)
	return number
}

func nonNegativeJSONIntegerOK(value interface{}) (int64, bool) {
	switch typed := value.(type) {
	case float64:
		if typed < 0 || typed != float64(int64(typed)) {
			return 0, false
		}
		return int64(typed), true
	case int64:
		return max(int64(0), typed), typed >= 0
	case int:
		return int64(max(0, typed)), typed >= 0
	case json.Number:
		parsed, err := typed.Int64()
		if err != nil || parsed < 0 {
			return 0, false
		}
		return parsed, true
	default:
		return 0, false
	}
}

// buildGatewayCompactionResponse turns the summary turn into the answer the
// client actually asked for: a single compaction item holding this gateway's
// sealed blob.
func buildGatewayCompactionResponse(response map[string]interface{}, blob, model string) map[string]interface{} {
	result := clonePayload(response)
	normalizeGatewayCompactionUsage(result)
	responseID := strings.TrimSpace(parseLooseStringAny(result["id"]))
	if responseID == "" {
		responseID = "resp_" + compactionRandomHex(16)
	}
	item := map[string]interface{}{
		"id":                "cmp_" + strings.TrimPrefix(responseID, "resp_"),
		"type":              "compaction",
		"encrypted_content": blob,
	}
	result["id"] = responseID
	result["object"] = "response"
	result["status"] = "completed"
	result["model"] = model
	result["output"] = []interface{}{item}
	delete(result, "output_text")
	return result
}

// compactionRandomHex mints the random half of a synthetic response id.
func compactionRandomHex(size int) string {
	buf := make([]byte, size)
	if _, err := rand.Read(buf); err != nil {
		return fmt.Sprintf("%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(buf)
}

// writeGatewayCompactionStream emits the synthetic SSE sequence Codex expects
// from a compaction turn. It mirrors the upstream event order so a client that
// tracks sequence numbers sees one consistent stream.
func writeGatewayCompactionStream(w io.Writer, response map[string]interface{}) error {
	flusher, _ := w.(http.Flusher)
	created := clonePayload(response)
	created["status"] = "in_progress"
	created["output"] = []interface{}{}
	item := response["output"].([]interface{})[0]
	events := []struct {
		name string
		data map[string]interface{}
	}{
		{"response.created", map[string]interface{}{"type": "response.created", "sequence_number": 0, "response": created}},
		{"response.in_progress", map[string]interface{}{"type": "response.in_progress", "sequence_number": 1, "response": clonePayload(created)}},
		{"response.output_item.added", map[string]interface{}{"type": "response.output_item.added", "sequence_number": 2, "output_index": 0, "item": item}},
		{"keepalive", map[string]interface{}{"type": "keepalive", "sequence_number": 3}},
		{"response.output_item.done", map[string]interface{}{"type": "response.output_item.done", "sequence_number": 4, "output_index": 0, "item": item}},
		{"response.completed", map[string]interface{}{"type": "response.completed", "sequence_number": 5, "response": response}},
	}
	for _, event := range events {
		encoded, err := json.Marshal(event.data)
		if err != nil {
			return err
		}
		if _, err := fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event.name, encoded); err != nil {
			return err
		}
		if flusher != nil {
			flusher.Flush()
		}
	}
	return nil
}

// compactionHTTPErrorIsTransient decides whether the sample turn can be retried
// against another account. A 4xx other than 408/429 describes the request, so
// repeating it only burns another summary generation.
func compactionHTTPErrorIsTransient(status int, body string) bool {
	lower := strings.ToLower(body)
	for _, marker := range []string{"prompt is too long", "maximum prompt length", "maximum context length", "context_length_exceeded", "too long for this model"} {
		if strings.Contains(lower, marker) {
			return false
		}
	}
	return status == http.StatusRequestTimeout || status == http.StatusTooManyRequests || status >= 500
}

// gatewayCompactionRetryPause is a variable so a test can drive the retry
// without sleeping for the production pause.
var gatewayCompactionRetryPause = gatewayCompactionRetryDelay

// SetCompactionCipher lets the deployment hand the handler the sealing key for
// gateway-owned compaction state. Without it the feature is off and compaction
// turns keep their previous behaviour (a plain upstream forward).
func (h *Handler) SetCompactionCipher(cipher *secureblob.Cipher) {
	if h == nil {
		return
	}
	codec := newGatewayCompactionCodec(cipher)
	h.compactionMu.Lock()
	h.compactionCode = codec
	h.compactionMu.Unlock()
}

func (h *Handler) compactionCodecSnapshot() *gatewayCompactionCodec {
	if h == nil {
		return nil
	}
	h.compactionMu.RLock()
	codec := h.compactionCode
	h.compactionMu.RUnlock()
	return codec
}

// GatewayCompactionEnabled reports whether this deployment can own compaction
// state. Callers use it to route compaction turns through the gateway.
func (h *Handler) GatewayCompactionEnabled() bool {
	return h.compactionCodecSnapshot().available()
}

// handleGatewayCompaction answers a compaction turn itself: it runs the canonical
// summary request upstream, seals the cleaned summary into a gateway blob, and
// returns a synthetic compaction response.
//
// The summary turn is retried on another account for transient failures only. A
// degenerate or malformed summary and any 4xx describe the request, so repeating
// them would burn another full summary generation for the same answer.
func (h *Handler) handleGatewayCompaction(w http.ResponseWriter, r *http.Request, modelID string, spec ModelSpec, payload map[string]interface{}, streaming bool) {
	codec := h.compactionCodecSnapshot()
	if !codec.available() {
		writeResponsesAPIError(w, http.StatusServiceUnavailable, "service_unavailable", "gateway compaction is not configured")
		return
	}
	if h == nil || h.buildClient() == nil {
		writeResponsesAPIError(w, http.StatusServiceUnavailable, "service_unavailable", "grok cli client not configured")
		return
	}
	sessionKey := sessionFromContext(r.Context()).Key
	sample := prepareGatewayCompactionSample(payload)
	sample["model"] = spec.UpstreamModel

	fail := func(status int, code, message string) {
		writeResponsesAPIError(w, status, code, message)
	}
	// lastErr is only used to decide whether another attempt is worthwhile; the
	// client never sees upstream prose from this path.
	var lastErr error
	for attempt := 1; attempt <= gatewayCompactionMaxAttempts; attempt++ {
		sess, err := h.openCLIAccountSession(r.Context(), nil, spec.UpstreamModel)
		if err != nil {
			// The pool's note names why it is empty, and this path used to put it in
			// the body: classify it the way every other entrance does, and let the
			// note stay in the log.
			answer := classifyGrokPoolFailure(err, "response_account_unavailable", grokResponseAccountUnavailableMessage)
			fail(answer.status, answer.code, answer.message)
			return
		}
		accountID := sess.acc.ID
		resp, callErr := h.doCLIWithAutoSwitchAt(r.Context(), sess, sample, spec.UpstreamModel, "/responses")
		if callErr != nil {
			sess.Close()
			lastErr = callErr
			if attempt < gatewayCompactionMaxAttempts && waitGatewayCompactionRetry(r.Context(), gatewayCompactionRetryPause) {
				continue
			}
			// The comment above says the client never sees upstream prose from this
			// path; that is true once the failure goes through the shared sanitizer
			// (err.Error() used to leak it here).
			slog.Warn("Reporting an upstream failure to the client", "error", callErr,
				"status", upstreamHTTPResponseStatus(callErr))
			fail(upstreamHTTPResponseStatus(callErr), "upstream_error", grokUpstreamFailureMessage(callErr))
			return
		}
		h.syncGrokQuota(sess.acc, resp.Header)
		data, readErr := io.ReadAll(io.LimitReader(resp.Body, maxNativeResponsesBytes+1))
		status := resp.StatusCode
		_ = resp.Body.Close()
		sess.Close()
		if readErr != nil {
			lastErr = readErr
			if attempt < gatewayCompactionMaxAttempts && waitGatewayCompactionRetry(r.Context(), gatewayCompactionRetryPause) {
				continue
			}
			fail(http.StatusBadGateway, "upstream_error", "upstream compaction response could not be read")
			return
		}
		if status < 200 || status >= 300 {
			lastErr = fmt.Errorf("upstream status=%d", status)
			if attempt < gatewayCompactionMaxAttempts && compactionHTTPErrorIsTransient(status, string(data)) &&
				waitGatewayCompactionRetry(r.Context(), gatewayCompactionRetryPause) {
				continue
			}
			fail(upstreamHTTPResponseStatus(lastErr), "upstream_error", "upstream compaction request failed")
			return
		}
		if len(data) > maxNativeResponsesBytes {
			fail(http.StatusBadGateway, "compaction_failed", "upstream compaction response was too large")
			return
		}
		parsed, parseErr := parseGatewayCompactionStream(data)
		if parseErr == nil && isDegenerateGatewayCompactionSummary(parsed.summary) {
			parseErr = errGatewayCompactionDegenerate
		}
		if parseErr != nil {
			lastErr = parseErr
			if gatewayCompactionErrorIsTransient(parseErr) && attempt < gatewayCompactionMaxAttempts &&
				waitGatewayCompactionRetry(r.Context(), gatewayCompactionRetryPause) {
				continue
			}
			fail(http.StatusBadGateway, "compaction_failed", "Grok Build compaction failed")
			return
		}
		blob, encodeErr := codec.encode(sessionKey, gatewayCompactionContinuation(parsed.summary))
		if encodeErr != nil {
			fail(http.StatusBadGateway, "compaction_failed", "gateway compaction state could not be encoded")
			return
		}
		result := buildGatewayCompactionResponse(parsed.response, blob, modelID)
		h.auditGatewayCompaction(r.Context(), accountID, modelID, result)
		if !streaming {
			writeJSONStatus(w, http.StatusOK, result)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("X-Accel-Buffering", "no")
		w.WriteHeader(http.StatusOK)
		if err := writeGatewayCompactionStream(w, result); err != nil {
			slog.Debug("gateway compaction stream write failed", "error", err)
		}
		return
	}
	slog.Debug("gateway compaction exhausted its attempts", "model", modelID, "error", lastErr)
	fail(http.StatusBadGateway, "compaction_failed", "Grok Build compaction failed")
}

// auditGatewayCompaction records the gateway-owned compaction turn. Token counts
// come from the summary turn the gateway itself ran, so the source is upstream.
func (h *Handler) auditGatewayCompaction(ctx context.Context, accountID int64, modelID string, result map[string]interface{}) {
	if h == nil || h.auditLogger == nil {
		return
	}
	usage, _ := result["usage"].(map[string]interface{})
	h.auditLogger.Log(ctx, audit.Event{
		Kind: audit.KindRequest, RequestID: middleware.GetRequestID(ctx), Action: "grok_compaction",
		APIKeyID: middleware.APIKeyID(ctx), AccountID: accountID, Model: modelID,
		Channel: "grok", Provider: ProviderBuild, Status: "success",
		InputTokens:  int(nonNegativeJSONInteger(usage["input_tokens"])),
		OutputTokens: int(nonNegativeJSONInteger(usage["output_tokens"])),
		TotalTokens:  int(nonNegativeJSONInteger(usage["total_tokens"])),
		UsageSource:  audit.UsageSourceUpstream,
	})
}

// waitGatewayCompactionRetry pauses between summary attempts, giving up as soon
// as the caller goes away.
func waitGatewayCompactionRetry(ctx context.Context, delay time.Duration) bool {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

// compactionErrorParam exposes the input index an expansion failure points at.
func compactionErrorParam(err error) string {
	var blobErr *compactionBlobError
	if errors.As(err, &blobErr) {
		return blobErr.Param()
	}
	return "input"
}

// responsesPayloadStreaming reports whether the caller asked for SSE. The parsed
// request is authoritative when the raw payload omitted the field.
func responsesPayloadStreaming(payload map[string]interface{}, fallback bool) bool {
	if value, ok := payload["stream"].(bool); ok {
		return value
	}
	return fallback
}
