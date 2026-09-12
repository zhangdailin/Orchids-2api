package provider

import (
	"orchids-api/internal/config"
	"orchids-api/internal/store"
	"orchids-api/internal/workbuddy"
)

type workBuddyProvider struct{}

// NewWorkBuddyProvider creates the WorkBuddy (www.workbuddy.ai) provider.
func NewWorkBuddyProvider() Provider { return workBuddyProvider{} }

func (workBuddyProvider) Name() string { return "workbuddy" }

func (workBuddyProvider) NewClient(acc *store.Account, cfg *config.Config) interface{} {
	return workbuddy.NewFromAccount(acc, cfg)
}
