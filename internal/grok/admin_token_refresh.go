package grok

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/goccy/go-json"

	apperrors "orchids-api/internal/errors"
	"orchids-api/internal/store"
)

type adminTokenRefreshRequest struct {
	Token       string   `json:"token"`
	Tokens      []string `json:"tokens"`
	Concurrency int      `json:"concurrency"`
	Model       string   `json:"model,omitempty"`
}

func collectRefreshTokens(req adminTokenRefreshRequest) []string {
	dedup := map[string]struct{}{}
	add := func(raw string) {
		token := NormalizeSSOToken(raw)
		if token == "" {
			return
		}
		dedup[token] = struct{}{}
	}

	add(req.Token)
	for _, raw := range req.Tokens {
		add(raw)
	}

	out := make([]string, 0, len(dedup))
	for token := range dedup {
		out = append(out, token)
	}
	sort.Strings(out)
	return out
}

func collectGrokAccountsByToken(accounts []*store.Account) map[string][]*store.Account {
	result := make(map[string][]*store.Account, len(accounts))
	for token, acc := range CollectWebSSOSourcesByToken(accounts, false) {
		result[token] = []*store.Account{acc}
	}
	return result
}

func (h *Handler) resolveTokenRefreshRequest(r *http.Request) (adminTokenRefreshRequest, []string, error) {
	var req adminTokenRefreshRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil && err.Error() != "EOF" {
		return req, nil, fmt.Errorf("invalid json")
	}
	req.Concurrency = normalizeNSFWConcurrency(req.Concurrency)
	req.Model = normalizeModelID(req.Model)

	tokens := collectRefreshTokens(req)
	if len(tokens) == 0 {
		// If no explicit tokens given, load all grok accounts
		if h.lb != nil && h.lb.Store != nil {
			accounts, err := h.lb.Store.ListAccounts(r.Context())
			if err != nil {
				return req, nil, fmt.Errorf("failed to list accounts: %w", err)
			}
			for _, acc := range CollectWebSSOSourcesByToken(accounts, false) {
				tokens = append(tokens, grokAccountToken(acc))
			}
			tokens = uniqueStrings(tokens)
		}
	}
	if len(tokens) == 0 {
		return req, nil, fmt.Errorf("no tokens provided")
	}
	return req, tokens, nil
}

func updateGrokUsageAccount(acc *store.Account, info *RateLimitInfo, status string) {
	if acc == nil {
		return
	}
	ApplyQuotaInfo(acc, info)
	status = strings.TrimSpace(status)
	if status == "" {
		acc.StatusCode = ""
		acc.LastAttempt = time.Time{}
		return
	}
	acc.StatusCode = status
	acc.LastAttempt = time.Now()
}

func (h *Handler) runTokenRefreshBatch(ctx context.Context, tokens []string, model string, tokenAccounts map[string][]*store.Account, concurrency int, onItem func(string, bool)) map[string]bool {
	var mu sync.Mutex
	results := make(map[string]bool, len(tokens))
	process := func(token string) {
		success := false
		if ctx.Err() == nil {
			callCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
			defer cancel()
			info, err := h.client.VerifyToken(callCtx, token, model)
			success = err == nil
			status := ""
			if err != nil {
				status = firstNonEmpty(apperrors.ClassifyAccountStatus(err.Error()), "500")
				slog.Warn("grok token usage refresh failed", "token", maskToken(token), "error", err)
			}
			for _, acc := range tokenAccounts[token] {
				if acc == nil {
					continue
				}
				updated := *acc
				updateGrokUsageAccount(&updated, info, status)
				if err := h.lb.Store.UpdateAccount(callCtx, &updated); err != nil {
					slog.Warn("update grok usage account failed", "account_id", acc.ID, "error", err)
				}
			}
		}
		mu.Lock()
		results[token] = success
		mu.Unlock()
		if onItem != nil {
			onItem(token, success)
		}
	}
	runWorkerPool(ctx, tokens, normalizeNSFWConcurrency(concurrency), process, process)
	return results
}

func (h *Handler) HandleAdminTokensRefresh(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodPost) {
		return
	}
	if !requireGrokStore(w, h) {
		return
	}
	req, tokens, err := h.resolveTokenRefreshRequest(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	accounts, err := h.lb.Store.ListAccounts(r.Context())
	if err != nil {
		http.Error(w, "failed to list accounts", http.StatusInternalServerError)
		return
	}
	tokenAccounts := collectGrokAccountsByToken(accounts)

	results := h.runTokenRefreshBatch(r.Context(), tokens, req.Model, tokenAccounts, req.Concurrency, nil)
	out := map[string]interface{}{
		"status":  "success",
		"results": results,
	}
	writeJSON(w, out)
}

func (h *Handler) HandleAdminTokensRefreshAsync(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodPost) {
		return
	}
	if !requireGrokStore(w, h) {
		return
	}
	req, tokens, err := h.resolveTokenRefreshRequest(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	accounts, err := h.lb.Store.ListAccounts(r.Context())
	if err != nil {
		http.Error(w, "failed to list accounts", http.StatusInternalServerError)
		return
	}
	tokenAccounts := collectGrokAccountsByToken(accounts)

	ctx, cancel := context.WithCancel(context.Background())
	task := newNSFWBatchTask(len(tokens), cancel)

	go func() {
		defer scheduleDeleteNSFWBatchTask(task.ID)
		h.runTokenRefreshBatch(ctx, tokens, req.Model, tokenAccounts, req.Concurrency, func(token string, ok bool) {
			task.record(token, ok, ok)
		})
		if ctx.Err() != nil {
			task.finish("cancelled", "")
			return
		}
		task.finish("done", "")
	}()

	writeAsyncTaskStarted(w, task)
}
