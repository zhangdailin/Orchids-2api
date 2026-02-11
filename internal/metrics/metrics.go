// Package metrics provides Prometheus metrics for the Orchids API.
package metrics

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

const namespace = "orchids"

var (
	// RequestsTotal counts total requests by method, path, and status.
	RequestsTotal = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: namespace,
			Name:      "http_requests_total",
			Help:      "Total number of HTTP requests.",
		},
		[]string{"method", "path", "status"},
	)

	// RequestDuration measures request latency in seconds.
	RequestDuration = promauto.NewHistogramVec(
		prometheus.HistogramOpts{
			Namespace: namespace,
			Name:      "http_request_duration_seconds",
			Help:      "HTTP request duration in seconds.",
			Buckets:   []float64{.005, .01, .025, .05, .1, .25, .5, 1, 2.5, 5, 10, 30, 60, 120},
		},
		[]string{"method", "path"},
	)

	// ActiveConnections tracks current active connections.
	ActiveConnections = promauto.NewGauge(
		prometheus.GaugeOpts{
			Namespace: namespace,
			Name:      "active_connections",
			Help:      "Current number of active connections.",
		},
	)

	// UpstreamRequestsTotal counts upstream API calls.
	UpstreamRequestsTotal = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: namespace,
			Name:      "upstream_requests_total",
			Help:      "Total number of upstream API requests.",
		},
		[]string{"account", "status"},
	)

	// UpstreamDuration measures upstream API latency.
	UpstreamDuration = promauto.NewHistogramVec(
		prometheus.HistogramOpts{
			Namespace: namespace,
			Name:      "upstream_request_duration_seconds",
			Help:      "Upstream API request duration in seconds.",
			Buckets:   []float64{.1, .25, .5, 1, 2.5, 5, 10, 30, 60, 120, 300},
		},
		[]string{"account"},
	)

	// TokensProcessed counts input/output tokens.
	TokensProcessed = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: namespace,
			Name:      "tokens_processed_total",
			Help:      "Total number of tokens processed.",
		},
		[]string{"direction"}, // "input" or "output"
	)

	// CacheHits counts cache hits and misses.
	CacheHits = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: namespace,
			Name:      "cache_operations_total",
			Help:      "Total cache operations.",
		},
		[]string{"cache", "result"}, // cache: "summary", result: "hit" or "miss"
	)

	// ToolCalls counts tool invocations.
	ToolCalls = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: namespace,
			Name:      "tool_calls_total",
			Help:      "Total tool calls.",
		},
		[]string{"tool"},
	)

	// ErrorsTotal counts errors by type.
	ErrorsTotal = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: namespace,
			Name:      "errors_total",
			Help:      "Total errors by type.",
		},
		[]string{"type"},
	)

	// AccountConnections tracks connections per account.
	AccountConnections = promauto.NewGaugeVec(
		prometheus.GaugeOpts{
			Namespace: namespace,
			Name:      "account_connections",
			Help:      "Current connections per account.",
		},
		[]string{"account"},
	)
)
