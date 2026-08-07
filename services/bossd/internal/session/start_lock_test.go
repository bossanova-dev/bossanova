package session

import (
	"testing"
	"time"

	gitpkg "github.com/recurser/bossd/internal/git"
)

// TestTargetStartLockMarginCoversTheWaitsItEnumerates keeps TargetStartLockTimeout's
// justification honest.
//
// That constant is BootstrapTimeout plus a two-minute margin, and the margin is
// documented as the rest of the hold: the start-path lock wait, plus — on the
// failure path — the artifact cleanup's two clone-gate acquisitions. Those are
// the parts of the hold that can be spent waiting on someone else, so they are
// the parts the margin has to cover.
//
// This is a real trip wire, not arithmetic for its own sake. The cleanup gate
// acquisitions were briefly given the CREATE-sized gitpkg.RepoCloneGateTimeout
// (20 minutes), which made a single failure cleanup able to hold its target lock
// for roughly forty minutes — twenty times the margin the comment claims. That
// change compiled, passed every test, and left the prose above it silently
// false.
func TestTargetStartLockMarginCoversTheWaitsItEnumerates(t *testing.T) {
	t.Parallel()

	margin := TargetStartLockTimeout - BootstrapTimeout
	if margin <= 0 {
		t.Fatalf("TargetStartLockTimeout (%s) leaves no margin over BootstrapTimeout (%s)", TargetStartLockTimeout, BootstrapTimeout)
	}

	// Purge then reap: two acquisitions, sequential, on the failure path.
	const cleanupGateAcquisitions = 2
	enumerated := StartPathLockTimeout + cleanupGateAcquisitions*gitpkg.RepoCloneCleanupGateTimeout
	if enumerated > margin {
		t.Fatalf("the waits TargetStartLockTimeout's comment enumerates total %s, which exceeds its %s margin:\n"+
			"  StartPathLockTimeout            = %s\n"+
			"  %d x RepoCloneCleanupGateTimeout = %s\n"+
			"either shorten a budget or rewrite the justification in start_lock.go",
			enumerated, margin, StartPathLockTimeout,
			cleanupGateAcquisitions, cleanupGateAcquisitions*gitpkg.RepoCloneCleanupGateTimeout)
	}
}

// TestCleanupCloneGateIsNotTheCreateBudget pins the DIRECTION of the split the
// two clone-gate budgets encode, independently of the arithmetic above.
//
// A create that finds the gate held has no alternative but to outwait the
// holder — there is no other route to its worktree. A cleanup that finds it held
// is giving up on artifacts belonging to a session that is already dead, on a
// caller that has no deadline of its own.
//
// Note what this is NOT: giving up is not a deferral. The row naming those
// artifacts is deleted, or moved to Blocked and out of the sweep's state set,
// immediately afterwards, and a surviving branch pushes the next same-titled
// create onto a `<branch>-2` path — so the cleanup is abandoned and logged at
// Error rather than retried. The trade is one abandoned worktree against
// stalling the shared poller tick and holding a target lock for tens of minutes,
// and it still points the same way. Collapsing the two budgets back into one
// number is the regression, whichever way it is collapsed.
func TestCleanupCloneGateIsNotTheCreateBudget(t *testing.T) {
	t.Parallel()

	if gitpkg.RepoCloneCleanupGateTimeout >= gitpkg.RepoCloneGateTimeout {
		t.Fatalf("RepoCloneCleanupGateTimeout (%s) must stay well under the create-sized RepoCloneGateTimeout (%s)",
			gitpkg.RepoCloneCleanupGateTimeout, gitpkg.RepoCloneGateTimeout)
	}
	// Cleanup runs on paths with no deadline of their own, including the daemon's
	// shared 2-minute poller tick. A budget at or above that stalls the ticker
	// that drives stranded-cron recovery as well as this sweep.
	if gitpkg.RepoCloneCleanupGateTimeout >= 2*time.Minute {
		t.Fatalf("RepoCloneCleanupGateTimeout (%s) is long enough to stall the daemon poller tick it runs on",
			gitpkg.RepoCloneCleanupGateTimeout)
	}
}
