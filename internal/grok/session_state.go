package grok

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"math"
	"net/http"
	"strings"
	"time"

	"github.com/goccy/go-json"

	"orchids-api/internal/middleware"
	"orchids-api/internal/store"
)

const grokSessionStateTTL = time.Hour

type sessionAffinityEntry struct {
	AccountID int64
	ExpiresAt time.Time
}

type reasoningReplayEntry struct {
	EncryptedContent string
	ExpiresAt        time.Time
}

type grokSessionContext struct {
	Key    string
	Replay bool
	Model  string
}

type grokSessionContextKey struct{}

func sessionFromContext(ctx context.Context) grokSessionContext {
	value, _ := ctx.Value(grokSessionContextKey{}).(grokSessionContext)
	return value
}

func withGrokSession(ctx context.Context, session grokSessionContext) context.Context {
	if strings.TrimSpace(session.Key) == "" {
		return ctx
	}
	return context.WithValue(ctx, grokSessionContextKey{}, session)
}

// prepareGrokSession produces a stable, tenant- and model-isolated upstream
// identity. Explicit client identities permit encrypted reasoning replay;
// message-prefix fallback identities are affinity-only.
func prepareGrokSession(r *http.Request, model, explicit string, messages []ChatMessage) grokSessionContext {
	seed := strings.TrimSpace(explicit)
	if seed == "" && r != nil {
		for _, header := range []string{"x-grok-session-id", "x-grok-conv-id", "x-session-id", "session-id"} {
			if seed = strings.TrimSpace(r.Header.Get(header)); seed != "" {
				break
			}
		}
	}
	explicitSession := seed != ""
	if seed == "" {
		var system, firstUser string
		for _, message := range messages {
			role := strings.ToLower(strings.TrimSpace(message.Role))
			text := strings.TrimSpace(chatMessageContentText(message.Content))
			if text == "" {
				continue
			}
			if (role == "system" || role == "developer") && system == "" {
				system = truncateSessionAnchor(text, 100)
			}
			if role == "user" && firstUser == "" {
				firstUser = truncateSessionAnchor(text, 200)
			}
		}
		if firstUser == "" {
			return grokSessionContext{}
		}
		seed = system + "\x00" + firstUser
	}
	owner := "anonymous"
	if r != nil {
		if value := strings.TrimSpace(middleware.APIKeyFingerprint(r.Context())); value != "" {
			owner = value
		}
	}
	source := fmt.Sprintf("orchids:grok-session:v1:%s:%s:%s", owner, normalizeModelID(model), seed)
	digest := sha256.Sum256([]byte(source))
	return grokSessionContext{Key: hex.EncodeToString(digest[:]), Replay: explicitSession, Model: normalizeModelID(model)}
}

func truncateSessionAnchor(value string, maxRunes int) string {
	runes := []rune(strings.TrimSpace(value))
	if len(runes) > maxRunes {
		runes = runes[:maxRunes]
	}
	return string(runes)
}

func affinityMapKey(session grokSessionContext, provider string) string {
	return strings.ToLower(strings.TrimSpace(provider)) + "\x00" + session.Model + "\x00" + session.Key
}

func (h *Handler) affinityAccount(ctx context.Context, provider string) int64 {
	session := sessionFromContext(ctx)
	if h == nil || session.Key == "" {
		return 0
	}
	key := affinityMapKey(session, provider)
	h.sessionMu.Lock()
	entry, ok := h.affinity[key]
	if ok && time.Now().Before(entry.ExpiresAt) {
		h.sessionMu.Unlock()
		return entry.AccountID
	}
	if ok {
		delete(h.affinity, key)
	}
	h.sessionMu.Unlock()
	if h.lb == nil || h.lb.Store == nil {
		return 0
	}
	persisted, err := h.lb.Store.GetSessionAffinity(ctx, provider, session.Model, session.Key)
	if err != nil || persisted == nil || persisted.AccountID == 0 {
		return 0
	}
	h.sessionMu.Lock()
	h.affinity[key] = sessionAffinityEntry{AccountID: persisted.AccountID, ExpiresAt: persisted.ExpiresAt}
	h.sessionMu.Unlock()
	return persisted.AccountID
}

func (h *Handler) bindAffinity(ctx context.Context, provider string, accountID int64) {
	session := sessionFromContext(ctx)
	if h == nil || session.Key == "" || accountID == 0 {
		return
	}
	h.sessionMu.Lock()
	h.affinity[affinityMapKey(session, provider)] = sessionAffinityEntry{AccountID: accountID, ExpiresAt: time.Now().Add(grokSessionStateTTL)}
	h.sessionMu.Unlock()
	if h.lb != nil && h.lb.Store != nil {
		_ = h.lb.Store.SaveSessionAffinity(ctx, &store.StoredSessionAffinity{
			Provider: provider, Model: session.Model, SessionKey: session.Key, AccountID: accountID,
		}, grokSessionStateTTL)
	}
}

func replayMapKey(model, key string) string {
	return normalizeModelID(model) + "\x00" + strings.TrimSpace(key)
}

func (h *Handler) loadReasoningReplay(model, key string) string {
	if h == nil || strings.TrimSpace(key) == "" {
		return ""
	}
	h.sessionMu.Lock()
	defer h.sessionMu.Unlock()
	mapKey := replayMapKey(model, key)
	entry, ok := h.replay[mapKey]
	if ok && time.Now().Before(entry.ExpiresAt) {
		return entry.EncryptedContent
	}
	if ok {
		delete(h.replay, mapKey)
	}
	if h.lb == nil || h.lb.Store == nil {
		return ""
	}
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	persisted, err := h.lb.Store.GetReasoningReplay(ctx, model, key)
	if err != nil || persisted == nil || !validReplayCipher(persisted.EncryptedContent) {
		return ""
	}
	if latest, ok := h.replay[mapKey]; ok && time.Now().Before(latest.ExpiresAt) {
		return latest.EncryptedContent
	}
	if h.replay == nil {
		h.replay = map[string]reasoningReplayEntry{}
	}
	h.replay[mapKey] = reasoningReplayEntry{EncryptedContent: persisted.EncryptedContent, ExpiresAt: persisted.ExpiresAt}
	return persisted.EncryptedContent
}

func (h *Handler) storeReasoningReplay(model, key, encrypted string) {
	if h == nil || strings.TrimSpace(key) == "" || !validReplayCipher(encrypted) {
		return
	}
	h.sessionMu.Lock()
	defer h.sessionMu.Unlock()
	if h.replay == nil {
		h.replay = map[string]reasoningReplayEntry{}
	}
	h.replay[replayMapKey(model, key)] = reasoningReplayEntry{EncryptedContent: encrypted, ExpiresAt: time.Now().Add(grokSessionStateTTL)}
	// Serialize persistence with invalidation. A detached save must not resurrect
	// a rejected ciphertext after a later request has already cleared it.
	if h.lb != nil && h.lb.Store != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
		defer cancel()
		_ = h.lb.Store.SaveReasoningReplay(ctx, &store.StoredReasoningReplay{Model: model, SessionKey: key, EncryptedContent: encrypted}, grokSessionStateTTL)
	}
}

func validReplayCipher(value string) bool {
	if value == "" || value != strings.TrimSpace(value) || len(value) > 8<<20 || strings.HasPrefix(value, "gAAAA") || strings.Contains(value, "=") {
		return false
	}
	decoded, err := base64.RawStdEncoding.DecodeString(value)
	if err != nil || len(decoded) < 50 {
		return false
	}
	var counts [256]int
	for _, b := range decoded {
		counts[b]++
	}
	entropy := 0.0
	for _, n := range counts {
		if n > 0 {
			p := float64(n) / float64(len(decoded))
			entropy -= p * math.Log2(p)
		}
	}
	return entropy/math.Log2(float64(min(len(decoded), 256))) >= 0.85
}

func (h *Handler) applyNativeReasoningReplay(model, key string, payload map[string]interface{}) {
	if h == nil || payload == nil || strings.TrimSpace(parseLooseStringAny(payload["previous_response_id"])) != "" {
		return
	}
	encrypted := h.loadReasoningReplay(model, key)
	if encrypted == "" {
		return
	}
	input, ok := payload["input"].([]interface{})
	if !ok {
		if text, textOK := payload["input"].(string); textOK && strings.TrimSpace(text) != "" {
			input = []interface{}{map[string]interface{}{
				"type": "message", "role": "user", "content": []interface{}{map[string]interface{}{"type": "input_text", "text": text}},
			}}
		} else {
			return
		}
	}
	if len(input) == 0 || nativeInputHasEncryptedReasoning(input) {
		return
	}
	replay := map[string]interface{}{"type": "reasoning", "summary": []interface{}{}, "encrypted_content": encrypted}
	insertAt := len(input)
	for index := len(input) - 1; index >= 0; index-- {
		item, _ := input[index].(map[string]interface{})
		if item != nil && strings.EqualFold(strings.TrimSpace(fmt.Sprint(item["role"])), "user") {
			insertAt = index
			break
		}
	}
	next := make([]interface{}, 0, len(input)+1)
	next = append(next, input[:insertAt]...)
	next = append(next, replay)
	next = append(next, input[insertAt:]...)
	payload["input"] = next
	includes := interfaceSlice(payload["include"])
	found := false
	for _, value := range includes {
		if strings.EqualFold(strings.TrimSpace(fmt.Sprint(value)), "reasoning.encrypted_content") {
			found = true
		}
	}
	if !found {
		includes = append(includes, "reasoning.encrypted_content")
		payload["include"] = includes
	}
}

func (h *Handler) clearReasoningReplay(ctx context.Context, model, key string) {
	if h == nil || strings.TrimSpace(key) == "" {
		return
	}
	h.sessionMu.Lock()
	defer h.sessionMu.Unlock()
	delete(h.replay, replayMapKey(model, key))
	if h.lb != nil && h.lb.Store != nil {
		_ = h.lb.Store.DeleteReasoningReplay(ctx, model, key)
	}
}

func stripInjectedReasoningReplay(payload map[string]interface{}) bool {
	input, ok := payload["input"].([]interface{})
	if !ok {
		return false
	}
	changed := false
	next := make([]interface{}, 0, len(input))
	for _, raw := range input {
		item, _ := raw.(map[string]interface{})
		if item != nil && strings.EqualFold(interfaceString(item["type"]), "compaction") {
			next = append(next, raw)
			continue
		}
		if item != nil && strings.EqualFold(interfaceString(item["type"]), "reasoning") && interfaceString(item["encrypted_content"]) != "" {
			// A reasoning item without its proof is not portable. Preserve its
			// readable summary as an ordinary assistant message instead.
			parts := []interface{}{}
			for _, summary := range interfaceMaps(item["summary"]) {
				if value := interfaceString(summary["text"]); value != "" {
					parts = append(parts, map[string]interface{}{"type": "output_text", "text": value})
				}
			}
			changed = true
			if len(parts) > 0 {
				next = append(next, map[string]interface{}{"type": "message", "role": "assistant", "content": parts})
			}
			continue
		}
		next = append(next, raw)
	}
	if changed {
		payload["input"] = next
	}
	return changed
}

func isReasoningReplayDecodeError(err error) bool {
	if err == nil || parseUpstreamStatus(err) != http.StatusBadRequest {
		return false
	}
	text := strings.ToLower(err.Error())
	return strings.Contains(text, "invalid_encrypted_content") ||
		strings.Contains(text, "could not decrypt the provided encrypted_content") ||
		strings.Contains(text, "could not decode the compaction blob")
}

func payloadHasCompactionInput(payload map[string]interface{}) bool {
	input, ok := payload["input"].([]interface{})
	if !ok {
		return false
	}
	for _, raw := range input {
		if item, ok := raw.(map[string]interface{}); ok && strings.EqualFold(interfaceString(item["type"]), "compaction") {
			return true
		}
	}
	return false
}

func nativeInputHasEncryptedReasoning(input []interface{}) bool {
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

func encryptedReasoningFromResponse(raw []byte) string {
	var response map[string]interface{}
	if json.Unmarshal(raw, &response) == nil {
		return responseEncryptedReasoning(response)
	}
	latest := ""
	_ = readResponseSSE(strings.NewReader(string(raw)), func(_ string, data string) error {
		var event map[string]interface{}
		if json.Unmarshal([]byte(data), &event) == nil {
			if encrypted := responseEncryptedReasoning(event); encrypted != "" {
				latest = encrypted
			}
		}
		return nil
	})
	return latest
}

// Only protocol reasoning items count; arbitrary nested tool arguments or
// metadata must not become the next request's encrypted reasoning state.
func responseEncryptedReasoning(event map[string]interface{}) string {
	response, _ := event["response"].(map[string]interface{})
	if response == nil {
		response = event
	}
	if encrypted := consoleExtractEncryptedReasoning(response); encrypted != "" {
		return encrypted
	}
	item, _ := event["item"].(map[string]interface{})
	if item == nil {
		item = event
	}
	if interfaceString(item["type"]) == "reasoning" {
		return interfaceString(item["encrypted_content"])
	}
	return ""
}
