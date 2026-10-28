package session

import (
	"context"
	"encoding/json"

	"github.com/recurser/bossalib/displaystatus"
	"github.com/recurser/bossalib/kit/vcs"
	"github.com/recurser/bossd/internal/db"
)

// restore seeds the tracker from each open PR's newest snapshot. Without it a
// restarted daemon serves every session with no PR status until the poll reads
// it again, one PR at a time, and every client shows each one in progress until
// then. A merged or closed PR is left to the poll: its terminal reconcile is
// what a restart retries.
func (p *DisplayPoller) restore(ctx context.Context) {
	if p.snapshots == nil {
		return
	}
	sessions, err := p.sessions.ListActive(ctx, "")
	if err != nil {
		p.logger.Warn().Err(err).Msg("display poller: list sessions to restore statuses")
		return
	}
	for _, sess := range sessions {
		if sess.PRNumber == nil {
			continue
		}
		newest, err := p.snapshots.ListBySession(ctx, sess.ID, 1)
		if err != nil || len(newest) == 0 {
			continue
		}
		if info, ok := restoredDisplayInfo(newest[0], sess.DisplayLabel); ok {
			// Persisted snapshots do not carry the PR marker; the first live poll
			// refreshes IsPartial from its title and body after a restart.
			p.tracker.Restore(sess.ID, info, newest[0].PolledAt)
		}
	}
}

// restoredDisplayInfo rebuilds what a snapshot's poll set on the tracker. The
// snapshot keeps the status, head and checks; the rest follows from them.
// GitHub reports a PR passing, approved or awaiting review only once it is
// mergeable, so those statuses restore it mergeable, and any other leaves it
// unknown. The snapshot omits the build receipt, so it is restored from the
// label the session last persisted: only a receipted head reads ready.
func restoredDisplayInfo(snap db.CheckSnapshot, label string) (vcs.DisplayInfo, bool) {
	st := vcs.DisplayStatus(snap.ComputedStatus)
	if st == vcs.DisplayStatusUnspecified || isTerminalDisplayStatus(st) {
		return vcs.DisplayInfo{}, false
	}
	info := vcs.DisplayInfo{Status: st, HeadSHA: snap.HeadSHA}
	switch st {
	case vcs.DisplayStatusPassing, vcs.DisplayStatusApproved, vcs.DisplayStatusReview:
		mergeable := true
		info.Mergeable = &mergeable
		info.HasBuildReceipt = displaystatus.IsReadyLabel(label)
	case vcs.DisplayStatusFailing:
		info.HasFailures = true
	case vcs.DisplayStatusRejected:
		info.HasChangesRequested = true
	case vcs.DisplayStatusNeedsHuman:
		var checks []vcs.CheckResult
		if snap.RawJSON != "" && json.Unmarshal([]byte(snap.RawJSON), &checks) == nil {
			verify := vcs.ClassifyVerify(checks)
			info.VerifyReason, info.VerifyLabel = verify.Reason, verify.Label
		}
	case vcs.DisplayStatusUnspecified, vcs.DisplayStatusIdle, vcs.DisplayStatusChecking, vcs.DisplayStatusConflict,
		vcs.DisplayStatusMerged, vcs.DisplayStatusClosed, vcs.DisplayStatusDraft, vcs.DisplayStatusVerifying:
	}
	return info, true
}
