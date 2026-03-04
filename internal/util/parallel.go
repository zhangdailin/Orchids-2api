// Package util 提供通用工具函数
package util

import (
	"context"
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
