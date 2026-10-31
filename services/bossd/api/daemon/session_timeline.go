package server

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/recurser/bossd/internal/domain"

	"connectrpc.com/connect"

	"github.com/recurser/bossalib/contract/daemon/alertstate"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/kit/vcs"
	"github.com/recurser/bossalib/models"

	"github.com/recurser/bossd/internal/evidence"
	"github.com/recurser/bossd/internal/session"
	"github.com/recurser/bossd/internal/sessionattention"
	"github.com/recurser/bossd/internal/status"
)

// EvidenceSource serves a session's current-head verify evidence summary, the
// open checklist items its verify park note recorded on head, and whether the
// operator's newest record on head is a legacy send-back.
// *evidence.Reader satisfies it.
type EvidenceSource interface {
	Proof(sessionID, head string) (evidence.ProofRecord, bool)
	Entry(sessionID string) (evidence.Entry, bool)
	OpenItems(sessionID, head string) []string
	SentBack(sessionID, head string) bool
}

// timelineRotationEventLimit bounds the rotation audit read. A session that
// rotated more often than this is far past the point where its oldest
// incidents matter to the timeline.
const timelineRotationEventLimit = 200

// GetSessionTimeline returns the dated steps of a session's run on its current
// PR head, assembled from persisted facts (agent runs, check snapshots,
// rotation events, session state) plus the evidence reader and the chat status
// tracker. See buildTimeline for the rules.
func (s *Server) GetSessionTimeline(ctx context.Context, req *connect.Request[pb.GetSessionTimelineRequest]) (*connect.Response[pb.GetSessionTimelineResponse], error) {
	sessionID := req.Msg.GetSessionId()
	if sessionID == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("session_id is required"))
	}
	sess, err := s.sessions.Get(ctx, sessionID)
	if err != nil {
		return nil, connect.NewError(connect.CodeNotFound, fmt.Errorf("session not found: %w", err))
	}
	facts, err := s.timelineFacts(ctx, sess)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	return connect.NewResponse(&pb.GetSessionTimelineResponse{Steps: buildTimeline(facts)}), nil
}

// timelineFacts gathers everything buildTimeline reads. Every source is
// optional: an unwired store contributes nothing rather than failing.
func (s *Server) timelineFacts(ctx context.Context, sess *models.Session) (timelineFacts, error) {
	f := timelineFacts{SessionTitle: sess.Title, HasPR: sess.PRNumber != nil, DismissedAlertKey: sess.DismissedAlertKey, IsFactoryRepo: s.attentionFactory(ctx, sess.RepoID), Now: time.Now()}
	if sess.TrackerID != nil {
		f.TrackerID = *sess.TrackerID
	}

	if s.agentRuns != nil {
		runs, err := s.agentRuns.List(ctx, wedgeRunFilter(sess.ID))
		if err != nil {
			return f, fmt.Errorf("list agent runs: %w", err)
		}
		f.Runs = runs
	}

	if f.HasPR && s.checkSnapshots != nil {
		if err := s.readTimelineSnapshots(ctx, sess.ID, &f); err != nil {
			return f, err
		}
	}

	if f.IsFactoryRepo && sess.PRNumber != nil && f.HeadSHA != "" {
		f.VerifyAdmission = s.attentionVerifyAdmission(sess.RepoID, *sess.PRNumber, f.HeadSHA)
	}
	if s.evidence != nil && f.HeadSHA != "" {
		if entry, ok := s.evidence.Entry(sess.ID); ok && strings.EqualFold(entry.Summary.Head, f.HeadSHA) {
			f.Evidence = &entry
		}
		f.IsSentBack = s.evidence.SentBack(sess.ID, f.HeadSHA)
	}

	if s.rotationEvents != nil {
		events, err := s.rotationEvents.ListBySession(ctx, sess.ID, timelineRotationEventLimit)
		if err != nil {
			return f, fmt.Errorf("list rotation events: %w", err)
		}
		f.RotationEvents = events
	}

	var chats []*domain.AgentChat
	if s.agentChats != nil {
		var err error
		chats, err = s.agentChats.ListBySession(ctx, sess.ID)
		if err != nil {
			return f, fmt.Errorf("list chats: %w", err)
		}
	}
	f.Chats = chats
	var live time.Time
	if s.chatStatus != nil {
		if activity, ok := latestAgentActivity(s.chatStatus, chats); ok {
			live = activity
		}
		f.Question = sessionattention.AskingChat(s.chatStatus, chats)
	}
	// Compute the same restart-proof activity HydrateRestartProofActivity puts
	// on pb.Session.last_agent_activity_at: judge the give-up on the rotation
	// events read above and PRDone on the newest snapshot, as
	// sessionattention.Derive does, and reuse the runs listed above rather than
	// listing them again.
	q := WedgeQuery{SessionID: sess.ID, WorktreePath: sess.WorktreePath, Live: live, Chats: chats}
	if n := len(f.Snapshots); n > 0 {
		q.PRDone = sessionattention.PRDone(vcs.DisplayStatus(f.Snapshots[n-1].ComputedStatus))
	}
	if giveUpAt, ok := sessionattention.NewestGiveUpAt(f.RotationEvents); ok {
		q.GiveUpAt = giveUpAt
	}
	w := s.wedgeActivity()
	runs := f.Runs
	w.Runs = func(context.Context, string) ([]domain.AgentRun, error) { return runs, nil }
	f.LastAgentActivityAt = w.RestartProofActivity(ctx, q)

	var display *status.DisplayEntry
	if s.displayTracker != nil {
		display = s.displayTracker.Get(sess.ID)
		applyTimelineDisplay(&f, display)
	}
	// No dated apiversion bump: the timeline drops its Blocked step when the
	// block no longer needs the operator (its run's agent is working, or its PR
	// is green and mergeable), as Session.attention already does.
	// GetSessionTimeline reaches the boss TUI, which stamps its built Current but
	// can point at a remote daemon of another version, and the web through
	// version-skewed bosso; both render whatever steps arrive. A timeline
	// response carries no blocked reason or state time to rebuild the step from. See docs/structure/contracts.md#skipping-a-bump-justify-it-in-the-diff.
	if alertstate.BlockedAlertCurrent(session.BlockedAlertFacts(sess, display)) {
		f.Blocked = sessionattention.BlockedRow(sess)
	}
	f.Repair = s.repairFact(sess)
	return f, nil
}

// repairFact reads the session's last repair outcome, deciding whether its
// alert is still current from the same facts the resolved-alert sweep reads
// (session.RepairAlertFacts), the repair lease's stall record included.
func (s *Server) repairFact(sess *models.Session) *repairFact {
	var display *status.DisplayEntry
	if s.displayTracker != nil {
		display = s.displayTracker.Get(sess.ID)
	}
	_, stalled := s.repairLease.StalledAt(sess.ID)
	return sessionattention.RepairFromSession(sess, session.RepairAlertFacts(sess, display, stalled))
}

// applyTimelineDisplay never carries live PR facts onto another persisted head.
func applyTimelineDisplay(f *timelineFacts, display *status.DisplayEntry) {
	if display == nil || f.HeadSHA == "" || !strings.EqualFold(display.HeadSHA, f.HeadSHA) {
		return
	}
	f.IsPartial = display.IsPartial
	f.Mergeable = display.Mergeable
}

// readTimelineSnapshots loads per-head phase starts and the current head's history.
func (s *Server) readTimelineSnapshots(ctx context.Context, sessionID string, f *timelineFacts) error {
	headStarts, err := s.checkSnapshots.ListHeadStartsBySession(ctx, sessionID)
	if err != nil {
		return fmt.Errorf("list check snapshot head starts: %w", err)
	}
	f.HeadStarts = headStarts
	newest, err := s.checkSnapshots.ListBySession(ctx, sessionID, 1)
	if err != nil {
		return fmt.Errorf("read newest check snapshot: %w", err)
	}
	if len(newest) > 0 && newest[0].HeadSHA != "" {
		f.HeadSHA = newest[0].HeadSHA
		snaps, err := s.checkSnapshots.ListBySessionHead(ctx, sessionID, f.HeadSHA)
		if err != nil {
			return fmt.Errorf("list check snapshots: %w", err)
		}
		f.Snapshots = snaps
	}
	return nil
}
