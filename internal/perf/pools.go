// Package perf provides performance optimization utilities including object pools.
package perf

import (
	"bufio"
	"bytes"
	"io"
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
	clear(m)
	MapPool.Put(m)
}

// ByteBufferPool provides reusable bytes.Buffer instances.
var ByteBufferPool = sync.Pool{
	New: func() interface{} {
		return bytes.NewBuffer(make([]byte, 0, 4096))
	},
}

// AcquireByteBuffer gets a bytes.Buffer from the pool.
func AcquireByteBuffer() *bytes.Buffer {
	return ByteBufferPool.Get().(*bytes.Buffer)
}

// ReleaseByteBuffer resets and returns a bytes.Buffer to the pool.
func ReleaseByteBuffer(b *bytes.Buffer) {
	if b == nil {
		return
	}
	b.Reset()
	ByteBufferPool.Put(b)
}

// BufioReaderPool provides reusable bufio.Reader instances.
var BufioReaderPool = sync.Pool{
	New: func() interface{} {
		return bufio.NewReaderSize(nil, 32768)
	},
}

// AcquireBufioReader gets a bufio.Reader from the pool.
func AcquireBufioReader(r io.Reader) *bufio.Reader {
	br := BufioReaderPool.Get().(*bufio.Reader)
	br.Reset(r)
	return br
}

// ReleaseBufioReader returns a bufio.Reader to the pool.
func ReleaseBufioReader(br *bufio.Reader) {
	if br == nil {
		return
	}
	br.Reset(nil)
	BufioReaderPool.Put(br)
}
