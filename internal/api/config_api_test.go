package api

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/alicebob/miniredis/v2"
	"github.com/goccy/go-json"

	"orchids-api/internal/config"
	"orchids-api/internal/grok"
	"orchids-api/internal/store"
)

func setupConfigAPI(t *testing.T) (*API, *store.Store, *miniredis.Miniredis) {
	t.Helper()

	s, mini := newTestStore(t, "test:")

	cfg := &config.Config{
		AdminPass:          "initial-secret",
		AdminToken:         "initial-token",
		EnableTokenCache:   true,
		TokenCacheTTL:      300,
		TokenCacheStrategy: "1",
		ProxyURL:           "http://127.0.0.1:7890",
		ProxyBypass:        []string{"example.com"},
	}
	config.ApplyDefaults(cfg)

	return New(s, "admin", "pass", cfg), s, mini
}

func TestHandleConfigListReturnsCodeFreeMaxShape(t *testing.T) {
	api, s, mini := setupConfigAPI(t)
	defer func() {
		_ = s.Close()
		mini.Close()
	}()

	req := httptest.NewRequest(http.MethodGet, "/api/config/list", nil)
	rec := httptest.NewRecorder()
	api.HandleConfigList(rec, req)

	var resp struct {
		Code int                    `json:"code"`
		Data map[string]interface{} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}

	if resp.Code != 0 {
		t.Fatalf("expected code 0, got %d", resp.Code)
	}
	if got := resp.Data["admin_pass"]; got != "initial-secret" {
		t.Fatalf("admin_pass=%v want initial-secret", got)
	}
	if got := resp.Data["admin_password"]; got != "initial-secret" {
		t.Fatalf("admin_password=%v want initial-secret", got)
	}
	if got := resp.Data["admin_token"]; got != "initial-token" {
		t.Fatalf("admin_token=%v want initial-token", got)
	}
	if got := resp.Data["token_cache_strategy"]; got != "1" {
		t.Fatalf("token_cache_strategy=%v want 1", got)
	}
	if got := resp.Data["proxy_url"]; got != "http://127.0.0.1:7890" {
		t.Fatalf("proxy_url=%v want http://127.0.0.1:7890", got)
	}
}

func TestHandleConfigSaveAcceptsCodeFreeMaxStylePayload(t *testing.T) {
	api, s, mini := setupConfigAPI(t)
	defer func() {
		_ = s.Close()
		mini.Close()
	}()

	body := `{
		"admin_password":"changed-secret",
		"enable_token_cache":"false",
		"token_cache_ttl":"900",
		"token_cache_strategy":"0",
		"grok_statsig_id":"browser-statsig",
		"grok_cf_clearance":"cf-clear",
		"grok_cf_bm":"bm-token",
		"proxy_url":"socks5://user:pass@127.0.0.1:1080",
		"proxy_bypass":"example.com, internal.local"
	}`
	req := httptest.NewRequest(http.MethodPost, "/api/config/save", strings.NewReader(body))
	rec := httptest.NewRecorder()
	api.HandleConfigSave(rec, req)

	var resp struct {
		Code int    `json:"code"`
		Msg  string `json:"msg"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}

	if resp.Code != 0 {
		t.Fatalf("expected code 0, got %d body=%s", resp.Code, rec.Body.String())
	}
	if resp.Msg != "success" {
		t.Fatalf("msg=%q want success", resp.Msg)
	}

	cfg := api.config.Load()
	if cfg == nil {
		t.Fatal("config not stored")
	}
	if cfg.AdminPass != "changed-secret" {
		t.Fatalf("AdminPass=%q want changed-secret", cfg.AdminPass)
	}
	if cfg.EnableTokenCache {
		t.Fatalf("EnableTokenCache=%v want false", cfg.EnableTokenCache)
	}
	if cfg.TokenCacheTTL != 900 {
		t.Fatalf("TokenCacheTTL=%d want 900", cfg.TokenCacheTTL)
	}
	if cfg.TokenCacheStrategy != "0" {
		t.Fatalf("TokenCacheStrategy=%q want 0", cfg.TokenCacheStrategy)
	}
	if cfg.GrokStatsigID != "browser-statsig" {
		t.Fatalf("GrokStatsigID=%q want browser-statsig", cfg.GrokStatsigID)
	}
	if cfg.GrokConfigCFClearance != "cf-clear" {
		t.Fatalf("GrokConfigCFClearance=%q want cf-clear", cfg.GrokConfigCFClearance)
	}
	if cfg.GrokConfigCFBM != "bm-token" {
		t.Fatalf("GrokConfigCFBM=%q want bm-token", cfg.GrokConfigCFBM)
	}
	if cfg.ProxyURL != "socks5://user:pass@127.0.0.1:1080" {
		t.Fatalf("ProxyURL=%q want socks5://user:pass@127.0.0.1:1080", cfg.ProxyURL)
	}
	if len(cfg.ProxyBypass) != 2 || cfg.ProxyBypass[0] != "example.com" || cfg.ProxyBypass[1] != "internal.local" {
		t.Fatalf("ProxyBypass=%v want [example.com internal.local]", cfg.ProxyBypass)
	}

	saved, err := s.GetSetting(context.Background(), "config")
	if err != nil {
		t.Fatalf("GetSetting(config) error = %v", err)
	}
	if !strings.Contains(saved, `"admin_pass":"changed-secret"`) {
		t.Fatalf("saved config missing updated admin_pass: %s", saved)
	}
	if !strings.Contains(saved, `"grok_statsig_id":"browser-statsig"`) {
		t.Fatalf("saved config missing grok_statsig_id: %s", saved)
	}
}

func TestHandleConfigSavePublishesImmutableSnapshot(t *testing.T) {
	api, s, mini := setupConfigAPI(t)
	defer func() {
		_ = s.Close()
		mini.Close()
	}()

	original := api.config.Load()
	if original == nil {
		t.Fatal("expected initial config")
	}

	body := `{
		"proxy_url":"http://alice:secret@127.0.0.1:9090"
	}`
	req := httptest.NewRequest(http.MethodPost, "/api/config/save", strings.NewReader(body))
	rec := httptest.NewRecorder()
	api.HandleConfigSave(rec, req)

	var resp struct {
		Code int `json:"code"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if resp.Code != 0 {
		t.Fatalf("expected code 0, got %d body=%s", resp.Code, rec.Body.String())
	}

	if original.ProxyURL != "http://127.0.0.1:7890" {
		t.Fatalf("published snapshot mutated in place: ProxyURL=%q", original.ProxyURL)
	}
	updated := api.config.Load()
	if updated == original {
		t.Fatal("expected a new immutable config snapshot")
	}
	if updated.ProxyURL != "http://alice:secret@127.0.0.1:9090" {
		t.Fatalf("updated ProxyURL=%q want new value", updated.ProxyURL)
	}
}

func TestHandleConfigSaveNotifiesRuntimeConsumers(t *testing.T) {
	api, s, mini := setupConfigAPI(t)
	defer func() {
		_ = s.Close()
		mini.Close()
	}()

	var notified *config.Config
	api.SetConfigChangeHook(func(cfg *config.Config) { notified = cfg })
	req := httptest.NewRequest(http.MethodPost, "/api/config/save", strings.NewReader(`{"proxy_url":"http://127.0.0.1:9091"}`))
	rec := httptest.NewRecorder()
	api.HandleConfigSave(rec, req)

	if notified == nil {
		t.Fatal("runtime config hook was not called")
	}
	if notified != api.config.Load() {
		t.Fatal("runtime consumers did not receive the published snapshot")
	}
	if notified.ProxyURL != "http://127.0.0.1:9091" {
		t.Fatalf("notified ProxyURL=%q", notified.ProxyURL)
	}
}

func TestPersistConfigConcurrentReadersSeeCompleteSnapshots(t *testing.T) {
	api, s, mini := setupConfigAPI(t)
	defer func() {
		_ = s.Close()
		mini.Close()
	}()

	const updates = 100
	var readers sync.WaitGroup
	errCh := make(chan error, 8)
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
				snapshot := api.config.Load()
				if snapshot == nil || snapshot.AdminUser == "admin" {
					continue
				}
				if snapshot.ProxyURL != "http://"+snapshot.AdminUser+".example" {
					select {
					case errCh <- fmt.Errorf("torn snapshot: user=%q proxy=%q", snapshot.AdminUser, snapshot.ProxyURL):
					default:
					}
					return
				}
			}
		}()
	}

	for i := range updates {
		current := api.config.Load()
		next := current.Clone()
		next.AdminUser = fmt.Sprintf("admin-%d", i)
		next.ProxyURL = "http://" + next.AdminUser + ".example"
		if err := api.persistConfig(context.Background(), current, next); err != nil {
			close(done)
			readers.Wait()
			t.Fatalf("persistConfig: %v", err)
		}
	}
	close(done)
	readers.Wait()
	close(errCh)
	for err := range errCh {
		t.Fatal(err)
	}
}

// A signing endpoint is validated where it is typed: the value decides whether
// account page metadata leaves the host, so an invalid one must not be stored.
func TestPersistConfigValidatesStatsigSignerURL(t *testing.T) {
	a, s, cleanup := newTestAPI(t)
	defer cleanup()
	ctx := context.Background()

	invalid := "http://public-signer.example.com/sign"
	cfg := &config.Config{GrokStatsigSignerURL: &invalid}
	if err := a.persistConfig(ctx, nil, cfg); err == nil {
		t.Fatal("an insecure public signer URL was stored")
	}
	if saved, err := s.GetSetting(ctx, "config"); err != nil || strings.Contains(saved, "public-signer") {
		t.Fatalf("the rejected value reached the store: %q err=%v", saved, err)
	}

	// grok2api's own endpoint, and an explicit opt-out, both store.
	valid := grok.DefaultStatsigSignerURL
	cfg = &config.Config{GrokStatsigSignerURL: &valid}
	if err := a.persistConfig(ctx, nil, cfg); err != nil {
		t.Fatalf("the reference signer URL was rejected: %v", err)
	}
	disabled := ""
	cfg = &config.Config{GrokStatsigSignerURL: &disabled}
	if err := a.persistConfig(ctx, nil, cfg); err != nil {
		t.Fatalf("an explicit opt-out was rejected: %v", err)
	}
	// Unset keeps the default behaviour and is always accepted.
	if err := a.persistConfig(ctx, nil, &config.Config{}); err != nil {
		t.Fatalf("an unset signer URL was rejected: %v", err)
	}
}
