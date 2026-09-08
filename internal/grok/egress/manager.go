package egress

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"orchids-api/internal/config"
	"orchids-api/internal/util"
)

// Manager owns the proxy pool, per-node health, Cloudflare clearance cache, and
// UA rotation. It is disabled by default (GrokEgressEnabled=false) so existing
// behavior is unchanged until configured. When enabled it is fail-closed: a
// request either gets a valid lease with clearance, or an error — it never
// silently falls back to a direct client.

const (
	healthSkipThreshold = 0.2
	nodeCooldown        = 30 * time.Second
)

// FeedbackOutcome categorizes a lease result for node health scoring. It is
// deliberately coarser than upstream classification: the manager only needs to
// know whether a node should be degraded, kept, or recovered.
type FeedbackOutcome int

const (
	OutcomeSuccess FeedbackOutcome = iota
	OutcomeTransportError
	OutcomeServerError
	OutcomeChallenge // persistent Cloudflare/DPoP challenge (node cannot serve)
	OutcomeRateLimited
	OutcomeAccountBlock
	OutcomeForbidden
)

type Manager struct {
	mu         sync.RWMutex
	cfg        *config.Config
	nodes      []Node
	health     map[string]float64
	unhealthy  map[string]time.Time // node -> cooldown expiry after degradation
	usedCount  map[string]int
	sticky     map[string]string // scope|affinity -> node name
	clearances map[string]clearanceState
	lastLease  map[string]leaseKeyInfo // scope|affinity -> last fingerprint/version
	version    uint64                  // clearance generation counter
	solver     clearanceSolver
}

type clearanceState struct {
	cookies     string
	userAgent   string
	refreshedAt time.Time
	invalid     bool
	version     uint64
}

type leaseKeyInfo struct {
	fingerprint string
	version     uint64
	nodeID      string
}

var errNoClient = errors.New("egress client not initialized")
var errNoHealthyNode = errors.New("egress no healthy node for scope")
var errClearanceUnavailable = errors.New("egress clearance unavailable")

// NewManager builds an egress manager from configuration. Returns nil when
// egress is disabled.
func NewManager(cfg *config.Config) *Manager {
	if cfg == nil || !cfg.GrokEgressEnabled {
		return nil
	}
	return &Manager{
		cfg:        cfg,
		nodes:      nodesFromConfig(cfg),
		health:     make(map[string]float64),
		unhealthy:  make(map[string]time.Time),
		usedCount:  make(map[string]int),
		sticky:     make(map[string]string),
		clearances: make(map[string]clearanceState),
		lastLease:  make(map[string]leaseKeyInfo),
		solver:     flaresolverrSolver{},
	}
}

// Enabled reports whether the manager is active.
func (m *Manager) Enabled() bool {
	return m != nil && m.cfg != nil && m.cfg.GrokEgressEnabled
}

// Acquire selects a healthy node for a scope, binding a UA + clearance
// fingerprint to it, and returns a lease. affinity (e.g. account identity)
// keeps the same account on the same exit so clearance stays valid. When no
// healthy node exists, or clearance cannot be resolved for a flare-solve node,
// Acquire fails closed.
func (m *Manager) Acquire(ctx context.Context, scope, affinity string) (*Lease, error) {
	if !m.Enabled() {
		return nil, errors.New("egress disabled")
	}
	node := m.pickNode(scope, affinity)
	if node == nil {
		return nil, errNoHealthyNode
	}
	fingerprint := m.fingerprint(*node, affinity)
	ua, cookies, version, err := m.resolveFingerprint(ctx, *node, fingerprint)
	if err != nil {
		return nil, err
	}
	m.mu.Lock()
	m.lastLease[scope+"|"+affinity] = leaseKeyInfo{fingerprint: fingerprint, version: version, nodeID: node.Name}
	m.mu.Unlock()

	// Isolate the connection pool by node + fingerprint so different clearance
	// bindings never share mismatched connection/TLS state.
	poolKey := "egress:" + node.Name + "|" + fingerprint
	client := util.GetSharedBrowserHTTPClientWithHeaderTimeout(poolKey, m.cfg.GrokRequestTimeout(strings.ToLower(strings.TrimSpace(scope))), 0, proxyFuncForNode(*node))

	lease := &Lease{
		NodeID:           node.Name,
		ProxyURL:         node.URL,
		UserAgent:        ua,
		CFCookies:        cookies,
		Scope:            strings.ToLower(strings.TrimSpace(scope)),
		clearanceKey:     fingerprint,
		clearanceVersion: version,
		client:           client,
		manager:          m,
	}
	return lease, nil
}

func (m *Manager) pickNode(scope, affinity string) *Node {
	m.mu.Lock()
	defer m.mu.Unlock()

	normalizedScope := strings.ToLower(strings.TrimSpace(scope))
	now := time.Now()
	var candidates []Node
	totalWeight := 0
	anyForScope := false
	for _, node := range m.nodes {
		if !nodeMatchesScope(node, normalizedScope) {
			continue
		}
		anyForScope = true
		if m.degradedLocked(node.Name, now) {
			continue
		}
		candidates = append(candidates, node)
		totalWeight += node.Weight
	}
	if len(candidates) == 0 {
		if anyForScope {
			recordAllNodesUnhealthy(normalizedScope)
		}
		return nil
	}

	// Sticky affinity: prefer the node this affinity last used when healthy.
	stickyAffinity := affinity
	if stickyAffinity == "" {
		stickyAffinity = "default"
	}
	stickyKey := "scope:" + normalizedScope + ":" + stickyAffinity
	if prev := m.sticky[stickyKey]; prev != "" {
		for i := range candidates {
			if candidates[i].Name == prev {
				recordNodeSelection(normalizedScope)
				return &candidates[i]
			}
		}
	}

	// Weighted round-robin over healthy candidates.
	seed := m.usedCount[stickyKey]
	m.usedCount[stickyKey] = seed + 1
	threshold := (seed + 1) % totalWeight
	var pick *Node
	for i := range candidates {
		threshold -= candidates[i].Weight
		if threshold < 0 {
			pick = &candidates[i]
			break
		}
	}
	if pick == nil {
		pick = &candidates[len(candidates)-1]
	}
	m.sticky[stickyKey] = pick.Name
	recordNodeSelection(normalizedScope)
	return pick
}

// degradedLocked reports whether a node is in its failure cooldown. Callers must
// hold m.mu. A zero health score means the node has never been probed and is
// usable; only scores pushed below the skip threshold by FeedbackOutcome trip
// the cooldown.
func (m *Manager) degradedLocked(name string, now time.Time) bool {
	score := m.health[name]
	if score >= healthSkipThreshold || score == 0 {
		return false
	}
	return now.Before(m.unhealthy[name])
}

func (m *Manager) fingerprint(node Node, affinity string) string {
	return strings.ToLower(strings.TrimSpace(node.Name)) + "|" + strings.ToLower(strings.TrimSpace(affinity))
}

// resolveFingerprint returns a stable (ua, cookies, version) pair for a
// fingerprint, solving/refreshing clearance when needed. A failed solve with no
// reusable cached clearance fails closed (error) so the caller never proceeds
// with empty cookies pretending to be valid.
func (m *Manager) resolveFingerprint(ctx context.Context, node Node, fingerprint string) (string, string, uint64, error) {
	m.mu.Lock()
	state, ok := m.clearances[fingerprint]
	m.mu.Unlock()

	if ok && !state.invalid && state.cookies != "" && time.Since(state.refreshedAt) < m.refreshInterval() {
		return state.userAgent, state.cookies, state.version, nil
	}

	cfg := m.clearanceConfig()
	ua := state.userAgent
	if ua == "" {
		ua = pickUserAgent(fingerprint)
	}
	cookies := state.cookies

	if cfg.Mode == "flaresolverr" && m.solver != nil {
		solved, err := m.solver.Solve(ctx, cfg, node.URL)
		if err != nil {
			// Reuse a stale-but-not-invalidated clearance rather than failing the
			// request outright, but never write a "fresh" success cache entry.
			if ok && !state.invalid && state.cookies != "" {
				slog.Warn("egress clearance solve failed; reusing stale clearance", "node", node.Name, "error", err)
				recordClearanceSolve(false)
				return state.userAgent, state.cookies, state.version, nil
			}
			recordClearanceSolve(false)
			return "", "", 0, fmt.Errorf("%w: solve for node %q: %w", errClearanceUnavailable, node.Name, err)
		}
		if solved.UserAgent != "" {
			ua = solved.UserAgent
		}
		if strings.TrimSpace(solved.Cookies) == "" {
			// Empty cookies after a "successful" solve is not a valid clearance.
			if ok && !state.invalid && state.cookies != "" {
				slog.Warn("egress clearance solve returned no cookies; reusing stale clearance", "node", node.Name)
				recordClearanceSolve(false)
				return state.userAgent, state.cookies, state.version, nil
			}
			recordClearanceSolve(false)
			return "", "", 0, fmt.Errorf("%w: solve returned empty cookies for node %q", errClearanceUnavailable, node.Name)
		}
		cookies = solved.Cookies
		recordClearanceSolve(true)
	} else if cfg.Mode != "flaresolverr" && cookies == "" {
		// Manual mode: pull from static config when present. Empty is acceptable
		// here — the operator explicitly chose manual without clearance.
		cookies = m.manualClearance()
	}

	m.mu.Lock()
	m.version++
	state = clearanceState{
		cookies:     cookies,
		userAgent:   ua,
		refreshedAt: time.Now(),
		version:     m.version,
	}
	m.clearances[fingerprint] = state
	m.mu.Unlock()
	return ua, cookies, state.version, nil
}

func (m *Manager) manualClearance() string {
	if m == nil || m.cfg == nil {
		return ""
	}
	parts := make([]string, 0, 2)
	if v := strings.TrimSpace(m.cfg.GrokConfigCFClearance); v != "" {
		parts = append(parts, "cf_clearance="+v)
	}
	if v := strings.TrimSpace(m.cfg.GrokConfigCFBM); v != "" {
		parts = append(parts, "__cf_bm="+v)
	}
	return strings.Join(parts, "; ")
}

func (m *Manager) clearanceConfig() ClearanceConfig {
	if m == nil || m.cfg == nil {
		return ClearanceConfig{Mode: "manual", TargetURL: "https://grok.com"}
	}
	return ClearanceConfig{
		Mode:            m.cfg.GrokClearanceModeOrDefault(),
		FlareSolverrURL: strings.TrimSpace(m.cfg.GrokFlareSolverrURL),
		TargetURL:       "https://grok.com",
		Timeout:         time.Minute,
	}
}

func (m *Manager) refreshInterval() time.Duration {
	if m == nil || m.cfg == nil {
		return 10 * time.Minute
	}
	return time.Duration(m.cfg.GrokClearanceRefreshIntervalOrDefault()) * time.Second
}

// FeedbackOutcome updates a node's health score from a classified outcome.
// Success recovers, transport/server/challenge degrade, and 429/account issues
// leave the node untouched (they are not the node's fault).
func (m *Manager) FeedbackOutcome(nodeID string, outcome FeedbackOutcome) {
	if m == nil || nodeID == "" {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	score := m.health[nodeID]
	wasDegraded := score < healthSkipThreshold && score != 0

	switch outcome {
	case OutcomeSuccess:
		newScore := score + (1.0-score)*0.1
		m.health[nodeID] = newScore
		if wasDegraded {
			delete(m.unhealthy, nodeID)
			recordNodeRecovery(m.scopeForNodeLocked(nodeID))
		}
	case OutcomeTransportError, OutcomeServerError, OutcomeChallenge:
		// Failures push below zero so a fresh node (score 0) degrades on the
		// first failure, and a cooldown window lets it retry/recover later.
		m.health[nodeID] = score*0.5 - 0.1
		m.unhealthy[nodeID] = time.Now().Add(nodeCooldown)
		if wasDegraded {
			recordNodeFailure(m.scopeForNodeLocked(nodeID), outcomeReason(outcome))
		}
	case OutcomeRateLimited, OutcomeAccountBlock, OutcomeForbidden:
		// No health change: the request failed for account/team-level reasons.
	}
}

func (m *Manager) scopeForNodeLocked(nodeID string) string {
	for _, node := range m.nodes {
		if node.Name == nodeID {
			return strings.ToLower(strings.TrimSpace(node.Scope))
		}
	}
	return "all"
}

func outcomeReason(outcome FeedbackOutcome) string {
	switch outcome {
	case OutcomeServerError:
		return "server"
	case OutcomeChallenge:
		return "challenge"
	default:
		return "transport"
	}
}

// InvalidateAffinityClearance invalidates the clearance used by the last lease
// for a scope+affinity. Used by request paths that do not hold the lease
// directly (e.g. CLI) after classifying a Cloudflare challenge.
func (m *Manager) InvalidateAffinityClearance(scope, affinity string) {
	if m == nil {
		return
	}
	m.mu.RLock()
	info, ok := m.lastLease[scope+"|"+affinity]
	m.mu.RUnlock()
	if !ok || info.fingerprint == "" {
		return
	}
	m.invalidateClearanceKey(info.fingerprint, info.version)
}

// FeedbackAffinityOutcome applies a node-health outcome to the node most
// recently leased for a scope+affinity. Used by request paths that do not hold
// the lease directly (e.g. CLI) to record a persistent challenge after retry.
func (m *Manager) FeedbackAffinityOutcome(scope, affinity string, outcome FeedbackOutcome) {
	if m == nil {
		return
	}
	m.mu.RLock()
	info, ok := m.lastLease[scope+"|"+affinity]
	m.mu.RUnlock()
	if !ok || info.nodeID == "" {
		return
	}
	m.FeedbackOutcome(info.nodeID, outcome)
}

// invalidateClearanceKey marks a fingerprint's clearance stale. When version is
// non-zero it only applies to that exact generation, so a concurrent re-solve
// that bumped the version is never invalidated by a stale call.
func (m *Manager) invalidateClearanceKey(key string, version uint64) {
	if m == nil || key == "" {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	state, ok := m.clearances[key]
	if !ok {
		return
	}
	if version != 0 && state.version != version {
		return
	}
	state.invalid = true
	m.clearances[key] = state
	recordClearanceInvalidation()
}
