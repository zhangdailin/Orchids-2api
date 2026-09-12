package api

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"time"

	"orchids-api/internal/config"
	"orchids-api/internal/store"
	"orchids-api/internal/workbuddy"
)

// errWorkBuddyMissingCredential is returned when neither a refresh token nor an
// access token was supplied.
var errWorkBuddyMissingCredential = errors.New("workbuddy account is missing credentials")

// The WorkBuddy international channel keeps its durable refresh token out of
// the generic RefreshToken slot: the admin UI must never receive it, and an
// ordinary edit that omits the token field must not wipe it.

// resolveWorkBuddyCredentials delegates to the client package so the admin API
// and the upstream client can never disagree about what a pasted credential
// means (session document, key=value pairs, JWT or opaque refresh token).
func resolveWorkBuddyCredentials(acc *store.Account) workbuddy.Credentials {
	return workbuddy.ResolveCredentials(acc)
}

// NormalizeWorkBuddyCredentials stores a newly submitted credential in the
// WorkBuddy fields. The identity (UID / signed-in address) comes from the
// credential itself: the access-token JWT carries the Keycloak claims, so a
// pasted session document is enough to label the account.
func NormalizeWorkBuddyCredentials(acc *store.Account) bool {
	if acc == nil {
		return false
	}
	creds := resolveWorkBuddyCredentials(acc)
	if !creds.HasCredential() {
		return false
	}
	accessToken, refreshToken, uid, email, expiresAt := creds.Fields()
	acc.WorkBuddyAccessToken = accessToken
	if refreshToken != "" {
		acc.WorkBuddyRefreshToken = refreshToken
	}
	if uid != "" {
		acc.WorkBuddyUID = uid
	}
	if email != "" {
		acc.Email = email
		if strings.TrimSpace(acc.Name) == "" {
			// The desktop client's nickname is the signed-in address.
			acc.Name = email
		}
	}
	if !expiresAt.IsZero() {
		acc.WorkBuddyExpiresAt = expiresAt
	}
	// The WorkBuddy credential lives in its own fields only: the generic slots
	// are shared with channels whose credentials have different semantics, and
	// writing a JWT there would leak it through the account list.
	acc.Token = ""
	acc.RefreshToken = ""
	acc.SessionCookie = ""
	acc.ClientCookie = ""
	return true
}

// PreserveWorkBuddyCredentialsOnEdit keeps server-side credentials when the
// admin UI submits an edit without re-entering the token (secrets are redacted
// on read, so an empty field means "keep").
func PreserveWorkBuddyCredentialsOnEdit(acc, existing *store.Account) {
	if acc == nil || existing == nil {
		return
	}
	if strings.TrimSpace(acc.WorkBuddyAccessToken) == "" {
		acc.WorkBuddyAccessToken = existing.WorkBuddyAccessToken
	}
	if strings.TrimSpace(acc.WorkBuddyRefreshToken) == "" {
		acc.WorkBuddyRefreshToken = existing.WorkBuddyRefreshToken
	}
	if strings.TrimSpace(acc.WorkBuddyUID) == "" {
		acc.WorkBuddyUID = existing.WorkBuddyUID
	}
	if acc.WorkBuddyExpiresAt.IsZero() {
		acc.WorkBuddyExpiresAt = existing.WorkBuddyExpiresAt
	}
	if !isFullWorkBuddyCatalog(acc.WorkBuddyModelIDs) {
		// An edit must not erase the account-scoped catalog snapshot, and the UI
		// has no way to submit it.
		acc.WorkBuddyModelIDs = append([]string(nil), existing.WorkBuddyModelIDs...)
		acc.WorkBuddyModelsSyncedAt = existing.WorkBuddyModelsSyncedAt
	}
	if acc.WorkBuddyQuota.SyncedAt.IsZero() {
		acc.WorkBuddyQuota = existing.WorkBuddyQuota
	}
	// Provider-observed usage and health are not editable through the account
	// form; a partial PUT must not zero the credit meter the table displays.
	acc.UsageLimit = existing.UsageLimit
	acc.UsageCurrent = existing.UsageCurrent
	acc.UsageTotal = existing.UsageTotal
	if acc.QuotaResetAt.IsZero() {
		acc.QuotaResetAt = existing.QuotaResetAt
	}
	if strings.TrimSpace(acc.StatusCode) == "" {
		acc.StatusCode = existing.StatusCode
	}
	if acc.LastAttempt.IsZero() {
		acc.LastAttempt = existing.LastAttempt
	}
}

// isFullWorkBuddyCatalog reports whether a snapshot carries a plausible catalog.
// The admin API never accepts the snapshot from a client, so this is a guard
// against an accidental partial overwrite rather than a validation rule.
func isFullWorkBuddyCatalog(ids []string) bool {
	return len(ids) >= 4
}

// RedactWorkBuddyOutput hides the durable refresh token and unrelated legacy
// secrets while leaving the access token visible (the management UI shows a
// truncated form so an operator can tell whether a credential is configured).
func RedactWorkBuddyOutput(acc *store.Account) *store.Account {
	if acc == nil {
		return nil
	}
	out := *acc
	out.WorkBuddyRefreshToken = ""
	out.RefreshToken = ""
	out.SessionCookie = ""
	out.Token = ""
	return &out
}

// WorkBuddyCredentialKey identifies an account by its durable credential so the
// duplicate detector can reject the same account twice.
func WorkBuddyCredentialKey(acc *store.Account) string {
	creds := resolveWorkBuddyCredentials(acc)
	for _, candidate := range []string{creds.RefreshToken, creds.AccessToken} {
		if strings.TrimSpace(candidate) != "" {
			return "workbuddy:" + candidate
		}
	}
	return ""
}

// verifyWorkBuddyAccount proves the credential works before it is persisted and
// applies the account-scoped model catalog and credit meter on the way.
func verifyWorkBuddyAccount(ctx context.Context, acc *store.Account, cfg *config.Config) (string, int, error) {
	if acc == nil {
		return "", 0, nil
	}
	client := workbuddy.NewFromAccount(acc, cfg)
	defer client.Close()

	// Identity first: a credential added before this channel stored the claims (or
	// pasted as a raw session document) still resolves to a UID and an address, so
	// the 账号/邮箱 column fills in on the very next sync.
	creds := workbuddy.ResolveCredentials(acc)
	if uid, _, email, _, _ := creds.Fields(); uid != "" || email != "" {
		if acc.WorkBuddyUID == "" {
			acc.WorkBuddyUID = uid
		}
		if strings.TrimSpace(acc.Email) == "" {
			acc.Email = email
		}
		if strings.TrimSpace(acc.Name) == "" && email != "" {
			acc.Name = email
		}
	}
	if !creds.HasCredential() {
		return "", 400, errWorkBuddyMissingCredential
	}

	models, err := client.FetchModels(ctx)
	if err != nil {
		return "", 502, err
	}

	ids := make([]string, 0, len(models))
	for _, model := range models {
		if id := strings.TrimSpace(model.ID); id != "" {
			ids = append(ids, id)
		}
	}
	if len(ids) > 0 {
		acc.WorkBuddyModelIDs = ids
		acc.WorkBuddyModelsSyncedAt = time.Now()
	}

	// The credit meter is a separate, optional endpoint. A failure must not turn
	// an otherwise usable account into an error; it only leaves the quota
	// unavailable until the next sync.
	quotaStatus := ""
	if quota, quotaErr := client.FetchQuota(ctx); quotaErr != nil {
		slog.Warn("WorkBuddy credit meter sync failed; leaving quota unavailable",
			"account_id", acc.ID, "error", quotaErr)
	} else {
		workbuddy.ApplyQuota(acc, quota)
		if acc.UsageLimit > 0 && acc.UsageCurrent <= 0 {
			// The plan is exhausted: keep the account but mark it so the
			// scheduler backs off instead of hammering a dead allowance.
			quotaStatus = "402"
		}
	}
	return quotaStatus, 0, nil
}
