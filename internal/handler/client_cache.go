package handler

import (
	"crypto/sha256"
	"encoding/hex"
	"io"
	"strconv"
	"strings"
	"sync"

	"orchids-api/internal/config"
	"orchids-api/internal/orchids"
	"orchids-api/internal/store"
	"orchids-api/internal/warp"
)

type cachedAccountClient struct {
	fingerprint string
	client      UpstreamClient
}

type accountClientCache struct {
	mu      sync.RWMutex
	entries map[int64]cachedAccountClient
	retired []UpstreamClient
}

type clientCloser interface {
	Close()
}

func newAccountClientCache() *accountClientCache {
	return &accountClientCache{entries: make(map[int64]cachedAccountClient)}
}

func (h *Handler) getOrCreateAccountClient(acc *store.Account) UpstreamClient {
	if acc == nil {
		return nil
	}
	if h == nil || h.clientCache == nil || acc.ID == 0 {
		return h.buildAccountClient(acc)
	}

	fingerprint := accountClientFingerprint(acc, h.config)

	h.clientCache.mu.RLock()
	entry, ok := h.clientCache.entries[acc.ID]
	h.clientCache.mu.RUnlock()
	if ok && entry.fingerprint == fingerprint && entry.client != nil {
		return entry.client
	}

	client := h.buildAccountClient(acc)
	if client == nil {
		return nil
	}

	h.clientCache.mu.Lock()
	defer h.clientCache.mu.Unlock()

	if entry, ok := h.clientCache.entries[acc.ID]; ok && entry.fingerprint == fingerprint && entry.client != nil {
		closeUpstreamClient(client)
		return entry.client
	}

	if entry, ok := h.clientCache.entries[acc.ID]; ok && entry.client != nil {
		h.clientCache.retired = append(h.clientCache.retired, entry.client)
	}

	h.clientCache.entries[acc.ID] = cachedAccountClient{
		fingerprint: fingerprint,
		client:      client,
	}

	return client
}

func (h *Handler) buildAccountClient(acc *store.Account) UpstreamClient {
	if acc == nil {
		return nil
	}
	var cfg *config.Config
	if h != nil {
		cfg = h.config
	}
	if h != nil && h.clientFactory != nil {
		return h.clientFactory(acc, cfg)
	}
	if strings.EqualFold(acc.AccountType, "warp") {
		return warp.NewFromAccount(acc, cfg)
	}
	return orchids.NewFromAccount(acc, cfg)
}

func (h *Handler) Close() {
	if h == nil {
		return
	}

	closers := make([]clientCloser, 0, 1)
	if c, ok := h.client.(clientCloser); ok {
		closers = append(closers, c)
	}

	if h.clientCache != nil {
		h.clientCache.mu.Lock()
		for _, entry := range h.clientCache.entries {
			if c, ok := entry.client.(clientCloser); ok {
				closers = append(closers, c)
			}
		}
		for _, client := range h.clientCache.retired {
			if c, ok := client.(clientCloser); ok {
				closers = append(closers, c)
			}
		}
		h.clientCache.entries = make(map[int64]cachedAccountClient)
		h.clientCache.retired = nil
		h.clientCache.mu.Unlock()
	}

	for _, c := range closers {
		c.Close()
	}
}

func closeUpstreamClient(client UpstreamClient) {
	if c, ok := client.(clientCloser); ok {
		c.Close()
	}
}

func accountClientFingerprint(acc *store.Account, cfg *config.Config) string {
	if acc == nil {
		return ""
	}

	hasher := sha256.New()
	writeString := func(value string) {
		_, _ = io.WriteString(hasher, value)
		_, _ = hasher.Write([]byte{0})
	}
	writeBool := func(value bool) {
		if value {
			_, _ = hasher.Write([]byte{1})
		} else {
			_, _ = hasher.Write([]byte{0})
		}
		_, _ = hasher.Write([]byte{0})
	}
	writeInt := func(value int) {
		_, _ = io.WriteString(hasher, strconv.Itoa(value))
		_, _ = hasher.Write([]byte{0})
	}
	writeInt64 := func(value int64) {
		_, _ = io.WriteString(hasher, strconv.FormatInt(value, 10))
		_, _ = hasher.Write([]byte{0})
	}

	writeInt64(acc.ID)
	writeString(acc.Name)
	writeString(acc.AccountType)
	writeBool(acc.NSFWEnabled)
	writeString(acc.SessionID)
	writeString(acc.ClientCookie)
	writeString(acc.RefreshToken)
	writeString(acc.DeviceID)
	writeString(acc.RequestID)
	writeString(acc.SessionCookie)
	writeString(acc.ClientUat)
	writeString(acc.ProjectID)
	writeString(acc.UserID)
	writeString(acc.AgentMode)
	writeString(acc.Email)
	writeString(acc.Token)
	writeInt64(acc.UpdatedAt.UnixNano())

	if cfg != nil {
		writeString(cfg.UpstreamMode)
		writeString(cfg.UpstreamURL)
		writeString(cfg.UpstreamToken)
		writeString(cfg.OrchidsAPIBaseURL)
		writeString(cfg.OrchidsWSURL)
		writeString(cfg.OrchidsAPIVersion)
		writeString(cfg.ProxyHTTP)
		writeString(cfg.ProxyHTTPS)
		writeString(cfg.ProxyUser)
		writeString(cfg.ProxyPass)
		writeBool(cfg.AutoRefreshToken)
		writeBool(cfg.DebugEnabled)
		writeBool(cfg.DebugLogSSE)
		writeBool(cfg.SuppressThinking)
		writeInt(cfg.MaxRetries)
		writeInt(cfg.RetryDelay)
		writeInt(cfg.RequestTimeout)
		writeInt(cfg.OrchidsMaxToolResults)
		writeInt(cfg.OrchidsMaxHistoryMessages)
		writeInt(cfg.WarpMaxToolResults)
		writeInt(cfg.WarpMaxHistoryMessages)
		for _, value := range cfg.OrchidsRunAllowlist {
			writeString(value)
		}
		for _, value := range cfg.ProxyBypass {
			writeString(value)
		}
		for _, value := range cfg.OrchidsFSIgnore {
			writeString(value)
		}
	}

	return hex.EncodeToString(hasher.Sum(nil))
}
