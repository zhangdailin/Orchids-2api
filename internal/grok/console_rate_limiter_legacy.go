package grok

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"
)

var (
	consoleTeamCooldownMu    sync.Mutex
	consoleTeamCooldownUntil time.Time
)

func init() {
	// Console is excluded from the default build. This bucket only exists when
	// someone explicitly opts into the legacy_console tag.
	endpointRateLimiters["console.x.ai"] = newTokenBucket(0.9, 1)
}

func consoleRateLimitEndpoint(ctx context.Context) error {
	if err := waitConsoleTeamCooldown(ctx); err != nil {
		return err
	}
	return rateLimitEndpoint(ctx, "console.x.ai")
}

func waitConsoleTeamCooldown(ctx context.Context) error {
	for {
		consoleTeamCooldownMu.Lock()
		until := consoleTeamCooldownUntil
		consoleTeamCooldownMu.Unlock()

		now := time.Now()
		if until.IsZero() || !now.Before(until) {
			if !until.IsZero() {
				consoleTeamCooldownMu.Lock()
				if consoleTeamCooldownUntil.Equal(until) {
					consoleTeamCooldownUntil = time.Time{}
				}
				consoleTeamCooldownMu.Unlock()
			}
			return nil
		}

		wait := time.Until(until)
		slog.Debug("Rate limiter: console team cooldown", "until", until.UTC().Format(time.RFC3339), "wait", wait.String())
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			return fmt.Errorf("grok upstream status=429 body=too_many_requests console.x.ai team rate limit cooling down until %s: %w", until.UTC().Format(time.RFC3339), ctx.Err())
		case <-timer.C:
		}
	}
}

func noteConsoleRateLimitError(err error) {
	if !isSharedGrokRateLimitError(err) {
		return
	}
	delay := consoleRateLimitCooldown(err)
	if delay <= 0 {
		return
	}
	until := time.Now().Add(delay)
	consoleTeamCooldownMu.Lock()
	if until.After(consoleTeamCooldownUntil) {
		consoleTeamCooldownUntil = until
	}
	consoleTeamCooldownMu.Unlock()
	slog.Warn("Rate limiter: console team rate limit cooldown", "cooldown", delay.String(), "error", err)
}

func consoleRateLimitCooldown(err error) time.Duration {
	if err == nil {
		return 0
	}
	lower := strings.ToLower(err.Error())
	switch {
	case strings.Contains(lower, "requests per minute") || strings.Contains(lower, "rpm"):
		return 75 * time.Second
	case strings.Contains(lower, "requests per second") || strings.Contains(lower, "rps"):
		return 5 * time.Second
	default:
		return 30 * time.Second
	}
}
