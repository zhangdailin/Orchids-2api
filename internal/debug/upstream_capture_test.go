package debug

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
)

func TestUpstreamCaptureRetainsInterleavedAttempts(t *testing.T) {
	ctx, c := WithCapture(context.Background(), "retry")
	headers := http.Header{"Authorization": []string{"Bearer private-credential"}, "Cookie": []string{"session-private"}}
	a := BeginUpstream(ctx, "POST", "https://first.invalid", headers, []byte(`{"input":"first"}`))
	b := BeginUpstream(ctx, "POST", "https://second.invalid", nil, []byte(`{"input":"second"}`))
	a.Response(&http.Response{StatusCode: 429, Header: http.Header{}}, nil)
	b.Response(&http.Response{StatusCode: 200, Header: http.Header{}}, nil)
	// Response A arrives after B starts. It must still belong to A.
	for _, tc := range []struct {
		a    *UpstreamAttempt
		body string
	}{{b, "second response"}, {a, "first response"}} {
		got, err := io.ReadAll(tc.a.CaptureBody(io.NopCloser(strings.NewReader(tc.body))))
		if err != nil || string(got) != tc.body {
			t.Fatal("tee changed data")
		}
	}
	sections := map[string]string{}
	for _, s := range c.Bundle().Sections {
		sections[s.Name] = s.Payload
		if strings.Contains(s.Payload, "private-credential") || strings.Contains(s.Payload, "session-private") {
			t.Fatal("credential persisted")
		}
	}
	for _, tc := range []struct{ name, want string }{
		{"upstream_001_request.json", "first.invalid"}, {"upstream_002_request.json", "second.invalid"},
		{"upstream_001_response.txt", "first response"}, {"upstream_002_response.txt", "second response"},
		{"upstream_001_result.json", "429"}, {"upstream_002_result.json", "200"},
	} {
		if !strings.Contains(sections[tc.name], tc.want) {
			t.Fatalf("%s: %q", tc.name, sections[tc.name])
		}
	}
}

func TestLoggerRetryRequestsAreNotOverwritten(t *testing.T) {
	ctx, c := WithCapture(context.Background(), "logger")
	l := NewForContext(ctx, false, false)
	for i := 1; i <= 2; i++ {
		l.LogUpstreamRequest(fmt.Sprintf("https://attempt-%d.invalid", i), nil, map[string]int{"attempt": i})
		l.LogUpstreamSSE("data", fmt.Sprintf("response-%d", i))
	}
	b := c.Bundle()
	if len(b.Sections) != 4 {
		t.Fatalf("retained sections=%d", len(b.Sections))
	}
}

func TestCaptureSectionAndTotalLimitsReportTruncation(t *testing.T) {
	_, c := WithCapture(context.Background(), "limits")
	for i := 0; i < maxCaptureSections+1; i++ {
		c.Append(fmt.Sprintf("small-%d", i), "x")
	}
	if !c.Bundle().Truncated {
		t.Fatal("silently discarded sections")
	}
	_, c = WithCapture(context.Background(), "bytes")
	for i := 0; i < 32; i++ {
		c.Append(fmt.Sprintf("large-%d", i), strings.Repeat("x", maxCaptureBytes))
	}
	b := c.Bundle()
	if !b.Truncated || b.Bytes > maxBundleBytes {
		t.Fatal("unbounded or unmarked", b.Bytes, b.Truncated)
	}
}
