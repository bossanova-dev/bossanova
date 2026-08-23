//go:build !e2e

package client

import (
	"testing"
	"time"

	"github.com/recurser/bossalib/config"
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
	got := slowRPCDeadline
	want := config.SwitchResultCeiling
	if got <= 0 || want <= 0 {
		t.Fatalf("invalid non-positive timeout operand: slowRPCDeadline (services/boss/internal/client/rpcdeadline.go) = %v, "+
			"config.SwitchResultCeiling (lib/bossalib/config/config.go) = %v", got, want)
	}
	if got != want {
		t.Fatalf("slowRPCDeadline (services/boss/internal/client/rpcdeadline.go) = %v, want config.SwitchResultCeiling "+
			"(lib/bossalib/config/config.go) = %v.\n"+
			"This equality pins the generic local-client slow RPC ceiling to the account-switch result ceiling. If "+
			"slowRPCDeadline rises, move SwitchResultCeiling and the settings-reference crossover with it; if "+
			"SwitchResultCeiling moves, keep slowRPCDeadline equal or document why the local TUI can now expire "+
			"switch results at a different boundary.\n"+
			"This guard cannot see an operator's runtime session_start_ready_deadline_seconds; it only holds the "+
			"compiled result ceiling that the derived switch budget crosses.",
			got, want)
	}
	if slowDeadlineRatio != slowRPCDeadline/defaultRPCDeadline {
		t.Fatalf("slowDeadlineRatio = %v, want slowRPCDeadline/defaultRPCDeadline = %v/%v = %v",
			slowDeadlineRatio, slowRPCDeadline, defaultRPCDeadline, slowRPCDeadline/defaultRPCDeadline)
	}
}
