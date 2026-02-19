package main

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"orchids-api/internal/auth"
	"orchids-api/internal/clerk"
	"orchids-api/internal/config"
	"orchids-api/internal/grok"
	"orchids-api/internal/loadbalancer"
	"orchids-api/internal/orchids"
	"orchids-api/internal/store"
	"orchids-api/internal/util"
	"orchids-api/internal/warp"
)

const (
	grokModelProbeLimitPerRun = 12
	grokModelProbeInterval    = 250 * time.Millisecond
)

var grokProbeCursor uint64

func startTokenRefreshLoop(ctx context.Context, cfg *config.Config, s *store.Store, lb *loadbalancer.LoadBalancer) {
	if !cfg.AutoRefreshToken {
		return
	}
	interval := time.Duration(cfg.TokenRefreshInterval) * time.Minute
	if interval <= 0 {
		interval = 30 * time.Minute
	}
	slog.Info("Auto refresh token enabled", "interval", interval.String())

	refreshAccounts := func() {
		accounts, err := s.GetEnabledAccounts(context.Background())
		if err != nil {
			slog.Error("Auto refresh token: list accounts failed", "error", err)
			return
		}
		for _, acc := range accounts {
			if strings.EqualFold(acc.AccountType, "warp") {
				if !acc.QuotaResetAt.IsZero() && time.Now().Before(acc.QuotaResetAt) {
					continue
				}
				if strings.TrimSpace(acc.RefreshToken) == "" && strings.TrimSpace(acc.ClientCookie) == "" {
					continue
				}
				warpClient := warp.NewFromAccount(acc, cfg)
				jwt, err := warpClient.RefreshAccount(context.Background())
				if err != nil {
					retryAfter := warp.RetryAfter(err)
					httpStatus := warp.HTTPStatusCode(err)
					if httpStatus == 401 || httpStatus == 403 {
						lb.MarkAccountStatus(context.Background(), acc, fmt.Sprintf("%d", httpStatus))
					} else if retryAfter > 0 {
						acc.QuotaResetAt = time.Now().Add(retryAfter)
						if updateErr := s.UpdateAccount(context.Background(), acc); updateErr != nil {
							slog.Warn("Auto refresh token: record warp retry-after failed", "account", acc.Name, "type", "warp", "error", updateErr)
						}
					}
					slog.Warn("Auto refresh token failed", "account", acc.Name, "type", "warp", "http_status", httpStatus, "error", err)
					continue
				}
				if jwt != "" {
					acc.Token = jwt
				}
				warpClient.SyncAccountState()

				// Sync Warp usage quota via GraphQL
				limitCtx, limitCancel := context.WithTimeout(context.Background(), 15*time.Second)
				limitInfo, bonuses, limitErr := warpClient.GetRequestLimitInfo(limitCtx)
				limitCancel()
				if limitErr != nil {
					slog.Warn("Warp usage sync failed", "account", acc.Name, "error", limitErr)
				} else if limitInfo != nil {
					if limitInfo.IsUnlimited {
						acc.Subscription = "unlimited"
					} else {
						acc.Subscription = "free"
					}
					totalLimit := float64(limitInfo.RequestLimit)
					for _, bg := range bonuses {
						totalLimit += float64(bg.RequestCreditsRemaining)
					}
					usedRequests := float64(limitInfo.RequestsUsedSinceLastRefresh)
					acc.UsageLimit = totalLimit
					acc.UsageCurrent = usedRequests
					if limitInfo.NextRefreshTime != "" {
						if t, err := time.Parse(time.RFC3339, limitInfo.NextRefreshTime); err == nil {
							acc.QuotaResetAt = t
						}
					}
					slog.Debug("Warp usage synced", "account", acc.Name, "limit", acc.UsageLimit, "used", acc.UsageCurrent, "subscription", acc.Subscription)
				}

				if err := s.UpdateAccount(context.Background(), acc); err != nil {
					slog.Warn("Auto refresh token: update account failed", "account", acc.Name, "type", "warp", "error", err)
				}
				continue
			}
			// Grok accounts store SSO tokens in ClientCookie and are not Clerk-backed.
			if strings.EqualFold(acc.AccountType, "grok") {
				continue
			}
			if strings.TrimSpace(acc.ClientCookie) == "" {
				continue
			}
			proxyFunc := http.ProxyFromEnvironment
			if cfg != nil {
				proxyFunc = util.ProxyFunc(cfg.ProxyHTTP, cfg.ProxyHTTPS, cfg.ProxyUser, cfg.ProxyPass, cfg.ProxyBypass)
			}
			info, err := clerk.FetchAccountInfoWithSessionProxy(acc.ClientCookie, acc.SessionCookie, proxyFunc)
			if err != nil {
				errLower := strings.ToLower(err.Error())
				switch {
				case strings.Contains(errLower, "status code 401") || strings.Contains(errLower, "unauthorized"):
					lb.MarkAccountStatus(context.Background(), acc, "401")
				case strings.Contains(errLower, "status code 403") || strings.Contains(errLower, "forbidden"):
					lb.MarkAccountStatus(context.Background(), acc, "403")
				case strings.Contains(errLower, "no active sessions"):
					lb.MarkAccountStatus(context.Background(), acc, "401")
				}
				slog.Warn("Auto refresh token failed", "account", acc.Name, "error", err)
				continue
			}
			if info.SessionID != "" {
				acc.SessionID = info.SessionID
			}
			if info.ClientUat != "" {
				acc.ClientUat = info.ClientUat
			}
			if info.ProjectID != "" {
				acc.ProjectID = info.ProjectID
			}
			if info.UserID != "" {
				acc.UserID = info.UserID
			}
			if info.Email != "" {
				acc.Email = info.Email
			}
			if info.JWT != "" {
				acc.Token = info.JWT
			}
			if info.ClientCookie != "" {
				acc.ClientCookie = info.ClientCookie
			}

			// Sync Orchids credits via RSC Server Action
			if info.JWT != "" {
				creditsCtx, creditsCancel := context.WithTimeout(context.Background(), 15*time.Second)
				uid := info.UserID
				if strings.TrimSpace(uid) == "" {
					uid = acc.UserID
				}
				creditsInfo, creditsErr := orchids.FetchCreditsWithProxy(creditsCtx, info.JWT, uid, proxyFunc)
				creditsCancel()
				if creditsErr != nil {
					slog.Warn("Orchids credits sync failed", "account", acc.Name, "error", creditsErr)
				} else if creditsInfo != nil {
					acc.Subscription = strings.ToLower(creditsInfo.Plan)
					acc.UsageCurrent = creditsInfo.Credits
					acc.UsageLimit = orchids.PlanCreditLimit(creditsInfo.Plan)
					slog.Debug("Orchids credits synced", "account", acc.Name, "credits", acc.UsageCurrent, "limit", acc.UsageLimit, "plan", acc.Subscription)
				}
			}

			if err := s.UpdateAccount(context.Background(), acc); err != nil {
				slog.Warn("Auto refresh token: update account failed", "account", acc.Name, "error", err)
				continue
			}
		}
	}

	go func() {
		defer func() {
			if err := recover(); err != nil {
				slog.Error("Panic in token refresh loop", "error", err)
			}
		}()
		refreshAccounts()
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				refreshAccounts()
			}
		}
	}()
}

func startAuthCleanupLoop(ctx context.Context) {
	go func() {
		defer func() {
			if err := recover(); err != nil {
				slog.Error("Panic in auth cleanup loop", "error", err)
			}
		}()
		ticker := time.NewTicker(time.Hour)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				auth.CleanupExpiredSessions()
			}
		}
	}()
}

func startModelSyncLoop(ctx context.Context, cfg *config.Config, s *store.Store) {
	go func() {
		defer func() {
			if err := recover(); err != nil {
				slog.Error("Panic in upstream model sync loop", "error", err)
			}
		}()

		syncModels := func() {
			fetchCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()

			proxyFunc := http.ProxyFromEnvironment
			if cfg != nil {
				proxyFunc = util.ProxyFunc(cfg.ProxyHTTP, cfg.ProxyHTTPS, cfg.ProxyUser, cfg.ProxyPass, cfg.ProxyBypass)
			}
			publicModels, pubErr := orchids.FetchPublicModelChoicesWithProxy(fetchCtx, proxyFunc)
			if pubErr != nil {
				slog.Warn("上游模型同步: 公共模型抓取失败，使用 fallback", "error", pubErr)
			}

			if len(publicModels) == 0 {
				slog.Debug("上游模型同步: 无模型返回")
				return
			}

			added := 0
			updated := 0
			disabled := 0
			publicSet := map[string]string{}
			for _, pm := range publicModels {
				modelID := strings.TrimSpace(pm.ID)
				if modelID == "" {
					continue
				}
				name := strings.TrimSpace(pm.Name)
				if name == "" {
					name = modelID
				}
				publicSet[modelID] = name
			}
			for modelID, name := range publicSet {
				if existing, err := s.GetModelByModelID(context.Background(), modelID); err == nil && existing != nil {
					needsUpdate := false
					if !strings.EqualFold(existing.Channel, "orchids") {
						existing.Channel = "Orchids"
						needsUpdate = true
					}
					if existing.Status != store.ModelStatusAvailable {
						existing.Status = store.ModelStatusAvailable
						needsUpdate = true
					}
					if strings.TrimSpace(existing.Name) != name {
						existing.Name = name
						needsUpdate = true
					}
					if needsUpdate {
						if err := s.UpdateModel(context.Background(), existing); err != nil {
							slog.Warn("上游模型同步: 更新模型失败", "model_id", modelID, "error", err)
						} else {
							updated++
						}
					}
					continue
				}
				newModel := &store.Model{
					Channel: "Orchids",
					ModelID: modelID,
					Name:    name,
					Status:  store.ModelStatusAvailable,
				}
				if err := s.CreateModel(context.Background(), newModel); err != nil {
					slog.Warn("上游模型同步: 创建模型失败", "model_id", modelID, "error", err)
					continue
				}
				added++
				slog.Info("上游模型同步: 新增模型", "model_id", modelID, "channel", "Orchids")
			}

			if existing, err := s.ListModels(context.Background()); err == nil {
				for _, m := range existing {
					if !strings.EqualFold(strings.TrimSpace(m.Channel), "orchids") {
						continue
					}
					id := strings.TrimSpace(m.ModelID)
					if id == "" {
						continue
					}
					if _, ok := publicSet[id]; ok {
						continue
					}
					if m.Status != store.ModelStatusOffline {
						m.Status = store.ModelStatusOffline
						if err := s.UpdateModel(context.Background(), m); err != nil {
							slog.Warn("上游模型同步: 下线模型失败", "model_id", id, "error", err)
							continue
						}
						disabled++
					}
				}
			}
			if added > 0 {
				slog.Info("上游模型同步完成", "total_public", len(publicModels), "added", added, "updated", updated, "disabled", disabled)
			} else {
				slog.Debug("上游模型同步完成，无新增", "total_public", len(publicModels), "updated", updated, "disabled", disabled)
			}
		}

		syncWarpModels := func() {
			accounts, err := s.GetEnabledAccounts(context.Background())
			if err != nil {
				slog.Warn("Warp 模型同步: 获取账号失败", "error", err)
				return
			}
			var warpAcc *store.Account
			for _, acc := range accounts {
				if strings.EqualFold(acc.AccountType, "warp") && strings.TrimSpace(acc.Token) != "" {
					warpAcc = acc
					break
				}
			}
			if warpAcc == nil {
				slog.Debug("Warp 模型同步: 无可用 Warp 账号")
				return
			}

			warpClient := warp.NewFromAccount(warpAcc, cfg)
			fetchCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()

			choices, err := warpClient.GetFeatureModelChoices(fetchCtx)
			if err != nil {
				slog.Warn("Warp 模型同步: 获取失败", "error", err)
				return
			}

			seen := make(map[string]bool)
			added := 0
			categories := []*warp.FeatureModelCategory{choices.AgentMode, choices.Planning, choices.Coding, choices.CliAgent}
			for _, cat := range categories {
				if cat == nil {
					continue
				}
				for _, choice := range cat.Choices {
					modelID := strings.TrimSpace(choice.ID)
					if modelID == "" || seen[modelID] {
						continue
					}
					seen[modelID] = true
					if _, err := s.GetModelByModelID(context.Background(), modelID); err == nil {
						continue
					}
					displayName := choice.DisplayName
					if displayName == "" {
						displayName = modelID
					}
					newModel := &store.Model{
						Channel: "Warp",
						ModelID: modelID,
						Name:    displayName + " (Warp)",
						Status:  store.ModelStatusAvailable,
					}
					if err := s.CreateModel(context.Background(), newModel); err != nil {
						slog.Warn("Warp 模型同步: 创建模型失败", "model_id", modelID, "error", err)
						continue
					}
					added++
					slog.Info("Warp 模型同步: 新增模型", "model_id", modelID, "name", displayName)
				}
			}
			if added > 0 {
				slog.Info("Warp 模型同步完成", "added", added)
			} else {
				slog.Debug("Warp 模型同步完成，无新增")
			}
		}

		syncGrokModels := func() {
			accounts, err := s.GetEnabledAccounts(context.Background())
			if err != nil {
				slog.Warn("Grok 模型同步: 获取账号失败", "error", err)
				return
			}

			var token string
			for _, acc := range accounts {
				if !strings.EqualFold(acc.AccountType, "grok") {
					continue
				}
				token = grok.NormalizeSSOToken(acc.ClientCookie)
				if token == "" {
					token = grok.NormalizeSSOToken(acc.RefreshToken)
				}
				if token != "" {
					break
				}
			}
			if token == "" {
				slog.Debug("Grok 模型同步: 无可用 Grok 账号")
				return
			}

			existingModels, err := s.ListModels(context.Background())
			if err != nil {
				slog.Warn("Grok 模型同步: 获取模型列表失败", "error", err)
				return
			}

			candidateSet := map[string]struct{}{}
			for _, m := range grok.SupportedModels {
				if m.IsImage || m.IsVideo {
					continue
				}
				id := strings.TrimSpace(m.ID)
				if id != "" {
					candidateSet[id] = struct{}{}
				}
			}
			for _, probe := range buildGrokVersionProbes(existingModels) {
				if probe != "" {
					candidateSet[probe] = struct{}{}
				}
			}

			publicCandidates, fetchErr := fetchPublicGrokModelIDs(context.Background())
			if fetchErr != nil {
				slog.Warn("Grok 模型同步: 公共模型源抓取失败", "error", fetchErr)
			} else {
				for _, id := range publicCandidates {
					if id != "" {
						candidateSet[id] = struct{}{}
					}
				}
			}

			if len(candidateSet) == 0 {
				slog.Debug("Grok 模型同步: 无候选模型")
				return
			}

			candidates := make([]string, 0, len(candidateSet))
			for id := range candidateSet {
				candidates = append(candidates, id)
			}
			sort.Strings(candidates)

			pendingModelIDs := make([]string, 0, len(candidates))
			pendingSeen := map[string]struct{}{}
			pendingNames := map[string]string{}
			for _, candidate := range candidates {
				spec, ok := grok.ResolveModelOrDynamic(candidate)
				if !ok || spec.IsImage || spec.IsVideo {
					continue
				}
				modelID := strings.TrimSpace(spec.ID)
				if modelID == "" {
					continue
				}
				if _, exists := pendingSeen[modelID]; exists {
					continue
				}
				if _, err := s.GetModelByModelID(context.Background(), modelID); err == nil {
					continue
				}
				pendingSeen[modelID] = struct{}{}
				pendingNames[modelID] = strings.TrimSpace(spec.Name)
				pendingModelIDs = append(pendingModelIDs, modelID)
			}
			if len(pendingModelIDs) == 0 {
				slog.Debug("Grok 模型同步: 无需探测候选", "candidates", len(candidates))
				return
			}

			probeModelIDs, limited := limitProbeModelIDs(pendingModelIDs, grokModelProbeLimitPerRun)
			if limited {
				slog.Info("Grok 模型同步: 本轮探测限流", "pending", len(pendingModelIDs), "limit", len(probeModelIDs))
			}

			grokClient := grok.New(cfg)
			added := 0
			checked := 0
			for i, modelID := range probeModelIDs {
				if i > 0 && !sleepWithContext(ctx, grokModelProbeInterval) {
					return
				}

				verifyCtx, cancel := context.WithTimeout(ctx, 12*time.Second)
				_, verifyErr := grokClient.GetUsage(verifyCtx, token, modelID)
				cancel()
				checked++
				if verifyErr != nil {
					slog.Debug("Grok 模型同步: 候选模型校验失败", "model_id", modelID, "error", verifyErr)
					continue
				}

				name := strings.TrimSpace(pendingNames[modelID])
				if name == "" {
					name = modelID
				}
				newModel := &store.Model{
					Channel: "Grok",
					ModelID: modelID,
					Name:    name,
					Status:  store.ModelStatusAvailable,
				}
				if err := s.CreateModel(context.Background(), newModel); err != nil {
					// Handle create races gracefully.
					if _, getErr := s.GetModelByModelID(context.Background(), modelID); getErr == nil {
						continue
					}
					slog.Warn("Grok 模型同步: 创建模型失败", "model_id", modelID, "error", err)
					continue
				}
				added++
				slog.Info("Grok 模型同步: 新增模型", "model_id", modelID)
			}

			if added > 0 {
				slog.Info("Grok 模型同步完成", "candidates", len(candidates), "pending", len(pendingModelIDs), "checked", checked, "added", added)
			} else {
				slog.Debug("Grok 模型同步完成，无新增", "candidates", len(candidates), "pending", len(pendingModelIDs), "checked", checked)
			}
		}

		// Wait 10 seconds at startup for token refresh to complete
		select {
		case <-ctx.Done():
			return
		case <-time.After(10 * time.Second):
		}
		syncModels()
		syncWarpModels()
		syncGrokModels()

		ticker := time.NewTicker(30 * time.Minute)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				syncModels()
				syncWarpModels()
				syncGrokModels()
			}
		}
	}()
}

var grokModelIDPattern = regexp.MustCompile(`\bgrok-[a-z0-9][a-z0-9.-]*\b`)

func fetchPublicGrokModelIDs(ctx context.Context) ([]string, error) {
	urls := []string{
		"https://x.ai/api",
		"https://docs.x.ai/docs/models",
	}

	client := &http.Client{Timeout: 20 * time.Second}
	found := map[string]struct{}{}
	lastErr := ""

	for _, u := range urls {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
		if err != nil {
			lastErr = err.Error()
			continue
		}
		req.Header.Set("User-Agent", "orchids-model-sync/1.0")

		resp, err := client.Do(req)
		if err != nil {
			lastErr = err.Error()
			continue
		}
		body, readErr := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
		resp.Body.Close()
		if readErr != nil {
			lastErr = readErr.Error()
			continue
		}
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			lastErr = fmt.Sprintf("status %d from %s", resp.StatusCode, u)
			continue
		}

		text := string(body)
		// Unescape common JS-escaped variants: grok-4\.2 / grok\u002d4.2
		text = strings.ReplaceAll(text, `\.`, ".")
		text = strings.ReplaceAll(text, `\u002d`, "-")
		text = strings.ReplaceAll(text, `\u002D`, "-")

		for _, id := range extractGrokModelIDsFromText(text) {
			found[id] = struct{}{}
		}
	}

	if len(found) == 0 {
		if lastErr == "" {
			lastErr = "no model ids discovered"
		}
		return nil, fmt.Errorf("%s", lastErr)
	}

	out := make([]string, 0, len(found))
	for id := range found {
		out = append(out, id)
	}
	sort.Strings(out)
	return out, nil
}

func extractGrokModelIDsFromText(text string) []string {
	matches := grokModelIDPattern.FindAllString(strings.ToLower(text), -1)
	if len(matches) == 0 {
		return nil
	}
	out := map[string]struct{}{}
	for _, raw := range matches {
		candidate := strings.Trim(raw, `"'()[]{}<>,;:!?\`)
		if candidate == "" {
			continue
		}
		spec, ok := grok.ResolveModelOrDynamic(candidate)
		if !ok || spec.IsImage || spec.IsVideo {
			continue
		}
		id := strings.TrimSpace(spec.ID)
		if id == "" || !strings.HasPrefix(id, "grok-") {
			continue
		}
		if grok.IsDeprecatedModelID(id) {
			continue
		}
		out[id] = struct{}{}
	}
	ids := make([]string, 0, len(out))
	for id := range out {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

func buildGrokVersionProbes(models []*store.Model) []string {
	maxMinorByMajor := map[int]int{}
	seenMajor := map[int]struct{}{}
	maxMajor := -1
	for _, m := range models {
		if m == nil {
			continue
		}
		if !strings.EqualFold(strings.TrimSpace(m.Channel), "grok") {
			continue
		}
		major, minor, ok := parseGrokMajorMinor(m.ModelID)
		if !ok {
			continue
		}
		seenMajor[major] = struct{}{}
		if major > maxMajor {
			maxMajor = major
		}
		if cur, exists := maxMinorByMajor[major]; !exists || minor > cur {
			maxMinorByMajor[major] = minor
		}
	}
	if len(seenMajor) == 0 {
		return nil
	}

	out := map[string]struct{}{}
	for major := range seenMajor {
		nextMinor := maxMinorByMajor[major] + 1
		candidate := fmt.Sprintf("grok-%d.%d", major, nextMinor)
		if grok.IsDeprecatedModelID(candidate) {
			continue
		}
		out[candidate] = struct{}{}
	}
	if maxMajor >= 0 {
		candidate := fmt.Sprintf("grok-%d", maxMajor+1)
		if !grok.IsDeprecatedModelID(candidate) {
			out[candidate] = struct{}{}
		}
	}

	ids := make([]string, 0, len(out))
	for id := range out {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

func parseGrokMajorMinor(modelID string) (major int, minor int, ok bool) {
	id := strings.ToLower(strings.TrimSpace(modelID))
	if !strings.HasPrefix(id, "grok-") {
		return 0, 0, false
	}
	rest := strings.TrimPrefix(id, "grok-")
	base := rest
	if idx := strings.Index(base, "-"); idx >= 0 {
		base = base[:idx]
	}
	if base == "" {
		return 0, 0, false
	}

	parts := strings.SplitN(base, ".", 2)
	majorNum, err := strconv.Atoi(parts[0])
	if err != nil {
		return 0, 0, false
	}
	minorNum := 0
	if len(parts) == 2 {
		v, err := strconv.Atoi(parts[1])
		if err != nil {
			return 0, 0, false
		}
		minorNum = v
	}
	return majorNum, minorNum, true
}

func isOrchidsAccountType(accountType string) bool {
	t := strings.ToLower(strings.TrimSpace(accountType))
	return t == "" || t == "orchids"
}

func limitProbeModelIDs(modelIDs []string, max int) ([]string, bool) {
	if max <= 0 || len(modelIDs) <= max {
		return modelIDs, false
	}
	start := int(atomic.AddUint64(&grokProbeCursor, uint64(max))-uint64(max)) % len(modelIDs)
	return probeModelWindow(modelIDs, max, start), true
}

func probeModelWindow(modelIDs []string, max int, start int) []string {
	n := len(modelIDs)
	if n == 0 || max <= 0 {
		return nil
	}
	if n <= max {
		out := make([]string, n)
		copy(out, modelIDs)
		return out
	}
	if start < 0 {
		start = (start%n + n) % n
	} else {
		start = start % n
	}

	out := make([]string, 0, max)
	for i := 0; i < max; i++ {
		idx := (start + i) % n
		out = append(out, modelIDs[idx])
	}
	return out
}

func sleepWithContext(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return true
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}
