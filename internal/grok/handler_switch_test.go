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

func TestShouldSwitchGrokAccount_ConsoleScenarios(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{name: "nil", err: nil, want: false},
		{name: "team rate limit", err: errors.New("grok upstream status=429 body=too many requests for team"), want: true},
		{name: "403", err: errors.New("grok upstream status=403 body=forbidden"), want: true},
		{name: "timeout", err: errors.New("context deadline exceeded"), want: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := shouldSwitchGrokAccount(tt.err); got != tt.want {
				t.Fatalf("shouldSwitchGrokAccount(%v)=%v want=%v", tt.err, got, tt.want)
			}
		})
	}
}

func TestUpstreamHTTPResponseStatus(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want int
	}{
		{name: "429", err: errors.New("grok upstream status=429 body=too many requests"), want: 429},
		{name: "403", err: errors.New("grok upstream status=403 body=forbidden"), want: 403},
		{name: "timeout", err: errors.New("context deadline exceeded"), want: 502},
		{name: "none", err: errors.New("grok upstream request failed"), want: 502},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := upstreamHTTPResponseStatus(tt.err); got != tt.want {
				t.Fatalf("upstreamHTTPResponseStatus(%v)=%v want=%v", tt.err, got, tt.want)
			}
		})
	}
}

func TestMarkAllGrokAccountStatuses(t *testing.T) {
	tests := []struct {
		name       string
		err        error
		wantMark   bool
		wantSwitch bool
	}{
		{name: "plain 403", err: errors.New("grok upstream status=403 body=forbidden"), wantMark: true, wantSwitch: true},
		{name: "anti bot 403", err: errors.New("grok upstream status=403 body=Request rejected by anti-bot rules"), wantMark: true, wantSwitch: true},
		{name: "401", err: errors.New("grok upstream status=401 body=unauthorized"), wantMark: true, wantSwitch: false},
		{name: "429", err: errors.New("grok upstream status=429 body=too many requests"), wantMark: true, wantSwitch: true},
		{name: "network", err: errors.New("read: connection reset by peer"), wantMark: true, wantSwitch: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := markAllGrokAccountStatuses(tt.err); got != tt.wantMark {
				t.Fatalf("markAllGrokAccountStatuses()=%v want mark=%v", got, tt.wantMark)
			}
			if got := shouldSwitchGrokAccount(tt.err); got != tt.wantSwitch {
				t.Fatalf("shouldSwitchGrokAccount()=%v want %v", got, tt.wantSwitch)
			}
		})
	}
}

func TestSkipExternalAttachmentFetchGrokAccountStatus(t *testing.T) {
	tests := []struct {
		name     string
		err      error
		wantMark bool
	}{
		{name: "nil", err: nil, wantMark: false},
		{name: "external cf challenge", err: errors.New("attachment upload failed: fetch url status=403 body=<html>Just a moment...</html>"), wantMark: false},
		{name: "external rate limit", err: errors.New("image upload failed: fetch url status=429 body=too many requests"), wantMark: false},
		{name: "grok upstream forbidden", err: errors.New("grok upstream status=403 body=forbidden"), wantMark: true},
		{name: "upload network", err: errors.New("grok upload failed: read: connection reset by peer"), wantMark: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := skipExternalAttachmentFetchGrokAccountStatus(tt.err); got != tt.wantMark {
				t.Fatalf("skipExternalAttachmentFetchGrokAccountStatus()=%v want mark=%v", got, tt.wantMark)
			}
		})
	}
}
