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

// blockedFinishClaim is a finish the run's own chat made from Blocked: the
// block it was made in, and the reason of a finish --blocked (nil for a plain
// finish).
type blockedFinishClaim struct {
	blockedAt time.Time
	report    *string
}

// blockedFinishes holds, per session, the claim an accepted finish from
// Blocked made. Like pending finishes it is in memory only, and so is the
// claim's reason: nothing reaches the session row until the finish fires, so a
// restart or a new chat that drops the pending finish leaves the row as it was.
type blockedFinishes struct {
	mu sync.Mutex
	at map[string]blockedFinishClaim
}

func (b *blockedFinishes) accept(sessionID string, claim blockedFinishClaim) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.at == nil {
		b.at = map[string]blockedFinishClaim{}
	}
	b.at[sessionID] = claim
}

// take returns the claim made in sess's current block, and forgets the
// session's claim either way.
func (b *blockedFinishes) take(sess *models.Session) (blockedFinishClaim, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	claim, ok := b.at[sess.ID]
	delete(b.at, sess.ID)
	ok = ok && sess.State == machine.Blocked && sess.StateEnteredAt != nil && claim.blockedAt.Equal(*sess.StateEnteredAt)
	return claim, ok
}

// AcceptBlockedRunFinish lets the run's own chat finish its Blocked run, in
// this block only. report is a finish --blocked's reason, nil for a plain
// finish. The run's next finalize then starts from Blocked, as one from
// ImplementingPlan would: a finish --blocked Blocks it again with the new
// reason and a new block time and leaves the PR draft and the repair budget
// alone; a plain finish readies the PR and gives repair a fresh budget. Nobody
// else can: another chat, the operator, a callback and the stranded sweep all
// leave a Blocked run where it is.
func (l *Lifecycle) AcceptBlockedRunFinish(sess *models.Session, requester string, report *string) error {
	if sess.AgentSessionID == nil || *sess.AgentSessionID != requester {
		return ErrNotRunChat
	}
	if sess.State != machine.Blocked || isZeroOutputSession(sess) || sess.StateEnteredAt == nil {
		return fmt.Errorf("%w (state %s)", ErrRunAlreadyFinalized, sess.State)
	}
	claim := blockedFinishClaim{blockedAt: *sess.StateEnteredAt}
	if report != nil {
		reason := truncateBlockedReason(*report)
		claim.report = &reason
	}
	l.blockedFinish.accept(sess.ID, claim)
	return nil
}

// finishBlockedRun finalizes a Blocked run whose own chat finished it. Blocked
// released the PR and branch, so the move to Finalizing takes the start-path
// lock and refuses while another live session holds them. Once the run is
// Finalizing, the claim's report replaces whatever run_blocked_reason the row
// held, so the finalize Blocks the run again only on this claim's word. A
// finalize that readies the PR ends the block: the reason and the repair budget
// are cleared, as a manual resume clears them.
func (l *Lifecycle) finishBlockedRun(ctx context.Context, sess *models.Session, claim blockedFinishClaim) (*FinalizeResult, error) {
	advanced := false
	blockedAt := claim.blockedAt
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
	report := claim.report
	if _, err := l.sessions.Update(ctx, sess.ID, db.UpdateSessionParams{RunBlockedReason: &report}); err != nil {
		return nil, fmt.Errorf("record blocked-run finish report: %w", err)
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
