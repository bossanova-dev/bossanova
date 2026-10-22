package session

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/recurser/bossalib/alertstate"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/models"
	"github.com/recurser/bossd/internal/status"
)

// alertDisplayReader is the slice of *status.DisplayTracker the resolved-alert
// sweep reads: the PR head, display status and failure flags last polled.
type alertDisplayReader interface {
	Get(sessionID string) *status.DisplayEntry
}

// alertStallClearer is the slice of *status.RepairLeaseManager the sweep uses.
type alertStallClearer interface {
	Active(sessionID string) bool
	StalledAt(sessionID string) (at time.Time, ok bool)
	ClearStall(sessionID string) bool
}

type alertClearDeps struct {
	display   alertDisplayReader
	leases    alertStallClearer
	onCleared func(ctx context.Context, sessionID string)
}

// SetAlertClearDeps wires the resolved-alert sweep (ClearResolvedAlerts). The
// display reader is required; leases may be nil (no stall records, no lease
// guard), and onCleared — called once per cleared session so the change reaches
// the reverse stream as an UPDATED delta — may be nil.
func (l *Lifecycle) SetAlertClearDeps(display alertDisplayReader, leases alertStallClearer, onCleared func(ctx context.Context, sessionID string)) {
	l.alertClear = alertClearDeps{display: display, leases: leases, onCleared: onCleared}
}

// ClearResolvedAlerts ends latched repair alerts whose condition is over
// (BOS-1535). A stored repair-blocked / repair-failed alert, and a live
// repair-stall record, are cleared once alertstate.RepairAlertResolution says
// the head moved past the failed head, CI went green, or the PR left the
// repair-eligible states. A session whose repair lease is active is never
// touched: the in-flight round owns its repair state. Clearing writes no
// history — last_repair_head_sha and the review fingerprint stay.
//
// It returns how many sessions had an alert cleared. A per-session write
// failure is logged and the sweep continues; the joined errors are returned.
func (l *Lifecycle) ClearResolvedAlerts(ctx context.Context) (int, error) {
	deps := l.alertClear
	if deps.display == nil || l.sessions == nil {
		return 0, nil
	}
	sessions, err := l.sessions.ListActive(ctx, "")
	if err != nil {
		return 0, fmt.Errorf("clear resolved alerts: list sessions: %w", err)
	}
	cleared := 0
	var errs []error
	for _, sess := range sessions {
		if ctx.Err() != nil {
			return cleared, ctx.Err()
		}
		ok, err := l.clearResolvedAlert(ctx, deps, sess)
		if err != nil {
			errs = append(errs, err)
		}
		if ok {
			cleared++
		}
	}
	return cleared, errors.Join(errs...)
}

// RepairAlertFacts is the alertstate.RepairFacts every bossd reader of a
// session's repair alert decides currency from: the stored repair outcome,
// the last-polled display entry (nil when the session has not been polled),
// and whether the repair lease holds a live stall record, which makes the
// recorded head unusable for the head-moved leg.
func RepairAlertFacts(sess *models.Session, display *status.DisplayEntry, stalled bool) alertstate.RepairFacts {
	facts := alertstate.RepairFacts{
		FailedHeadSHA: alertstate.FailedHeadFor(sess.LastRepairHeadSHA, clampInt32(sess.LastRepairAttemptCount),
			sess.LastRepairStartedAt, sess.LastRepairBlockedAt, stalled),
		NoPR: sess.PRNumber == nil,
	}
	if display != nil {
		facts.CurrentHeadSHA = display.HeadSHA
		facts.DisplayStatus = pb.DisplayStatus(clampInt32(int(display.Status)))
		facts.HasFailures = display.HasFailures
		facts.HasChangesRequested = display.HasChangesRequested
	}
	return facts
}

// BlockedAlertFacts is the alertstate.BlockedFacts every bossd reader of a
// Blocked session's alert decides currency from: the head of its last counted
// attempt, the last-polled display entry (nil when never polled), and whether
// the blocked-run sweep saw the run's own chat working in this block.
func BlockedAlertFacts(sess *models.Session, display *status.DisplayEntry) alertstate.BlockedFacts {
	facts := alertstate.BlockedFacts{}
	if display == nil {
		return facts
	}
	facts.RunWorking = sess.StateEnteredAt != nil && !display.BlockedRunWorkingAt.IsZero() &&
		display.BlockedRunWorkingAt.Equal(*sess.StateEnteredAt)
	if sess.LastAttemptHeadSHA != nil {
		facts.AttemptHeadSHA = *sess.LastAttemptHeadSHA
	}
	facts.CurrentHeadSHA = display.HeadSHA
	facts.DisplayStatus = pb.DisplayStatus(clampInt32(int(display.Status)))
	facts.HasFailures = display.HasFailures
	facts.HasChangesRequested = display.HasChangesRequested
	facts.Mergeable = display.Mergeable
	return facts
}

// clearResolvedAlert applies the sweep to one session and reports whether an
// alert was cleared.
func (l *Lifecycle) clearResolvedAlert(ctx context.Context, deps alertClearDeps, sess *models.Session) (bool, error) {
	stored := sess.LastRepairBlockedReason != "" || sess.LastRepairAttemptCount != 0
	stalled := false
	if deps.leases != nil {
		// Active runs the lease manager's lazy evaluation first, so a stall
		// detected on this very read is visible to StalledAt below.
		if deps.leases.Active(sess.ID) {
			return false, nil
		}
		_, stalled = deps.leases.StalledAt(sess.ID)
	}
	if !stored && !stalled {
		return false, nil
	}

	reason := alertstate.RepairAlertResolution(RepairAlertFacts(sess, deps.display.Get(sess.ID), stalled))
	if reason == "" {
		return false, nil
	}

	if stored {
		if err := l.sessions.SetRepairAlertCleared(ctx, sess.ID); err != nil {
			l.logger.Warn().Err(err).Str("session_id", sess.ID).Msg("clear resolved alerts: clear repair alert failed")
			return false, fmt.Errorf("clear repair alert %s: %w", sess.ID, err)
		}
	}
	if stalled {
		deps.leases.ClearStall(sess.ID)
	}
	l.logger.Info().
		Str("session_id", sess.ID).
		Str("reason", string(reason)).
		Bool("stored_alert", stored).
		Bool("stall_record", stalled).
		Msg("cleared resolved repair alert")
	if deps.onCleared != nil {
		deps.onCleared(ctx, sess.ID)
	}
	return true, nil
}
