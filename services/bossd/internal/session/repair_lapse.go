package session

import (
	"time"

	"github.com/recurser/bossalib/machine"
	"github.com/recurser/bossalib/models"
)

// RepairActivity reports whether a session holds a live repair lease. It
// mirrors taskorchestrator.RepairActivity and is satisfied by
// *status.RepairLeaseManager, whose Active value is the one hydrated onto
// Session.repair_active.
type RepairActivity interface{ Active(sessionID string) bool }

// RepairLapseGrace is how long a session may sit in FixingChecks without a
// live repair lease before the PR's facts release it (BOS-2162). The repair
// plugin sweeps every minute and takes the lease only once its chat starts, so
// five minutes covers a normal start with room to spare while still releasing
// a declined session within about two poll sweeps.
const RepairLapseGrace = 5 * time.Minute

// repairLapsed reports whether sess is in FixingChecks with no live repair
// lease and its grace has passed, so a check or conflict result may release it.
// A nil activity disables the release, keeping unwired callers on the
// historical behaviour. A missing StateEnteredAt counts as past the grace:
// nothing shows a repair started recently.
func repairLapsed(sess *models.Session, activity RepairActivity, now time.Time) bool {
	if sess.State != machine.FixingChecks || activity == nil || activity.Active(sess.ID) {
		return false
	}
	return sess.StateEnteredAt == nil || now.Sub(*sess.StateEnteredAt) >= RepairLapseGrace
}

// repairLapseElapsed is how long sess has been in its state, for the release
// log line; zero when the entry time is unknown.
func repairLapseElapsed(sess *models.Session, now time.Time) time.Duration {
	if sess.StateEnteredAt == nil {
		return 0
	}
	return now.Sub(*sess.StateEnteredAt)
}

// isRepairReleaseEvent reports whether ev is one of the check or conflict
// results that releases a lapsed FixingChecks session.
func isRepairReleaseEvent(ev machine.Event) bool {
	return ev == machine.ChecksPassed || ev == machine.ChecksFailed || ev == machine.ConflictDetected
}
