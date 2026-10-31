// Package alertstate decides whether a latched session alert is still current.
//
// bossd persists some alerts (repair blocked, repair failed, repair stalled)
// and the clients derive others from history (the auth-wedge hint). Each one
// needs an end condition, and every reader — the bossd clearing sweep, the TUI
// hints, the attention producer — must agree on it, so the rules live here as
// pure predicates.
package alertstate

import (
	"time"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

// RepairFacts is what a reader knows about a session's repair alert.
type RepairFacts struct {
	// FailedHeadSHA is the PR head the last repair round ran against
	// (Session.last_repair_head_sha). Empty when unknown.
	FailedHeadSHA string
	// CurrentHeadSHA is the PR head as last polled. Empty when unknown.
	CurrentHeadSHA string
	// DisplayStatus is the PR's polled display status.
	DisplayStatus       pb.DisplayStatus
	HasFailures         bool
	HasChangesRequested bool
	// NoPR is true only when the reader KNOWS the session has no PR. A reader
	// that cannot tell leaves it false, which keeps the alert.
	NoPR bool
}

// FailedHeadFor returns the head a repair alert's head-moved leg may compare
// against, or "" when that leg must not apply. last_repair_head_sha is written
// only when a real round's outcome is recorded, so it names the alert's head
// only when a failed round exists (attemptCount > 0) and no blocked refusal is
// newer than that round's start. A blocked-only or blocked-newer alert, or a
// live stall (whose round has no recorded outcome yet), may be on a newer head
// than the recorded one; it clears via the CI-green / left-states legs only.
func FailedHeadFor(headSHA string, attemptCount int32, startedAt, blockedAt *time.Time, stalled bool) string {
	if stalled || attemptCount <= 0 {
		return ""
	}
	if blockedAt != nil && (startedAt == nil || blockedAt.After(*startedAt)) {
		return ""
	}
	return headSHA
}

// ClearReason names why a repair alert is no longer current. The empty value
// means the alert is still current.
type ClearReason string

const (
	// ClearHeadMoved: the PR head advanced past the head the repair failed on.
	ClearHeadMoved ClearReason = "head-moved"
	// ClearCIGreen: CI settled green (or is re-running clean) since the failure.
	ClearCIGreen ClearReason = "ci-green"
	// ClearLeftRepairStates: the PR is merged, closed, a draft, or gone.
	ClearLeftRepairStates ClearReason = "pr-left-repair-states"
	// ClearRunWorking: a blocked run's own agent is working on a turn the
	// block did not see, or parked on a callback it armed in the block. It
	// lasts only while the agent works or the callback is live.
	ClearRunWorking ClearReason = "run-working"
	// ClearCIPending: the PR's checks are running with nothing failed yet, so
	// whether the block still needs the operator is not known. It lasts only
	// while they run: a failure or a settled PR that is not green and
	// mergeable makes the alert current again.
	ClearCIPending ClearReason = "ci-pending"
)

// RepairAlertResolution returns why the repair alert described by f has ended,
// or "" while it is still current. Unknown inputs (an empty head, an unpolled
// status) never end an alert: the failure mode is a stale alert, not a lost one.
func RepairAlertResolution(f RepairFacts) ClearReason {
	if f.NoPR || leftRepairStates(f.DisplayStatus) {
		return ClearLeftRepairStates
	}
	if f.FailedHeadSHA != "" && f.CurrentHeadSHA != "" && f.FailedHeadSHA != f.CurrentHeadSHA {
		return ClearHeadMoved
	}
	if CIResolved(f.DisplayStatus, f.HasFailures, f.HasChangesRequested) {
		return ClearCIGreen
	}
	return ""
}

// BlockedFacts describes the last counted attempt, the PR's current polled
// state, and whether the run's own agent is working again.
type BlockedFacts struct {
	AttemptHeadSHA, CurrentHeadSHA   string
	DisplayStatus                    pb.DisplayStatus
	HasFailures, HasChangesRequested bool
	Mergeable                        *bool
	// RunWorking: the run's own chat is mid-turn on a message newer than the
	// block, or parked on a progress callback it armed in the block. The alert
	// is suspended while it holds, not resolved.
	RunWorking bool
}

// BlockedResolution returns why a blocked alert has ended. A working agent
// suspends it, and so do checks still running with nothing failed; only settled, mergeable,
// non-draft PRs clear it. Unknown facts keep the alert current.
func BlockedResolution(f BlockedFacts) ClearReason {
	if f.RunWorking {
		return ClearRunWorking
	}
	if f.DisplayStatus == pb.DisplayStatus_DISPLAY_STATUS_CHECKING && !f.HasFailures && !f.HasChangesRequested {
		return ClearCIPending
	}
	if f.Mergeable == nil || !*f.Mergeable || f.HasFailures || f.HasChangesRequested {
		return ""
	}
	switch f.DisplayStatus {
	case pb.DisplayStatus_DISPLAY_STATUS_PASSING,
		pb.DisplayStatus_DISPLAY_STATUS_APPROVED,
		pb.DisplayStatus_DISPLAY_STATUS_REVIEW,
		pb.DisplayStatus_DISPLAY_STATUS_VERIFYING,
		pb.DisplayStatus_DISPLAY_STATUS_NEEDS_HUMAN:
		return ClearCIGreen
	default:
		return ""
	}
}

// BlockedAlertCurrent reports whether the blocked alert described by f remains current.
func BlockedAlertCurrent(f BlockedFacts) bool { return BlockedResolution(f) == "" }

// leftRepairStates reports whether the PR is in a state repair never acts on.
func leftRepairStates(status pb.DisplayStatus) bool {
	switch status {
	case pb.DisplayStatus_DISPLAY_STATUS_MERGED,
		pb.DisplayStatus_DISPLAY_STATUS_CLOSED,
		pb.DisplayStatus_DISPLAY_STATUS_DRAFT:
		return true
	default:
		return false
	}
}

// RepairAlertCurrent reports whether the repair alert described by f is still
// current.
func RepairAlertCurrent(f RepairFacts) bool {
	return RepairAlertResolution(f) == ""
}

// CIResolved is the "CI is green" rule the TUI used to hide a stale repair
// failure: a passing (or merged/closed) PR, or a settled/in-flight state with no
// failing checks and no outstanding changes request.
func CIResolved(status pb.DisplayStatus, hasFailures, hasChangesRequested bool) bool {
	switch status {
	case pb.DisplayStatus_DISPLAY_STATUS_MERGED,
		pb.DisplayStatus_DISPLAY_STATUS_CLOSED,
		pb.DisplayStatus_DISPLAY_STATUS_PASSING:
		return true
	case pb.DisplayStatus_DISPLAY_STATUS_CHECKING,
		// The verify statuses replace the Checking a pending boss/verify used to
		// produce, and ordinary CI has settled under them (BOS-1382).
		pb.DisplayStatus_DISPLAY_STATUS_VERIFYING,
		pb.DisplayStatus_DISPLAY_STATUS_NEEDS_HUMAN:
		return !hasFailures && !hasChangesRequested
	default:
		return false
	}
}

// WedgeRecovery is what a reader knows about whether a session got past a
// lost login. Zero fields mean "nothing known", never "recovered". Every
// reader — the TUI hint, the web hint, the bossd session timeline — fills it
// from the same two pb.Session facts, so they reach the same answer.
type WedgeRecovery struct {
	// LastAgentActivityAt is the newest real agent output across the
	// session's chats: pb.Session.last_agent_activity_at as bossd hydrates
	// it. bossd fills it from the live chat tracker and, when the session's
	// newest lost-login or usage-limit event gave up and live output is not
	// after it, from the persisted transcripts and agent runs after that
	// give-up, so a bossd restart or a stopped chat does not forget a
	// recovery.
	LastAgentActivityAt time.Time
	// PRDone is true when the session's PR is merged or closed: its work is over.
	PRDone bool
}

// RecoveredSince reports whether anything after t shows the session past the
// lost login: the PR is done, or real agent output came after t. An event
// with no timestamp cannot be ordered, so only PRDone recovers it.
func (r WedgeRecovery) RecoveredSince(t time.Time) bool {
	if r.PRDone {
		return true
	}
	if t.IsZero() {
		return false
	}
	return r.LastAgentActivityAt.After(t)
}

// AuthWedgeCurrent reports whether the session is still auth-wedged: the newest
// rotation event (events are newest-first) is a respawn-cap-exhausted episode
// AND rec does not show recovery since it (WedgeRecovery.RecoveredSince). A
// healthy chat writes no rotation event, so without the recovery rule nothing
// would end the wedge. The session timeline applies the same rule to its open
// rotation incident.
func AuthWedgeCurrent(events []*pb.RotationEvent, rec WedgeRecovery) bool {
	if len(events) == 0 {
		return false
	}
	latest := events[0]
	if latest.GetOutcome() != pb.RotationOutcome_ROTATION_OUTCOME_RESPAWN_CAP_EXHAUSTED {
		return false
	}
	var at time.Time
	if ts := latest.GetCreatedAt(); ts != nil {
		at = ts.AsTime()
	}
	return !rec.RecoveredSince(at)
}
