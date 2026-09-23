package api

// Account refresh: how one channel's credential and allowance are re-proved.
//
// The channels agree on the contract — return the status the scheduler should
// record, the HTTP status the caller should see, and the failure — but disagree
// entirely on how to get there, so each keeps its own function. This was five
// if-blocks inside refreshAccountState; the dispatch is now a lookup.

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	apperrors "orchids-api/internal/errors"
	"orchids-api/internal/puter"
	"orchids-api/internal/store"
	"orchids-api/internal/warp"
)

// accountRefreshers dispatches a refresh by account type. A channel that is
// absent is an unsupported account type.
var accountRefreshers = map[string]func(*API, context.Context, *store.Account) (string, int, error){
	"warp":      refreshWarpAccountState,
	"grok":      refreshGrokAccountState,
	"puter":     refreshPuterAccountState,
	"qoder":     refreshQoderAccountState,
	"workbuddy": refreshWorkBuddyAccountState,
	"cline":     refreshClineAccountState,
}

// refreshWarpAccountState re-proves a Warp session and re-reads the quota it
// unlocks. Entitlement is judged separately: a 403 without a billable probe is
// not proof that the account lost AI access.
func refreshWarpAccountState(a *API, ctx context.Context, acc *store.Account) (string, int, error) {
	cfg := a.config.Load()
	warpClient := warp.NewFromAccount(acc, cfg)
	result, err := warpClient.RefreshAccountState(ctx, acc, true)
	if err != nil {
		httpStatus := http.StatusBadRequest
		if code := warp.HTTPStatusCode(err); code >= 400 {
			httpStatus = code
		}
		accountStatus := ""
		if httpStatus == http.StatusUnauthorized || httpStatus == http.StatusForbidden || httpStatus == http.StatusTooManyRequests {
			accountStatus = strconv.Itoa(httpStatus)
		}
		return accountStatus, httpStatus, fmt.Errorf("failed to refresh warp account: %w", err)
	}
	if result.QuotaError != nil {
		slog.Warn("Warp quota sync failed after refresh; keeping account available", "account_id", acc.ID, "error", result.QuotaError)
	}
	modelDiscoveryConfirmed := false
	var modelDiscoveryErr error
	if a.store != nil && acc.ID != 0 {
		modelCtx, modelCancel := context.WithTimeout(ctx, 15*time.Second)
		features, source, modelErr := warpClient.FetchDiscoveredFeatureModelChoices(modelCtx)
		modelCancel()
		choices := warp.AgentModeModelChoices(features)
		featureConfig := warp.AccountFeatureConfigFromChoices(features)
		if modelErr == nil && len(choices) > 0 {
			modelDiscoveryConfirmed = true
			discovery := warp.AccountModelDiscovery{
				AccountID:     acc.ID,
				Source:        source,
				Choices:       choices,
				FeatureConfig: featureConfig,
			}
			if err := warp.UpsertAccountModelDiscoveries(ctx, a.store, discovery); err != nil {
				slog.Warn("Warp model choices sync failed after refresh", "account_id", acc.ID, "source", source, "error", err)
			}
		} else if modelErr != nil {
			modelDiscoveryErr = modelErr
			slog.Warn("Warp model choices fetch failed after refresh", "account_id", acc.ID, "error", modelErr)
		} else {
			modelDiscoveryErr = fmt.Errorf("warp model discovery returned no enabled models")
		}
	}
	if strings.TrimSpace(acc.StatusCode) == "403" {
		if !modelDiscoveryConfirmed {
			if modelDiscoveryErr == nil {
				modelDiscoveryErr = fmt.Errorf("warp model discovery unavailable")
			}
			return "403", http.StatusForbidden, fmt.Errorf("failed to verify warp AI entitlement without a billable probe: %w", modelDiscoveryErr)
		}
	}
	return "", 0, nil
}

// refreshGrokAccountState re-verifies a Grok credential.
func refreshGrokAccountState(a *API, ctx context.Context, acc *store.Account) (string, int, error) {
	if verifyErr := verifyGrokAccount(ctx, acc, a.config.Load(), a.store); verifyErr != nil {
		message := strings.ToLower(verifyErr.Error())
		if strings.Contains(message, "missing sso token") || strings.Contains(message, "missing oauth token") {
			return "", http.StatusBadRequest, fmt.Errorf("failed to verify grok account: %w", verifyErr)
		}
		status := apperrors.ClassifyAccountStatus(verifyErr.Error())
		return status, httpStatusFromAccountStatus(status), fmt.Errorf("failed to verify grok account: %w", verifyErr)
	}
	return "", 0, nil
}

// refreshPuterAccountState re-reads the Puter monthly allowance, which is the
// only thing that proves the credential still works.
func refreshPuterAccountState(a *API, ctx context.Context, acc *store.Account) (string, int, error) {
	if puter.ResolveAuthToken(acc) == "" {
		return "", http.StatusBadRequest, fmt.Errorf("failed to verify puter account: missing auth token")
	}
	usage, usageErr := puterFetchMonthlyUsage(ctx, acc, a.config.Load())
	if usageErr == nil {
		if puter.ApplyMonthlyUsage(acc, usage) {
			return store.AccountStatusPuterQuotaExhausted, 0, nil
		}
		return "", 0, nil
	}
	usageStatus := apperrors.ClassifyAccountStatus(usageErr.Error())
	httpStatus := http.StatusBadGateway
	if usageStatus != "" {
		httpStatus = httpStatusFromAccountStatus(usageStatus)
	}
	return usageStatus, httpStatus, fmt.Errorf("failed to fetch puter usage: %w", usageErr)
}

// refreshQoderAccountState re-verifies a Qoder credential through the store,
// which may also persist a rotated refresh token.
func refreshQoderAccountState(a *API, ctx context.Context, acc *store.Account) (string, int, error) {
	return verifyThroughStore("qoder", errQoderMissingCredential, func() (string, int, error) {
		return verifyQoderAccountWithStore(ctx, acc, a.config.Load(), a.store)
	})
}

// refreshWorkBuddyAccountState re-verifies a WorkBuddy credential through the
// store, which may also persist a rotated refresh token.
func refreshWorkBuddyAccountState(a *API, ctx context.Context, acc *store.Account) (string, int, error) {
	return verifyThroughStore("workbuddy", errWorkBuddyMissingCredential, func() (string, int, error) {
		return verifyWorkBuddyAccountWithStore(ctx, acc, a.config.Load(), a.store)
	})
}

// refreshClineAccountState re-verifies a Cline credential through the store,
// which may also persist a rotated refresh token.
func refreshClineAccountState(a *API, ctx context.Context, acc *store.Account) (string, int, error) {
	return verifyThroughStore("cline", errClineMissingCredential, func() (string, int, error) {
		return verifyClineAccountWithStore(ctx, acc, a.config.Load(), a.store)
	})
}

// verifyThroughStore runs a channel's verification and maps its failure onto the
// account status the scheduler records.
//
// Qoder and WorkBuddy prove a credential the same way and report through the same
// store, and both have to tell three failures apart: a missing credential is an
// operator error (400), an upstream verdict the classifier recognises carries its
// own status, and anything else keeps whatever the verify path reported. Only the
// channel's name differs, so the mapping is written once here.
func verifyThroughStore(channel string, missingCredential error, verify func() (string, int, error)) (string, int, error) {
	status, httpStatus, verifyErr := verify()
	if verifyErr == nil {
		return status, httpStatus, nil
	}
	failure := fmt.Errorf("failed to verify %s account: %w", channel, verifyErr)
	if errors.Is(verifyErr, missingCredential) {
		return "", http.StatusBadRequest, failure
	}
	if classified := apperrors.ClassifyAccountStatus(verifyErr.Error()); classified != "" {
		return classified, httpStatusFromAccountStatus(classified), failure
	}
	return status, httpStatus, failure
}
