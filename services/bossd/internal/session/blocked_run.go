package session

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	bossanovav1 "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/machine"
	"github.com/recurser/bossalib/models"
	"github.com/recurser/bossd/internal/agent"
	"github.com/recurser/bossd/internal/db"
)

// blockedRunMarker records, per session, the block during which the run's own
// chat is working (zero clears it) and reports whether the record changed.
// *status.DisplayTracker implements it; BlockedAlertFacts reads it back.
type blockedRunMarker interface {
	SetBlockedRunWorking(sessionID string, blockedAt time.Time) bool
}

type blockedRunDeps struct {
	working  chatStatusReader
	marker   blockedRunMarker
	onMarked func(ctx context.Context, sessionID string)
}

// SetBlockedRunDeps wires MarkWorkingBlockedRuns: the mid-turn reader, the
// marker the served attention reads, and onMarked, called when a mark changes
// so the session reaches the reverse stream. A nil working reader marks
// nothing; marker and onMarked may be nil.
func (l *Lifecycle) SetBlockedRunDeps(working ChatWorkingFunc, marker blockedRunMarker, onMarked func(ctx context.Context, sessionID string)) {
	l.blockedRun = blockedRunDeps{marker: marker, onMarked: onMarked}
	if working != nil {
		l.blockedRun.working = working
	}
}

// MarkWorkingBlockedRuns marks each Blocked unattended run whose own chat is
// mid-turn on a message newer than the block, so Session.attention, the legacy
// hint and the timeline read in progress instead of FIX. The mark lapses on
// the first sweep that finds the chat idle, and the row reads FIX again.
//
// It never moves the state. A turn is not a fix: a question, an answer, a
// rebase or a callback all take one. Only the run's own explicit finish moves
// a Blocked run (AcceptBlockedRunFinish).
//
// A mid-turn reading alone proves nothing: a pane redraw (an operator
// attaching, a daemon restart re-seeding the poller) reads as working. The
// transcript only grows when the agent takes a turn. Another live chat in the
// session suspends the mark.
func (l *Lifecycle) MarkWorkingBlockedRuns(ctx context.Context) {
	if l.blockedRun.working == nil || l.agentChats == nil {
		return
	}
	rows, err := l.sessions.ListByState(ctx, int(machine.Blocked))
	if err != nil {
		l.logger.Warn().Err(err).Msg("blocked-run mark: list blocked sessions failed")
		return
	}
	for _, s := range rows {
		l.markBlockedRun(ctx, s, blockedRunMarkable(s) && l.blockedRunWorking(ctx, s))
	}
}

// blockedRunMarkable reports whether s is a Blocked unattended run that can
// read in progress: not archived, not parked on a usage limit, not zero-output,
// with a known block time.
func blockedRunMarkable(s *models.Session) bool {
	return s != nil && s.State == machine.Blocked && isUnattendedSession(s) && !isZeroOutputSession(s) &&
		CanResumeSessionRun(s) && s.RotationResumeAt == nil && s.StateEnteredAt != nil
}

// blockedRunWorking reports whether the run's own chat is mid-turn on a
// transcript message newer than the block. Another live chat in the session,
// or a run chat it cannot find, reads as not working.
func (l *Lifecycle) blockedRunWorking(ctx context.Context, s *models.Session) bool {
	if s.AgentSessionID == nil {
		return false
	}
	chats, err := l.agentChats.ListBySession(ctx, s.ID)
	if err != nil {
		return false
	}
	run := runChat(chats, *s.AgentSessionID)
	if run == nil || l.hasActiveOtherChat(ctx, s, chats) || !l.blockedRun.working.IsWorking(run.AgentSessionID) {
		return false
	}
	client, ok := l.agents[run.AgentName]
	return ok && client != nil && transcriptAdvancedSince(ctx, client, s.WorktreePath, run, *s.StateEnteredAt)
}

// runChat is the chat whose agent session id is the run's.
func runChat(chats []*models.AgentChat, agentSessionID string) *models.AgentChat {
	for _, c := range chats {
		if c != nil && c.AgentSessionID == agentSessionID {
			return c
		}
	}
	return nil
}

// markBlockedRun records whether s's run is working in this block.
func (l *Lifecycle) markBlockedRun(ctx context.Context, s *models.Session, working bool) {
	if l.blockedRun.marker == nil {
		return
	}
	var blockedAt time.Time
	if working {
		blockedAt = *s.StateEnteredAt
	}
	if l.blockedRun.marker.SetBlockedRunWorking(s.ID, blockedAt) && l.blockedRun.onMarked != nil {
		l.blockedRun.onMarked(ctx, s.ID)
	}
}

// transcriptAdvancedSince reports whether the chat's newest transcript message
// is dated at or after since. It tries the provider session id first (codex
// keys its rollout by it), then the agent session id. A missing transcript or
// an unparsable timestamp proves nothing.
func transcriptAdvancedSince(ctx context.Context, client agent.AgentRunnerClient, workDir string, chat *models.AgentChat, since time.Time) bool {
	ids := []string{chat.AgentSessionID}
	if p := knownProviderSessionID(chat); p != "" && p != chat.AgentSessionID {
		ids = []string{p, chat.AgentSessionID}
	}
	for _, id := range ids {
		resp, err := client.ReadTranscript(ctx, &bossanovav1.ReadTranscriptRequest{WorkDir: workDir, AgentSessionId: id, MaxMessages: 1})
		if err != nil || !resp.GetExists() {
			continue
		}
		msgs := resp.GetMessages()
		if len(msgs) == 0 {
			return false
		}
		ts, err := time.Parse(time.RFC3339Nano, msgs[len(msgs)-1].GetTimestamp())
		return err == nil && !ts.Before(since.Truncate(time.Millisecond))
	}
	return false
}

// ErrNotRunChat is returned by AcceptBlockedRunFinish when the requester is
// not the run's own chat.
var ErrNotRunChat = errors.New("only the run's own chat can finish a blocked run")

// blockedFinishes holds, per session, the block an accepted finish from Blocked
// was made in. Like pending finishes it is in memory only: a restart drops
// both.
type blockedFinishes struct {
	mu sync.Mutex
	at map[string]time.Time
}

func (b *blockedFinishes) accept(sessionID string, blockedAt time.Time) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.at == nil {
		b.at = map[string]time.Time{}
	}
	b.at[sessionID] = blockedAt
}

// holds reports whether a finish was accepted in sess's current block.
func (b *blockedFinishes) holds(sess *models.Session) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	at, ok := b.at[sess.ID]
	return ok && sess.State == machine.Blocked && sess.StateEnteredAt != nil && at.Equal(*sess.StateEnteredAt)
}

// take is holds, and forgets the session's accepted finish either way.
func (b *blockedFinishes) take(sess *models.Session) bool {
	ok := b.holds(sess)
	b.mu.Lock()
	delete(b.at, sess.ID)
	b.mu.Unlock()
	return ok
}

// AcceptBlockedRunFinish lets the run's own chat finish its Blocked run, in
// this block only. The run's next finalize then starts from Blocked, as one
// from ImplementingPlan would: a finish --blocked (ReportRunBlocked) Blocks it
// again with the new reason and a new block time and leaves the PR draft and
// the repair budget alone; a plain finish readies the PR and gives repair a
// fresh budget. Nobody else can: another chat, the operator, a callback and
// the stranded sweep all leave a Blocked run where it is.
func (l *Lifecycle) AcceptBlockedRunFinish(sess *models.Session, requester string) error {
	if sess.AgentSessionID == nil || *sess.AgentSessionID != requester {
		return ErrNotRunChat
	}
	if sess.State != machine.Blocked || isZeroOutputSession(sess) || sess.StateEnteredAt == nil {
		return fmt.Errorf("%w (state %s)", ErrRunAlreadyFinalized, sess.State)
	}
	l.blockedFinish.accept(sess.ID, *sess.StateEnteredAt)
	return nil
}

// finishBlockedRun finalizes a Blocked run whose own chat finished it. Blocked
// released the PR and branch, so the move to Finalizing takes the start-path
// lock and refuses while another live session holds them. A finalize that
// readies the PR ends the block: the reason and the repair budget are cleared,
// as a manual resume clears them.
func (l *Lifecycle) finishBlockedRun(ctx context.Context, sess *models.Session) (*FinalizeResult, error) {
	advanced := false
	blockedAt := *sess.StateEnteredAt
	_, release, ok := lockRevival(ctx, l.sessions, sess, l.logger, "blocked-run finish", func(f *models.Session) bool {
		return f.State == machine.Blocked && f.StateEnteredAt != nil && f.StateEnteredAt.Equal(blockedAt)
	})
	if ok {
		var err error
		advanced, err = l.sessions.UpdateStateConditionalFrom(ctx, sess.ID, int(machine.Finalizing), []int{int(machine.Blocked)})
		release()
		if err != nil {
			return nil, fmt.Errorf("advance to finalizing: %w", err)
		}
	}
	if !advanced {
		return &FinalizeResult{NoOp: true}, nil
	}
	res, err := l.finalizeAdvanced(ctx, sess.ID)
	if err != nil || res.Deleted || res.Outcome != models.CronJobOutcomePRCreated {
		return res, err
	}
	var nilReason *string
	if _, err := l.sessions.Update(ctx, sess.ID, db.UpdateSessionParams{BlockedReason: &nilReason}); err != nil {
		l.logger.Warn().Err(err).Str("session", sess.ID).Msg("blocked-run finish: clear blocked reason failed")
	}
	if err := l.sessions.ClearRepairAlert(ctx, sess.ID); err != nil {
		l.logger.Warn().Err(err).Str("session", sess.ID).Msg("blocked-run finish: clear repair alert failed")
	}
	return res, nil
}
