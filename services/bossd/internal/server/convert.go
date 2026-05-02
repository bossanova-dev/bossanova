// Package server implements the ConnectRPC DaemonService handler.
package server

import (
	"context"
	"time"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/machine"
	"github.com/recurser/bossalib/models"
	"github.com/recurser/bossalib/vcs"
	"github.com/recurser/bossd/internal/db"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// repoToProto converts a domain Repo to its protobuf representation.
func repoToProto(r *models.Repo) *pb.Repo {
	p := &pb.Repo{
		Id:                      r.ID,
		DisplayName:             r.DisplayName,
		LocalPath:               r.LocalPath,
		OriginUrl:               r.OriginURL,
		DefaultBaseBranch:       r.DefaultBaseBranch,
		WorktreeBaseDir:         r.WorktreeBaseDir,
		CanAutoMerge:            r.CanAutoMerge,
		CanAutoMergeDependabot:  r.CanAutoMergeDependabot,
		CanAutoAddressReviews:   r.CanAutoAddressReviews,
		CanAutoResolveConflicts: r.CanAutoResolveConflicts,
		MergeStrategy:           string(r.MergeStrategy),
		LinearApiKey:            r.LinearAPIKey,
		CreatedAt:               timestamppb.New(r.CreatedAt),
		UpdatedAt:               timestamppb.New(r.UpdatedAt),
	}
	if r.SetupScript != nil {
		p.SetupScript = r.SetupScript
	}
	return p
}

// SessionToProto converts a domain Session to its protobuf representation.
func SessionToProto(s *models.Session) *pb.Session {
	p := &pb.Session{
		Id:                s.ID,
		RepoId:            s.RepoID,
		Title:             s.Title,
		Plan:              s.Plan,
		WorktreePath:      s.WorktreePath,
		BranchName:        s.BranchName,
		BaseBranch:        s.BaseBranch,
		State:             pb.SessionState(s.State),
		LastCheckState:    pb.ChecksOverall(s.LastCheckState),
		AutomationEnabled: s.AutomationEnabled,
		AttemptCount:      int32(s.AttemptCount),
		CreatedAt:         timestamppb.New(s.CreatedAt),
		UpdatedAt:         timestamppb.New(s.UpdatedAt),
		DisplayLabel:      s.DisplayLabel,
		DisplayIntent:     pb.DisplayIntent(s.DisplayIntent),
		DisplaySpinner:    s.DisplaySpinner,
	}
	if s.ClaudeSessionID != nil {
		p.ClaudeSessionId = s.ClaudeSessionID
	}
	if s.PRNumber != nil {
		n := int32(*s.PRNumber)
		p.PrNumber = &n
	}
	if s.PRURL != nil {
		p.PrUrl = s.PRURL
	}
	if s.TrackerID != nil {
		p.TrackerId = s.TrackerID
	}
	if s.TrackerURL != nil {
		p.TrackerUrl = s.TrackerURL
	}
	if s.TmuxSessionName != nil {
		p.TmuxSessionName = s.TmuxSessionName
	}
	if s.BlockedReason != nil {
		p.BlockedReason = s.BlockedReason
	}
	if s.ArchivedAt != nil {
		p.ArchivedAt = timestamppb.New(*s.ArchivedAt)
	}
	return p
}

// claudeChatToProto converts a domain ClaudeChat to its protobuf representation.
func claudeChatToProto(c *models.ClaudeChat) *pb.ClaudeChat {
	out := &pb.ClaudeChat{
		Id:        c.ID,
		SessionId: c.SessionID,
		ClaudeId:  c.ClaudeID,
		Title:     c.Title,
		DaemonId:  c.DaemonID,
		CreatedAt: timestamppb.New(c.CreatedAt),
	}
	if c.TmuxSessionName != nil {
		out.TmuxSessionName = *c.TmuxSessionName
	}
	return out
}

// cronJobToProto converts a domain CronJob to its protobuf representation.
// The sessions store is consulted to derive last_run_status (RUNNING vs.
// FAILED vs. IDLE) — the proto's last_run_status field is computed, not
// persisted.
func cronJobToProto(ctx context.Context, c *models.CronJob, sessions db.SessionStore) *pb.CronJob {
	p := &pb.CronJob{
		Id:            c.ID,
		RepoId:        c.RepoID,
		Name:          c.Name,
		Prompt:        c.Prompt,
		Schedule:      c.Schedule,
		Enabled:       c.Enabled,
		CreatedAt:     timestamppb.New(c.CreatedAt),
		UpdatedAt:     timestamppb.New(c.UpdatedAt),
		LastRunStatus: cronJobStatus(ctx, c, sessions),
	}
	if c.Timezone != nil {
		p.Timezone = *c.Timezone
	}
	if c.LastRunSessionID != nil {
		p.LastRunSessionId = *c.LastRunSessionID
	}
	if c.LastRunAt != nil {
		p.LastRunAt = timestamppb.New(*c.LastRunAt)
	}
	if c.LastRunOutcome != nil {
		p.LastRunOutcome = string(*c.LastRunOutcome)
	}
	if c.NextRunAt != nil {
		p.NextRunAt = timestamppb.New(*c.NextRunAt)
	}
	return p
}

// cronJobStatus derives the run-status enum for a cron job. It mirrors
// scheduler.previousRunActive's logic so the daemon has a single source of
// truth for "is this run still going" — with one deliberate divergence: a
// Blocked session counts as not-running here so a failed finalize surfaces
// as FAILED instead of perpetually RUNNING. The scheduler's overlap check
// must NOT treat Blocked as terminal (you may want to refire on top of a
// stuck run); the STATUS column should.
//
// Order matters: a re-fire after a previous failure must show RUNNING, not
// FAILED. MarkFireStarted updates last_run_session_id but leaves
// last_run_outcome untouched, so the stale outcome would otherwise win.
func cronJobStatus(ctx context.Context, job *models.CronJob, sessions db.SessionStore) pb.CronJobStatus {
	if job.LastRunSessionID != nil && *job.LastRunSessionID != "" {
		sess, err := sessions.Get(ctx, *job.LastRunSessionID)
		if err == nil && sess != nil && sess.ArchivedAt == nil && !cronStatusInactiveState(sess.State) {
			return pb.CronJobStatus_CRON_JOB_STATUS_RUNNING
		}
	}
	if job.LastRunOutcome != nil && isCronFailureOutcome(*job.LastRunOutcome) {
		return pb.CronJobStatus_CRON_JOB_STATUS_FAILED
	}
	return pb.CronJobStatus_CRON_JOB_STATUS_IDLE
}

// cronStatusInactiveState reports whether a session should NOT count as
// "running" when deriving cron STATUS. Diverges from cron.isTerminalState
// (which controls overlap-skip): Blocked is included here so a failed
// finalize surfaces as FAILED in the STATUS column instead of staying
// stuck on RUNNING until the user manually archives.
func cronStatusInactiveState(st machine.State) bool {
	return st == machine.Merged || st == machine.Closed || st == machine.Blocked
}

// isCronFailureOutcome reports whether a recorded outcome represents a
// terminal failure (FAILED status). Successful and idle outcomes
// (pr_created, deleted_no_changes, pr_skipped_no_github, failed_recovered)
// fall through to IDLE.
func isCronFailureOutcome(o models.CronJobOutcome) bool {
	switch o {
	case models.CronJobOutcomePRFailed,
		models.CronJobOutcomeChatSpawnFailed,
		models.CronJobOutcomeCleanupFailed,
		models.CronJobOutcomeFireFailed:
		return true
	default:
		return false
	}
}

// constructPRURL is a package-local alias for vcs.ConstructPRURL.
func constructPRURL(originURL string, prNumber int) string {
	return vcs.ConstructPRURL(originURL, prNumber)
}

// attentionStatusToProto converts a vcs.AttentionStatus to its protobuf representation.
// Returns nil if the session does not need attention.
func attentionStatusToProto(a vcs.AttentionStatus) *pb.AttentionStatus {
	if !a.NeedsAttention {
		return nil
	}
	return &pb.AttentionStatus{
		NeedsAttention: true,
		Reason:         pb.AttentionReason(a.Reason),
		Summary:        a.Summary,
		Since:          timestamppb.New(a.Since),
	}
}

// protoToTimestamp converts an optional protobuf Timestamp to *time.Time.
func protoToTimestamp(ts *timestamppb.Timestamp) *time.Time {
	if ts == nil {
		return nil
	}
	t := ts.AsTime()
	return &t
}
