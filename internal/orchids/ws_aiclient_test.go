package orchids

import (
	"sync"
	"testing"

	"orchids-api/internal/upstream"
)

func TestHandleOrchidsMessageCreditsExhausted(t *testing.T) {
	t.Parallel()

	c := &Client{}
	state := &requestState{}
	var got []upstream.SSEMessage
	var fsWG sync.WaitGroup

	msg := map[string]interface{}{
		"type": EventCreditsExhausted,
		"data": map[string]interface{}{
			"message": "You have run out of credits. Please upgrade your plan to continue.",
		},
	}

	shouldBreak := c.handleOrchidsMessage(
		msg,
		[]byte(`{"type":"coding_agent.credits_exhausted"}`),
		state,
		func(m upstream.SSEMessage) { got = append(got, m) },
		nil,
		nil,
		&fsWG,
		"",
	)

	if !shouldBreak {
		t.Fatal("expected credits exhausted to break message loop")
	}
	if state.errorMsg == "" {
		t.Fatal("expected credits exhausted to set request error")
	}
	if len(got) != 1 {
		t.Fatalf("expected one forwarded error message, got %d", len(got))
	}
	if got[0].Type != "error" {
		t.Fatalf("expected error message type, got %q", got[0].Type)
	}
	if got[0].Event["code"] != "credits_exhausted" {
		t.Fatalf("expected credits_exhausted code, got %#v", got[0].Event["code"])
	}
}
