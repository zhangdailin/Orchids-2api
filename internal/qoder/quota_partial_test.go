package qoder

import (
	"context"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"orchids-api/internal/store"
)

func TestFetchQuotaUsageFailurePreservesSnapshot(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name      string
		usageCode int
		usageBody string
		planCode  int
		wantError string
	}{
		{"plan succeeds", http.StatusServiceUnavailable, `{"error":"usage unavailable"}`, http.StatusOK, "503"},
		{"status fallback succeeds", http.StatusServiceUnavailable, `{"error":"usage unavailable"}`, http.StatusNotFound, "503"},
		{"usage cannot decode", http.StatusOK, `{`, http.StatusOK, "decode /api/v2/quota/usage"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				switch r.URL.Path {
				case "/api/v2/quota/usage":
					w.WriteHeader(tc.usageCode)
					_, _ = w.Write([]byte(tc.usageBody))
				case "/api/v2/user/plan":
					w.WriteHeader(tc.planCode)
					_, _ = w.Write([]byte(`{"plan_tier_name":"Pro","is_paid_plan":true}`))
				case "/api/v3/user/status":
					_, _ = w.Write([]byte(`{"userTag":"Pro","isQuotaExceeded":false}`))
				default:
					http.NotFound(w, r)
				}
			}))
			defer server.Close()

			acc := signedTestAccount()
			reset := timeDate(2026, time.September, 23, 0, 0, 0)
			acc.UsageLimit = 100
			acc.UsageCurrent = 0
			acc.QuotaResetAt = reset
			acc.StatusCode = store.AccountStatusQoderQuotaExhausted
			acc.QoderQuota = store.QoderQuotaSnapshot{
				Limit: 100, Used: 100, Remaining: 0, Exhausted: true,
				PlanTier: "Free", Unit: "credits", LastKnownLimit: 100,
				ResetAt: reset, SyncedAt: reset.Add(-time.Hour),
			}
			before := *acc
			client := NewFromAccount(acc, nil)
			setTestEndpoints(client, server.URL, server.URL, server.URL)
			quota, err := client.FetchQuota(context.Background())
			if err == nil || !strings.Contains(err.Error(), tc.wantError) {
				t.Fatalf("FetchQuota() error = %v, want %q", err, tc.wantError)
			}
			if quota != nil {
				t.Fatalf("FetchQuota() returned a partial snapshot: %+v", quota)
			}
			// Even an unconditional ApplyQuota must not install zero counters or
			// clear known exhaustion after the failed fetch.
			ApplyQuota(acc, quota)
			if !reflect.DeepEqual(*acc, before) {
				t.Fatalf("failed quota refresh changed account: got %+v, want %+v", *acc, before)
			}
		})
	}
}

func TestFetchQuotaUsageSuccessWithOptionalMetadata(t *testing.T) {
	t.Parallel()

	for _, statusWorks := range []bool{false, true} {
		name := "metadata unavailable"
		if statusWorks {
			name = "status fallback"
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				switch r.URL.Path {
				case "/api/v2/quota/usage":
					_, _ = w.Write([]byte(`{"isQuotaExceeded":false,"userQuota":{"total":100,"used":20,"remaining":80,"unit":"credits"}}`))
				case "/api/v3/user/status":
					if statusWorks {
						_, _ = w.Write([]byte(`{"userTag":"Pro","nextResetAt":1790208000000}`))
						return
					}
					http.Error(w, "unavailable", http.StatusServiceUnavailable)
				default:
					http.Error(w, "unavailable", http.StatusServiceUnavailable)
				}
			}))
			defer server.Close()

			acc := signedTestAccount()
			acc.QoderQuota.Exhausted = true
			client := NewFromAccount(acc, nil)
			setTestEndpoints(client, server.URL, server.URL, server.URL)
			quota, err := client.FetchQuota(context.Background())
			if err != nil {
				t.Fatalf("FetchQuota() error = %v", err)
			}
			if quota.Limit != 100 || quota.Used != 20 || quota.Remaining != 80 || quota.Exhausted {
				t.Fatalf("usage snapshot = %+v", quota)
			}
			if statusWorks && (quota.PlanTier != "Pro" || quota.ResetAt.IsZero()) {
				t.Fatalf("status fallback metadata = %+v", quota)
			}
			ApplyQuota(acc, quota)
			if acc.QoderQuota.Exhausted || acc.UsageLimit != 100 || acc.UsageCurrent != 80 {
				t.Fatalf("successful usage refresh did not replace exhausted snapshot: %+v", acc.QoderQuota)
			}
		})
	}
}
