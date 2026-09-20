package tokencache

import "time"

func (c *MemoryCache) cleanupLoop() {
	c.runCleanup(c.done, 30*time.Second)
}

// Close 停止后台清理 goroutine
func (c *MemoryCache) Close() {
	if c == nil {
		return
	}
	stopCleanup(c.done)
}
