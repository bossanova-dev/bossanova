//go:build !e2e

package client

import (
	"testing"
	"time"
)

// TestProductionIgnoresRPCDeadlineEnv is the safety half of the build-tag pair:
// a production boss must not read BOSS_RPC_DEADLINE_E2E at all, so no stray
// export can shorten the bound (stranding nothing but breaking a busy daemon)
// or lengthen it (re-opening the indefinite hang BOS-723 closed).
//
// It sets the variable to a value that would be accepted by the e2e parser, so
// the test fails the moment the production variant grows an env read.
func TestProductionIgnoresRPCDeadlineEnv(t *testing.T) {
	t.Setenv("BOSS_RPC_DEADLINE_E2E", "1ms")
	if d, ok := e2eRPCDeadlineOverride(); ok {
		t.Fatalf("production e2eRPCDeadlineOverride() = %v, true; want no override", d)
	}
	if defaultRPCDeadline != 30*time.Second {
		t.Fatalf("defaultRPCDeadline = %v; want the 30s production default", defaultRPCDeadline)
	}
	// Both bound assertions are constant pins rather than guards on the
	// override wiring: in a !e2e build applyE2ERPCDeadlineOverride always
	// declines, so nothing here can move them. The load-bearing assertion in
	// this test is the one above — that the production variant declines at all.
	if slowRPCDeadline != 120*time.Second {
		t.Fatalf("slowRPCDeadline = %v; want the 120s production bound, aligned with the daemon's http.Server WriteTimeout", slowRPCDeadline)
	}
}
