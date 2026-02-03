// Package util 提供通用工具函数
package util

import (
	"context"
	"fmt"
	"runtime"
	"sync"
	"time"
)

// ParallelFor 并行执行 n 个任务，每个任务接收索引 [0, n)
// 自动根据 CPU 核心数调整并发度，对于小批量任务会串行执行以避免 goroutine 开销
func ParallelFor(n int, fn func(int)) {
	if n <= 0 {
		return
	}

	// 并发阈值：少于此数量时串行处理更高效
	const parallelThreshold = 8

	if n < parallelThreshold {
		// 串行处理小批量
		for i := 0; i < n; i++ {
			fn(i)
		}
		return
	}

	workers := runtime.GOMAXPROCS(0)
	if workers < 1 {
		workers = 1
	}
	if workers > n {
		workers = n
	}

	var wg sync.WaitGroup
	jobs := make(chan int, workers)

	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for idx := range jobs {
				func() {
					defer func() {
						if r := recover(); r != nil {
							// Prevent crash from panic in worker
						}
					}()
					fn(idx)
				}()
			}
		}()
	}

	for i := 0; i < n; i++ {
		jobs <- i
	}
	close(jobs)
	wg.Wait()
}

// ParallelForWithContext 支持取消的并行执行
func ParallelForWithContext(ctx context.Context, n int, fn func(context.Context, int) error) error {
	if n <= 0 {
		return nil
	}

	const parallelThreshold = 8

	if n < parallelThreshold {
		for i := 0; i < n; i++ {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if err := fn(ctx, i); err != nil {
				return err
			}
		}
		return nil
	}

	workers := runtime.GOMAXPROCS(0)
	if workers < 1 {
		workers = 1
	}
	if workers > n {
		workers = n
	}

	var wg sync.WaitGroup
	jobs := make(chan int, workers)
	errCh := make(chan error, 1)

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for idx := range jobs {
				func() {
					defer func() {
						if r := recover(); r != nil {
							select {
							case errCh <- fmt.Errorf("worker panic: %v", r):
								cancel()
							default:
							}
						}
					}()
					if ctx.Err() != nil {
						return
					}
					if err := fn(ctx, idx); err != nil {
						select {
						case errCh <- err:
							cancel()
						default:
						}
						return
					}
				}()
			}
		}()
	}

	go func() {
		for i := 0; i < n; i++ {
			select {
			case <-ctx.Done():
				close(jobs)
				return
			case jobs <- i:
			}
		}
		close(jobs)
	}()

	wg.Wait()

	select {
	case err := <-errCh:
		return err
	default:
		return ctx.Err()
	}
}

// ParallelMap 并行映射，返回结果切片（保持顺序）
func ParallelMap[T any, R any](items []T, fn func(T) R) []R {
	n := len(items)
	if n == 0 {
		return nil
	}

	results := make([]R, n)
	ParallelFor(n, func(i int) {
		results[i] = fn(items[i])
	})
	return results
}

// SleepWithContext 可取消的休眠，返回 false 表示被取消
func SleepWithContext(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return true
	}
	timer := time.NewTimer(d)
	defer func() {
		if !timer.Stop() {
			select {
			case <-timer.C:
			default:
			}
		}
	}()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

// Retry 重试执行函数，直到成功或达到最大重试次数
func Retry(ctx context.Context, maxRetries int, delay time.Duration, fn func() error) error {
	var lastErr error
	for i := 0; i <= maxRetries; i++ {
		if ctx.Err() != nil {
			return ctx.Err()
		}

		lastErr = fn()
		if lastErr == nil {
			return nil
		}

		if i < maxRetries {
			if !SleepWithContext(ctx, delay) {
				return ctx.Err()
			}
		}
	}
	return lastErr
}

// RetryWithBackoff 指数退避重试
func RetryWithBackoff(ctx context.Context, maxRetries int, initialDelay time.Duration, maxDelay time.Duration, fn func() error) error {
	var lastErr error
	delay := initialDelay

	for i := 0; i <= maxRetries; i++ {
		if ctx.Err() != nil {
			return ctx.Err()
		}

		lastErr = fn()
		if lastErr == nil {
			return nil
		}

		if i < maxRetries {
			if !SleepWithContext(ctx, delay) {
				return ctx.Err()
			}
			// 指数退避
			delay *= 2
			if delay > maxDelay {
				delay = maxDelay
			}
		}
	}
	return lastErr
}
