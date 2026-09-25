package loadbalancer

import (
	"testing"

	"orchids-api/internal/store"
)

// Selection runs on every routed request, so its per-call allocations are worth
// pinning: the id buffer and the candidate sets are borrowed from a pool and must
// stay off the heap in the steady state.
func benchmarkSelection(b *testing.B, n int, tracker ConnTracker) {
	accounts := make([]*store.Account, n)
	for i := range accounts {
		accounts[i] = &store.Account{
			ID:          int64(i + 1),
			Name:        "acc",
			Weight:      1 + i%4,
			AccountType: "workbuddy",
		}
	}
	lb := &LoadBalancer{connTracker: tracker}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if acc := lb.selectAccountWithTracker(accounts, nil); acc == nil {
			b.Fatal("no account selected")
		}
	}
}

func BenchmarkSelectAccountWithTracker_Single(b *testing.B) {
	benchmarkSelection(b, 1, NewMemoryConnTracker())
}

func BenchmarkSelectAccountWithTracker_Pool16(b *testing.B) {
	benchmarkSelection(b, 16, NewMemoryConnTracker())
}

func BenchmarkSelectAccountWithTracker_Pool256(b *testing.B) {
	benchmarkSelection(b, 256, NewMemoryConnTracker())
}

func BenchmarkSelectAccountWithTracker_Pool4096(b *testing.B) {
	benchmarkSelection(b, 4096, NewMemoryConnTracker())
}

func BenchmarkSelectAccountWithTracker_FixedTracker(b *testing.B) {
	benchmarkSelection(b, 16, &fixedConnTracker{counts: map[int64]int64{}})
}
