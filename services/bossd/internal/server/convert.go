// Package server implements the ConnectRPC DaemonService handler.
package server

import (
	"context"
	"strings"
	"time"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/machine"
	"github.com/recurser/bossalib/models"
	"github.com/recurser/bossalib/vcs"
	"github.com/recurser/bossd/internal/db"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func protoString(s string) string {
	return strings.ToValidUTF8(s, "\uFFFD")
}

func protoStringPtr(s *string) *string {
	if s == nil {
		return nil
	}
	v := protoString(*s)
	return &v
}

// CanonicalRepoOriginURL returns the canonical https://<host>/<owner>/<repo>
// form of originURL via vcs.NormalizeRepoURL. All bossd paths that populate
// pb.Session.RepoOriginUrl must route through this — bosso's webhook
// dispatcher matches sessions by canonical URL, so a raw SSH form on the
// session record causes "no_ready_daemon" drops even with a matching daemon.
//
// Returns the input unchanged when NormalizeRepoURL can't parse it: a
// hand-crafted bad URL is rarer than an empty string, and empty would
// silently match every daemon with no repoIDs.
func CanonicalRepoOriginURL(originURL string) string {
	if originURL == "" {
		return ""
	}
	if canonical := vcs.NormalizeRepoURL(originURL); canonical != "" {
		return canonical
	}
	return originURL
}

// repoToProto converts a domain Repo to its protobuf representation.
func repoToProto(r *models.Repo) *pb.Repo {
	p := &pb.Repo{
		Id:                      protoString(r.ID),
		DisplayName:             protoString(r.DisplayName),
		LocalPath:               protoString(r.LocalPath),
		OriginUrl:               protoString(r.OriginURL),
		DefaultBaseBranch:       protoString(r.DefaultBaseBranch),
		WorktreeBaseDir:         protoString(r.WorktreeBaseDir),
		CanAutoMerge:            r.CanAutoMerge,
		CanAutoMergeDependabot:  r.CanAutoMergeDependabot,
		CanAutoAddressReviews:   r.CanAutoAddressReviews,
		CanAutoResolveConflicts: r.CanAutoResolveConflicts,
		MergeStrategy:           protoString(string(r.MergeStrategy)),
		LinearApiKey:            protoString(r.LinearAPIKey),
		SentryApiKey:            protoString(r.SentryAPIKey),
		SentryOrg:               protoString(r.SentryOrg),
		CreatedAt:               timestamppb.New(r.CreatedAt),
		UpdatedAt:               timestamppb.New(r.UpdatedAt),
	}
	if r.SetupScript != nil {
		p.SetupScript = protoStringPtr(r.SetupScript)
	}
	return p
}

// SessionToProto converts a domain Session to its protobuf representation.
func SessionToProto(s *models.Session) *pb.Session {
	p := &pb.Session{
		Id:                protoString(s.ID),
		RepoId:            protoString(s.RepoID),
		Title:             protoString(s.Title),
		Plan:              protoString(s.Plan),
		WorktreePath:      protoString(s.WorktreePath),
		BranchName:        protoString(s.BranchName),
		BaseBranch:        protoString(s.BaseBranch),
		State:             pb.SessionState(s.State),
		LastCheckState:    pb.ChecksOverall(s.LastCheckState),
		AgentName:         protoString(s.AgentName),
		AutomationEnabled: s.AutomationEnabled,
		AttemptCount:      int32(s.AttemptCount),
		CreatedAt:         timestamppb.New(s.CreatedAt),
		UpdatedAt:         timestamppb.New(s.UpdatedAt),
		DisplayLabel:      protoString(s.DisplayLabel),
		DisplayIntent:     pb.DisplayIntent(s.DisplayIntent),
		DisplaySpinner:    s.DisplaySpinner,
	}
	if s.AgentSessionID != nil {
		p.AgentSessionId = protoStringPtr(s.AgentSessionID)
	}
	if s.PRNumber != nil {
		n := int32(*s.PRNumber)
		p.PrNumber = &n
	}
	if s.PRURL != nil {
		p.PrUrl = protoStringPtr(s.PRURL)
	}
	if s.TrackerID != nil {
		p.TrackerId = protoStringPtr(s.TrackerID)
	}
	if s.TrackerURL != nil {
		p.TrackerUrl = protoStringPtr(s.TrackerURL)
	}
	if s.TmuxSessionName != nil {
		p.TmuxSessionName = protoStringPtr(s.TmuxSessionName)
	}
	if s.BlockedReason != nil {
		p.BlockedReason = protoStringPtr(s.BlockedReason)
	}
	if s.ArchivedAt != nil {
		p.ArchivedAt = timestamppb.New(*s.ArchivedAt)
	}
	// Last repair-attempt diagnostics, persisted by the repair plugin via
	// HostService.RecordRepairOutcome. Forwarded here so `boss show <id>`
	// and the TUI list view can surface "failing ⚠ repair: claude not in
	// PATH (3×)" hints without an extra round trip.
	p.LastRepairRunnerError = protoString(s.LastRepairRunnerError)
	p.LastRepairExitError = protoString(s.LastRepairExitError)
	p.LastRepairAttemptCount = int32(s.LastRepairAttemptCount)
	p.LastRepairHeadSha = protoString(s.LastRepairHeadSHA)
	p.LastRepairDisplayStatus = pb.DisplayStatus(s.LastRepairDisplayStatus)
	p.LastRepairReviewFingerprint = s.LastRepairReviewFingerprint
	if s.LastRepairStartedAt != nil {
		p.LastRepairStartedAt = timestamppb.New(*s.LastRepairStartedAt)
	}
	return p
}

func suppressStaleConflictAttention(p *pb.Session) {
	if p == nil || p.GetAttentionStatus() == nil {
		return
	}
	if p.GetAttentionStatus().GetReason() != pb.AttentionReason_ATTENTION_REASON_MERGE_CONFLICT_UNRESOLVABLE {
		return
	}
	if p.GetDisplayStatus() == pb.DisplayStatus_DISPLAY_STATUS_CONFLICT {
		return
	}
	p.AttentionStatus = nil
}

// agentChatToProto converts a domain AgentChat to its protobuf representation.
func agentChatToProto(c *models.AgentChat) *pb.ClaudeChat {
	out := &pb.ClaudeChat{
		Id:             protoString(c.ID),
		SessionId:      protoString(c.SessionID),
		AgentSessionId: protoString(c.AgentSessionID),
		AgentName:      protoString(c.AgentName),
		Title:          protoString(c.Title),
		DaemonId:       protoString(c.DaemonID),
		CreatedAt:      timestamppb.New(c.CreatedAt),
	}
	if c.TmuxSessionName != nil {
		out.TmuxSessionName = protoString(*c.TmuxSessionName)
	}
	if c.ProviderSessionID != nil {
		out.ProviderSessionId = protoString(*c.ProviderSessionID)
	}
	if c.StartError != nil {
		out.StartError = protoString(*c.StartError)
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
		AgentName:     protoString(c.AgentName),
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
// Blocked session counts as not-running here so housekeeping failures do not
// leave cron STATUS stuck on RUNNING. The scheduler's overlap check must NOT
// treat Blocked as terminal (you may want to refire on top of a stuck run);
// the STATUS column should. After inactive-session fall-through, only
// fire_failed maps to FAILED.
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
// (which controls overlap-skip): Blocked is included here because PR/chat/
// cleanup housekeeping problems are session attention states, not active cron
// runs. Only fire_failed later maps cron STATUS to FAILED.
func cronStatusInactiveState(st machine.State) bool {
	return st == machine.Merged || st == machine.Closed || st == machine.Blocked
}

// isCronFailureOutcome reports whether a recorded outcome should paint the
// cron job's STATUS column red (FAILED).
//
// Contract: cron FAILED means the scheduled RUN did not happen - the agent
// never started. It does NOT mean the agent ran fine but bossd's post-run
// PR/cleanup/chat housekeeping hit a snag. A cron job is not obliged to
// produce a PR; "no PR" is not a failure.
//
//   - fire_failed: the cron fire never reached a session (CreateSession
//     errored) - the agent never ran. This is the only genuine cron failure.
//
// Every other outcome means the agent RAN. Housekeeping problems among them
// surface as a Blocked session (attention-needed) in the sessions list via
// FinalizeSession's needsAttention path - NOT as a red cron:
//   - pr_created, deleted_no_changes, pr_skipped_no_github - successful runs
//   - failed_recovered - daemon restarted mid-finalize; run already happened
//   - pr_failed - the agent ran; only PR creation/lookup failed (often a
//     transient gh/GitHub error). Shown as a Blocked session, not red cron.
//   - chat_spawn_failed - the PR was created before the chat-spawn step failed
//   - cleanup_failed - the run completed; only worktree cleanup errored
func isCronFailureOutcome(o models.CronJobOutcome) bool {
	return o == models.CronJobOutcomeFireFailed
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
