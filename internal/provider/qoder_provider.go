package provider

import (
	"orchids-api/internal/config"
	"orchids-api/internal/qoder"
	"orchids-api/internal/store"
)

type qoderProvider struct{}

// NewQoderProvider creates the Qoder (qoder.com CLI OAuth) provider.
func NewQoderProvider() Provider { return qoderProvider{} }

func (qoderProvider) Name() string { return "qoder" }

func (qoderProvider) NewClient(acc *store.Account, cfg *config.Config) interface{} {
	return qoder.NewFromAccount(acc, cfg)
}
