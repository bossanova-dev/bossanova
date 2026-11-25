package session

import (
	"context"
	"time"

	"github.com/recurser/bossalib/contract/cloud/sqlutil"
	"github.com/recurser/bossalib/kit/vcs"
	"github.com/recurser/bossalib/machine"
	"github.com/recurser/bossalib/models"
	"github.com/recurser/bossd/internal/db"
)

// checkStateLatch maps an aggregate check verdict to the persisted CheckState
// latch value. It returns ok=false when the verdict carries no head SHA or is
// unknown, meaning the caller must leave any existing latch untouched.
func checkStateLatch(v vcs.CheckVerdict) (machine.CheckState, bool) {
	if v.HeadSHA == "" {
		return machine.CheckStateUnspecified, false
	}
	switch v.State {
	case vcs.CheckVerdictGreen:
		if v.DemonstratedPass() {
			return machine.CheckStatePassed, true
		}
		return machine.CheckStateUnspecified, true
	case vcs.CheckVerdictFailing:
		return machine.CheckStateFailed, true
	case vcs.CheckVerdictPending:
		return machine.CheckStatePending, true
	case vcs.CheckVerdictUnknown:
		return machine.CheckStateUnspecified, false
	default:
		return machine.CheckStateUnspecified, false
	}
}

// latchCheckState persists the evaluated check verdict for sess at head when
// either the mapped state or the head SHA changed, guarded against concurrent
// writers (such as Dispatcher or a webhook RefreshPR) that may have persisted a
// newer verdict while pollSession was in flight. Heads with an unreconciled
// cancelled workflow check are skipped so a dispatcher-recorded PASSED verdict
// is never overwritten with FAILED. Store errors are logged at Warn and never
// fail the poll.
func (p *DisplayPoller) latchCheckState(ctx context.Context, sess *models.Session, head string, checks []vcs.CheckResult, required []string) {
	if p.sessions == nil || sess == nil || vcs.HasCancelledWorkflowCheck(checks) {
		return
	}
	state, ok := checkStateLatch(vcs.EvaluateChecksRequiring(head, checks, nil, required))
	if !ok {
		return
	}
	if sess.LastCheckState == state && sess.LastCheckStateHeadSHA != nil && *sess.LastCheckStateHeadSHA == head {
		return
	}
	if latchStore, ok := p.sessions.(db.CheckStateLatchStore); ok {
		if _, err := latchStore.SetCheckStateIfUnchanged(
			ctx,
			sess.ID,
			sess.LastCheckState,
			sess.LastCheckStateHeadSHA,
			sess.LastCheckStateAt,
			state,
			head,
		); err != nil {
			p.logger.Warn().Err(err).Str("session", sess.ID).Msg("display poller: latch check state")
		}
		return
	}
	cur, err := p.sessions.Get(ctx, sess.ID)
	if err != nil {
		p.logger.Warn().Err(err).Str("session", sess.ID).Msg("display poller: get session for check state latch")
		return
	}
	if !sameCheckStateLatch(cur, sess.LastCheckState, sess.LastCheckStateHeadSHA, sess.LastCheckStateAt) {
		return
	}
	checkState := int(state)
	checkHeadSHA := head
	checkHeadSHAPtr := &checkHeadSHA
	checkObservedAt := sqlutil.TimeNow()
	checkObservedAtPtr := &checkObservedAt
	if _, err := p.sessions.Update(ctx, sess.ID, db.UpdateSessionParams{
		LastCheckState:        &checkState,
		LastCheckStateHeadSHA: &checkHeadSHAPtr,
		LastCheckStateAt:      &checkObservedAtPtr,
	}); err != nil {
		p.logger.Warn().Err(err).Str("session", sess.ID).Msg("display poller: latch check state")
	}
}

func sameCheckStateLatch(sess *models.Session, expectedState machine.CheckState, expectedHeadSHA *string, expectedAt *time.Time) bool {
	if sess == nil || sess.LastCheckState != expectedState {
		return false
	}
	if !sameOptionalString(sess.LastCheckStateHeadSHA, expectedHeadSHA) {
		return false
	}
	return sameOptionalString(sqlutil.FormatOptionalTime(sess.LastCheckStateAt), sqlutil.FormatOptionalTime(expectedAt))
}

func sameOptionalString(a, b *string) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}
