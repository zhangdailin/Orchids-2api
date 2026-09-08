package handler

import (
	"bytes"
	"net/http"
	"testing"
)

func TestComputeRequestHash_ChangesWithAuthPathBody(t *testing.T) {
	h := &Handler{}
	mkReq := func(path, auth string) *http.Request {
		r, _ := http.NewRequest(http.MethodPost, "http://example.com"+path, bytes.NewReader([]byte("{}")))
		if auth != "" {
			r.Header.Set("Authorization", auth)
		}
		return r
	}
	bodyA := []byte(`{"a":1}`)
	bodyB := []byte(`{"a":2}`)

	h1 := h.computeRequestHash(mkReq("/v1/messages", "Bearer x"), bodyA)
	h2 := h.computeRequestHash(mkReq("/v1/messages", "Bearer x"), bodyA)
	if h1 != h2 {
		t.Fatalf("expected stable hash, got %q vs %q", h1, h2)
	}

	if h1 == h.computeRequestHash(mkReq("/v1/messages", "Bearer y"), bodyA) {
		t.Fatalf("expected auth to affect hash")
	}
	if h1 == h.computeRequestHash(mkReq("/v1/other", "Bearer x"), bodyA) {
		t.Fatalf("expected path to affect hash")
	}
	if h1 == h.computeRequestHash(mkReq("/v1/messages", "Bearer x"), bodyB) {
		t.Fatalf("expected body to affect hash")
	}
}
