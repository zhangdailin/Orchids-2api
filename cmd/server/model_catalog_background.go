package main

import (
	"context"
	"log/slog"
	"time"

	"orchids-api/internal/config"
	"orchids-api/internal/store"
)

var (
	modelCatalogInitialDelay = 30 * time.Second
	modelCatalogInterval     = 30 * time.Minute
)

// startModelCatalogRefreshLoop makes model management an upstream projection,
// rather than a table that changes only while an operator has the page open.
// Each channel refresh is still guarded by the distributed lease and the
// discovery round's completeness rules: partial/failed rounds may add positive
// observations but cannot delete last-known-good rows.
func startModelCatalogRefreshLoop(ctx context.Context, configSnapshot func() *config.Config, s *store.Store) {
	if s == nil || configSnapshot == nil {
		return
	}
	go func() {
		timer := time.NewTimer(modelCatalogInitialDelay)
		defer timer.Stop()
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		}
		refresh := func() {
			for _, channel := range []string{"Warp", "Puter", "WorkBuddy", "Qoder", "Cline", "Grok"} {
				release, acquired := acquireDistributedModelRefresh(ctx, s, channel)
				if !acquired {
					continue
				}
				result, err := syncModelsForChannelConcurrent(ctx, configSnapshot(), s, channel, defaultModelRefreshConcurrency)
				release()
				if err != nil {
					if !isNoActiveAccounts(err) && ctx.Err() == nil {
						slog.Warn("Automatic model catalog refresh failed; keeping last known state", "channel", channel, "error", err)
					}
					continue
				}
				slog.Info("Automatic model catalog reconciled", "channel", channel, "added", result.Added, "updated", result.Updated, "deleted", result.Deleted, "partial", result.Partial)
			}
		}
		refresh()
		ticker := time.NewTicker(modelCatalogInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				refresh()
			}
		}
	}()
}
