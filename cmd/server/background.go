package main

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"orchids-api/internal/auth"
	"orchids-api/internal/clerk"
	"orchids-api/internal/config"
	apperrors "orchids-api/internal/errors"
	"orchids-api/internal/grok"
	"orchids-api/internal/loadbalancer"
	"orchids-api/internal/orchids"
	"orchids-api/internal/store"
	"orchids-api/internal/util"
	"orchids-api/internal/warp"
)

func preserveLatestAccountStatus(ctx context.Context, s *store.Store, acc *store.Account) {
	if s == nil || acc == nil || acc.ID == 0 {
		return
	}
	latest, err := s.GetAccount(ctx, acc.ID)
	if err != nil || latest == nil {
		return
	}

	latestStatus := strings.TrimSpace(latest.StatusCode)
	if latestStatus == "" {
		return
	}

	// Auto refresh works on a snapshot loaded at loop start. Preserve newer
	// request-path status markers so a successful token/quota sync does not
	// accidentally clear a recent blocked/cooldown state in Redis.
	if strings.TrimSpace(acc.StatusCode) == "" {
		acc.StatusCode = latestStatus
		acc.LastAttempt = latest.LastAttempt
	}
}

func startTokenRefreshLoop(ctx context.Context, cfg *config.Config, s *store.Store, lb *loadbalancer.LoadBalancer) {
	if !cfg.AutoRefreshToken {
		return
	}
	interval := time.Duration(cfg.TokenRefreshInterval) * time.Minute
	if interval <= 0 {
		interval = 30 * time.Minute
	}
	slog.Debug("Auto refresh token enabled", "interval", interval.String())

	refreshAccounts := func() {
		accounts, err := s.GetEnabledAccounts(context.Background())
		if err != nil {
			slog.Error("Auto refresh token: list accounts failed", "error", err)
			return
		}
		for _, acc := range accounts {
			if strings.TrimSpace(acc.Name) == "" {
				continue
			}
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
					warp.ApplyRequestLimitInfoToAccount(acc, limitInfo, bonuses)
					slog.Debug("Warp usage synced", "account", acc.Name, "limit", acc.UsageLimit, "used", acc.UsageCurrent, "subscription", acc.Subscription)
				}

				preserveLatestAccountStatus(context.Background(), s, acc)

				if err := s.UpdateAccount(context.Background(), acc); err != nil {
					slog.Warn("Auto refresh token: update account failed", "account", acc.Name, "type", "warp", "error", err)
				}
				continue
			}
			// Grok accounts store SSO tokens in ClientCookie and are not Clerk-backed.
			if strings.EqualFold(acc.AccountType, "grok") {
				grokClient := grok.New(cfg)
				token := grok.NormalizeSSOToken(acc.ClientCookie)
				if token == "" {
					token = grok.NormalizeSSOToken(acc.RefreshToken)
				}
				if token == "" {
					slog.Warn("Auto refresh token skipped", "account", acc.Name, "type", "grok", "error", "empty token")
					continue
				}

				verifyCtx, verifyCancel := context.WithTimeout(context.Background(), 60*time.Second)
				info, verifyErr := grokClient.VerifyToken(verifyCtx, token, strings.TrimSpace(acc.AgentMode))
				verifyCancel()
				if verifyErr != nil {
					statusCode := apperrors.ClassifyAccountStatus(verifyErr.Error())
					if statusCode == "" {
						statusCode = "500"
					}
					acc.StatusCode = statusCode
					acc.LastAttempt = time.Now()
					if err := s.UpdateAccount(context.Background(), acc); err != nil {
						slog.Warn("Auto refresh token: update account failed", "account", acc.Name, "type", "grok", "error", err)
					}
					slog.Warn("Auto refresh token failed", "account", acc.Name, "type", "grok", "status", statusCode, "error", verifyErr)
					continue
				}

				if info != nil {
					grok.ApplyQuotaInfo(acc, info)
				}
				acc.StatusCode = ""
				acc.LastAttempt = time.Time{}
				if err := s.UpdateAccount(context.Background(), acc); err != nil {
					slog.Warn("Auto refresh token: update account failed", "account", acc.Name, "type", "grok", "error", err)
				}
				continue
			}
			proxyFunc := http.ProxyFromEnvironment
			if cfg != nil {
				proxyFunc = util.ProxyFuncFromConfig(cfg)
			}
			if strings.TrimSpace(acc.ClientCookie) == "" {
				jwt := strings.TrimSpace(acc.Token)
				if jwt == "" {
					continue
				}
				if sid, sub := clerk.ParseSessionInfoFromJWT(jwt); sub != "" {
					if acc.SessionID == "" && sid != "" {
						acc.SessionID = sid
					}
					if acc.UserID == "" {
						acc.UserID = sub
					}
				}
				creditsInfo, creditsErr := orchids.FetchCreditsWithProxy(context.Background(), jwt, acc.UserID, proxyFunc)
				if creditsErr != nil {
					slog.Warn("Orchids credits sync failed (token-only)", "account", acc.Name, "error", creditsErr)
					continue
				}
				if creditsInfo != nil {
					acc.Subscription = strings.ToLower(creditsInfo.Plan)
					acc.UsageCurrent = creditsInfo.Credits
					acc.UsageLimit = orchids.PlanCreditLimit(creditsInfo.Plan)
					slog.Debug("Orchids credits synced (token-only)", "account", acc.Name, "credits", acc.UsageCurrent, "limit", acc.UsageLimit, "plan", acc.Subscription)
				}
				if err := s.UpdateAccount(context.Background(), acc); err != nil {
					slog.Warn("Auto refresh token: update account failed (token-only)", "account", acc.Name, "error", err)
				}
				continue
			}
			info, err := clerk.FetchAccountInfoWithSessionContextProxy(acc.ClientCookie, acc.SessionCookie, acc.ClientUat, acc.SessionID, proxyFunc)
			if err != nil {
				errLower := strings.ToLower(err.Error())
				if strings.Contains(errLower, "no active sessions") {
					jwt := strings.TrimSpace(acc.Token)
					if jwt == "" {
						orchidsClient := orchids.NewFromAccount(acc, cfg)
						if refreshed, jwtErr := orchidsClient.GetToken(); jwtErr == nil {
							jwt = strings.TrimSpace(refreshed)
						}
					}
					if jwt != "" {
						acc.Token = jwt
						if sid, sub := clerk.ParseSessionInfoFromJWT(jwt); sub != "" {
							if acc.SessionID == "" && sid != "" {
								acc.SessionID = sid
							}
							if acc.UserID == "" {
								acc.UserID = sub
							}
						}
						creditsInfo, creditsErr := orchids.FetchCreditsWithProxy(context.Background(), jwt, acc.UserID, proxyFunc)
						if creditsErr != nil {
							slog.Warn("Orchids credits sync failed (fallback)", "account", acc.Name, "error", creditsErr)
						} else if creditsInfo != nil {
							acc.Subscription = strings.ToLower(creditsInfo.Plan)
							acc.UsageCurrent = creditsInfo.Credits
							acc.UsageLimit = orchids.PlanCreditLimit(creditsInfo.Plan)
							slog.Debug("Orchids credits synced (fallback)", "account", acc.Name, "credits", acc.UsageCurrent, "limit", acc.UsageLimit, "plan", acc.Subscription)
						}
						if err := s.UpdateAccount(context.Background(), acc); err != nil {
							slog.Warn("Auto refresh token: update account failed (fallback)", "account", acc.Name, "error", err)
						}
						continue
					}
				}
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

func hasOrchidsModelSyncCredentials(acc *store.Account) bool {
	if acc == nil || !strings.EqualFold(acc.AccountType, "orchids") {
		return false
	}
	return strings.TrimSpace(acc.Token) != "" ||
		strings.TrimSpace(acc.ClientCookie) != "" ||
		strings.TrimSpace(acc.SessionID) != ""
}

func fetchOrchidsModelChoices(ctx context.Context, cfg *config.Config, s *store.Store) ([]orchidsPublicModelChoice, string, error) {
	probeCfg := refreshModelRequestConfig(cfg, "orchids")
	accounts, err := s.GetEnabledAccounts(ctx)
	if err == nil {
		for _, acc := range accounts {
			if !hasOrchidsModelSyncCredentials(acc) {
				continue
			}
			client := orchids.NewFromAccount(acc, probeCfg)
			upstreamModels, fetchErr := client.FetchUpstreamModels(ctx)
			client.Close()
			if fetchErr == nil && len(upstreamModels) > 0 {
				out := make([]orchidsPublicModelChoice, 0, len(upstreamModels))
				for _, m := range upstreamModels {
					id := strings.TrimSpace(m.ID)
					if id == "" {
						continue
					}
					out = append(out, orchidsPublicModelChoice{
						ID:   id,
						Name: id,
					})
				}
				if len(out) > 0 {
					return normalizeOrchidsModelChoices(out), "upstream_api", nil
				}
			}
			if fetchErr != nil {
				err = fetchErr
				break
			}
		}
	}

	proxyFunc := http.ProxyFromEnvironment
	if cfg != nil {
		proxyFunc = util.ProxyFuncFromConfig(cfg)
	}
	publicModels, fallbackErr := fetchOrchidsPublicModelChoicesWithProxy(ctx, proxyFunc)
	if fallbackErr != nil {
		if err != nil {
			return normalizeOrchidsModelChoices(publicModels), "public_page_fallback", fmt.Errorf("upstream api fetch failed: %v; fallback failed: %w", err, fallbackErr)
		}
		return normalizeOrchidsModelChoices(publicModels), "public_page_fallback", fallbackErr
	}
	return normalizeOrchidsModelChoices(publicModels), "public_page_fallback", err
}

func normalizeOrchidsModelChoices(items []orchidsPublicModelChoice) []orchidsPublicModelChoice {
	if len(items) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(items))
	out := make([]orchidsPublicModelChoice, 0, len(items))
	for _, item := range items {
		id := strings.TrimSpace(item.ID)
		if resolved, ok := orchids.ResolveOrchidsModelID(id); ok {
			id = resolved
		} else {
			id = strings.ToLower(id)
		}
		if id == "" {
			continue
		}
		if _, exists := seen[id]; exists {
			continue
		}
		seen[id] = struct{}{}
		name := strings.TrimSpace(item.Name)
		if name == "" {
			name = id
		}
		out = append(out, orchidsPublicModelChoice{
			ID:   id,
			Name: name,
		})
	}
	return out
}
