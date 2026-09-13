package api

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"time"

	"orchids-api/internal/config"
	"orchids-api/internal/qoder"
	"orchids-api/internal/store"
)

// errQoderMissingCredential is returned when an account carries no device
// credential.
var errQoderMissingCredential = errors.New("qoder account is missing an OAuth credential")

// The Qoder channel is OAuth-only: an account is created by the device
// authorization flow, never by pasting a personal access token. The device
// refresh token is the durable credential and is rotated by the upstream, so it
// is kept out of the generic RefreshToken slot and is never returned by the
// account API.

// QoderCredentialKey identifies an account by its durable credential so the
// duplicate detector can reject the same account twice.
func QoderCredentialKey(acc *store.Account) string {
	creds := qoder.ResolveCredentials(acc)
	for _, candidate := range []string{creds.RefreshToken, creds.AccessToken} {
		if strings.TrimSpace(candidate) != "" {
			return "qoder:" + candidate
		}
	}
	return ""
}

// RedactQoderOutput hides the durable refresh token and the derived runtime
// material. The access token stays visible in truncated form so an operator can
// tell that a credential is configured without either secret leaving the
// server.
func RedactQoderOutput(acc *store.Account) *store.Account {
	if acc == nil {
		return nil
	}
	out := *acc
	out.QoderRefreshToken = ""
	out.QoderRuntimeInfo = ""
	out.QoderRuntimeKey = ""
	out.QoderJobToken = ""
	// The generic slots are shared with other channels; a Qoder record never
	// writes them, and clearing them here keeps a legacy row from leaking.
	out.RefreshToken = ""
	out.Token = ""
	out.SessionCookie = ""
	out.ClientCookie = ""
	return &out
}

// PreserveQoderCredentialsOnEdit keeps the server-side credential and derived
// material when the admin UI submits an edit without re-entering them (secrets
// are redacted on read, so an empty field means "keep").
func PreserveQoderCredentialsOnEdit(acc, existing *store.Account) {
	if acc == nil || existing == nil {
		return
	}
	if strings.TrimSpace(acc.QoderAccessToken) == "" {
		acc.QoderAccessToken = existing.QoderAccessToken
	}
	if strings.TrimSpace(acc.QoderRefreshToken) == "" {
		acc.QoderRefreshToken = existing.QoderRefreshToken
	}
	if strings.TrimSpace(acc.QoderMachineID) == "" {
		acc.QoderMachineID = existing.QoderMachineID
	}
	if strings.TrimSpace(acc.QoderUserID) == "" {
		acc.QoderUserID = existing.QoderUserID
	}
	if strings.TrimSpace(acc.QoderUserName) == "" {
		acc.QoderUserName = existing.QoderUserName
	}
	if strings.TrimSpace(acc.QoderOrganizationID) == "" {
		acc.QoderOrganizationID = existing.QoderOrganizationID
	}
	if len(acc.QoderOrganizationTags) == 0 {
		acc.QoderOrganizationTags = append([]string(nil), existing.QoderOrganizationTags...)
	}
	if acc.QoderExpiresAt.IsZero() {
		acc.QoderExpiresAt = existing.QoderExpiresAt
	}
	if strings.TrimSpace(acc.QoderRuntimeInfo) == "" {
		acc.QoderRuntimeInfo = existing.QoderRuntimeInfo
	}
	if strings.TrimSpace(acc.QoderRuntimeKey) == "" {
		acc.QoderRuntimeKey = existing.QoderRuntimeKey
	}
	// The identity in the runtime plaintext is immutable for a credential: an
	// edit must not change the UID without re-deriving, or every request would
	// be signed with material that no longer matches the account.
	if strings.TrimSpace(existing.QoderUserID) != "" {
		acc.QoderUserID = existing.QoderUserID
	}
	// The catalog snapshot and its provenance are not editable through the form.
	if len(acc.QoderModelIDs) == 0 {
		acc.QoderModelIDs = append([]string(nil), existing.QoderModelIDs...)
		acc.QoderModelsSyncedAt = existing.QoderModelsSyncedAt
	}
	// Provider-observed usage and health are not editable either.
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
	if acc.VerifiedAt.IsZero() {
		acc.VerifiedAt = existing.VerifiedAt
	}
}

// NormalizeQoderCredentials binds the account's device identity to its
// credential. A credential without a machine id cannot sign a request: the
// upstream rejects a device identity that did not perform the authorization.
func NormalizeQoderCredentials(acc *store.Account) bool {
	if acc == nil {
		return false
	}
	creds := qoder.ResolveCredentials(acc)
	if !creds.HasCredential() {
		return false
	}
	accessToken, refreshToken, expiresAt, uid, name, email, orgID, orgTags := creds.Fields()
	acc.QoderAccessToken = accessToken
	if refreshToken != "" {
		acc.QoderRefreshToken = refreshToken
	}
	if !expiresAt.IsZero() {
		acc.QoderExpiresAt = expiresAt
	}
	if uid != "" {
		acc.QoderUserID = uid
	}
	if name != "" {
		acc.QoderUserName = name
	}
	if email != "" {
		acc.Email = email
	}
	if orgID != "" {
		acc.QoderOrganizationID = orgID
	}
	if len(orgTags) > 0 {
		acc.QoderOrganizationTags = orgTags
	}
	// The credential lives in its own fields only: the generic slots are shared
	// with channels whose credentials mean something else, and writing a Qoder
	// token there would leak it through another channel's account list.
	acc.Token = ""
	acc.RefreshToken = ""
	acc.SessionCookie = ""
	acc.ClientCookie = ""
	return true
}

// verifyQoderAccount proves the credential works before it is persisted and
// applies the account-scoped catalog on the way.
//
// The signed catalog read is the verification: it exercises every layer of the
// credential chain (device token, runtime fields, COSY signature), so a
// successful read is stronger evidence than a bare HTTP 200 from the token
// endpoint.
func verifyQoderAccount(ctx context.Context, acc *store.Account, cfg *config.Config) (string, int, error) {
	if acc == nil {
		return "", 0, nil
	}
	creds := qoder.ResolveCredentials(acc)
	if !creds.HasCredential() {
		return "", 400, errQoderMissingCredential
	}
	if strings.TrimSpace(acc.QoderMachineID) == "" {
		return "", 400, errors.New("qoder account is missing its device identity; sign in again")
	}

	client := qoder.NewFromAccount(acc, cfg)
	defer client.Close()

	// The runtime pair encrypts the UID, so it cannot be produced before the
	// identity is known. A credential imported without one is completed here.
	if profile, err := client.FetchProfile(ctx, acc.QoderAccessToken); err == nil {
		if uid := strings.TrimSpace(profile.UID); uid != "" {
			acc.QoderUserID = uid
		}
		if name := strings.TrimSpace(profile.Name); name != "" {
			acc.QoderUserName = name
		}
		if email := strings.TrimSpace(profile.Email); email != "" {
			acc.Email = email
		}
		if orgID := strings.TrimSpace(profile.OrgID); orgID != "" {
			acc.QoderOrganizationID = orgID
		}
		if len(profile.OrgTags) > 0 {
			acc.QoderOrganizationTags = profile.OrgTags
		}
	} else {
		slog.Debug("Qoder profile lookup failed during verification", "account_id", acc.ID, "error", err)
	}
	if strings.TrimSpace(acc.QoderUserID) == "" {
		return "", 400, errors.New("qoder account has no user id; sign in again")
	}

	if err := client.PrepareRuntimeFields(ctx); err != nil {
		return "", 502, err
	}
	fields := client.RuntimeFields()
	acc.QoderRuntimeInfo = fields.EncryptUserInfo
	acc.QoderRuntimeKey = fields.Key

	catalog, err := client.FetchModels(ctx)
	if err != nil {
		return "", 502, err
	}
	if ids := qoder.CatalogSnapshot(catalog); len(ids) > 0 {
		acc.QoderModelIDs = ids
		acc.QoderModelsSyncedAt = time.Now()
	}
	return "", 0, nil
}
