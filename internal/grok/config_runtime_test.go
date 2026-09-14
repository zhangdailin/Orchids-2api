package grok

import (
	"sync"
	"testing"

	"orchids-api/internal/config"
)

func TestSetConfigIsSafeForConcurrentReaders(t *testing.T) {
	h := NewHandler(&config.Config{GrokAPIBaseURL: "https://one.example"}, nil)
	var readers sync.WaitGroup
	done := make(chan struct{})
	for range 8 {
		readers.Add(1)
		go func() {
			defer readers.Done()
			for {
				select {
				case <-done:
					return
				default:
				}
				cfg := h.configSnapshot()
				client := h.webClient()
				cliClient := h.buildClient()
				if cfg == nil || client == nil || cliClient == nil {
					t.Error("runtime snapshot contains a nil component")
					return
				}
			}
		}()
	}
	for range 100 {
		h.SetConfig(&config.Config{GrokAPIBaseURL: "https://two.example"})
		h.SetConfig(&config.Config{GrokAPIBaseURL: "https://one.example"})
	}
	close(done)
	readers.Wait()
}
