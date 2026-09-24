package api

import (
	"orchids-api/internal/store"
	"testing"
	"time"
)

func TestPoolCountsLoginIndependentOfCooldown(t *testing.T) {
	now := time.Now()
	accounts := []*store.Account{
		{ID: 1, AccountType: "grok", Enabled: true, StatusCode: "401", LastAttempt: now, VerifiedAt: now, ModelCooldowns: map[string]time.Time{"model": now.Add(time.Minute)}},
		{ID: 2, AccountType: "grok", Enabled: true, StatusCode: "401", LastAttempt: now.Add(-time.Hour), VerifiedAt: now.Add(-time.Hour)},
		{ID: 3, AccountType: "grok", Enabled: true},
		{ID: 4, AccountType: "grok", Enabled: false, StatusCode: "401"},
		{ID: 5, AccountType: "grok", Enabled: true, StatusCode: "401"},
	}
	enabled, available, login, cooldowns := poolCounts(accounts, "grok", now)
	if enabled != 4 || available != 2 || login != 3 || cooldowns != 1 {
		t.Fatalf("enabled=%d available=%d login=%d cooldowns=%d", enabled, available, login, cooldowns)
	}
	accounts[0].StatusCode = ""
	_, _, login, _ = poolCounts(accounts, "grok", now)
	if login != 2 {
		t.Fatalf("recovered login count=%d", login)
	}
}
