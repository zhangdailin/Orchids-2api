package egress

import (
	"context"
	"testing"
	"time"

	"orchids-api/internal/config"
)

func TestLeaseUsesBuildConfiguredDeadline(t *testing.T) {
	manager := NewManager(&config.Config{GrokEgressEnabled: true, GrokEgressNodes: []config.EgressNodeConfig{{Name: "limits-direct", Scope: "all"}}, GrokBuildTimeout: 900})
	lease, err := manager.Acquire(context.Background(), "cli", "test")
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Release()
	if lease.client.Timeout != 900*time.Second {
		t.Fatalf("timeout=%v", lease.client.Timeout)
	}
}
