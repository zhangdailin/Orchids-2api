package warp

import (
	"errors"
	"fmt"
	"testing"
)

func TestRequestIDFromErrorSurvivesWrapping(t *testing.T) {
	base := errors.New("request timed out")
	err := fmt.Errorf("stream failed: %w", AttachRequestMetadata(base, "conversation-1", "request-1"))
	if got := RequestIDFromError(err); got != "request-1" {
		t.Fatalf("RequestIDFromError() = %q, want request-1", got)
	}
	if !errors.Is(err, base) {
		t.Fatal("request ID wrapper must preserve the original error")
	}
}
