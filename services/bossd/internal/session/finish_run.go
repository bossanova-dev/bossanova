package session

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sort"
	"time"

	"github.com/recurser/bossalib/machine"
	"github.com/recurser/bossalib/models"

	"github.com/recurser/bossd/internal/db"
)

// finishQuietWindow is how long every agent log of a session must have gone
// unwritten before an explicit finish (FinishUnattendedRun) may finalize it.
//
// The chat-status tracker reads a chat as idle the moment its turn ends, even
// while background subagents it dispatched keep running and writing to the
// pane — finalizing on tracker-idle alone would reintroduce the BOS-702
// premature-finalize class. A background subagent keeps the pane log moving,
// so a minute of silence is strong evidence the run really is over. The cost
// of a one-off idle redraw (an "update available" banner) is one more window,
// where the log-silence sweep (cronAgentIdleThreshold) restarts fifteen
// minutes. A subagent silent for longer than this window can still be
// finished early; that is the trade-off, and it is no worse than the sweep's
// own behaviour with a longer window.
const finishQuietWindow = 60 * time.Second

// ErrNotUnattendedRun is returned by FinishUnattendedRun for a session that is
// not an unattended run: only those have a finalize pipeline to hand off to.
var ErrNotUnattendedRun = errors.New("session is not an unattended run")

// IsUnattendedSession reports whether sess is an unattended run (cron,
// tmux_unattended, or a tmux-hosted detach run): the sessions an explicit
// finish may finalize.
func IsUnattendedSession(sess *models.Session) bool {
	return isUnattendedSession(sess)
}

// FinishUnattendedRun finalizes an unattended run that has told bossd it is
// done. It runs the same pipeline as the stranded-run sweep, from the same
// state set (periodicReapStateInts, widened for a zero-output session), so a
// finished run records its cron outcome and is cleaned up exactly as the sweep
// would have done fifteen minutes later.
//
// The finalize is idempotent through its conditional transition: when the
// sweep or the Stop-hook gate got there first the result is NoOp, which is
// success. An archived session is NoOp too — its worktree is gone, and the
// sweep skips archived rows for the same reason. A Blocked run is finalized
// only when its own chat finished it in this block (AcceptBlockedRunFinish).
func (l *Lifecycle) FinishUnattendedRun(ctx context.Context, sessionID string) (*FinalizeResult, error) {
	sess, err := l.sessions.Get(ctx, sessionID)
	if err != nil {
		return nil, fmt.Errorf("get session: %w", err)
	}
	if !isUnattendedSession(sess) {
		return nil, ErrNotUnattendedRun
	}
	if sess.ArchivedAt != nil {
		return &FinalizeResult{NoOp: true}, nil
	}
	if l.blockedFinish.take(sess) {
		return l.finishBlockedRun(ctx, sess)
	}
	return l.finalizeSessionFrom(ctx, sessionID, finishStateInts(sess))
}

// finishStateInts is the state set an explicit finish finalizes sess from.
func finishStateInts(sess *models.Session) []int {
	expected := periodicReapStateInts()
	if isZeroOutputSession(sess) {
		expected = withZeroOutputReapStates(expected)
	}
	return expected
}

// reportStateInts is the state set a blocked report is accepted in: the finish
// set, or Blocked when the run's own chat finished it in this block.
func (l *Lifecycle) reportStateInts(sess *models.Session) []int {
	if l.blockedFinish.holds(sess) {
		return []int{int(machine.Blocked)}
	}
	return finishStateInts(sess)
}

// ErrRunAlreadyFinalized is returned by ReportRunBlocked for a session whose
// finalize has already run or started: the report can no longer change it.
var ErrRunAlreadyFinalized = errors.New("unattended run is already finalized")

// ReportRunBlocked records that an unattended run ended BLOCKED on a person.
// The reason is persisted in the session's run_blocked_reason, which no other
// writer touches, so every later finalize of this run — the deferred finish,
// the Stop-hook gate, the stranded sweep, before or after a daemon restart or a
// usage-limit park — leaves its PR draft and Blocks the session with the
// reason. Reporting twice rewrites the same field. A Blocked run accepts a
// report only from its own chat's finish (AcceptBlockedRunFinish).
func (l *Lifecycle) ReportRunBlocked(ctx context.Context, sessionID, reason string) error {
	sess, err := l.sessions.Get(ctx, sessionID)
	if err != nil {
		return fmt.Errorf("get session: %w", err)
	}
	if !isUnattendedSession(sess) {
		return ErrNotUnattendedRun
	}
	if !slices.Contains(l.reportStateInts(sess), int(sess.State)) {
		return fmt.Errorf("%w (state %s)", ErrRunAlreadyFinalized, sess.State)
	}
	reason = truncateBlockedReason(reason)
	reasonPtr := &reason
	if _, err := l.sessions.Update(ctx, sessionID, db.UpdateSessionParams{RunBlockedReason: &reasonPtr}); err != nil {
		return fmt.Errorf("persist run blocked reason: %w", err)
	}
	l.logger.Info().Str("session", sessionID).Str("reason", reason).Msg("unattended run reported blocked")
	return nil
}

// UnattendedRunQuiet reports which chat, if any, still has recent output. It
// returns the agent session id of the first chat (in sorted order) whose agent
// log was written less than finishQuietWindow ago, or "" when every known log
// is quiet.
//
// An absent log does not block: the tracker and the requester settle window
// already gate that chat, and a failed pipe-pane must not pin a finish forever.
// The log-silence sweep remains the backstop for such a run.
func (l *Lifecycle) UnattendedRunQuiet(ctx context.Context, sessionID string) (string, error) {
	return l.unattendedRunQuietAt(ctx, sessionID, time.Now())
}

func (l *Lifecycle) unattendedRunQuietAt(ctx context.Context, sessionID string, now time.Time) (string, error) {
	if l.agentChats == nil || l.agentLogsDir == "" {
		return "", nil
	}
	chats, err := l.agentChats.ListBySession(ctx, sessionID)
	if err != nil {
		return "", fmt.Errorf("list chats: %w", err)
	}
	ids := make([]string, 0, len(chats))
	for _, c := range chats {
		if c != nil && c.AgentSessionID != "" {
			ids = append(ids, c.AgentSessionID)
		}
	}
	sort.Strings(ids)
	for _, id := range ids {
		idle, known := agentLogIdleFor(l.agentLogsDir, id, now)
		if known && idle < finishQuietWindow {
			return id, nil
		}
	}
	return "", nil
}
