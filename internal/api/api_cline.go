package api

import (
	"context"
	"errors"
	"log/slog"
	"strings"

	"orchids-api/internal/cline"
	"orchids-api/internal/config"
	"orchids-api/internal/store"
)

// errClineMissingCredential is returned when an account carries no Cline
// credential.
var errClineMissingCredential = errors.New("cline account is missing an OAuth credential")

// The Cline channel is OAuth-only: an account is created by the WorkOS device
// authorization flow, never by pasting a personal access token. The Cline
// refresh token is the durable credential and is rotated by the upstream, so it
// is kept out of the generic RefreshToken slot and is never returned by the
// account API.

// ClineCredentialKey identifies an account by its durable credential so the
// duplicate detector can reject the same account twice.
func ClineCredentialKey(acc *store.Account) string {
	creds := cline.ResolveCredentials(acc)
	for _, candidate := range []string{creds.RefreshToken, creds.AccessToken} {
		if strings.TrimSpace(candidate) != "" {
			return "cline:" + candidate
		}
	}
	return ""
}

// RedactClineOutput hides the durable refresh token. The access token stays
// visible in truncated form so an operator can tell that a credential is
// configured without either secret leaving the server.
func RedactClineOutput(acc *store.Account) *store.Account {
	if acc == nil {
		return nil
	}
	out := *acc
	out.ClineRefreshToken = ""
	// The generic slots are shared with other channels; a Cline record never
	// writes them, and clearing them here keeps a legacy row from leaking.
	out.RefreshToken = ""
	out.Token = ""
	out.SessionCookie = ""
	out.ClientCookie = ""
	return &out
}

// PreserveClineCredentialsOnEdit keeps the server-side credential when the admin
// UI submits an edit without re-entering it (secrets are redacted on read, so an
// empty field means "keep").
func PreserveClineCredentialsOnEdit(acc, existing *store.Account) {
	if acc == nil || existing == nil {
		return
	}
	if strings.TrimSpace(acc.ClineAccessToken) == "" {
		acc.ClineAccessToken = existing.ClineAccessToken
	}
	if strings.TrimSpace(acc.ClineRefreshToken) == "" {
		acc.ClineRefreshToken = existing.ClineRefreshToken
	}
	if acc.ClineExpiresAt.IsZero() {
		acc.ClineExpiresAt = existing.ClineExpiresAt
	}
	if strings.TrimSpace(acc.ClineEmail) == "" {
		acc.ClineEmail = existing.ClineEmail
	}
	if len(acc.ClineModelIDs) == 0 {
		acc.ClineModelIDs = append([]string(nil), existing.ClineModelIDs...)
	}
}

// NormalizeClineCredentials validates a Cline record's credential.
//
// The credential lives in its own fields only: the generic slots are shared with
// channels whose credentials mean something else, and writing a Cline token
// there would leak it through another channel's account list.
func NormalizeClineCredentials(acc *store.Account) bool {
	if acc == nil {
		return false
	}
	creds := cline.ResolveCredentials(acc)
	if !creds.HasCredential() {
		return false
	}
	accessToken, refreshToken, expiresAt, email := creds.Fields()
	acc.ClineAccessToken = accessToken
	if refreshToken != "" {
		acc.ClineRefreshToken = refreshToken
	}
	if !expiresAt.IsZero() {
		acc.ClineExpiresAt = expiresAt
	}
	if email != "" {
		acc.ClineEmail = email
	}
	acc.Token = ""
	acc.RefreshToken = ""
	acc.SessionCookie = ""
	acc.ClientCookie = ""
	return true
}

// verifyClineAccount checks that a Cline account is still usable and refreshes
// the material it owns.
//
// Scope is deliberately credential liveness plus the account-scoped catalog.
// The model feed is a separate read on the same host, so a failure leaves the
// snapshot untouched rather than marking the credential dead: an account whose
// feed read fails is still able to run the models it last observed.
func verifyClineAccountWithStore(ctx context.Context, acc *store.Account, cfg *config.Config, accountStore cline.AccountUpdater) (string, int, error) {
	if acc == nil {
		return "", 0, nil
	}
	creds := cline.ResolveCredentials(acc)
	if !creds.HasCredential() {
		return "", 400, errClineMissingCredential
	}

	client := cline.NewFromAccount(acc, cfg)
	defer client.Close()
	if accountStore != nil {
		client.SetAccountStore(accountStore)
	}

	// The catalog comes from the upstream recommended-models feed. A failed read
	// leaves the snapshot untouched rather than installing a compiled-in list, so
	// an unreadable catalog stays visible as "not observed yet".
	if models, catalogErr := client.FetchUpstreamModels(ctx); catalogErr != nil {
		slog.Warn("Cline catalog read failed; leaving the observed snapshot unchanged",
			"account_id", acc.ID, "error", catalogErr)
	} else if ids := cline.CatalogSnapshot(models); len(ids) > 0 {
		acc.ClineModelIDs = ids
	}

	// Renewing is the proof the credential still works: the refresh endpoint is
	// the only control-plane call that answers for a rotated token, and a
	// rejection there means the account has to be authorized again.
	if _, err := client.FetchModels(ctx); err != nil {
		return "", 502, err
	}
	return "", 0, nil
}
