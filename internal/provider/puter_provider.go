package provider

import (
	"orchids-api/internal/config"
	"orchids-api/internal/puter"
	"orchids-api/internal/store"
)

type puterProvider struct{}

func NewPuterProvider() Provider { return puterProvider{} }

func (puterProvider) Name() string { return "puter" }

func (puterProvider) NewClient(acc *store.Account, cfg *config.Config) interface{} {
	return puter.NewFromAccount(acc, cfg)
}
