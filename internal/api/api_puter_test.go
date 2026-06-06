package api

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"orchids-api/internal/config"
	"orchids-api/internal/puter"
	"orchids-api/internal/store"
)

func TestRefreshAccountState_PuterRequiresAuthToken(t *testing.T) {
	t.Parallel()

	prevVerify := puterVerifyAccount
	t.Cleanup(func() { puterVerifyAccount = prevVerify })

	a := New(nil, "", "", &config.Config{})
	acc := &store.Account{AccountType: "puter"}

	status, httpStatus, err := a.refreshAccountState(context.Background(), acc)
	if err == nil {
		t.Fatal("expected error")
	}
	if status != "" {
		t.Fatalf("status=%q want empty", status)
	}
	if httpStatus != http.StatusBadRequest {
		t.Fatalf("httpStatus=%d want %d", httpStatus, http.StatusBadRequest)
	}
}

func TestRefreshAccountState_PuterUsageFailureDoesNotFallbackToVerify(t *testing.T) {
	prevVerify := puterVerifyAccount
	prevUsage := puterFetchMonthlyUsage
	t.Cleanup(func() {
		puterVerifyAccount = prevVerify
		puterFetchMonthlyUsage = prevUsage
	})

	puterFetchMonthlyUsage = func(ctx context.Context, acc *store.Account, cfg *config.Config) (*puter.MonthlyUsage, error) {
		return nil, errors.New("usage endpoint unavailable")
	}
	puterVerifyAccount = func(ctx context.Context, acc *store.Account, cfg *config.Config) error {
		t.Fatal("puter verifier should not be called after usage sync failure")
		return nil
	}

	a := New(nil, "", "", &config.Config{})
	acc := &store.Account{AccountType: "puter", ClientCookie: "puter-token"}

	status, httpStatus, err := a.refreshAccountState(context.Background(), acc)
	if err == nil {
		t.Fatal("refreshAccountState() expected error")
	}
	if status != "" || httpStatus != http.StatusBadGateway {
		t.Fatalf("status=%q httpStatus=%d want empty/502", status, httpStatus)
	}
}

func TestRefreshAccountState_PuterSyncsMonthlyUsage(t *testing.T) {
	prevVerify := puterVerifyAccount
	prevUsage := puterFetchMonthlyUsage
	t.Cleanup(func() {
		puterVerifyAccount = prevVerify
		puterFetchMonthlyUsage = prevUsage
	})

	puterFetchMonthlyUsage = func(ctx context.Context, acc *store.Account, cfg *config.Config) (*puter.MonthlyUsage, error) {
		return &puter.MonthlyUsage{
			AllowanceInfo: puter.UsageAllowanceInfo{
				Remaining:           13494935.4,
				MonthUsageAllowance: 25000000,
			},
		}, nil
	}
	puterVerifyAccount = func(ctx context.Context, acc *store.Account, cfg *config.Config) error {
		t.Fatal("verification ping should not run when usage sync succeeds")
		return nil
	}

	a := New(nil, "", "", &config.Config{})
	acc := &store.Account{AccountType: "puter", ClientCookie: "puter-token"}

	status, httpStatus, err := a.refreshAccountState(context.Background(), acc)
	if err != nil {
		t.Fatalf("refreshAccountState() error = %v", err)
	}
	if status != "" || httpStatus != 0 {
		t.Fatalf("unexpected status=%q httpStatus=%d", status, httpStatus)
	}
	if acc.UsageCurrent != 13494935.4 || acc.UsageLimit != 25000000 {
		t.Fatalf("unexpected usage current=%v limit=%v", acc.UsageCurrent, acc.UsageLimit)
	}
}

func TestRefreshAccountState_PuterMonthlyUsageExhaustedCompletesWith402Status(t *testing.T) {
	prevVerify := puterVerifyAccount
	prevUsage := puterFetchMonthlyUsage
	t.Cleanup(func() {
		puterVerifyAccount = prevVerify
		puterFetchMonthlyUsage = prevUsage
	})

	puterFetchMonthlyUsage = func(ctx context.Context, acc *store.Account, cfg *config.Config) (*puter.MonthlyUsage, error) {
		return &puter.MonthlyUsage{
			AllowanceInfo: puter.UsageAllowanceInfo{
				Remaining:           0,
				MonthUsageAllowance: 25000000,
			},
		}, nil
	}
	puterVerifyAccount = func(ctx context.Context, acc *store.Account, cfg *config.Config) error {
		t.Fatal("verification ping should not run when usage sync succeeds")
		return nil
	}

	a := New(nil, "", "", &config.Config{})
	acc := &store.Account{AccountType: "puter", ClientCookie: "puter-token"}

	status, httpStatus, err := a.refreshAccountState(context.Background(), acc)
	if err != nil {
		t.Fatalf("refreshAccountState() error = %v", err)
	}
	if status != "402" {
		t.Fatalf("status=%q want 402", status)
	}
	if httpStatus != 0 {
		t.Fatalf("httpStatus=%d want 0", httpStatus)
	}
	if acc.UsageCurrent != 0 || acc.UsageLimit != 25000000 {
		t.Fatalf("unexpected usage current=%v limit=%v", acc.UsageCurrent, acc.UsageLimit)
	}
}

func TestRefreshAccountState_PuterPropagatesUpstreamStatus(t *testing.T) {
	prevVerify := puterVerifyAccount
	prevUsage := puterFetchMonthlyUsage
	t.Cleanup(func() {
		puterVerifyAccount = prevVerify
		puterFetchMonthlyUsage = prevUsage
	})

	puterFetchMonthlyUsage = func(ctx context.Context, acc *store.Account, cfg *config.Config) (*puter.MonthlyUsage, error) {
		return nil, errors.New("usage endpoint unavailable")
	}
	puterVerifyAccount = func(ctx context.Context, acc *store.Account, cfg *config.Config) error {
		t.Fatal("puter verifier should not be called after usage sync failure")
		return nil
	}

	a := New(nil, "", "", &config.Config{})
	acc := &store.Account{AccountType: "puter", ClientCookie: "puter-token"}

	status, httpStatus, err := a.refreshAccountState(context.Background(), acc)
	if err == nil {
		t.Fatal("expected error")
	}
	if status != "" {
		t.Fatalf("status=%q want empty", status)
	}
	if httpStatus != http.StatusBadGateway {
		t.Fatalf("httpStatus=%d want %d", httpStatus, http.StatusBadGateway)
	}
}

func TestRefreshAccountState_PuterInsufficientFundsCompletesWith402Status(t *testing.T) {
	prevVerify := puterVerifyAccount
	prevUsage := puterFetchMonthlyUsage
	t.Cleanup(func() {
		puterVerifyAccount = prevVerify
		puterFetchMonthlyUsage = prevUsage
	})

	puterFetchMonthlyUsage = func(ctx context.Context, acc *store.Account, cfg *config.Config) (*puter.MonthlyUsage, error) {
		return nil, errors.New("usage endpoint unavailable")
	}
	puterVerifyAccount = func(ctx context.Context, acc *store.Account, cfg *config.Config) error {
		t.Fatal("puter verifier should not be called after usage sync failure")
		return nil
	}

	a := New(nil, "", "", &config.Config{})
	acc := &store.Account{AccountType: "puter", ClientCookie: "puter-token"}

	status, httpStatus, err := a.refreshAccountState(context.Background(), acc)
	if err == nil {
		t.Fatal("expected error")
	}
	if status != "" {
		t.Fatalf("status=%q want empty", status)
	}
	if httpStatus != http.StatusBadGateway {
		t.Fatalf("httpStatus=%d want %d", httpStatus, http.StatusBadGateway)
	}
}
