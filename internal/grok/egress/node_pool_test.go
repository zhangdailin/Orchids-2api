package egress

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"orchids-api/internal/config"
)

func TestNodesFromConfigDisabled(t *testing.T) {
	cfg := &config.Config{GrokEgressEnabled: false}
	if nodes := nodesFromConfig(cfg); len(nodes) != 0 {
		t.Fatalf("disabled egress should yield no nodes, got %d", len(nodes))
	}
}

func TestNodesFromConfig(t *testing.T) {
	cfg := &config.Config{
		GrokEgressEnabled: true,
		GrokEgressNodes: []config.EgressNodeConfig{
			{Name: "a", URL: "http://proxy1:8080", Weight: 2, Scope: "app_chat"},
			{Name: "b", URL: "", Scope: "all"}, // direct
		},
	}
	nodes := nodesFromConfig(cfg)
	if len(nodes) != 2 {
		t.Fatalf("expected 2 nodes, got %d", len(nodes))
	}
	if nodes[0].Weight != 2 {
		t.Fatalf("node a weight = %d", nodes[0].Weight)
	}
	if !nodes[0].Proxied {
		t.Fatal("node a should be proxied")
	}
	if nodes[1].Proxied {
		t.Fatal("node b should be direct")
	}
}

func TestNodeMatchesScope(t *testing.T) {
	node := Node{Name: "n", Scope: "app_chat"}
	if !nodeMatchesScope(node, "app_chat") {
		t.Fatal("app_chat node should match app_chat scope")
	}
	if nodeMatchesScope(node, "cli") {
		t.Fatal("app_chat node should not match cli scope")
	}
	all := Node{Name: "n", Scope: "all"}
	if !nodeMatchesScope(all, "cli") || !nodeMatchesScope(all, "console") {
		t.Fatal("all scope should match any scope")
	}
}

func TestManagerAcquireDisabled(t *testing.T) {
	cfg := &config.Config{GrokEgressEnabled: false}
	m := NewManager(cfg)
	if m != nil {
		t.Fatal("disabled egress should yield nil manager")
	}
}

func TestManagerAcquireDirectNode(t *testing.T) {
	cfg := &config.Config{
		GrokEgressEnabled: true,
		GrokEgressNodes:   []config.EgressNodeConfig{{Name: "direct", Scope: "all"}},
	}
	m := NewManager(cfg)
	if m == nil {
		t.Fatal("expected manager")
	}
	lease, err := m.Acquire(context.Background(), "app_chat", "acct-1")
	if err != nil {
		t.Fatalf("acquire failed: %v", err)
	}
	defer lease.Release()
}

func TestManagerUnhealthyNodeSkipped(t *testing.T) {
	cfg := &config.Config{
		GrokEgressEnabled: true,
		GrokEgressNodes: []config.EgressNodeConfig{
			{Name: "bad", Scope: "app_chat"},
			{Name: "good", Scope: "app_chat"},
		},
	}
	m := NewManager(cfg)
	m.FeedbackOutcome("bad", OutcomeServerError)
	for i := 0; i < 10; i++ {
		lease, err := m.Acquire(context.Background(), "app_chat", "acct-3")
		if err != nil {
			t.Fatalf("acquire %d failed: %v", i, err)
		}
		if lease.NodeID == "bad" {
			t.Fatalf("unhealthy node should be skipped, got %q", lease.NodeID)
		}
		lease.Release()
	}
}

func TestFeedbackHealth(t *testing.T) {
	cfg := &config.Config{GrokEgressEnabled: true}
	m := NewManager(cfg)
	m.FeedbackOutcome("n1", OutcomeSuccess)
	m.mu.RLock()
	score := m.health["n1"]
	m.mu.RUnlock()
	if score <= 0 {
		t.Fatalf("expected positive health after success, got %f", score)
	}
	m.FeedbackOutcome("n1", OutcomeServerError)
	m.mu.RLock()
	scoreAfter := m.health["n1"]
	m.mu.RUnlock()
	if scoreAfter >= score {
		t.Fatalf("expected health to drop after failure: before=%f after=%f", score, scoreAfter)
	}
}

func TestFeedbackOutcome_RateLimitKeepsHealth(t *testing.T) {
	m := NewManager(&config.Config{GrokEgressEnabled: true})
	m.FeedbackOutcome("n1", OutcomeSuccess)
	m.mu.RLock()
	before := m.health["n1"]
	m.mu.RUnlock()
	m.FeedbackOutcome("n1", OutcomeRateLimited)
	m.mu.RLock()
	after := m.health["n1"]
	m.mu.RUnlock()
	if after != before {
		t.Fatalf("rate limit should not change node health: before=%f after=%f", before, after)
	}
}

func TestParseProxyURL(t *testing.T) {
	valid := []string{
		"http://proxy1:8080",
		"socks5://user:pass@proxy1:1080",
		"socks5h://proxy1:1080",
	}
	for _, raw := range valid {
		if _, err := parseProxyURL(raw); err != nil {
			t.Fatalf("expected %q to parse, got %v", raw, err)
		}
	}
	invalid := []string{
		"https://proxy1:8443", // https proxy unsupported by the browser transport
		"socks4://proxy1:1080",
		"trojan://secret@host:443",
		"://missing-scheme",
		"http://", // no host
		"http://host:notaport",
	}
	for _, raw := range invalid {
		if _, err := parseProxyURL(raw); err == nil {
			t.Fatalf("expected %q to be rejected", raw)
		}
	}
}

func TestAcquireFailsClosedWhenNoNodes(t *testing.T) {
	m := NewManager(&config.Config{GrokEgressEnabled: true})
	if m == nil {
		t.Fatal("expected manager for enabled egress")
	}
	if _, err := m.Acquire(context.Background(), "app_chat", "acct"); err == nil {
		t.Fatal("expected error when no nodes configured")
	}
}

func TestNodeCooldownGrowsAndCaps(t *testing.T) {
	cases := map[int]time.Duration{
		0:  nodeCooldown,
		1:  nodeCooldown,
		2:  2 * nodeCooldown,
		3:  4 * nodeCooldown,
		4:  8 * nodeCooldown,
		5:  16 * nodeCooldown,
		6:  nodeCooldownMax,
		20: nodeCooldownMax,
	}
	for failures, want := range cases {
		if got := nodeCooldownFor(failures); got != want {
			t.Fatalf("nodeCooldownFor(%d) = %s, want %s", failures, got, want)
		}
		if got := nodeCooldownFor(failures); got > nodeCooldownMax {
			t.Fatalf("nodeCooldownFor(%d) = %s exceeds the cap", failures, got)
		}
	}
}

func TestFeedbackOutcomeBacksOffExponentially(t *testing.T) {
	m := &Manager{
		cfg:       &config.Config{GrokEgressEnabled: true},
		nodes:     []Node{{Name: "n1", Scope: "all", Weight: 1}},
		health:    map[string]float64{},
		unhealthy: map[string]time.Time{},
		failures:  map[string]int{},
		lastError: map[string]string{},
		lastProbe: map[string]time.Time{},
	}
	m.FeedbackOutcome("n1", OutcomeTransportError)
	first := time.Until(m.unhealthy["n1"])
	if first > nodeCooldown+2*time.Second || first < nodeCooldown-2*time.Second {
		t.Fatalf("first cooldown = %s, want about %s", first, nodeCooldown)
	}
	m.FeedbackOutcome("n1", OutcomeTransportError)
	second := time.Until(m.unhealthy["n1"])
	if second < first {
		t.Fatalf("second cooldown %s must be longer than the first %s", second, first)
	}
	if m.lastError["n1"] != "transport" {
		t.Fatalf("last error = %q, want transport", m.lastError["n1"])
	}
	// A success clears both the cooldown and the accumulated backoff.
	m.FeedbackOutcome("n1", OutcomeSuccess)
	if m.failures["n1"] != 0 {
		t.Fatalf("failures = %d, want 0 after success", m.failures["n1"])
	}
	if _, cooling := m.unhealthy["n1"]; cooling {
		t.Fatal("a successful node must leave the cooldown map")
	}
}

func TestHealthSnapshotNeverLeaksProxyURL(t *testing.T) {
	m := &Manager{
		cfg:       &config.Config{GrokEgressEnabled: true},
		nodes:     []Node{{Name: "eu-1", URL: "http://user:secret@proxy.internal:8080", Scope: "app_chat", Weight: 1}},
		health:    map[string]float64{},
		unhealthy: map[string]time.Time{},
		failures:  map[string]int{},
		lastError: map[string]string{},
		lastProbe: map[string]time.Time{},
	}
	m.FeedbackOutcome("eu-1", OutcomeServerError)
	snapshot := m.HealthSnapshot()
	if len(snapshot) != 1 {
		t.Fatalf("snapshot = %#v, want one node", snapshot)
	}
	encoded := fmt.Sprint(snapshot)
	for _, leak := range []string{"secret", "proxy.internal", "8080"} {
		if strings.Contains(encoded, leak) {
			t.Fatalf("health snapshot leaked %q: %s", leak, encoded)
		}
	}
	if snapshot[0]["failures"] != 1 || snapshot[0]["healthy"] != false {
		t.Fatalf("snapshot = %#v, want a degraded node with one failure", snapshot[0])
	}
}

func TestHealthPersistsAcrossManagers(t *testing.T) {
	dir := t.TempDir()
	cfg := &config.Config{GrokEgressEnabled: true, MediaDir: dir, GrokEgressNodes: []config.EgressNodeConfig{
		{Name: "n1", URL: "http://127.0.0.1:1", Scope: "all"},
	}}
	first := NewManager(cfg)
	if first == nil {
		t.Fatal("manager must be created when egress is enabled")
	}
	first.FeedbackOutcome("n1", OutcomeTransportError)
	first.FeedbackOutcome("n1", OutcomeTransportError)
	// The write is throttled; force a flush by clearing the throttle.
	first.mu.Lock()
	first.lastPersist = time.Time{}
	first.persistHealthLocked()
	first.mu.Unlock()

	second := NewManager(cfg)
	if second == nil {
		t.Fatal("second manager must be created")
	}
	second.mu.RLock()
	failures := second.failures["n1"]
	_, cooling := second.unhealthy["n1"]
	second.mu.RUnlock()
	if failures != 2 {
		t.Fatalf("restored failures = %d, want 2", failures)
	}
	if !cooling {
		t.Fatal("restored node must keep its cooldown")
	}
	// A node that no longer exists in the configuration must not come back.
	cfg.GrokEgressNodes = []config.EgressNodeConfig{{Name: "other", URL: "http://127.0.0.1:1", Scope: "all"}}
	third := NewManager(cfg)
	third.mu.RLock()
	_, restored := third.failures["n1"]
	third.mu.RUnlock()
	if restored {
		t.Fatal("health of a removed node must not be restored")
	}
}
