package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"

	"orchids-api/internal/audit"
	"orchids-api/internal/config"
	"orchids-api/internal/middleware"
	"orchids-api/internal/store"
)

// probeRecorder captures the probe requests a stub listener receives plus the
// journal entries probeOnce emitted.
type probeRecorder struct {
	mu       sync.Mutex
	paths    []string
	bodies   []string
	headers  []http.Header
	events   []audit.Event
	response int
}

func (p *probeRecorder) server() *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body := make([]byte, r.ContentLength)
		if r.ContentLength > 0 {
			_, _ = r.Body.Read(body)
		}
		p.mu.Lock()
		p.paths = append(p.paths, r.URL.Path)
		p.bodies = append(p.bodies, string(body))
		p.headers = append(p.headers, r.Header.Clone())
		status := p.response
		p.mu.Unlock()
		if status == 0 {
			status = http.StatusOK
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
}

func (p *probeRecorder) Log(_ context.Context, event audit.Event) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.events = append(p.events, event)
}

func (p *probeRecorder) snapshot() ([]string, []string, []http.Header, []audit.Event) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.paths...), append([]string(nil), p.bodies...),
		append([]http.Header(nil), p.headers...), append([]audit.Event(nil), p.events...)
}

func newProbeStore(t *testing.T, accounts ...*store.Account) *store.Store {
	t.Helper()
	mini := miniredis.RunT(t)
	s, err := store.New(store.Options{StoreMode: "redis", RedisAddr: mini.Addr(), RedisDB: 0, RedisPrefix: "probe:"})
	if err != nil {
		t.Fatalf("store.New() error = %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	for _, acc := range accounts {
		if err := s.CreateAccount(context.Background(), acc); err != nil {
			t.Fatalf("CreateAccount(%s) error = %v", acc.Name, err)
		}
	}
	return s
}

// TestProbeTargets_SkipsUnusableAccounts pins which accounts justify a probe: a
// disabled, cooling-down or internal SSO runtime account is not evidence that
// the channel is unreachable, so probing it would only add noise.
func TestProbeTargets_SkipsUnusableAccounts(t *testing.T) {
	t.Parallel()

	now := time.Now()
	accounts := []*store.Account{
		{ID: 1, Name: "live", AccountType: "grok", Enabled: true},
		{ID: 2, Name: "disabled", AccountType: "grok", Enabled: false},
		{ID: 3, Name: "cooling", AccountType: "grok", Enabled: true, StatusCode: "429", LastAttempt: now},
		{ID: 4, Name: "sso-child", AccountType: "grok", Enabled: true, GrokSSOParentID: 1},
		{ID: 5, Name: "untyped", AccountType: "", Enabled: true},
	}

	targets := probeTargets(accounts, &config.Config{})
	if len(targets) != 1 {
		t.Fatalf("probeTargets() = %d targets, want 1 (only the usable grok account)", len(targets))
	}
	if targets[0].Channel != "grok" || targets[0].Path != "/v1/responses" {
		t.Fatalf("target = %+v, want the grok /v1/responses probe", targets[0])
	}

	if got := probeTargets([]*store.Account{accounts[1], accounts[2], accounts[3]}, &config.Config{}); len(got) != 0 {
		t.Fatalf("probeTargets() with no usable account = %+v, want none", got)
	}
}

// TestProbeTargets_UsesConfiguredModel covers the reported defect that probers
// hardcoded a model the deployment may not serve: the configured model must win,
// and the built-in default is only the fallback.
func TestProbeTargets_UsesConfiguredModel(t *testing.T) {
	t.Parallel()

	accounts := []*store.Account{{ID: 1, AccountType: "grok", Enabled: true}}

	defaultTargets := probeTargets(accounts, &config.Config{})
	if len(defaultTargets) != 1 || !strings.Contains(defaultTargets[0].Payload, defaultProbeModel) {
		t.Fatalf("default probe payload = %+v, want it to name %q", defaultTargets, defaultProbeModel)
	}
	if defaultTargets[0].Model != probeModelLabel {
		t.Fatalf("probe journal model = %q, want the synthetic label %q", defaultTargets[0].Model, probeModelLabel)
	}

	configured := probeTargets(accounts, &config.Config{GrokProbeModel: "grok-4.5-fast"})
	if len(configured) != 1 {
		t.Fatalf("probeTargets() = %d targets, want 1", len(configured))
	}
	if !strings.Contains(configured[0].Payload, `"model":"grok-4.5-fast"`) {
		t.Fatalf("probe payload = %q, want the configured model", configured[0].Payload)
	}
	if !strings.Contains(configured[0].Payload, `"stream":false`) {
		t.Fatalf("probe payload = %q, want a non-streaming request", configured[0].Payload)
	}
}

// TestResolveProbeAPIKey covers the reported failure where probes were rejected
// by our own inference-auth middleware and therefore measured nothing about the
// upstream. The probe must carry the public key when auth is on, and be skipped
// (rather than produce a stream of local 401s) when no key exists.
func TestResolveProbeAPIKey(t *testing.T) {
	t.Parallel()

	if got := resolveProbeAPIKey(nil); got != "" {
		t.Fatalf("resolveProbeAPIKey(nil) = %q, want empty", got)
	}
	if got := resolveProbeAPIKey(&config.Config{}); got != "" {
		t.Fatalf("resolveProbeAPIKey(no key, auth on) = %q, want empty", got)
	}
	off := false
	if got := resolveProbeAPIKey(&config.Config{PublicKey: "public-key", InferenceAuth: &off}); got != "" {
		t.Fatalf("resolveProbeAPIKey(auth off) = %q, want empty (no key needed)", got)
	}
	if got := resolveProbeAPIKey(&config.Config{PublicKey: "  public-key  "}); got != "public-key" {
		t.Fatalf("resolveProbeAPIKey() = %q, want the trimmed public key", got)
	}
}

// TestProbeOnce_SkipsWithoutKeyUnderInferenceAuth pins the guard: with auth on
// and no key configured, no probe is issued at all.
func TestProbeOnce_SkipsWithoutKeyUnderInferenceAuth(t *testing.T) {
	t.Parallel()

	recorder := &probeRecorder{}
	upstream := recorder.server()
	defer upstream.Close()

	s := newProbeStore(t, &store.Account{Name: "grok-live", AccountType: "grok", Enabled: true})

	recorded := probeOnce(context.Background(), s, &config.Config{}, recorder, upstream.URL, upstream.Client())
	if recorded != 0 {
		t.Fatalf("probeOnce() recorded = %d, want 0 while no key is configured", recorded)
	}
	paths, _, _, events := recorder.snapshot()
	if len(paths) != 0 || len(events) != 0 {
		t.Fatalf("probeOnce() issued %v with %d journal events, want none", paths, len(events))
	}
}

// TestProbeOnce_SendsKeyAndJournalsOutcome covers the happy path end to end: the
// managed key is attached, the synthetic marker header is present, and the
// outcome lands in the journal against the probed channel (not the probe itself).
func TestProbeOnce_SendsKeyAndJournalsOutcome(t *testing.T) {
	t.Parallel()

	recorder := &probeRecorder{}
	upstream := recorder.server()
	defer upstream.Close()

	s := newProbeStore(t, &store.Account{Name: "grok-live", AccountType: "grok", Enabled: true})
	cfg := &config.Config{PublicKey: "public-key", GrokProbeModel: "grok-4.5-fast"}

	recorded := probeOnce(context.Background(), s, cfg, recorder, upstream.URL, upstream.Client())
	if recorded != 1 {
		t.Fatalf("probeOnce() recorded = %d, want 1", recorded)
	}

	paths, bodies, headers, events := recorder.snapshot()
	if len(paths) != 1 || paths[0] != "/v1/responses" {
		t.Fatalf("probe paths = %v, want [/v1/responses]", paths)
	}
	var payload struct {
		Model  string `json:"model"`
		Stream bool   `json:"stream"`
	}
	if err := json.Unmarshal([]byte(bodies[0]), &payload); err != nil {
		t.Fatalf("probe body %q is not JSON: %v", bodies[0], err)
	}
	if payload.Model != "grok-4.5-fast" || payload.Stream {
		t.Fatalf("probe payload = %+v, want the configured non-streaming model", payload)
	}
	if got := headers[0].Get("Authorization"); got != "Bearer public-key" {
		t.Fatalf("probe Authorization = %q, want the configured public key", got)
	}
	if got := headers[0].Get(middleware.ProbeHeader); got != "1" {
		t.Fatalf("probe marker header %s = %q, want 1", middleware.ProbeHeader, got)
	}

	if len(events) != 1 {
		t.Fatalf("journal events = %d, want 1", len(events))
	}
	event := events[0]
	if event.Action != "channel_probe" || event.Kind != audit.KindSystem {
		t.Fatalf("journal event = %+v, want a system channel_probe", event)
	}
	if event.Channel != middleware.ProbeChannel || event.Provider != "grok" {
		t.Fatalf("journal channel/provider = %q/%q, want probe/grok", event.Channel, event.Provider)
	}
	if event.Status != "ok" {
		t.Fatalf("journal status = %q, want ok", event.Status)
	}
}

// TestProbeOnce_RecordsUpstreamStatus checks that a non-2xx answer is captured
// with its status text, so the overview can explain a probe failure.
func TestProbeOnce_RecordsUpstreamStatus(t *testing.T) {
	t.Parallel()

	recorder := &probeRecorder{response: http.StatusServiceUnavailable}
	upstream := recorder.server()
	defer upstream.Close()

	s := newProbeStore(t, &store.Account{Name: "grok-live", AccountType: "grok", Enabled: true})
	off := false
	cfg := &config.Config{InferenceAuth: &off}

	if recorded := probeOnce(context.Background(), s, cfg, recorder, upstream.URL, upstream.Client()); recorded != 1 {
		t.Fatalf("probeOnce() recorded = %d, want 1", recorded)
	}
	_, _, headers, events := recorder.snapshot()
	if got := headers[0].Get("Authorization"); got != "" {
		t.Fatalf("probe Authorization = %q, want none while inference auth is off", got)
	}
	if len(events) != 1 || events[0].Status != "error" {
		t.Fatalf("journal events = %+v, want one error entry", events)
	}
	if !strings.Contains(events[0].Error, "503") {
		t.Fatalf("journal error = %q, want it to name the upstream status", events[0].Error)
	}
}
