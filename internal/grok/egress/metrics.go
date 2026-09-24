package egress

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// Prometheus metrics for the egress layer. Labels are deliberately low
// cardinality: Build CLI scope and outcome. They never carry proxy
// URLs, proxy credentials, cookies, OAuth tokens, User-Agents, affinities
// or account identifiers.

var (
	grokEgressSelections = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: "grok",
		Subsystem: "egress",
		Name:      "node_selections_total",
		Help:      "Egress node selections by scope.",
	}, []string{"scope"})

	grokEgressNodeFailures = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: "grok",
		Subsystem: "egress",
		Name:      "node_failures_total",
		Help:      "Egress node degradations by scope and reason (transport|server|challenge).",
	}, []string{"scope", "reason"})

	grokEgressNodeRecoveries = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: "grok",
		Subsystem: "egress",
		Name:      "node_recoveries_total",
		Help:      "Egress node recoveries after a successful response by scope.",
	}, []string{"scope"})

	grokEgressAllNodesUnhealthy = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: "grok",
		Subsystem: "egress",
		Name:      "all_nodes_unhealthy_total",
		Help:      "Times no healthy node was available for a scope.",
	}, []string{"scope"})
)

func recordNodeSelection(scope string) {
	grokEgressSelections.WithLabelValues(scope).Inc()
}

func recordNodeFailure(scope, reason string) {
	grokEgressNodeFailures.WithLabelValues(scope, reason).Inc()
}

func recordNodeRecovery(scope string) {
	grokEgressNodeRecoveries.WithLabelValues(scope).Inc()
}

func recordAllNodesUnhealthy(scope string) {
	grokEgressAllNodesUnhealthy.WithLabelValues(scope).Inc()
}
