package tokencache

import "time"

func (c *MemoryCache) cleanupLoop() {
	defer close(c.stopped)
	c.runCleanup(c.done, 30*time.Second)
}

// Close stops the background cleanup goroutine and waits for it to exit.
func (c *MemoryCache) Close() {
	if c == nil {
		return
	}
	stopCleanup(c.done)
	<-c.stopped
}
