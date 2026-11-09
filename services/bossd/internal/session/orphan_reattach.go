package session

import (
	"context"

	"github.com/recurser/bossalib/machine"
	"github.com/recurser/bossalib/models"

	"github.com/recurser/bossd/internal/db"
)

// hasLiveChatPane reports whether any of the session's chats has a live pane:
// its tmux session exists and the agent process in it has not exited. This is
// the predicate the chat status poller applies before it polls a chat, and so
// what `boss chats` reports as a running chat. A pane left behind by
// remain-on-exit after its agent exited is not live.
//
// Any tmux or store error reads as "no live pane". Both callers then keep
// today's behaviour: the restart sweep still orphans a run tmux cannot vouch
// for, and the recovery sweep leaves an orphan alone.
func (l *Lifecycle) hasLiveChatPane(ctx context.Context, sessionID string) bool {
	if l.tmux == nil || l.agentChats == nil {
		return false
	}
	chats, err := l.agentChats.ListBySession(ctx, sessionID)
	if err != nil {
		l.logger.Debug().Err(err).Str("session", sessionID).Msg("live chat pane check: list chats failed")
		return false
	}
	for _, chat := range chats {
		if chat == nil || chat.IsRetired() || chat.TmuxSessionName == nil || *chat.TmuxSessionName == "" {
			continue
		}
		name := *chat.TmuxSessionName
		if !l.tmux.HasSession(ctx, name) {
			continue
		}
		if dead, err := l.tmux.PaneDead(ctx, name); err == nil && !dead {
			return true
		}
	}
	return false
}

// RestoreLiveOrphans returns false orphans to ImplementingPlan. A false orphan
// is a session the daemon-restart sweep marked Orphaned (exactly
// OrphanedHeadlessRunReason) that has a live chat pane, for example a chat the
// operator reopened after the restart. The run never died, so nothing is
// started: the Reattach transition moves the row back and clears the marker,
// keeping its agent session id. A session with no live pane stays Orphaned.
// Returns the number of sessions restored.
func (l *Lifecycle) RestoreLiveOrphans(ctx context.Context) (restored int) {
	if l.sessions == nil || l.tmux == nil || l.agentChats == nil {
		return 0
	}
	store, ok := l.sessions.(db.OrphanReattachStore)
	if !ok {
		l.logger.Error().Msg("orphan reattach sweep: session store does not support atomic orphan reattach")
		return 0
	}
	orphaned, err := l.sessions.ListByState(ctx, int(machine.Orphaned))
	if err != nil {
		l.logger.Warn().Err(err).Msg("orphan reattach sweep: list orphaned sessions failed")
		return 0
	}
	for _, sess := range orphaned {
		if !isRestartOrphan(sess) || !l.hasLiveChatPane(ctx, sess.ID) {
			continue
		}
		if l.reattachOrphan(ctx, store, sess) {
			restored++
		}
	}
	return restored
}

// isRestartOrphan reports whether sess is an unarchived Orphaned row carrying
// the daemon-restart marker.
func isRestartOrphan(sess *models.Session) bool {
	return sess != nil && sess.State == machine.Orphaned && sess.ArchivedAt == nil &&
		sess.BlockedReason != nil && *sess.BlockedReason == OrphanedHeadlessRunReason
}

func (l *Lifecycle) reattachOrphan(ctx context.Context, store db.OrphanReattachStore, sess *models.Session) bool {
	sm := machine.New(sess.State)
	if err := sm.Fire(machine.Reattach); err != nil {
		l.logger.Error().Err(err).Str("session", sess.ID).Msg("orphan reattach sweep: state machine refused reattach")
		return false
	}
	// Orphaned released the PR and branch, so another session may hold them
	// now; reattaching would put two live sessions on one PR. Hold the
	// start-path lock across the write, as the orphan resume does.
	_, release, ok := lockRevival(ctx, l.sessions, sess, l.logger, "orphan reattach sweep", isRestartOrphan)
	if !ok {
		return false
	}
	advanced, err := store.MarkOrphanReattached(ctx, sess.ID, OrphanedHeadlessRunReason, int(sm.State()))
	release()
	if err != nil {
		l.logger.Error().Err(err).Str("session", sess.ID).Msg("orphan reattach sweep: transition failed")
		return false
	}
	if !advanced {
		return false
	}
	l.logger.Warn().
		Str("session", sess.ID).
		Str("state", sm.State().String()).
		Msg("orphan reattach sweep: a chat pane is live, so the restart orphan was false; reattached without starting a run")
	return true
}
