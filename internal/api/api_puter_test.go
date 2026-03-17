package api

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"orchids-api/internal/config"
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

func TestRefreshAccountState_PuterAcceptsValidToken(t *testing.T) {
	prevVerify := puterVerifyAccount
	t.Cleanup(func() { puterVerifyAccount = prevVerify })

	called := false
	puterVerifyAccount = func(ctx context.Context, acc *store.Account, cfg *config.Config) error {
		called = true
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
	if !called {
		t.Fatal("expected verifier to be called")
	}
}

func TestRefreshAccountState_PuterPropagatesUpstreamStatus(t *testing.T) {
	prevVerify := puterVerifyAccount
	t.Cleanup(func() { puterVerifyAccount = prevVerify })

	puterVerifyAccount = func(ctx context.Context, acc *store.Account, cfg *config.Config) error {
		return errors.New("unexpected status code 401: unauthorized")
	}

	a := New(nil, "", "", &config.Config{})
	acc := &store.Account{AccountType: "puter", ClientCookie: "puter-token"}

	status, httpStatus, err := a.refreshAccountState(context.Background(), acc)
	if err == nil {
		t.Fatal("expected error")
	}
	if status != "401" {
		t.Fatalf("status=%q want 401", status)
	}
	if httpStatus != http.StatusUnauthorized {
		t.Fatalf("httpStatus=%d want %d", httpStatus, http.StatusUnauthorized)
	}
}
