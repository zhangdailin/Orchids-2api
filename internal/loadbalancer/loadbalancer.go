package loadbalancer

import (
	"errors"
	"sync"
	"time"

	"orchids-api/internal/store"
)

const defaultCacheTTL = 5 * time.Second

type LoadBalancer struct {
	store          *store.Store
	mu             sync.RWMutex
	cachedAccounts []*store.Account
	cacheExpires   time.Time
	cacheTTL       time.Duration
	activeConns    map[int64]int
}

func New(s *store.Store) *LoadBalancer {
	return NewWithCacheTTL(s, defaultCacheTTL)
}

func NewWithCacheTTL(s *store.Store, cacheTTL time.Duration) *LoadBalancer {
	if cacheTTL <= 0 {
		cacheTTL = defaultCacheTTL
	}
	return &LoadBalancer{
		store:       s,
		cacheTTL:    cacheTTL,
		activeConns: make(map[int64]int),
	}
}

func (lb *LoadBalancer) GetNextAccount() (*store.Account, error) {
	return lb.GetNextAccountExcluding(nil)
}

func (lb *LoadBalancer) GetNextAccountExcluding(excludeIDs []int64) (*store.Account, error) {
	accounts, err := lb.getEnabledAccounts()
	if err != nil {
		return nil, err
	}

	if len(excludeIDs) > 0 {
		excludeSet := make(map[int64]bool)
		for _, id := range excludeIDs {
			excludeSet[id] = true
		}
		var filtered []*store.Account
		for _, acc := range accounts {
			if !excludeSet[acc.ID] {
				filtered = append(filtered, acc)
			}
		}
		accounts = filtered
	}

	if len(accounts) == 0 {
		return nil, errors.New("no enabled accounts available")
	}

	account := lb.selectAccount(accounts)

	if err := lb.store.IncrementRequestCount(account.ID); err != nil {
		return nil, err
	}

	return account, nil
}

func (lb *LoadBalancer) getEnabledAccounts() ([]*store.Account, error) {
	now := time.Now()

	lb.mu.RLock()
	if len(lb.cachedAccounts) > 0 && now.Before(lb.cacheExpires) {
		accounts := make([]*store.Account, len(lb.cachedAccounts))
		copy(accounts, lb.cachedAccounts)
		lb.mu.RUnlock()
		return accounts, nil
	}
	lb.mu.RUnlock()

	accounts, err := lb.store.GetEnabledAccounts()
	if err != nil {
		return nil, err
	}

	lb.mu.Lock()
	lb.cachedAccounts = accounts
	lb.cacheExpires = now.Add(lb.cacheTTL)
	lb.mu.Unlock()

	cached := make([]*store.Account, len(accounts))
	copy(cached, accounts)
	return cached, nil
}

func (lb *LoadBalancer) selectAccount(accounts []*store.Account) *store.Account {
	if len(accounts) == 1 {
		return accounts[0]
	}

	lb.mu.RLock()
	defer lb.mu.RUnlock()

	var bestAccount *store.Account
	minScore := float64(-1)

	for _, acc := range accounts {
		weight := acc.Weight
		if weight <= 0 {
			weight = 1
		}

		conns := lb.activeConns[acc.ID]
		score := float64(conns) / float64(weight)

		if bestAccount == nil || score < minScore {
			bestAccount = acc
			minScore = score
		}
	}

	if bestAccount != nil {
		return bestAccount
	}
	return accounts[0]
}

func (lb *LoadBalancer) GetStats() map[int64]int {
	lb.mu.RLock()
	defer lb.mu.RUnlock()
	
	stats := make(map[int64]int, len(lb.activeConns))
	for id, count := range lb.activeConns {
		stats[id] = count
	}
	return stats
}

func (lb *LoadBalancer) AcquireConnection(accountID int64) {
	lb.mu.Lock()
	lb.activeConns[accountID]++
	lb.mu.Unlock()
}

func (lb *LoadBalancer) ReleaseConnection(accountID int64) {
	lb.mu.Lock()
	if lb.activeConns[accountID] > 0 {
		lb.activeConns[accountID]--
	}
	lb.mu.Unlock()
}
