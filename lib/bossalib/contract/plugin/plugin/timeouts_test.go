package plugin

import (
	"testing"
	"time"
)

func TestHeadlessCapabilityTimeoutBudgets(t *testing.T) {
	if HeadlessCapabilityProbeTimeout != time.Minute {
		t.Fatalf("probe timeout = %s, want 1m", HeadlessCapabilityProbeTimeout)
	}
	if HeadlessCapabilityPreflightRPCTimeout-HeadlessCapabilityProbeTimeout < 30*time.Second {
		t.Fatalf("RPC timeout %s must leave at least 30s beyond probe timeout %s", HeadlessCapabilityPreflightRPCTimeout, HeadlessCapabilityProbeTimeout)
	}
}
