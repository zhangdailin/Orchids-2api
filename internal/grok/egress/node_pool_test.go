package egress

import (
	"context"
	"errors"
	"strings"
	"testing"

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
	if lease.UserAgent == "" {
		t.Fatal("expected a user agent")
	}
}

func TestManagerBuildLeaseHasNoBrowserIdentity(t *testing.T) {
	cfg := &config.Config{
		GrokEgressEnabled: true,
		GrokEgressNodes:   []config.EgressNodeConfig{{Name: "direct", Scope: "all"}},
	}
	m := NewManager(cfg)
	lease, err := m.Acquire(context.Background(), "cli", "acct-build")
	if err != nil {
		t.Fatalf("acquire failed: %v", err)
	}
	defer lease.Release()
	if lease.UserAgent != "" || lease.CFCookies != "" {
		t.Fatalf("Build lease leaked browser identity: ua=%q cookies=%q", lease.UserAgent, lease.CFCookies)
	}
}

func TestManagerFingerprintIncludesProxyAndSolver(t *testing.T) {
	cfg := &config.Config{GrokEgressEnabled: true, GrokFlareSolverrURL: "http://solver-a:8191"}
	m := NewManager(cfg)
	a := m.fingerprint(Node{Name: "same", URL: "http://proxy-a:8080"}, "acct")
	b := m.fingerprint(Node{Name: "same", URL: "http://proxy-b:8080"}, "acct")
	if a == b {
		t.Fatal("proxy URL change must change clearance fingerprint")
	}
	cfg.GrokFlareSolverrURL = "http://solver-b:8191"
	c := m.fingerprint(Node{Name: "same", URL: "http://proxy-a:8080"}, "acct")
	if a == c {
		t.Fatal("solver URL change must change clearance fingerprint")
	}
	if strings.Contains(a, "proxy-a") || strings.Contains(a, "solver-a") {
		t.Fatalf("fingerprint must not expose configuration: %q", a)
	}
}

func TestManagerAcquireStableFingerprint(t *testing.T) {
	cfg := &config.Config{
		GrokEgressEnabled: true,
		GrokEgressNodes:   []config.EgressNodeConfig{{Name: "direct", Scope: "all"}},
	}
	m := NewManager(cfg)
	lease1, err := m.Acquire(context.Background(), "console", "acct-2")
	if err != nil {
		t.Fatalf("acquire1 failed: %v", err)
	}
	lease1.Release()
	lease2, err := m.Acquire(context.Background(), "console", "acct-2")
	if err != nil {
		t.Fatalf("acquire2 failed: %v", err)
	}
	defer lease2.Release()
	if lease1.UserAgent != lease2.UserAgent {
		t.Fatalf("same affinity should map to same UA: %q vs %q", lease1.UserAgent, lease2.UserAgent)
	}
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

func TestClearanceInvalidateVersionPrecision(t *testing.T) {
	cfg := &config.Config{
		GrokEgressEnabled: true,
		GrokEgressNodes:   []config.EgressNodeConfig{{Name: "direct", Scope: "all"}},
	}
	m := NewManager(cfg)
	ctx := context.Background()

	lease1, err := m.Acquire(ctx, "app_chat", "acct")
	if err != nil {
		t.Fatalf("acquire1 failed: %v", err)
	}
	lease1.InvalidateClearance()
	lease1.Release()

	// Re-acquire re-solves clearance and bumps the version.
	lease2, err := m.Acquire(ctx, "app_chat", "acct")
	if err != nil {
		t.Fatalf("acquire2 failed: %v", err)
	}
	defer lease2.Release()
	if lease2.clearanceVersion == lease1.clearanceVersion {
		t.Fatal("expected a new clearance version after invalidation")
	}

	// A stale invalidation (old version) must not invalidate the new clearance.
	m.invalidateClearanceKey(lease2.clearanceKey, lease1.clearanceVersion)
	m.mu.RLock()
	state, ok := m.clearances[lease2.clearanceKey]
	m.mu.RUnlock()
	if !ok || state.invalid {
		t.Fatal("stale invalidation must not invalidate a newer clearance")
	}
}

type failingSolver struct{}

func (failingSolver) Solve(context.Context, ClearanceConfig, string) (clearanceSolution, error) {
	return clearanceSolution{}, errors.New("solver unavailable")
}

func TestAcquireFailsClosedOnSolverFailure(t *testing.T) {
	cfg := &config.Config{
		GrokEgressEnabled:          true,
		GrokEgressNodes:            []config.EgressNodeConfig{{Name: "direct", Scope: "all"}},
		GrokClearanceMode:          "flaresolverr",
		GrokFlareSolverrURL:        "http://127.0.0.1:8191",
		GrokClearanceRefreshInterv: 600,
	}
	m := NewManager(cfg)
	if m == nil {
		t.Fatal("expected manager")
	}
	m.solver = failingSolver{}
	lease, err := m.Acquire(context.Background(), "app_chat", "acct")
	if err == nil {
		lease.Release()
		t.Fatal("expected fail-closed acquire error when clearance solve fails")
	}
	if lease != nil {
		t.Fatal("expected nil lease on failure")
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
