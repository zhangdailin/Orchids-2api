package util

import (
	"context"
	"sync/atomic"
	"testing"
	"time"
)

func TestParallelFor(t *testing.T) {
	t.Run("empty", func(t *testing.T) {
		ParallelFor(0, func(i int) {
			t.Error("should not be called")
		})
	})

	t.Run("single item", func(t *testing.T) {
		var called int32
		ParallelFor(1, func(i int) {
			atomic.AddInt32(&called, 1)
		})
		if called != 1 {
			t.Errorf("called = %d, want 1", called)
		}
	})

	t.Run("small batch (serial)", func(t *testing.T) {
		n := 5
		results := make([]int, n)
		ParallelFor(n, func(i int) {
			results[i] = i * 2
		})
		for i := 0; i < n; i++ {
			if results[i] != i*2 {
				t.Errorf("results[%d] = %d, want %d", i, results[i], i*2)
			}
		}
	})

	t.Run("large batch (parallel)", func(t *testing.T) {
		n := 100
		var counter int64
		ParallelFor(n, func(i int) {
			atomic.AddInt64(&counter, 1)
		})
		if counter != int64(n) {
			t.Errorf("counter = %d, want %d", counter, n)
		}
	})
}

func TestSleepWithContext(t *testing.T) {
	t.Run("zero duration", func(t *testing.T) {
		if !SleepWithContext(context.Background(), 0) {
			t.Error("SleepWithContext(0) = false, want true")
		}
	})

	t.Run("negative duration", func(t *testing.T) {
		if !SleepWithContext(context.Background(), -time.Second) {
			t.Error("SleepWithContext(-1s) = false, want true")
		}
	})

	t.Run("normal sleep", func(t *testing.T) {
		start := time.Now()
		if !SleepWithContext(context.Background(), 50*time.Millisecond) {
			t.Error("SleepWithContext() = false, want true")
		}
		if elapsed := time.Since(start); elapsed < 40*time.Millisecond {
			t.Errorf("elapsed = %v, want >= 40ms", elapsed)
		}
	})

	t.Run("canceled context", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())

		go func() {
			time.Sleep(20 * time.Millisecond)
			cancel()
		}()

		start := time.Now()
		if SleepWithContext(ctx, 1*time.Second) {
			t.Error("SleepWithContext() = true, want false for canceled context")
		}
		if elapsed := time.Since(start); elapsed > 100*time.Millisecond {
			t.Errorf("elapsed = %v, want < 100ms", elapsed)
		}
	})
}
