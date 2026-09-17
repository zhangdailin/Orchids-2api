// Package perf provides performance optimization utilities including object pools.
package perf

import (
	"strings"
	"sync"
)

// StringBuilderPool provides reusable strings.Builder instances.
var StringBuilderPool = sync.Pool{
	New: func() any { return &strings.Builder{} },
}

// AcquireStringBuilder gets a strings.Builder from the pool.
func AcquireStringBuilder() *strings.Builder {
	return StringBuilderPool.Get().(*strings.Builder)
}

// maxPooledBuilderBytes is the largest buffer worth keeping in the pool. Reset
// keeps the backing array, so a builder that grew to hold a long streamed answer
// would stay parked at that size and pin the memory until the next collection.
const maxPooledBuilderBytes = 64 << 10

// ReleaseStringBuilder returns a strings.Builder to the pool after resetting it.
// An oversized builder is dropped instead of pooled, so the pool itself stays
// small and a fresh buffer is allocated on the next acquire.
func ReleaseStringBuilder(sb *strings.Builder) {
	if sb == nil {
		return
	}
	if sb.Cap() > maxPooledBuilderBytes {
		return
	}
	sb.Reset()
	StringBuilderPool.Put(sb)
}
