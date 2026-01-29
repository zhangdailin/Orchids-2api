package summarycache

import "sync/atomic"

type Stats struct {
	hits   uint64
	misses uint64
}

func NewStats() *Stats {
	return &Stats{}
}

func (s *Stats) Hit() {
	if s == nil {
		return
	}
	atomic.AddUint64(&s.hits, 1)
}

func (s *Stats) Miss() {
	if s == nil {
		return
	}
	atomic.AddUint64(&s.misses, 1)
}

func (s *Stats) Snapshot() (uint64, uint64) {
	if s == nil {
		return 0, 0
	}
	return atomic.LoadUint64(&s.hits), atomic.LoadUint64(&s.misses)
}
