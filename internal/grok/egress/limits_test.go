package egress

import (
	"context"
	"orchids-api/internal/config"
	"testing"
	"time"
)

func TestLeaseUsesProviderConfiguredDeadline(t *testing.T) {
	manager := NewManager(&config.Config{GrokEgressEnabled: true, GrokEgressNodes: []config.EgressNodeConfig{{Name: "limits-direct", Scope: "all"}}, GrokWebTimeout: 700, GrokConsoleTimeout: 800, GrokBuildTimeout: 900})
	for scope, seconds := range map[string]int{"app_chat": 700, "console": 800, "cli": 900} {
		lease, err := manager.Acquire(context.Background(), scope, "test")
		if err != nil {
			t.Fatal(err)
		}
		if lease.client.Timeout != time.Duration(seconds)*time.Second {
			t.Fatalf("%s timeout=%v", scope, lease.client.Timeout)
		}
		lease.Release()
	}
}
