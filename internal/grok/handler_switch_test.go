package grok

import (
	"errors"
	"testing"
)

func TestShouldSwitchGrokAccount(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{name: "nil", err: nil, want: false},
		{name: "403", err: errors.New("grok upstream status=403 body=forbidden"), want: true},
		{name: "429", err: errors.New("grok upstream status=429 body=too many requests"), want: true},
		{name: "rate limit code", err: errors.New("imagine websocket error: rate_limit_exceeded: Image rate limit exceeded"), want: true},
		{name: "timeout", err: errors.New("Client.Timeout exceeded while awaiting headers"), want: true},
		{name: "deadline", err: errors.New("context deadline exceeded"), want: true},
		{name: "connection reset", err: errors.New("read: connection reset by peer"), want: true},
		{name: "client canceled", err: errors.New("context canceled"), want: false},
		{name: "other", err: errors.New("grok upstream status=404 body=model not found"), want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := shouldSwitchGrokAccount(tt.err); got != tt.want {
				t.Fatalf("shouldSwitchGrokAccount(%v)=%v want=%v", tt.err, got, tt.want)
			}
		})
	}
}
