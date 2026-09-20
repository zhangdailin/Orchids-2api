package grok

import (
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"time"

	"orchids-api/internal/store"
)

// Quality guard.
//
// Ported from chenyme/grok2api (application/gateway/quality_retry.go). A
// degraded upstream answers 200 with a response that looks fine but carries no
// reasoning: the visible text arrives as one late dump, or the reasoning is a
// cipher-only stub with zero reasoning tokens while the answer streams out.
// Those responses are indistinguishable from a healthy one by status alone, so
// the account keeps serving them and every caller keeps receiving them.
//
// This gateway streams directly to the client and therefore cannot retract a
// dump that has already been written. What it can do — and what this file does
// — is stop the credential from producing more of them: the first degraded turn
// parks the credential for a cooldown, a second one disables it. A non-streaming
// turn, which is buffered anyway, is withheld and retried on another account.
const (
	// qualityCooldown is how long a credential stops serving after a degraded
	// response.
	qualityCooldown = 12 * time.Hour
	// qualityDisableThreshold is the number of degraded turns that disables the
	// credential outright.
	qualityDisableThreshold = 2
	// qualityBurstVisible and qualityBurstReasoning describe the "late dump"
	// shape: a short visible answer billed with a large reasoning count.
	qualityBurstVisible   = int64(32)
	qualityBurstReasoning = int64(80)
	// qualityBurstFlushMS is the window in which a dump's visible text arrives
	// after the reasoning bill.
	qualityBurstFlushMS = int64(2000)
	// qualityMinVisible is the smallest visible answer that may be judged at all.
	qualityMinVisible = int64(8)
)

// qualitySignals is what one turn observed about the upstream response.
type qualitySignals struct {
	// ExpectReasoning is true when the request asked for reasoning (an effort
	// other than none, or an active reasoning replay).
	ExpectReasoning bool
	// SawReasoning is true when any reasoning reached the caller: a summary, a
	// reasoning text delta, or a decrypted reasoning item.
	SawReasoning bool
	// VisibleChars and ReasoningChars are what actually streamed.
	VisibleChars   int64
	ReasoningChars int64
	// EncryptedChars is the length of opaque reasoning (encrypted_content).
	EncryptedChars int64
	// ReasoningTokens is the billed reasoning count when the upstream reported
	// one.
	ReasoningTokens int64
	// FirstVisibleMS is when the first visible text arrived, measured from the
	// start of the turn. Negative means it never arrived.
	FirstVisibleMS int64
	// Terminal is true when the response reached a terminal event.
	Terminal bool
	// ToolCalls is the number of tool calls produced.
	ToolCalls int
}

// qualityDegraded reports whether this response is a degraded one: the request
// asked for reasoning, the turn produced an answer, and no reasoning arrived.
//
// A turn that only called tools is not judged: the model is allowed to act
// without narrating its reasoning, and a false positive here disables a working
// credential.
func qualityDegraded(sig qualitySignals) bool {
	if !sig.ExpectReasoning || sig.SawReasoning || !sig.Terminal {
		return false
	}
	if sig.ToolCalls > 0 {
		return false
	}
	if sig.VisibleChars < qualityMinVisible && sig.ReasoningChars <= 0 {
		return false
	}
	// A large reasoning bill with almost no visible output that arrived in one
	// late burst is the flagship dump shape.
	if sig.ReasoningTokens >= qualityBurstReasoning &&
		sig.VisibleChars > 0 && sig.VisibleChars < qualityBurstVisible &&
		sig.FirstVisibleMS >= 0 && sig.FirstVisibleMS < qualityBurstFlushMS {
		return true
	}
	// No reasoning at all despite an explicit request.
	return sig.ReasoningChars <= 0
}

// qualityExpectsReasoning reports whether the request asked for reasoning.
func qualityExpectsReasoning(req *ChatCompletionsRequest, replay bool) bool {
	if replay {
		return true
	}
	if req == nil || req.ReasoningEffort == nil {
		return false
	}
	return !strings.EqualFold(strings.TrimSpace(*req.ReasoningEffort), "none")
}

// applyQualityGuard records a degraded turn against the credential. The first
// offence parks it, the second disables it; both are persisted.
func (h *Handler) applyQualityGuard(ctx context.Context, acc *store.Account, sig qualitySignals) bool {
	if h == nil || acc == nil || h.lb == nil || h.lb.Store == nil || !qualityDegraded(sig) {
		return false
	}
	acc.QualityFailures++
	acc.QualityCooldownUntil = time.Now().UTC().Add(qualityCooldown)
	if acc.QualityFailures >= qualityDisableThreshold {
		acc.Enabled = false
		acc.StatusMessage = "disabled after repeated degraded responses without reasoning"
		slog.Warn("grok quality guard: credential disabled after repeated degraded responses",
			"account_id", acc.ID, "failures", acc.QualityFailures)
	} else {
		acc.StatusMessage = "parked: upstream returned no reasoning for a reasoning request"
		slog.Warn("grok quality guard: credential parked after a degraded response",
			"account_id", acc.ID, "failures", acc.QualityFailures)
	}
	// The verdict needs its own writer: UpdateAccount copies a fixed field list
	// that does not include the quality fields, so going through it parked the
	// credential only in memory. The Enabled change from a second offence still
	// travels through UpdateAccount.
	if err := h.lb.Store.UpdateAccountQuality(ctx, acc.ID, acc.QualityFailures, acc.QualityCooldownUntil); err != nil {
		slog.Warn("grok quality guard: failed to persist the account verdict", "account_id", acc.ID, "error", err)
	}
	if acc.ID != 0 {
		if err := h.lb.Store.UpdateAccount(ctx, acc); err != nil {
			slog.Warn("grok quality guard: failed to persist the account state", "account_id", acc.ID, "error", err)
		}
	}
	return true
}

// clearQualityGuard resets the counter after a healthy turn, so the two-strike
// rule counts consecutive offences rather than a lifetime total.
func (h *Handler) clearQualityGuard(ctx context.Context, acc *store.Account) {
	if h == nil || acc == nil || h.lb == nil || h.lb.Store == nil || acc.QualityFailures == 0 {
		return
	}
	acc.QualityFailures = 0
	acc.QualityCooldownUntil = time.Time{}
	if err := h.lb.Store.UpdateAccountQuality(ctx, acc.ID, 0, time.Time{}); err != nil {
		slog.Warn("grok quality guard: failed to clear the account verdict", "account_id", acc.ID, "error", err)
	}
}

// qualitySignalsFromResponse derives the signals from a complete non-streaming
// response body, which is what the buffered path has.
func qualitySignalsFromResponse(chat map[string]interface{}, elapsed time.Duration, expectReasoning bool) qualitySignals {
	sig := qualitySignals{ExpectReasoning: expectReasoning, Terminal: true, FirstVisibleMS: -1}
	if chat == nil {
		return sig
	}
	if usage, _ := chat["usage"].(map[string]interface{}); usage != nil {
		if details, _ := usage["completion_tokens_details"].(map[string]interface{}); details != nil {
			sig.ReasoningTokens = int64(interfaceToInt(details["reasoning_tokens"]))
		}
		if sig.ReasoningTokens == 0 {
			sig.ReasoningTokens = int64(interfaceToInt(usage["reasoning_tokens"]))
		}
	}
	choices, _ := chat["choices"].([]interface{})
	if len(choices) == 0 {
		return sig
	}
	choice, _ := choices[0].(map[string]interface{})
	message, _ := choice["message"].(map[string]interface{})
	if message == nil {
		return sig
	}
	sig.VisibleChars = int64(len(streamString(message["content"])))
	if reasoning := streamString(firstDefined(message["reasoning_content"], message["reasoning"])); reasoning != "" {
		sig.SawReasoning = true
		sig.ReasoningChars = int64(len(reasoning))
	}
	if encrypted := streamString(message["reasoning_encrypted_content"]); encrypted != "" {
		sig.EncryptedChars = int64(len(encrypted))
	}
	if items := interfaceSlice(message["x_grok_reasoning"]); len(items) > 0 {
		sig.SawReasoning = true
		sig.ReasoningChars++
	}
	sig.ToolCalls = len(interfaceSlice(message["tool_calls"]))
	if sig.VisibleChars > 0 {
		sig.FirstVisibleMS = elapsed.Milliseconds()
	}
	return sig
}

// qualitySignalsFromSSE derives the signals from a buffered SSE transcript.
func qualitySignalsFromSSE(raw []byte, elapsed time.Duration, expectReasoning bool) qualitySignals {
	sig := qualitySignals{ExpectReasoning: expectReasoning, FirstVisibleMS: -1}
	_ = readResponseSSE(strings.NewReader(string(raw)), func(_ string, data string) error {
		if data == "[DONE]" {
			sig.Terminal = true
			return nil
		}
		var event map[string]interface{}
		if json.Unmarshal([]byte(data), &event) != nil {
			return nil
		}
		switch interfaceString(event["type"]) {
		case "response.completed", "response.done", "response.failed", "error":
			sig.Terminal = true
		}
		if delta := streamString(event["delta"]); delta != "" {
			switch interfaceString(event["type"]) {
			case "response.output_text.delta":
				if sig.FirstVisibleMS < 0 {
					sig.FirstVisibleMS = elapsed.Milliseconds()
				}
				sig.VisibleChars += int64(len(delta))
			case "response.reasoning_summary_text.delta", "response.reasoning_text.delta":
				sig.SawReasoning = true
				sig.ReasoningChars += int64(len(delta))
			}
		}
		item, _ := event["item"].(map[string]interface{})
		if item != nil {
			if encrypted := streamString(item["encrypted_content"]); encrypted != "" {
				sig.EncryptedChars = int64(len(encrypted))
			}
			if interfaceString(item["type"]) == "reasoning" {
				sig.SawReasoning = true
			}
			if interfaceString(item["type"]) == "function_call" || interfaceString(item["type"]) == "custom_tool_call" {
				sig.ToolCalls++
			}
		}
		if usage := consoleUsage(event); len(usage) > 0 {
			if details, _ := usage["completion_tokens_details"].(map[string]interface{}); details != nil {
				sig.ReasoningTokens = int64(interfaceToInt(details["reasoning_tokens"]))
			}
		}
		return nil
	})
	return sig
}

// applyConsoleQualityGuard feeds one finished turn into the quality policy.
//
// A healthy turn clears the counter; a degraded turn parks the credential (and
// disables it on the second offence). The response itself is left alone: this
// gateway has already streamed it, and rewriting a delivered answer would be
// worse than cooling the credential that produced it.
func (h *Handler) applyConsoleQualityGuard(ctx context.Context, acc *store.Account, outcome chatOutcome) {
	if h == nil || acc == nil {
		return
	}
	if outcome.Err != nil || !outcome.Quality.Terminal {
		return
	}
	if qualityDegraded(outcome.Quality) {
		h.applyQualityGuard(ctx, acc, outcome.Quality)
		return
	}
	h.clearQualityGuard(ctx, acc)
}
