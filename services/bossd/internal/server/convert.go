// Package server implements the ConnectRPC DaemonService handler.
package server

import (
	"context"
	"math"
	"strings"
	"time"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/machine"
	"github.com/recurser/bossalib/models"
	"github.com/recurser/bossalib/vcs"
	"github.com/recurser/bossd/internal/cron"
	"github.com/recurser/bossd/internal/db"
	"github.com/recurser/bossd/internal/status"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// clampInt32 converts n to int32, clamping to the int32 range instead of
// wrapping. bossd counts/enums/offsets never exceed int32 in practice; the
// clamp is a gosec-G115-satisfying guard, not a behaviour change on real
// inputs.
func clampInt32(n int) int32 {
	if n > math.MaxInt32 {
		return math.MaxInt32
	}
	if n < math.MinInt32 {
		return math.MinInt32
	}
	return int32(n)
}

// HydrateDisplayEntry stamps the per-axis display-tracker fields onto a Session
// proto: DisplayStatus, the failure/changes-requested/repairing/setting-up/
// merging flags, PR mergeability, and the derived MergeBlock. These live ONLY in
// the in-memory DisplayTracker, not on models.Session, so every read path that
// needs them must apply this — the local GetSession/ListSessions RPCs AND the
// reverse-stream projection that feeds the cloud/web read model (without it the
// web sees DisplayStatus=UNSPECIFIED, so anything gating on PASSING — e.g. the
// web Merge button — never lights up). No-op when p or e is nil.
func HydrateDisplayEntry(p *pb.Session, e *status.DisplayEntry) {
	if p == nil || e == nil {
		return
	}
	if p.GetLastCheckStateHeadSha() != "" && p.GetLastCheckStateHeadSha() == e.HeadSHA {
		p.LastCheckState = p.GetLastCheckStateObserved()
	}
	p.DisplayStatus = pb.DisplayStatus(clampInt32(int(e.Status)))
	p.DisplayHasFailures = e.HasFailures
	p.DisplayHasChangesRequested = e.HasChangesRequested
	p.DisplayIsRepairing = e.IsRepairing
	p.DisplaySettingUp = e.SettingUp
	p.DisplayMerging = e.Merging
	p.ArchivePending = e.Archiving
	p.PrMergeable = e.Mergeable
	p.MergeBlock = displayEntryToMergeBlock(e)
}

// displayEntryToMergeBlock derives the structured MergeBlock proto from a
// cached DisplayEntry via vcs.DeriveMergeBlock (the single source of truth for
// the DisplayStatus→gate mapping). Returns nil for a nil entry.
func displayEntryToMergeBlock(e *status.DisplayEntry) *pb.MergeBlock {
	if e == nil {
		return nil
	}
	mb := vcs.DeriveMergeBlock(e.Status, e.HasFailures, e.ChangesRequestedBy)
	return &pb.MergeBlock{
		Gate:              pb.MergeBlock_Gate(clampInt32(int(mb.Gate))),
		Detail:            mb.Detail,
		BlockingReviewers: mb.BlockingReviewers,
		DisplayStatus:     pb.DisplayStatus(clampInt32(int(mb.Status))),
	}
}

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
		Id:                              protoString(r.ID),
		DisplayName:                     protoString(r.DisplayName),
		LocalPath:                       protoString(r.LocalPath),
		OriginUrl:                       protoString(r.OriginURL),
		DefaultBaseBranch:               protoString(r.DefaultBaseBranch),
		WorktreeBaseDir:                 protoString(r.WorktreeBaseDir),
		CanAutoMerge:                    r.CanAutoMerge,
		CanAutoMergeDependabot:          r.CanAutoMergeDependabot,
		CanAutoRepair:                   r.CanAutoRepair,
		ShouldArchiveSessionsAfterMerge: r.ShouldArchiveSessionsAfterMerge,
		CanAutoDeleteBranches:           r.CanAutoDeleteBranches,
		ShouldKeepBranchesCurrent:       r.ShouldKeepBranchesCurrent,
		MergeStrategy:                   protoString(string(r.MergeStrategy)),
		LinearApiKey:                    protoString(r.LinearAPIKey),
		SentryApiKey:                    protoString(r.SentryAPIKey),
		SentryOrg:                       protoString(r.SentryOrg),
		CreatedAt:                       timestamppb.New(r.CreatedAt),
		UpdatedAt:                       timestamppb.New(r.UpdatedAt),
	}
	if r.SetupScript != nil {
		p.SetupScript = protoStringPtr(r.SetupScript)
	}
	return p
}

// mergeStrategyToProtoEnum maps a domain MergeStrategy to the proto enum used by
// the masked RepoSettings surface. Any invalid/empty value normalizes to MERGE
// (mirroring models.ParseMergeStrategy and the DB default).
func mergeStrategyToProtoEnum(m models.MergeStrategy) pb.MergeStrategy {
	switch models.ParseMergeStrategy(string(m)) {
	case models.MergeStrategyRebase:
		return pb.MergeStrategy_MERGE_STRATEGY_REBASE
	case models.MergeStrategySquash:
		return pb.MergeStrategy_MERGE_STRATEGY_SQUASH
	default:
		return pb.MergeStrategy_MERGE_STRATEGY_MERGE
	}
}

// mergeStrategyFromProtoEnum maps a proto MergeStrategy enum back to the domain
// type. MERGE_STRATEGY_UNSPECIFIED (the default-constructed zero) falls back to
// the default MergeStrategyMerge.
func mergeStrategyFromProtoEnum(m pb.MergeStrategy) models.MergeStrategy {
	switch m {
	case pb.MergeStrategy_MERGE_STRATEGY_REBASE:
		return models.MergeStrategyRebase
	case pb.MergeStrategy_MERGE_STRATEGY_SQUASH:
		return models.MergeStrategySquash
	default:
		return models.MergeStrategyMerge
	}
}

// repoToRepoSettings builds the web-safe, secret-masked RepoSettings projection
// of a Repo. It carries ONLY non-secret fields plus has_* booleans reporting
// whether a secret is present; it MUST NEVER read a key value into the message.
// updated_at is the optimistic-concurrency token the client echoes back on the
// next UpdateRepo.
func repoToRepoSettings(r *models.Repo) *pb.RepoSettings {
	settings := &pb.RepoSettings{
		Id:                              protoString(r.ID),
		DisplayName:                     protoString(r.DisplayName),
		MergeStrategy:                   mergeStrategyToProtoEnum(r.MergeStrategy),
		CanAutoMerge:                    r.CanAutoMerge,
		CanAutoMergeDependabot:          r.CanAutoMergeDependabot,
		CanAutoRepair:                   r.CanAutoRepair,
		ShouldArchiveSessionsAfterMerge: r.ShouldArchiveSessionsAfterMerge,
		SentryOrg:                       protoString(r.SentryOrg),
		HasLinearKey:                    r.LinearAPIKey != "",
		HasSentryKey:                    r.SentryAPIKey != "",
		UpdatedAt:                       timestamppb.New(r.UpdatedAt),
	}
	if r.SetupScript != nil {
		settings.SetupScript = protoStringPtr(r.SetupScript)
	}
	return settings
}

// SessionToProto converts a domain Session to its protobuf representation.
func SessionToProto(s *models.Session) *pb.Session {
	p := &pb.Session{
		Id:                     protoString(s.ID),
		RepoId:                 protoString(s.RepoID),
		Title:                  protoString(s.Title),
		Plan:                   protoString(s.Plan),
		WorktreePath:           protoString(s.WorktreePath),
		BranchName:             protoString(s.BranchName),
		BaseBranch:             protoString(s.BaseBranch),
		State:                  pb.SessionState(clampInt32(int(s.State))),
		LastCheckState:         pb.ChecksOverall_CHECKS_OVERALL_UNSPECIFIED,
		LastCheckStateObserved: pb.ChecksOverall(clampInt32(int(s.LastCheckState))),
		LastCheckStateHeadSha:  s.LastCheckStateHeadSHA,
		AgentName:              protoString(s.AgentName),
		IsAutomationEnabled:    s.IsAutomationEnabled,
		AttemptCount:           clampInt32(s.AttemptCount),
		CreatedAt:              timestamppb.New(s.CreatedAt),
		UpdatedAt:              timestamppb.New(s.UpdatedAt),
		DisplayLabel:           protoString(s.DisplayLabel),
		DisplayIntent:          pb.DisplayIntent(s.DisplayIntent),
		DisplaySpinner:         s.DisplaySpinner,
	}
	if s.AgentSessionID != nil {
		p.AgentSessionId = protoStringPtr(s.AgentSessionID)
	}
	if s.PRNumber != nil {
		n := clampInt32(*s.PRNumber)
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
	if s.AccountID != nil {
		p.AccountId = protoStringPtr(s.AccountID)
	}
	if s.ArchivedAt != nil {
		p.ArchivedAt = timestamppb.New(*s.ArchivedAt)
	}
	if s.LastCheckStateAt != nil {
		p.LastCheckStateAt = timestamppb.New(*s.LastCheckStateAt)
	}
	if s.StateEnteredAt != nil {
		p.StateEnteredAt = timestamppb.New(*s.StateEnteredAt)
	}
	// Last repair-attempt diagnostics, persisted by the repair plugin via
	// HostService.RecordRepairOutcome. Forwarded here so `boss show <id>`
	// and the TUI list view can surface "failing ⚠ repair: claude not in
	// PATH (3×)" hints without an extra round trip.
	p.LastRepairRunnerError = protoString(s.LastRepairRunnerError)
	p.LastRepairExitError = protoString(s.LastRepairExitError)
	p.LastRepairAttemptCount = clampInt32(s.LastRepairAttemptCount)
	p.LastRepairHeadSha = protoString(s.LastRepairHeadSHA)
	p.LastRepairDisplayStatus = pb.DisplayStatus(s.LastRepairDisplayStatus)
	p.LastRepairReviewFingerprint = s.LastRepairReviewFingerprint
	// Blocked-refusal lane: a start-refusal reason recorded without bumping the
	// failure count. Cleared on the next real outcome, so a non-empty reason is
	// the latest repair-related state.
	p.LastRepairBlockedReason = s.LastRepairBlockedReason
	// Non-fatal setup-script failure, surfaced so `boss show <id>` and the TUI
	// can flag a degraded session. Empty for clean runs.
	p.SetupError = protoString(s.SetupError)
	if s.LastRepairStartedAt != nil {
		p.LastRepairStartedAt = timestamppb.New(*s.LastRepairStartedAt)
	}
	if s.LastRepairBlockedAt != nil {
		p.LastRepairBlockedAt = timestamppb.New(*s.LastRepairBlockedAt)
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
// persisted. gating is the set of job IDs currently mid-gate; when a job
// ID appears there, GATING overrides all other status logic.
func cronJobToProto(ctx context.Context, c *models.CronJob, sessions db.SessionStore, activity cron.ActivityChecker, gating map[string]struct{}) *pb.CronJob {
	status := cronJobStatus(ctx, c, sessions, activity)
	if gating != nil {
		if _, ok := gating[c.ID]; ok {
			status = pb.CronJobStatus_CRON_JOB_STATUS_GATING
		}
	}
	p := &pb.CronJob{
		Id:                    c.ID,
		RepoId:                c.RepoID,
		Name:                  c.Name,
		Prompt:                c.Prompt,
		Schedule:              c.Schedule,
		AgentName:             protoString(c.AgentName),
		Model:                 c.Model,
		IsEnabled:             c.IsEnabled,
		GateCommand:           c.GateCommand,
		ShouldRunSetupCommand: c.ShouldRunSetupCommand,
		IsZeroOutput:          c.IsZeroOutput,
		LastRunAgentName:      protoString(c.LastRunAgentName),
		CreatedAt:             timestamppb.New(c.CreatedAt),
		UpdatedAt:             timestamppb.New(c.UpdatedAt),
		LastRunStatus:         status,
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

// githubCallbackToProto converts a domain GithubCallback to its protobuf
// representation (BOS-467). Nullable timestamp/string columns map to unset
// proto fields. The message body is copied through for the registering owner;
// it is a secret and must never be logged by callers of this converter.
func githubCallbackToProto(c *models.GithubCallback) *pb.GithubCallback {
	p := &pb.GithubCallback{
		Id:                      c.ID,
		TargetChatId:            c.TargetChatID,
		RepoOwner:               c.RepoOwner,
		RepoName:                c.RepoName,
		PrNumber:                clampInt32(c.PRNumber),
		Trigger:                 string(c.Trigger),
		State:                   string(c.State),
		Message:                 c.Message,
		ShouldRequireTransition: c.ShouldRequireTransition,
		HasObservedBaseline:     c.HasObservedBaseline,
		AttemptCount:            clampInt32(c.AttemptCount),
		ExpiresAt:               timestamppb.New(c.ExpiresAt),
		CreatedAt:               timestamppb.New(c.CreatedAt),
		UpdatedAt:               timestamppb.New(c.UpdatedAt),
	}
	if c.GroupID != nil {
		p.GroupId = *c.GroupID
	}
	if c.LeaseOwner != nil {
		p.LeaseOwner = *c.LeaseOwner
	}
	if c.LastError != nil {
		p.LastError = *c.LastError
	}
	if c.LastEvent != nil {
		p.LastEvent = *c.LastEvent
	}
	if c.LeaseDeadlineAt != nil {
		p.LeaseDeadlineAt = timestamppb.New(*c.LeaseDeadlineAt)
	}
	if c.NextAttemptAt != nil {
		p.NextAttemptAt = timestamppb.New(*c.NextAttemptAt)
	}
	if c.TriggeredAt != nil {
		p.TriggeredAt = timestamppb.New(*c.TriggeredAt)
	}
	if c.DeliveredAt != nil {
		p.DeliveredAt = timestamppb.New(*c.DeliveredAt)
	}
	return p
}

// noteToProto converts a domain Note to its protobuf representation (BOS-550).
// Nullable provenance columns map to empty strings — a note whose session was
// deleted keeps the id it was written with, so an empty value means "never
// session-scoped", not "the session is gone".
//
// Unlike githubCallbackToProto's message, a note's body is not a secret: it is
// recorded to be read back, so it is copied through on every read surface.
func noteToProto(n *models.Note) *pb.Note {
	p := &pb.Note{
		Id:        n.ID,
		RepoId:    n.RepoID,
		Body:      n.Body,
		Tags:      n.Tags,
		CreatedAt: timestamppb.New(n.CreatedAt),
		UpdatedAt: timestamppb.New(n.UpdatedAt),
	}
	if n.SessionID != nil {
		p.SessionId = *n.SessionID
	}
	if n.ChatID != nil {
		p.ChatId = *n.ChatID
	}
	return p
}

// accountToProto converts a domain Account to its protobuf representation.
// It maps METADATA ONLY — the credential blob is never a field on either side
// (locked decision D3) and is never read here.
func accountToProto(a *models.Account) *pb.Account {
	p := &pb.Account{
		Id:            a.ID,
		Provider:      string(a.Provider),
		Label:         a.Label,
		Status:        string(a.Status),
		Priority:      clampInt32(a.Priority),
		Health:        string(a.Health),
		Tier:          a.Tier,
		AllowedModels: a.AllowedModels,
		LastTestError: a.LastTestError,
		CreatedAt:     timestamppb.New(a.CreatedAt),
		UpdatedAt:     timestamppb.New(a.UpdatedAt),
	}
	if a.CooldownUntil != nil {
		p.CooldownUntil = timestamppb.New(*a.CooldownUntil)
	}
	if a.LastUsedAt != nil {
		p.LastUsedAt = timestamppb.New(*a.LastUsedAt)
	}
	if a.LastTestOkAt != nil {
		p.LastTestOkAt = timestamppb.New(*a.LastTestOkAt)
	}
	if a.Usage != nil {
		p.Usage = &pb.UsageSnapshot{
			Util_5H:  a.Usage.Util5h,
			Util_7D:  a.Usage.Util7d,
			Status:   a.Usage.Status,
			PlanTier: a.Usage.PlanTier,
		}
		if a.Usage.Reset5h != nil {
			p.Usage.Reset_5H = timestamppb.New(*a.Usage.Reset5h)
		}
		if a.Usage.Reset7d != nil {
			p.Usage.Reset_7D = timestamppb.New(*a.Usage.Reset7d)
		}
		if a.Usage.FetchedAt != nil {
			p.Usage.FetchedAt = timestamppb.New(*a.Usage.FetchedAt)
		}
	}
	return p
}

// cronJobStatus derives the run-status enum for a cron job. Its primary path is
// agent liveness: when an activity checker is wired it shares the scheduler's
// CronActivityChecker (single source of truth for "is this run still going"),
// so STATUS reads RUNNING only when RunActive reports a live ImplementingPlan
// run. When no checker is wired it falls back to the session-state heuristic
// cronStatusInactiveState — with one deliberate divergence: a Blocked session
// counts as not-running there so housekeeping failures do not leave cron STATUS
// stuck on RUNNING. The scheduler's overlap check must NOT treat Blocked as
// terminal (you may want to refire on top of a stuck run); the STATUS column
// should. After inactive-session fall-through, only fire_failed maps to FAILED.
//
// Order matters: a re-fire after a previous failure must show RUNNING, not
// FAILED. MarkFireStarted updates last_run_session_id but leaves
// last_run_outcome untouched, so the stale outcome would otherwise win.
func cronJobStatus(ctx context.Context, job *models.CronJob, sessions db.SessionStore, activity cron.ActivityChecker) pb.CronJobStatus {
	if job.LastRunSessionID != nil && *job.LastRunSessionID != "" {
		sess, err := sessions.Get(ctx, *job.LastRunSessionID)
		if err == nil && sess != nil && sess.ArchivedAt == nil {
			if activity != nil {
				// Liveness path (BOS-332): STATUS is RUNNING only when the
				// agent is actively working. RunActive is stricter than the
				// state-based fallback below — it counts only a live
				// ImplementingPlan run, so a post-implement wait (ReadyForReview,
				// AwaitingChecks, …) or a stranded pre-implement state reads as
				// not-running and falls through to GATED/FAILED/IDLE.
				if activity.RunActive(sess) {
					return pb.CronJobStatus_CRON_JOB_STATUS_RUNNING
				}
			} else if !cronStatusInactiveState(sess.State) {
				// Nil-activity fallback (legacy + widened): no liveness checker
				// wired, so approximate "still running" from session state.
				return pb.CronJobStatus_CRON_JOB_STATUS_RUNNING
			}
		}
	}
	// A gate block records outcome=gated without touching last_run_session_id,
	// so if a PRIOR fire's session is still non-terminal it shows RUNNING above
	// rather than GATED — which is accurate (that earlier run genuinely is still
	// in flight) and self-corrects once it terminates and the next gated tick
	// records again. GATED therefore applies once no live session remains.
	if job.LastRunOutcome != nil && *job.LastRunOutcome == models.CronJobOutcomeGated {
		return pb.CronJobStatus_CRON_JOB_STATUS_GATED
	}
	if job.LastRunOutcome != nil && isCronFailureOutcome(*job.LastRunOutcome) {
		return pb.CronJobStatus_CRON_JOB_STATUS_FAILED
	}
	return pb.CronJobStatus_CRON_JOB_STATUS_IDLE
}

// cronStatusInactiveState reports whether a session should NOT count as
// "running" when deriving cron STATUS. It is the fallback used only when no
// liveness checker is wired into cronJobStatus; the liveness path (RunActive)
// is stricter — only a live ImplementingPlan run counts as active — so this
// helper deliberately diverges from it in the same spirit as its Blocked
// divergence below.
//
// Diverges from cron.isTerminalState (which controls overlap-skip): Blocked is
// included here because PR/chat/cleanup housekeeping problems are session
// attention states, not active cron runs. Beyond the terminal Merged/Closed and
// Blocked, the post-implement waiting states (ReadyForReview, AwaitingChecks,
// FixingChecks, GreenDraft) and the terminal Orphaned state are also excluded:
// the agent is no longer working in any of them, so cron STATUS must not read
// RUNNING just because the row is non-terminal. Only fire_failed later maps
// cron STATUS to FAILED.
func cronStatusInactiveState(st machine.State) bool {
	switch st {
	case machine.Merged, machine.Closed, machine.Blocked,
		machine.ReadyForReview, machine.Orphaned,
		machine.AwaitingChecks, machine.FixingChecks, machine.GreenDraft:
		return true
	default:
		return false
	}
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
//     errored) - the agent never ran.
//   - gate_failed: the gate command could not be evaluated at all (timeout,
//     launch failure, shell 126/127), so the fire was blocked with the gate
//     condition UNKNOWN and the agent never ran (BOS-881). Deliberately
//     distinct from `gated`, which is a healthy skip the gate actually decided
//     and which keeps its own CRON_JOB_STATUS_GATED branch in cronJobStatus.
//     Collapsing the two is what let a repo-wide broken PATH read as a quiet,
//     healthy backlog sweep.
//
// Those two are the genuine cron failures. Every other outcome means the agent
// RAN (or, for `gated`, that the gate ran and said no). Housekeeping problems
// surface as a Blocked session (attention-needed) in the sessions list via
// FinalizeSession's needsAttention path - NOT as a red cron:
//   - pr_created, deleted_no_changes, pr_skipped_no_github - successful runs
//   - failed_recovered - daemon restarted mid-finalize; run already happened
//   - pr_failed - the agent ran; only PR creation/lookup failed (often a
//     transient gh/GitHub error). Shown as a Blocked session, not red cron.
//   - pr_no_changes - the agent ran and the PR was opened, but the branch
//     changes nothing against its base, so finalize refused to advertise it as
//     ready for review (BOS-591). Reachable from a cron run since that backstop
//     is deliberately not cron-exempt. Shown as a Blocked session, not red cron.
//   - chat_spawn_failed - the PR was created before the chat-spawn step failed
//   - cleanup_failed - the run completed; only worktree cleanup errored
//   - worktree_gone - finalize ran against an already-removed worktree
//     (archived/deleted session); a benign no-op, not a run failure
func isCronFailureOutcome(o models.CronJobOutcome) bool {
	return o == models.CronJobOutcomeFireFailed || o == models.CronJobOutcomeGateFailed
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
		Reason:         pb.AttentionReason(clampInt32(int(a.Reason))),
		Summary:        a.Summary,
		Since:          timestamppb.New(a.Since),
	}
}

// protoTime converts an optional protobuf Timestamp to a time.Time, mapping an
// absent timestamp to the ZERO time rather than the Unix epoch that
// (*timestamppb.Timestamp)(nil).AsTime() yields. Callers that treat "absent" as
// "no evidence" (e.g. the repair stall heartbeat) need the zero value.
func protoTime(ts *timestamppb.Timestamp) time.Time {
	if ts == nil {
		return time.Time{}
	}
	return ts.AsTime()
}

// protoToTimestamp converts an optional protobuf Timestamp to *time.Time.
func protoToTimestamp(ts *timestamppb.Timestamp) *time.Time {
	if ts == nil {
		return nil
	}
	t := ts.AsTime()
	return &t
}
