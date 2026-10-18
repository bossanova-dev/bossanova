package server

import (
	"context"
	"fmt"
	"time"

	"github.com/recurser/bossalib/safego"

	"github.com/recurser/bossd/internal/db"
)

// trackerWritesRetrying is the operator merge's tracker outcome when the write
// did not land and is queued to be tried again.
const trackerWritesRetrying = "retrying"

// trackerRetryDelays spaces the retries of a post-merge tracker write that did
// not land, one delay per retry. After the last retry fails it is given up.
var trackerRetryDelays = []time.Duration{time.Minute, 5 * time.Minute, 30 * time.Minute, 2 * time.Hour, 8 * time.Hour}

// trackerMergeRetryInterval is how often the retry loop looks for due entries.
const trackerMergeRetryInterval = 30 * time.Second

// trackerMergeAttemptBudget bounds one retried mark-merged.
const trackerMergeAttemptBudget = 5 * time.Minute

// trackerMergeQueueBudget bounds one queue or note write, which must not
// depend on what is left of the merge's own budget.
const trackerMergeQueueBudget = 10 * time.Second

// trackerSetupGaps are the reasons a tracker write is unavailable that a retry
// would only repeat: nothing to write to, or credentials missing or refused.
var trackerSetupGaps = map[string]bool{
	"no-ticket-line":              true,
	"no-capability":               true,
	"no-adapter":                  true,
	"missing-credential":          true,
	"TRACKER_CREDENTIALS_MISSING": true,
	"unauthorized":                true,
	"forbidden":                   true,
}

// trackerMergeOutcome is one post-merge tracker write's outcome: the
// toolbox's verdict, why it did not happen, and the ticket it tried to move.
type trackerMergeOutcome struct {
	writes, why, ticket string
}

// trackerMergeNext is what an outcome leaves to do.
type trackerMergeNext int

const (
	// trackerMergeSettled: the write landed, there was nothing to write, or
	// the setup cannot write at all and the outcome already says so.
	trackerMergeSettled trackerMergeNext = iota
	// trackerMergeRetry: the write did not land and may on another attempt.
	trackerMergeRetry
	// trackerMergeStuck: the tracker refuses the write for good.
	trackerMergeStuck
)

func (o trackerMergeOutcome) next() trackerMergeNext {
	switch o.writes {
	case "ok", "none":
		return trackerMergeSettled
	case "permanent":
		return trackerMergeStuck
	case "unavailable":
		if trackerSetupGaps[o.why] {
			return trackerMergeSettled
		}
	}
	return trackerMergeRetry
}

func (o trackerMergeOutcome) reason() string {
	if o.why != "" {
		return o.why
	}
	return o.writes
}

// queueContext detaches ctx and bounds it for one queue or note write.
func queueContext(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), trackerMergeQueueBudget)
}

// owePendingTrackerMerge queues the session's tracker write before it runs, as
// one attempt that did not finish, due once the merge's budget is spent. A
// daemon that dies mid-write then still retries it; settleTrackerMerge
// replaces or clears the entry once the write returns.
func (s *Server) owePendingTrackerMerge(ctx context.Context, id string) {
	if s.pendingTrackerMerges == nil {
		return
	}
	from := time.Now()
	if deadline, ok := ctx.Deadline(); ok && deadline.After(from) {
		from = deadline
	}
	qctx, cancel := queueContext(ctx)
	defer cancel()
	err := s.pendingTrackerMerges.Schedule(qctx, db.PendingTrackerMerge{
		SessionID:     id,
		AttemptCount:  1,
		NextAttemptAt: from.Add(trackerRetryDelays[0]),
		LastReason:    "unfinished",
	})
	if err != nil {
		s.logger.Warn().Err(err).Str("session", id).Msg("operator merge: could not queue the tracker write before it ran")
	}
}

// settleTrackerMerge acts on an operator merge's tracker outcome and returns
// it as the operator should see it: a write that may land later is queued and
// reported as retrying, a write the tracker refuses for good is noted for the
// operator, and either of those or a settled one clears what was owed.
func (s *Server) settleTrackerMerge(ctx context.Context, id string, o trackerMergeOutcome) trackerMergeOutcome {
	qctx, cancel := queueContext(ctx)
	defer cancel()
	switch o.next() {
	case trackerMergeRetry:
		if s.pendingTrackerMerges == nil {
			return o
		}
		err := s.pendingTrackerMerges.Schedule(qctx, db.PendingTrackerMerge{
			SessionID:     id,
			AttemptCount:  1,
			NextAttemptAt: time.Now().Add(trackerRetryDelays[0]),
			LastReason:    o.reason(),
		})
		if err != nil {
			s.logger.Warn().Err(err).Str("session", id).Msg("operator merge: could not queue the tracker write for a retry")
			return o
		}
		o.writes = trackerWritesRetrying
		return o
	case trackerMergeStuck:
		s.noteTicketNotMoved(qctx, id, o.ticket)
	case trackerMergeSettled:
	}
	s.clearPendingTrackerMerge(qctx, id)
	return o
}

func (s *Server) clearPendingTrackerMerge(ctx context.Context, id string) {
	if s.pendingTrackerMerges == nil {
		return
	}
	if err := s.pendingTrackerMerges.Delete(ctx, id); err != nil {
		s.logger.Warn().Err(err).Str("session", id).Msg("tracker write: could not clear the queue entry")
	}
}

// noteTicketNotMoved leaves the operator a note on the session's repo saying
// its merged PR's ticket could not be moved. One note per session.
func (s *Server) noteTicketNotMoved(ctx context.Context, id, ticket string) {
	log := s.logger.With().Str("session", id).Str("ticket", ticket).Logger()
	if s.notes == nil {
		log.Warn().Msg("tracker write: no note store to report a ticket that could not be moved")
		return
	}
	sess, err := s.sessions.Get(ctx, id)
	if err != nil || sess.PRNumber == nil {
		log.Warn().Err(err).Msg("tracker write: could not read the session to report a ticket that could not be moved")
		return
	}
	body := fmt.Sprintf("PR #%d merged, but its ticket could not be moved", *sess.PRNumber)
	if ticket != "" {
		body = fmt.Sprintf("PR #%d merged, but its ticket %s could not be moved", *sess.PRNumber, ticket)
	}
	key := "ticket-not-moved:" + id
	if _, err := s.notes.Create(ctx, db.CreateNoteParams{
		RepoID:         sess.RepoID,
		SessionID:      &id,
		Body:           body,
		Tags:           []string{"tracker"},
		IdempotencyKey: &key,
	}); err != nil {
		log.Warn().Err(err).Msg("tracker write: could not note a ticket that could not be moved")
		return
	}
	s.nudgeNoteSync()
}

// RunTrackerMergeRetries retries the queued post-merge tracker writes, once at
// start and then every trackerMergeRetryInterval, until ctx ends.
func (s *Server) RunTrackerMergeRetries(ctx context.Context) <-chan struct{} {
	return safego.Go(s.logger, func() {
		if s.pendingTrackerMerges == nil {
			return
		}
		ticker := time.NewTicker(trackerMergeRetryInterval)
		defer ticker.Stop()
		for {
			s.retryPendingTrackerMerges(ctx, time.Now())
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
		}
	})
}

// retryPendingTrackerMerges tries every entry due at now once.
func (s *Server) retryPendingTrackerMerges(ctx context.Context, now time.Time) {
	due, err := s.pendingTrackerMerges.ListDue(ctx, now)
	if err != nil {
		s.logger.Warn().Err(err).Msg("tracker write retry: could not list the queue")
		return
	}
	for _, p := range due {
		if ctx.Err() != nil {
			return
		}
		s.retryTrackerMerge(ctx, p, now)
	}
}

// retryTrackerMerge runs mark-merged for one queued session, in its worktree
// or, once the session is archived, in the repo's checkout. A write that may
// still land is rescheduled until the retries run out; one that will not, or
// that ran out of retries, is noted for the operator. Either way, and once
// settled, it leaves the queue.
func (s *Server) retryTrackerMerge(ctx context.Context, p db.PendingTrackerMerge, now time.Time) {
	attempt := p.AttemptCount + 1
	log := s.logger.With().Str("session", p.SessionID).Int("attempt", attempt).Logger()
	o := trackerMergeOutcome{writes: "failed"}
	target, env, err := s.verifyTargetIn(ctx, p.SessionID, false, true)
	if err != nil {
		o.why = err.Error()
	} else {
		actx, cancel := context.WithTimeout(ctx, trackerMergeAttemptBudget)
		o = s.markMerged(actx, s.verifyVerbsFor(env), target)
		cancel()
	}
	if ctx.Err() != nil {
		return
	}
	next := o.next()
	if next == trackerMergeRetry && attempt <= len(trackerRetryDelays) {
		err := s.pendingTrackerMerges.Schedule(ctx, db.PendingTrackerMerge{
			SessionID:     p.SessionID,
			AttemptCount:  attempt,
			NextAttemptAt: now.Add(trackerRetryDelays[attempt-1]),
			LastReason:    o.reason(),
		})
		if err != nil {
			log.Warn().Err(err).Msg("tracker write retry: could not reschedule")
		}
		return
	}
	ev := log.Info()
	if next != trackerMergeSettled {
		s.noteTicketNotMoved(ctx, p.SessionID, o.ticket)
		ev = log.Warn()
	}
	ev.Str("tracker_writes", o.writes).Str("tracker_why", o.why).Msg("tracker write retry: finished")
	s.clearPendingTrackerMerge(ctx, p.SessionID)
}
