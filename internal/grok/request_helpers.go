package grok

import (
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"orchids-api/internal/grok/egress"
)

// doUpstreamHTTP owns response execution, decompression and SSE idleness for
// Web, Console and Build. Callers supply authentication and replay policy.
func doUpstreamHTTP(req *http.Request, do func(*http.Request) (*http.Response, error), idle time.Duration) (*http.Response, error) {
	resp, err := do(req)
	if err != nil {
		return nil, err
	}
	if err := decodeHTTPResponseBody(resp); err != nil {
		_ = resp.Body.Close()
		return nil, fmt.Errorf("grok upstream decode failed: %w", err)
	}
	if idle > 0 && resp.StatusCode == http.StatusOK && strings.Contains(strings.ToLower(resp.Header.Get("Content-Type")), "text/event-stream") {
		resp.Body = wrapBuildSemanticIdle(resp.Body, idle)
	}
	return resp, nil
}

// leaseResponseBody wraps an upstream response body so the egress lease is
// released when the body is closed (or discarded), on every exit path.
type leaseResponseBody struct {
	io.ReadCloser
	release func()
}

func (b *leaseResponseBody) Close() error {
	err := b.ReadCloser.Close()
	if b.release != nil {
		b.release()
		b.release = nil
	}
	return err
}

// egressOutcomeForKind maps a classified upstream error kind to the coarser
// node-health outcome. Persistent challenges degrade the node (it cannot serve
// this request); rate limits and account issues do not.
func egressOutcomeForKind(kind UpstreamErrorKind) egress.FeedbackOutcome {
	switch kind {
	case UpstreamErrorCloudflareChallenge, UpstreamErrorDPoPChallenge:
		return egress.OutcomeChallenge
	case UpstreamErrorRateLimited:
		return egress.OutcomeRateLimited
	case UpstreamErrorAccountBlock:
		return egress.OutcomeAccountBlock
	case UpstreamErrorGenericForbidden:
		return egress.OutcomeForbidden
	default:
		return egress.OutcomeTransportError
	}
}
