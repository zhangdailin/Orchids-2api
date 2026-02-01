// Package perf provides performance optimization utilities including object pools.
package perf

import (
	"strings"
	"sync"
)

// StringBuilderPool provides reusable strings.Builder instances.
var StringBuilderPool = sync.Pool{
	New: func() interface{} {
		return &strings.Builder{}
	},
}

// AcquireStringBuilder gets a strings.Builder from the pool.
func AcquireStringBuilder() *strings.Builder {
	return StringBuilderPool.Get().(*strings.Builder)
}

// ReleaseStringBuilder returns a strings.Builder to the pool after resetting it.
func ReleaseStringBuilder(sb *strings.Builder) {
	if sb == nil {
		return
	}
	sb.Reset()
	StringBuilderPool.Put(sb)
}

// MapPool provides reusable map[string]interface{} instances.
var MapPool = sync.Pool{
	New: func() interface{} {
		return make(map[string]interface{}, 16)
	},
}

// AcquireMap gets a map from the pool.
func AcquireMap() map[string]interface{} {
	return MapPool.Get().(map[string]interface{})
}

// ReleaseMap clears and returns a map to the pool.
func ReleaseMap(m map[string]interface{}) {
	if m == nil {
		return
	}
	// Prevent memory leak: don't pool overly large maps
	if len(m) > 256 {
		return
	}
	// Clear map before returning (Go 1.21+ optimized)
	clear(m)
	MapPool.Put(m)
}
