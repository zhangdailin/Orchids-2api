package grok

import (
	"bytes"
	"context"
	"fmt"
	"github.com/goccy/go-json"
	"log/slog"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"orchids-api/internal/store"
)

type adminNSFWEnableRequest struct {
	AccountIDs  []int64  `json:"account_ids"`
	Token       string   `json:"token"`
	Tokens      []string `json:"tokens"`
	Concurrency int      `json:"concurrency"`
}

type nsfwItemResult struct {
	Success     bool   `json:"success"`
	HTTPStatus  int    `json:"http_status"`
	GRPCStatus  int    `json:"grpc_status,omitempty"`
	GRPCMessage string `json:"grpc_message,omitempty"`
	Error       string `json:"error,omitempty"`
	AccountID   int64  `json:"account_id,omitempty"`
	AccountName string `json:"account_name,omitempty"`
}

type nsfwTarget struct {
	Token       string
	AccountID   int64
	AccountName string
}

type nsfwBatchTask struct {
	ID      string
	Total   int
	Created time.Time

	mu      sync.RWMutex
	done    int
	ok      int
	fail    int
	status  string
	errMsg  string
	results map[string]interface{}
	result  interface{}

	cancel   context.CancelFunc
	doneOnce sync.Once
	doneCh   chan struct{}
}

const nsfwBatchTaskTTL = 5 * time.Minute
const grokNSFWCapabilityEnabled = false

var (
	nsfwBatchTasksMu sync.Mutex
	nsfwBatchTasks   = map[string]*nsfwBatchTask{}
)

func maskToken(raw string) string {
	token := strings.TrimSpace(raw)
	if token == "" {
		return ""
	}
	if len(token) <= 20 {
		return token
	}
	return token[:8] + "..." + token[len(token)-8:]
}

func isGrokAccount(acc *store.Account) bool {
	if acc == nil {
		return false
	}
	t := strings.ToLower(strings.TrimSpace(acc.AccountType))
	mode := strings.ToLower(strings.TrimSpace(acc.AgentMode))
	return t == "grok" || mode == "grok"
}

func grokAccountToken(acc *store.Account) string {
	if acc == nil {
		return ""
	}
	raw := strings.TrimSpace(acc.ClientCookie)
	if raw == "" {
		raw = strings.TrimSpace(acc.RefreshToken)
	}
	return NormalizeSSOToken(raw)
}

func collectNSFWTargets(req adminNSFWEnableRequest, accounts []*store.Account) []nsfwTarget {
	accountByID := make(map[int64]*store.Account, len(accounts))
	for _, acc := range accounts {
		if acc == nil {
			continue
		}
		accountByID[acc.ID] = acc
	}

	dedup := map[string]nsfwTarget{}
	add := func(token string, acc *store.Account) {
		t := strings.TrimSpace(token)
		if t == "" {
			return
		}
		if _, exists := dedup[t]; exists {
			return
		}
		item := nsfwTarget{Token: t}
		if acc != nil {
			item.AccountID = acc.ID
			item.AccountName = strings.TrimSpace(acc.Name)
		}
		dedup[t] = item
	}

	// Explicit token(s) have highest priority.
	add(NormalizeSSOToken(req.Token), nil)
	for _, item := range req.Tokens {
		add(NormalizeSSOToken(item), nil)
	}

	if len(req.AccountIDs) > 0 {
		for _, id := range req.AccountIDs {
			acc := accountByID[id]
			if !isGrokAccount(acc) {
				continue
			}
			add(grokAccountToken(acc), acc)
		}
	} else if strings.TrimSpace(req.Token) == "" && len(req.Tokens) == 0 {
		// No explicit target: default to all grok accounts.
		for _, acc := range accounts {
			if !isGrokAccount(acc) {
				continue
			}
			add(grokAccountToken(acc), acc)
		}
	}

	out := make([]nsfwTarget, 0, len(dedup))
	for _, item := range dedup {
		out = append(out, item)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Token < out[j].Token })
	return out
}

func normalizeNSFWConcurrency(v int) int {
	if v <= 0 {
		return 5
	}
	if v > 20 {
		return 20
	}
	return v
}

func (h *Handler) resolveNSFWTargets(r *http.Request) (adminNSFWEnableRequest, []nsfwTarget, map[string][]*store.Account, error) {
	var req adminNSFWEnableRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil && err.Error() != "EOF" {
		return req, nil, nil, fmt.Errorf("invalid json")
	}
	req.Concurrency = normalizeNSFWConcurrency(req.Concurrency)

	accounts, err := h.lb.Store.ListAccounts(r.Context())
	if err != nil {
		return req, nil, nil, fmt.Errorf("failed to list accounts")
	}
	targets := collectNSFWTargets(req, accounts)
	if len(targets) == 0 {
		return req, nil, nil, fmt.Errorf("no tokens available")
	}
	return req, targets, collectGrokAccountsByToken(accounts), nil
}

func (h *Handler) runNSFWEnableBatch(
	ctx context.Context,
	targets []nsfwTarget,
	tokenAccounts map[string][]*store.Account,
	concurrency int,
	onItem func(masked string, res nsfwItemResult),
) (int, map[string]nsfwItemResult) {
	concurrency = normalizeNSFWConcurrency(concurrency)

	var (
		mu      sync.Mutex
		okCount int
		results = make(map[string]nsfwItemResult, len(targets))
	)
	sem := make(chan struct{}, concurrency)
	wg := sync.WaitGroup{}

	for _, item := range targets {
		target := item
		wg.Add(1)
		go func() {
			defer wg.Done()
			select {
			case sem <- struct{}{}:
			case <-ctx.Done():
				res := nsfwItemResult{
					Success:     false,
					HTTPStatus:  0,
					GRPCStatus:  -1,
					AccountID:   target.AccountID,
					AccountName: target.AccountName,
					Error:       ctx.Err().Error(),
				}
				key := maskToken(target.Token)
				if key == "" {
					key = "unknown"
				}
				mu.Lock()
				results[key] = res
				mu.Unlock()
				if onItem != nil {
					onItem(key, res)
				}
				return
			}
			defer func() { <-sem }()

			callCtx, cancel := context.WithTimeout(ctx, 90*time.Second)
			defer cancel()

			outcome := h.client.EnableNSFWDetailed(callCtx, target.Token)
			res := nsfwItemResult{
				Success:     outcome.Success,
				HTTPStatus:  outcome.HTTPStatus,
				GRPCStatus:  outcome.GRPCStatus,
				GRPCMessage: outcome.GRPCMessage,
				Error:       strings.TrimSpace(outcome.Error),
				AccountID:   target.AccountID,
				AccountName: target.AccountName,
			}
			if !res.Success && res.Error == "" {
				res.Error = "enable nsfw failed"
			}
			if res.Success {
				if accounts := tokenAccounts[target.Token]; len(accounts) > 0 {
					for _, acc := range accounts {
						if acc == nil {
							continue
						}
						if acc.NSFWEnabled {
							continue
						}
						acc.NSFWEnabled = true
						if updateErr := h.lb.Store.UpdateAccount(callCtx, acc); updateErr != nil {
							slog.Warn("persist nsfw account state failed", "account_id", acc.ID, "error", updateErr)
						}
					}
				}
			}

			key := maskToken(target.Token)
			if key == "" {
				key = "unknown"
			}

			mu.Lock()
			results[key] = res
			if res.Success {
				okCount++
			}
			mu.Unlock()

			if onItem != nil {
				onItem(key, res)
			}
		}()
	}
	wg.Wait()
	return okCount, results
}

func newNSFWBatchTask(total int, cancel context.CancelFunc) *nsfwBatchTask {
	id := randomHex(16)
	if id == "" {
		id = fmt.Sprintf("%d", time.Now().UnixNano())
	}
	task := &nsfwBatchTask{
		ID:      id,
		Total:   total,
		Created: time.Now(),
		status:  "running",
		results: map[string]interface{}{},
		cancel:  cancel,
		doneCh:  make(chan struct{}),
	}
	nsfwBatchTasksMu.Lock()
	nsfwBatchTasks[id] = task
	nsfwBatchTasksMu.Unlock()
	return task
}

func getNSFWBatchTask(id string) (*nsfwBatchTask, bool) {
	nsfwBatchTasksMu.Lock()
	task, ok := nsfwBatchTasks[strings.TrimSpace(id)]
	nsfwBatchTasksMu.Unlock()
	return task, ok
}

func deleteNSFWBatchTask(id string) {
	nsfwBatchTasksMu.Lock()
	delete(nsfwBatchTasks, strings.TrimSpace(id))
	nsfwBatchTasksMu.Unlock()
}

func scheduleDeleteNSFWBatchTask(id string, ttl time.Duration) {
	go func() {
		if ttl <= 0 {
			ttl = nsfwBatchTaskTTL
		}
		time.Sleep(ttl)
		deleteNSFWBatchTask(id)
	}()
}

func (t *nsfwBatchTask) record(masked string, ok bool, payload interface{}) {
	if t == nil {
		return
	}
	if strings.TrimSpace(masked) == "" {
		masked = "unknown"
	}
	t.mu.Lock()
	t.results[masked] = payload
	t.done++
	if ok {
		t.ok++
	} else {
		t.fail++
	}
	t.mu.Unlock()
}

func (t *nsfwBatchTask) requestCancel() {
	if t == nil {
		return
	}
	t.mu.Lock()
	if t.status == "running" {
		t.status = "cancelling"
	}
	cancel := t.cancel
	t.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

func (t *nsfwBatchTask) finish(status string, errMsg string) {
	if t == nil {
		return
	}
	t.mu.Lock()
	if strings.TrimSpace(status) == "" {
		status = "done"
	}
	t.status = status
	t.errMsg = strings.TrimSpace(errMsg)
	t.mu.Unlock()
	t.doneOnce.Do(func() { close(t.doneCh) })
}

func (t *nsfwBatchTask) setResult(payload interface{}) {
	if t == nil {
		return
	}
	t.mu.Lock()
	t.result = payload
	t.mu.Unlock()
}

func (t *nsfwBatchTask) snapshot() map[string]interface{} {
	if t == nil {
		return map[string]interface{}{}
	}
	t.mu.RLock()
	results := make(map[string]interface{}, len(t.results))
	for k, v := range t.results {
		results[k] = v
	}
	out := map[string]interface{}{
		"task_id":   t.ID,
		"status":    t.status,
		"total":     t.Total,
		"processed": t.done,
		"done":      t.done,
		"ok":        t.ok,
		"fail":      t.fail,
		"summary": map[string]interface{}{
			"total": t.Total,
			"done":  t.done,
			"ok":    t.ok,
			"fail":  t.fail,
		},
		"results": results,
	}
	if t.errMsg != "" {
		out["error"] = t.errMsg
	}
	if t.result != nil {
		out["result"] = t.result
	}
	t.mu.RUnlock()
	return out
}

func parseBatchTaskPath(rawPath string) (taskID string, action string, ok bool) {
	prefixes := []string{"/api/v1/admin/batch/", "/v1/admin/batch/"}
	prefix := ""
	for _, item := range prefixes {
		if strings.HasPrefix(rawPath, item) {
			prefix = item
			break
		}
	}
	if prefix == "" {
		return "", "", false
	}
	trim := strings.Trim(strings.TrimPrefix(rawPath, prefix), "/")
	parts := strings.Split(trim, "/")
	if len(parts) != 2 {
		return "", "", false
	}
	taskID = strings.TrimSpace(parts[0])
	action = strings.ToLower(strings.TrimSpace(parts[1]))
	if taskID == "" {
		return "", "", false
	}
	if action != "stream" && action != "cancel" {
		return "", "", false
	}
	return taskID, action, true
}

func (h *Handler) HandleAdminNSFWEnable(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !grokNSFWCapabilityEnabled {
		http.Error(w, "grok nsfw capability is disabled", http.StatusGone)
		return
	}
	if h == nil || h.lb == nil || h.lb.Store == nil {
		http.Error(w, "store not configured", http.StatusServiceUnavailable)
		return
	}
	req, targets, tokenAccounts, err := h.resolveNSFWTargets(r)
	if err != nil {
		status := http.StatusBadRequest
		if strings.Contains(strings.ToLower(err.Error()), "failed to list accounts") {
			status = http.StatusInternalServerError
		}
		http.Error(w, err.Error(), status)
		return
	}

	okCount, results := h.runNSFWEnableBatch(r.Context(), targets, tokenAccounts, req.Concurrency, nil)
	out := map[string]interface{}{
		"status": "success",
		"summary": map[string]interface{}{
			"total": len(targets),
			"ok":    okCount,
			"fail":  len(targets) - okCount,
		},
		"results": results,
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(out)
}

func (h *Handler) HandleAdminNSFWEnableAsync(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !grokNSFWCapabilityEnabled {
		http.Error(w, "grok nsfw capability is disabled", http.StatusGone)
		return
	}
	if h == nil || h.lb == nil || h.lb.Store == nil {
		http.Error(w, "store not configured", http.StatusServiceUnavailable)
		return
	}
	req, targets, tokenAccounts, err := h.resolveNSFWTargets(r)
	if err != nil {
		status := http.StatusBadRequest
		if strings.Contains(strings.ToLower(err.Error()), "failed to list accounts") {
			status = http.StatusInternalServerError
		}
		http.Error(w, err.Error(), status)
		return
	}

	ctx, cancel := context.WithCancel(context.Background())
	task := newNSFWBatchTask(len(targets), cancel)

	go func() {
		defer scheduleDeleteNSFWBatchTask(task.ID, nsfwBatchTaskTTL)
		_, _ = h.runNSFWEnableBatch(ctx, targets, tokenAccounts, req.Concurrency, func(masked string, res nsfwItemResult) {
			task.record(masked, res.Success, res)
		})
		if ctx.Err() != nil {
			task.finish("cancelled", "")
			return
		}
		task.finish("done", "")
	}()

	out := map[string]interface{}{
		"status":  "success",
		"task_id": task.ID,
		"total":   len(targets),
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(out)
}

func (h *Handler) HandleAdminBatchTask(w http.ResponseWriter, r *http.Request) {
	taskID, action, ok := parseBatchTaskPath(r.URL.Path)
	if !ok {
		http.Error(w, "task not found", http.StatusNotFound)
		return
	}
	task, exists := getNSFWBatchTask(taskID)
	if !exists {
		http.Error(w, "task not found", http.StatusNotFound)
		return
	}

	switch action {
	case "cancel":
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		task.requestCancel()
		out := map[string]interface{}{
			"status": "success",
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(out)
		return
	case "stream":
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")
		flusher, _ := w.(http.Flusher)

		var lastPayload []byte
		send := func(payload map[string]interface{}) bool {
			raw := encodeJSONBytes(payload)
			if bytes.Equal(raw, lastPayload) {
				return true
			}
			lastPayload = append(lastPayload[:0], raw...)
			writeSSEBytes(w, "", raw)
			if flusher != nil {
				flusher.Flush()
			}
			return r.Context().Err() == nil
		}

		initial := task.snapshot()
		initial["type"] = "snapshot"
		if !send(initial) {
			return
		}
		ticker := time.NewTicker(1 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-r.Context().Done():
				return
			case <-task.doneCh:
				final := task.snapshot()
				switch strings.ToLower(strings.TrimSpace(fmt.Sprint(final["status"]))) {
				case "done":
					final["type"] = "done"
				case "cancelled":
					final["type"] = "cancelled"
				case "error":
					final["type"] = "error"
				default:
					final["type"] = "done"
				}
				_ = send(final)
				return
			case <-ticker.C:
				tick := task.snapshot()
				tick["type"] = "snapshot"
				_ = send(tick)
			}
		}
	default:
		http.Error(w, "task not found", http.StatusNotFound)
		return
	}
}
