package grok

import (
	"fmt"
	"github.com/goccy/go-json"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"orchids-api/internal/store"
)

type adminTokenEntry struct {
	Token    string
	Pool     string
	Status   string
	Quota    int64
	UseCount int64
	Note     string
}

func normalizeAdminTokenStatus(raw string) string {
	status := strings.ToLower(strings.TrimSpace(raw))
	switch status {
	case "active", "cooling", "expired":
		return status
	case "invalid":
		return "expired"
	default:
		return "active"
	}
}

func accountStatusFromAdminTokenStatus(status string) string {
	switch normalizeAdminTokenStatus(status) {
	case "active":
		return ""
	case "cooling":
		return "429"
	default:
		return "401"
	}
}

func adminTokenStatusFromAccount(acc *store.Account) string {
	if acc == nil {
		return "expired"
	}
	status := strings.TrimSpace(acc.StatusCode)
	if status == "" {
		return "active"
	}
	if status == "429" {
		return "cooling"
	}
	return "expired"
}

func inferTokenPool(acc *store.Account) string {
	if acc == nil {
		return "ssoBasic"
	}
	sub := strings.ToLower(strings.TrimSpace(acc.Subscription))
	if strings.Contains(sub, "heavy") {
		return "ssoHeavy"
	}
	if strings.Contains(sub, "super") || strings.Contains(sub, "pro") {
		return "ssoSuper"
	}
	if strings.Contains(sub, "lite") {
		return "ssoLite"
	}
	if InferQuotaLimit(acc) >= superDefaultQuota {
		return "ssoSuper"
	}
	return "ssoBasic"
}

func normalizeGrokPoolName(pool string) string {
	switch strings.ToLower(strings.TrimSpace(pool)) {
	case "ssoheavy", "heavy":
		return "heavy"
	case "ssosuper", "super", "pro":
		return "super"
	case "ssolite", "lite":
		return "lite"
	case "ssobasic", "basic", "":
		return "basic"
	default:
		return strings.ToLower(strings.TrimSpace(pool))
	}
}

func normalizeGrokPoolCandidates(pools []string) []string {
	out := make([]string, 0, len(pools))
	seen := map[string]struct{}{}
	for _, raw := range pools {
		pool := normalizeGrokPoolName(raw)
		if pool == "" {
			continue
		}
		if _, ok := seen[pool]; ok {
			continue
		}
		seen[pool] = struct{}{}
		out = append(out, pool)
	}
	return out
}

func grokAccountPool(acc *store.Account) string {
	return normalizeGrokPoolName(inferTokenPool(acc))
}

func parseInt64FromAny(v interface{}) (int64, bool) {
	switch t := v.(type) {
	case int:
		return int64(t), true
	case int32:
		return int64(t), true
	case int64:
		return t, true
	case float64:
		return int64(t), true
	case float32:
		return int64(t), true
	case json.Number:
		if n, err := t.Int64(); err == nil {
			return n, true
		}
		if f, err := t.Float64(); err == nil {
			return int64(f), true
		}
		return 0, false
	case string:
		n, err := strconv.ParseInt(strings.TrimSpace(t), 10, 64)
		if err != nil {
			return 0, false
		}
		return n, true
	default:
		return 0, false
	}
}

func collectAdminTokenEntries(payload map[string]interface{}) []adminTokenEntry {
	dedup := map[string]adminTokenEntry{}

	for pool, raw := range payload {
		items, ok := raw.([]interface{})
		if !ok {
			continue
		}
		poolName := strings.TrimSpace(pool)
		if poolName == "" {
			poolName = "ssoBasic"
		}

		for _, item := range items {
			entry := adminTokenEntry{
				Pool:     poolName,
				Status:   "active",
				Quota:    -1,
				UseCount: -1,
			}

			switch v := item.(type) {
			case string:
				entry.Token = NormalizeSSOToken(v)
			case map[string]interface{}:
				rawToken, ok := v["token"].(string)
				if !ok {
					continue
				}
				entry.Token = NormalizeSSOToken(rawToken)
				if s, ok := v["status"].(string); ok {
					entry.Status = normalizeAdminTokenStatus(s)
				}
				if s, ok := v["note"].(string); ok {
					entry.Note = strings.TrimSpace(s)
				}
				if n, ok := parseInt64FromAny(v["quota"]); ok {
					entry.Quota = n
				}
				if n, ok := parseInt64FromAny(v["use_count"]); ok {
					entry.UseCount = n
				}
			default:
				continue
			}

			entry.Token = strings.TrimSpace(entry.Token)
			if entry.Token == "" {
				continue
			}
			entry.Status = normalizeAdminTokenStatus(entry.Status)
			dedup[entry.Token] = entry
		}
	}

	out := make([]adminTokenEntry, 0, len(dedup))
	for _, entry := range dedup {
		out = append(out, entry)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Token < out[j].Token })
	return out
}

func applyTokenEntryToAccount(acc *store.Account, entry adminTokenEntry) {
	if acc == nil {
		return
	}

	token := strings.TrimSpace(entry.Token)
	if token != "" {
		acc.ClientCookie = "sso=" + token
	}
	acc.RefreshToken = ""
	acc.AccountType = "grok"
	// Default to a valid Grok model for health checks.
	acc.AgentMode = "grok-4.20-0309-non-reasoning"
	acc.Enabled = true
	if acc.Weight <= 0 {
		acc.Weight = 1
	}

	note := strings.TrimSpace(entry.Note)
	if note != "" {
		acc.Name = note
	}
	if strings.TrimSpace(acc.Name) == "" {
		mask := maskToken(token)
		if mask == "" {
			mask = "unknown"
		}
		acc.Name = "grok-" + mask
	}

	switch normalizeGrokPoolName(entry.Pool) {
	case "heavy":
		acc.Subscription = "heavy"
	case "super":
		acc.Subscription = "super"
	case "lite":
		acc.Subscription = "lite"
	default:
		acc.Subscription = "basic"
	}

	if entry.Quota >= 0 {
		remaining := float64(entry.Quota)
		limit := InferQuotaLimit(acc)
		if remaining > limit {
			limit = remaining
		}
		acc.UsageLimit = limit
		acc.UsageCurrent = remaining
	} else if acc.UsageLimit <= 0 {
		acc.UsageLimit = InferQuotaLimit(acc)
	}
	if entry.UseCount >= 0 {
		acc.RequestCount = entry.UseCount
	}

	acc.StatusCode = accountStatusFromAdminTokenStatus(entry.Status)
	if acc.StatusCode == "" {
		acc.LastAttempt = time.Time{}
	} else if acc.LastAttempt.IsZero() {
		acc.LastAttempt = time.Now()
	}
}

func disableGrokAccount(acc *store.Account) {
	if acc == nil {
		return
	}
	acc.Enabled = false
	acc.StatusCode = ""
	acc.LastAttempt = time.Time{}
}

func (h *Handler) HandleAdminTokens(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		h.handleAdminTokensList(w, r)
	case http.MethodPost:
		h.handleAdminTokensUpdate(w, r)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (h *Handler) handleAdminTokensList(w http.ResponseWriter, r *http.Request) {
	if h == nil || h.lb == nil || h.lb.Store == nil {
		http.Error(w, "store not configured", http.StatusServiceUnavailable)
		return
	}

	accounts, err := h.lb.Store.ListAccounts(r.Context())
	if err != nil {
		http.Error(w, "failed to list accounts", http.StatusInternalServerError)
		return
	}

	pools := map[string][]map[string]interface{}{}
	seen := map[string]struct{}{}
	for _, acc := range accounts {
		if !isGrokAccount(acc) || acc == nil || !acc.Enabled {
			continue
		}
		token := grokAccountToken(acc)
		if token == "" {
			continue
		}
		if _, ok := seen[token]; ok {
			continue
		}
		seen[token] = struct{}{}

		pool := inferTokenPool(acc)
		remaining := acc.UsageCurrent
		if remaining < 0 {
			remaining = 0
		}
		item := map[string]interface{}{
			"token":      token,
			"status":     adminTokenStatusFromAccount(acc),
			"quota":      int64(remaining),
			"note":       strings.TrimSpace(acc.Name),
			"fail_count": 0,
			"use_count":  acc.RequestCount,
			"tags":       []string{},
		}
		pools[pool] = append(pools[pool], item)
	}

	for pool := range pools {
		sort.Slice(pools[pool], func(i, j int) bool {
			return fmt.Sprint(pools[pool][i]["token"]) < fmt.Sprint(pools[pool][j]["token"])
		})
	}

	if len(pools) == 0 {
		pools["ssoBasic"] = []map[string]interface{}{}
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(pools)
}

func (h *Handler) handleAdminTokensUpdate(w http.ResponseWriter, r *http.Request) {
	if h == nil || h.lb == nil || h.lb.Store == nil {
		http.Error(w, "store not configured", http.StatusServiceUnavailable)
		return
	}

	var payload map[string]interface{}
	if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
		http.Error(w, "invalid json", http.StatusBadRequest)
		return
	}
	entries := collectAdminTokenEntries(payload)
	desired := make(map[string]adminTokenEntry, len(entries))
	for _, entry := range entries {
		desired[entry.Token] = entry
	}

	accounts, err := h.lb.Store.ListAccounts(r.Context())
	if err != nil {
		http.Error(w, "failed to list accounts", http.StatusInternalServerError)
		return
	}
	existing := collectGrokAccountsByToken(accounts)

	for token, entry := range desired {
		if list := existing[token]; len(list) > 0 {
			acc := *list[0]
			applyTokenEntryToAccount(&acc, entry)
			if err := h.lb.Store.UpdateAccount(r.Context(), &acc); err != nil {
				http.Error(w, "failed to update token", http.StatusInternalServerError)
				return
			}
			continue
		}

		acc := &store.Account{
			Name:         strings.TrimSpace(entry.Note),
			AccountType:  "grok",
			AgentMode:    "grok",
			ClientCookie: "sso=" + token,
			Enabled:      true,
			Weight:       1,
		}
		applyTokenEntryToAccount(acc, entry)
		if err := h.lb.Store.CreateAccount(r.Context(), acc); err != nil {
			http.Error(w, "failed to create token", http.StatusInternalServerError)
			return
		}
	}

	for token, list := range existing {
		if _, ok := desired[token]; ok {
			continue
		}
		for _, src := range list {
			if src == nil || !src.Enabled {
				continue
			}
			acc := *src
			disableGrokAccount(&acc)
			if err := h.lb.Store.UpdateAccount(r.Context(), &acc); err != nil {
				http.Error(w, "failed to disable token", http.StatusInternalServerError)
				return
			}
		}
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]interface{}{
		"status":  "success",
		"message": "Token 已更新",
	})
}
