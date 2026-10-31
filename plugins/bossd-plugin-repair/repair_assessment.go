package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sort"
	"strings"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	bossanovav1 "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/kit/chatlabel"
	"github.com/recurser/bossalib/sessionreason"
)

// baseMoveStreak counts the base tips a conflicted or failing head was retried on. A clean
// exit resets the persisted failure count, so an agent that exits cleanly
// without fixing the PR would otherwise start a new chat on every base
// move with nothing ever escalating. Like refusalStreak it is in memory: a
// plugin restart only delays the block.
type baseMoveStreak struct {
	headSHA string
	baseSHA string
	count   int
}

// noteBaseMoveLocked records a retry of headSHA on base tip baseSHA and reports
// whether the head has already used its maxRepairLoopAttempts base moves, in
// which case the caller blocks instead of retrying. A head move starts a fresh
// streak. A retry on the tip already counted is the same move: a refused start
// records no base, so the next sweep sees the same move again. Callers hold mu.
func (m *repairMonitor) noteBaseMoveLocked(sessionID, headSHA, baseSHA string) (baseMoveStreak, bool) {
	streak := m.baseMoves[sessionID]
	if streak.headSHA != headSHA {
		streak = baseMoveStreak{headSHA: headSHA}
	}
	if streak.count > 0 && streak.baseSHA == baseSHA {
		return streak, false
	}
	if streak.count >= maxRepairLoopAttempts {
		return streak, true
	}
	streak.baseSHA = baseSHA
	streak.count++
	m.baseMoves[sessionID] = streak
	return streak, false
}

// blockSession stops retrying a session repair cannot fix and hands it to a
// person: the session moves to Blocked with the concrete reason, which takes
// it out of the repairable set. A failed block floors the next retry at the
// base cooldown, so a session the machine will not block is not hammered.
func (m *repairMonitor) blockSession(ctx context.Context, sessionID, reason string) {
	log := m.logger.With().Str("session_id", sessionID).Str("reason", reason).Logger()
	if _, err := m.host.FireSessionEvent(ctx, &bossanovav1.FireSessionEventRequest{
		SessionId:     sessionID,
		Event:         bossanovav1.SessionEvent_SESSION_EVENT_BLOCK,
		BlockedReason: reason,
	}); err != nil {
		m.mu.Lock()
		m.cooldowns[sessionID] = time.Now()
		m.mu.Unlock()
		log.Warn().Err(err).Msg("repair gave up but could not block the session")
		return
	}
	if _, recoverable := sessionreason.ParseRepairCouldNotStart(&reason); recoverable {
		m.markRefusalBlocked(sessionID)
	} else {
		m.clearRefusals(sessionID)
	}
	m.mu.Lock()
	delete(m.baseMoves, sessionID)
	m.mu.Unlock()
	log.Info().Msg("repair gave up; session blocked for a person")
}

func shortSHA(sha string) string {
	if len(sha) > 12 {
		return sha[:12]
	}
	return sha
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

// sessionInfo is the small bundle maybeRepair needs about a target session
// to make its decisions. Decoupling from the proto session type keeps the
// idle-gate logic isolated and easy to test.
type sessionInfo struct {
	BlockedReason               *string
	LastRepairBlockedAt         time.Time
	StartBlock                  *sessionreason.RepairStartBlock
	Repairable                  bool
	HasActiveChat               bool
	LastChatActivityAt          time.Time // zero if no live chat or unknown
	RepoName                    string
	SessionTitle                string
	HeadSHA                     string
	LastRepairHeadSHA           string
	LastRepairDisplayStatus     bossanovav1.DisplayStatus
	LastRepairRunnerError       string
	LastRepairExitError         string
	LastRepairReviewFingerprint string
	LastRepairBaseSHA           string
	RepoOriginURL               string
	BaseBranch                  string
	PRNumber                    int32
	// LastRepairStartedAt and LastRepairAttemptCount are sourced from
	// the daemon-persisted last_repair_* columns so the cooldown gate
	// survives daemon restarts and plugin reloads. The previous
	// implementation only consulted the in-memory `m.cooldowns` map,
	// which reset to empty on every restart and let the failed PRs
	// re-fire immediately — one of the reasons the production counter
	// reached 321×.
	LastRepairStartedAt    time.Time
	LastRepairAttemptCount int32
}

type reviewFingerprintComment struct {
	Path    string `json:"path"`
	HasPath bool   `json:"has_path"`
	Line    int32  `json:"line"`
	HasLine bool   `json:"has_line"`
	Author  string `json:"author"`
	State   int32  `json:"state"`
	Body    string `json:"body"`
}

func reviewFingerprint(comments []*bossanovav1.ReviewComment) (string, error) {
	if len(comments) == 0 {
		return "", nil
	}
	parts := make([]string, 0, len(comments))
	for _, c := range comments {
		if c == nil || !isActionableReviewComment(c) {
			continue
		}
		comment := reviewFingerprintComment{
			HasPath: c.Path != nil,
			HasLine: c.Line != nil,
			Author:  c.GetAuthor(),
			State:   int32(c.GetState()),
			Body:    c.GetBody(),
		}
		if comment.HasPath {
			comment.Path = c.GetPath()
		}
		if comment.HasLine {
			comment.Line = c.GetLine()
		}
		encoded, err := json.Marshal(comment)
		if err != nil {
			return "", err
		}
		parts = append(parts, string(encoded))
	}
	if len(parts) == 0 {
		return "", nil
	}
	sort.Strings(parts)
	payload, err := json.Marshal(parts)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(payload)
	return hex.EncodeToString(sum[:]), nil
}

func isActionableReviewComment(c *bossanovav1.ReviewComment) bool {
	switch c.GetState() {
	case bossanovav1.ReviewState_REVIEW_STATE_CHANGES_REQUESTED,
		bossanovav1.ReviewState_REVIEW_STATE_COMMENTED:
		return true
	default:
		return false
	}
}

// lookupSession resolves the daemon's view of the session and returns the
// fields maybeRepair needs. Returns Repairable=false as a fail-safe if the
// session is in a state where repair would be invalid for the given
// displayStatus (eg. implementing_plan where check failures are expected),
// if the state cannot be determined, or if the session is not found.
//
// The repairable-state set is keyed off displayStatus because the safety
// reasoning differs per signal — see isRepairableState.
func (m *repairMonitor) lookupSession(ctx context.Context, sessionID string, displayStatus bossanovav1.DisplayStatus) sessionInfo {
	resp, err := m.host.ListSessions(ctx)
	if err != nil {
		m.logger.Warn().Err(err).
			Str("session_id", sessionID).
			Msg("failed to list sessions, assuming not repairable")
		return sessionInfo{}
	}

	for _, sess := range resp.GetSessions() {
		if sess.GetId() != sessionID {
			continue
		}
		info := sessionInfo{
			HasActiveChat:               sess.GetHasActiveChat(),
			BlockedReason:               sess.BlockedReason,
			RepoName:                    sess.GetRepoDisplayName(),
			SessionTitle:                sess.GetTitle(),
			HeadSHA:                     sess.GetPrDisplayHeadSha(),
			LastRepairHeadSHA:           sess.GetLastRepairHeadSha(),
			LastRepairDisplayStatus:     sess.GetLastRepairDisplayStatus(),
			LastRepairRunnerError:       sess.GetLastRepairRunnerError(),
			LastRepairExitError:         sess.GetLastRepairExitError(),
			LastRepairReviewFingerprint: sess.GetLastRepairReviewFingerprint(),
			LastRepairBaseSHA:           sess.GetLastRepairBaseSha(),
			RepoOriginURL:               sess.GetRepoOriginUrl(),
			BaseBranch:                  sess.GetBaseBranch(),
			PRNumber:                    sess.GetPrNumber(),
			LastRepairAttemptCount:      sess.GetLastRepairAttemptCount(),
		}
		if ts := sess.GetLastChatActivityAt(); ts != nil {
			info.LastChatActivityAt = ts.AsTime()
		}
		if ts := sess.GetLastRepairStartedAt(); ts != nil {
			info.LastRepairStartedAt = ts.AsTime()
		}
		// Per-repo kill switch: skip repair entirely when the repo's auto-repair
		// toggle is off, regardless of session state. This is the single gate that
		// makes the "Automatic repair" repo setting effective for both the
		// edge-triggered (NotifyStatusChange) and startup-sweep paths.
		if !sess.GetRepoCanAutoRepair() {
			m.logger.Info().
				Str("session_id", sessionID).
				Str("repo", info.RepoName).
				Str("session_name", info.SessionTitle).
				Msg("automatic repair disabled for repo, skipping repair")
			return info
		}
		if sess.GetIsVerifyOnly() {
			m.logger.Info().
				Str("session_id", sessionID).
				Str("repo", info.RepoName).
				Str("session_name", info.SessionTitle).
				Msg("session adopted only for verify, skipping repair")
			return info
		}

		if ts := sess.GetLastRepairBlockedAt(); ts != nil {
			info.LastRepairBlockedAt = ts.AsTime()
		}
		if sess.GetState() == bossanovav1.SessionState_SESSION_STATE_BLOCKED {
			if block, ok := sessionreason.ParseRepairCouldNotStart(info.BlockedReason); ok {
				info.StartBlock = &block
			}
		}
		state := sess.GetState()
		if isRepairableState(state, displayStatus) {
			info.Repairable = true
			return info
		}
		m.logger.Info().
			Str("session_id", sessionID).
			Str("session_name", info.SessionTitle).
			Str("repo", info.RepoName).
			Str("state", state.String()).
			Str("display_status", displayStatus.String()).
			Msg("session not in repairable state, skipping repair")
		return info
	}

	m.logger.Warn().
		Str("session_id", sessionID).
		Msg("session not found, assuming not repairable")
	return sessionInfo{}
}

func sessionByID(sessions []*bossanovav1.Session, sessionID string) *bossanovav1.Session {
	for _, sess := range sessions {
		if sess.GetId() == sessionID {
			return sess
		}
	}
	return nil
}

// currentReviewFingerprint fetches a stable hash of the PR's actionable review
// comments. It deliberately does NOT gate on display status: a PR agent can
// leave a new review comment (with sub-comments) without the host flipping the
// PR to REJECTED, so a green/PASSING PR can still have fresh feedback that must
// re-trigger the repair loop. The returned bool reports whether the fingerprint
// is authoritative (true even for zero comments, where the fingerprint is "");
// false means we could not fetch it and callers should fall back to the legacy
// head-SHA + display-status duplicate guard.
func (m *repairMonitor) currentReviewFingerprint(ctx context.Context, info sessionInfo) (string, bool) {
	if info.RepoOriginURL == "" || info.PRNumber == 0 {
		m.logger.Warn().
			Str("repo", info.RepoName).
			Str("session_name", info.SessionTitle).
			Msg("session missing repo origin or PR number; using legacy duplicate-repair guard")
		return "", false
	}
	resp, err := m.host.GetReviewComments(ctx, &bossanovav1.GetReviewCommentsRequest{
		RepoOriginUrl: info.RepoOriginURL,
		PrNumber:      info.PRNumber,
	})
	if err != nil {
		m.logger.Warn().Err(err).
			Str("repo", info.RepoName).
			Int32("pr_number", info.PRNumber).
			Msg("failed to fetch review comments; using legacy duplicate-repair guard")
		return "", false
	}
	fingerprint, err := reviewFingerprint(resp.GetComments())
	if err != nil {
		m.logger.Warn().Err(err).
			Str("repo", info.RepoName).
			Int32("pr_number", info.PRNumber).
			Msg("failed to compute review fingerprint; using legacy duplicate-repair guard")
		return "", false
	}
	return fingerprint, true
}

// currentBaseSHA reads the live base tip for conflicts and failing checks.
// Review feedback does not depend on the base and skips the read.
// false means the tip is unknown, and
// the duplicate guards fall back to head SHA + display status.
func (m *repairMonitor) currentBaseSHA(ctx context.Context, info sessionInfo, displayStatus bossanovav1.DisplayStatus) (string, bool) {
	if displayStatus != bossanovav1.DisplayStatus_DISPLAY_STATUS_CONFLICT && displayStatus != bossanovav1.DisplayStatus_DISPLAY_STATUS_FAILING {
		return "", false
	}
	if info.RepoOriginURL == "" || info.BaseBranch == "" {
		m.logger.Warn().
			Str("repo", info.RepoName).
			Str("session_name", info.SessionTitle).
			Msg("session missing repo origin or base branch; using legacy duplicate-repair guard")
		return "", false
	}
	resp, err := m.host.GetBranchHead(ctx, &bossanovav1.GetBranchHeadRequest{
		RepoOriginUrl: info.RepoOriginURL,
		Branch:        info.BaseBranch,
	})
	if err != nil || resp.GetSha() == "" {
		m.logger.Warn().Err(err).
			Str("repo", info.RepoName).
			Str("base_branch", info.BaseBranch).
			Msg("failed to read base branch tip; using legacy duplicate-repair guard")
		return "", false
	}
	return resp.GetSha(), true
}

// isRepairableState answers "is it safe and meaningful to run /boss-repair
// on this (state, displayStatus) pair?".
//
//   - FAILING / REJECTED: the signal only exists in the CI/review cycle, so
//     repair is gated to those states. Outside the cycle the agent has
//     nothing to look at.
//   - CONFLICT: a PR can become unmergeable at any post-PR lifecycle stage
//     once main moves; the repair skill rebases-and-pushes regardless of
//     plan-completion or finalize state. We therefore also allow Finalizing.
//
// Blocked stays excluded everywhere: it is the manual-intervention terminal
// state and auto-repair would defeat the max-attempts guardrail.
// ImplementingPlan stays excluded: the idle-chat heuristic owns that phase.
func isRepairableState(state bossanovav1.SessionState, displayStatus bossanovav1.DisplayStatus) bool {
	switch state {
	case bossanovav1.SessionState_SESSION_STATE_AWAITING_CHECKS,
		bossanovav1.SessionState_SESSION_STATE_FIXING_CHECKS,
		bossanovav1.SessionState_SESSION_STATE_GREEN_DRAFT,
		bossanovav1.SessionState_SESSION_STATE_READY_FOR_REVIEW:
		return true
	case bossanovav1.SessionState_SESSION_STATE_FINALIZING:
		return displayStatus == bossanovav1.DisplayStatus_DISPLAY_STATUS_CONFLICT
	default:
		return false
	}
}

// hasUnresolvedReviewFeedback reports whether a PR still carries open
// changes-requested review feedback. REJECTED always does. The verify statuses
// outrank REJECTED (BOS-1382), so on them an open changes-requested review
// rides along as metadata and still counts.
func hasUnresolvedReviewFeedback(status bossanovav1.DisplayStatus, changesRequested bool) bool {
	switch status {
	case bossanovav1.DisplayStatus_DISPLAY_STATUS_REJECTED:
		return true
	case bossanovav1.DisplayStatus_DISPLAY_STATUS_VERIFYING,
		bossanovav1.DisplayStatus_DISPLAY_STATUS_NEEDS_HUMAN:
		return changesRequested
	default:
		return false
	}
}

func assessPostRepairStatus(session *bossanovav1.Session) postRepairAssessment {
	if session == nil {
		return postRepairAssessment{
			Status: postRepairStatusUnknown,
			Reason: "session missing",
		}
	}

	assessment := postRepairAssessment{
		DisplayStatus: session.GetDisplayStatus(),
		HasFailures:   session.GetDisplayHasFailures(),
		HeadSHA:       session.GetPrDisplayHeadSha(),
	}

	if hasUnresolvedReviewFeedback(session.GetDisplayStatus(), session.GetDisplayHasChangesRequested()) {
		assessment.Status = postRepairStatusNeedsRepair
		assessment.Reason = "review feedback"
		return assessment
	}

	switch session.GetDisplayStatus() {
	case bossanovav1.DisplayStatus_DISPLAY_STATUS_CHECKING:
		assessment.Status = postRepairStatusPending
		assessment.Reason = "checks still running"
	case bossanovav1.DisplayStatus_DISPLAY_STATUS_PASSING,
		bossanovav1.DisplayStatus_DISPLAY_STATUS_APPROVED,
		bossanovav1.DisplayStatus_DISPLAY_STATUS_REVIEW:
		assessment.Status = postRepairStatusClean
		assessment.Reason = "checks passed"
	// BOS-1382: both verify statuses mean ordinary CI is green and the verify
	// stage holds the head. Waiting on them as "pending" could wait forever on a
	// park; a later verify failure arrives as FAILING through the normal trigger.
	// Open review feedback on them was handled above.
	case bossanovav1.DisplayStatus_DISPLAY_STATUS_VERIFYING:
		assessment.Status = postRepairStatusClean
		assessment.Reason = "checks passed; verification in progress"
	case bossanovav1.DisplayStatus_DISPLAY_STATUS_NEEDS_HUMAN:
		assessment.Status = postRepairStatusClean
		assessment.Reason = "parked for a human"
	case bossanovav1.DisplayStatus_DISPLAY_STATUS_FAILING:
		assessment.Status = postRepairStatusNeedsRepair
		assessment.Reason = "checks failed"
	case bossanovav1.DisplayStatus_DISPLAY_STATUS_CONFLICT:
		assessment.Status = postRepairStatusNeedsRepair
		assessment.Reason = "merge conflict"
	case bossanovav1.DisplayStatus_DISPLAY_STATUS_UNSPECIFIED:
		assessment.Status = postRepairStatusUnknown
		assessment.Reason = "display status unspecified"
	case bossanovav1.DisplayStatus_DISPLAY_STATUS_IDLE,
		bossanovav1.DisplayStatus_DISPLAY_STATUS_DRAFT,
		bossanovav1.DisplayStatus_DISPLAY_STATUS_MERGED,
		bossanovav1.DisplayStatus_DISPLAY_STATUS_CLOSED:
		assessment.Status = postRepairStatusClean
		assessment.Reason = "not repairable"
	default:
		assessment.Status = postRepairStatusUnknown
		assessment.Reason = "display status unrecognized"
	}

	return assessment
}

func (a postRepairAssessment) withReviewComparison(
	previousReviewFingerprint string,
	previousReviewFingerprintAvailable bool,
) postRepairAssessment {
	// Both fingerprints must be authoritative to compare. An empty string is a
	// valid value here (it means "fetched, zero actionable comments"), so we
	// gate on availability rather than emptiness — otherwise a zero-comments to
	// one-comment transition would be silently dropped.
	if !previousReviewFingerprintAvailable || !a.ReviewFingerprintAvailable {
		return a
	}
	if previousReviewFingerprint != a.ReviewFingerprint && a.ReviewFingerprint != "" {
		a.Status = postRepairStatusNeedsRepair
		a.Reason = "new review feedback"
	}
	return a
}

func (m *repairMonitor) waitForPostRepairAssessment(
	ctx context.Context,
	sessionID string,
	currentDisplayStatus bossanovav1.DisplayStatus,
	currentHasFailures bool,
	currentHeadSHA string,
	previousReviewFingerprint string,
	previousReviewFingerprintAvailable bool,
) postRepairAssessment {
	m.mu.Lock()
	pollInterval := m.config.postRepairPollInterval()
	waitTimeout := m.config.postRepairWaitTimeout()
	m.mu.Unlock()

	waitCtx, cancel := context.WithTimeout(ctx, waitTimeout)
	defer cancel()

	cancelledAssessment := func(err error) postRepairAssessment {
		if err == context.DeadlineExceeded {
			return postRepairAssessment{
				Status: postRepairStatusPending,
				Reason: "post-repair checks did not settle before timeout",
			}
		}
		return postRepairAssessment{
			Status: postRepairStatusUnknown,
			Reason: "post-repair wait cancelled: " + err.Error(),
		}
	}

	for {
		if err := waitCtx.Err(); err != nil {
			return cancelledAssessment(err)
		}

		resp, err := m.host.ListSessions(waitCtx)
		if err != nil {
			if ctxErr := waitCtx.Err(); ctxErr != nil {
				return cancelledAssessment(ctxErr)
			}
			return postRepairAssessment{
				Status: postRepairStatusUnknown,
				Reason: "list sessions failed: " + err.Error(),
			}
		}

		sess := sessionByID(resp.GetSessions(), sessionID)
		assessment := assessPostRepairStatus(sess)
		// Populate the review fingerprint once checks have reached a terminal
		// status (clean or needs-repair). We deliberately skip the fetch while
		// checks are still pending: that avoids a per-poll GetReviewComments RPC
		// and stops a comment landing mid-check from cutting the wait short.
		// Fetching it on needs-repair too keeps the duplicate-input guard and the
		// next iteration's baseline accurate. The new-feedback upgrade only
		// applies on a clean status, since that is the one case the base
		// assessment would otherwise miss (a green PR with a fresh comment that
		// the host has not flipped to REJECTED).
		if assessment.Status == postRepairStatusClean || assessment.Status == postRepairStatusNeedsRepair {
			info := sessionInfo{
				RepoName:      sess.GetRepoDisplayName(),
				SessionTitle:  sess.GetTitle(),
				RepoOriginURL: sess.GetRepoOriginUrl(),
				PRNumber:      sess.GetPrNumber(),
			}
			reviewFingerprint, reviewFingerprintAvailable := m.currentReviewFingerprint(waitCtx, info)
			assessment.ReviewFingerprint = reviewFingerprint
			assessment.ReviewFingerprintAvailable = reviewFingerprintAvailable
			if assessment.Status == postRepairStatusClean {
				assessment = assessment.withReviewComparison(previousReviewFingerprint, previousReviewFingerprintAvailable)
			}
		}

		switch assessment.Status {
		case postRepairStatusClean, postRepairStatusUnknown:
			return assessment
		case postRepairStatusNeedsRepair:
			if postRepairInputChanged(
				currentDisplayStatus,
				currentHasFailures,
				currentHeadSHA,
				previousReviewFingerprint,
				previousReviewFingerprintAvailable,
				assessment,
			) {
				return assessment
			}
			m.logger.Debug().
				Str("session_id", sessionID).
				Str("reason", assessment.Reason).
				Int32("display_status", int32(assessment.DisplayStatus)).
				Bool("has_failures", assessment.HasFailures).
				Str("head_sha", assessment.HeadSHA).
				Msg("post-repair assessment still matches pre-repair input, continuing wait")
			fallthrough
		case postRepairStatusPending:
			timer := time.NewTimer(pollInterval)
			select {
			case <-waitCtx.Done():
				timer.Stop()
				return cancelledAssessment(waitCtx.Err())
			case <-timer.C:
			}
		default:
			assessment.Status = postRepairStatusUnknown
			assessment.Reason = "post-repair status unrecognized"
			return assessment
		}
	}
}

// advanceStuckSession checks if a session is stuck in ImplementingPlan and
// should be advanced to the next state. A session is considered stuck when:
//  1. It is in ImplementingPlan state
//  2. It has a branch or a PR — a zero-output session (quick chat) has
//     neither, has no PR lifecycle to enter, and is never advanced (BOS-1514)
//  3. It has no active Claude Code chat (heartbeat tracker shows no live processes)
//  4. Its updated_at is older than the configured stuck timeout
//  5. It has no running workflows (is idle)
//
// Returns true if the session was advanced.
func (m *repairMonitor) advanceStuckSession(ctx context.Context, sess *bossanovav1.Session) bool {
	if sess.GetState() != bossanovav1.SessionState_SESSION_STATE_IMPLEMENTING_PLAN {
		return false
	}

	// The Session proto does not carry is_quick_chat; a quick chat runs in
	// the repo checkout, so it leaves the branch empty and never has a PR.
	// PrNumber is checked for presence, not GetPrNumber() == 0. The host's
	// FireSessionEvent guard is the authoritative backstop.
	if sess.GetBranchName() == "" && sess.PrNumber == nil {
		m.logger.Debug().
			Str("session_id", sess.GetId()).
			Msg("zero-output session (no branch, no PR), not advancing")
		return false
	}

	// Do not advance sessions when the workflow is stopped or paused.
	m.mu.Lock()
	if m.stopped || m.paused {
		m.mu.Unlock()
		return false
	}
	m.mu.Unlock()

	// Extract updatedAt early so both hasActiveChat and stuck timeout can use it.
	updatedAt := sess.GetUpdatedAt()

	// Skip if the session has an active Claude Code chat process,
	// unless stuck far beyond normal timeout (zombie/idle chat).
	if sess.GetHasActiveChat() {
		m.mu.Lock()
		forceTimeout := m.testForceAdvanceTimeout
		if forceTimeout == 0 {
			forceTimeout = m.config.forceAdvanceTimeout()
		}
		m.mu.Unlock()

		if updatedAt == nil || time.Since(updatedAt.AsTime()) < forceTimeout {
			m.logger.Debug().
				Str("session_id", sess.GetId()).
				Msg("session has active chat, not advancing")
			return false
		}
		m.logger.Warn().
			Str("session_id", sess.GetId()).
			Str("title", sess.GetTitle()).
			Dur("stuck_for", time.Since(updatedAt.AsTime())).
			Msg("force-advancing stuck session despite active chat (zombie process suspected)")
	} else {
		// Check the stuck timeout (for sessions WITHOUT active chat).
		m.mu.Lock()
		timeout := m.testStuckTimeout
		if timeout == 0 {
			timeout = m.config.stuckTimeout()
		}
		m.mu.Unlock()

		if updatedAt == nil || time.Since(updatedAt.AsTime()) < timeout {
			return false
		}
	}

	// Fire PlanComplete to advance the state machine. With host-side
	// workflow CRUD removed, the daemon's per-session active-run mutex on
	// StartClaudeRun is the only source of truth for "session has work in
	// flight" — we no longer have a host-side dedup query to consult here.
	// In practice the active-chat heuristic above already gates this path.
	m.logger.Info().
		Str("session_id", sess.GetId()).
		Str("title", sess.GetTitle()).
		Str("repo", sess.GetRepoDisplayName()).
		Dur("stuck_for", time.Since(updatedAt.AsTime())).
		Msg("advancing stuck ImplementingPlan session")

	if _, err := m.host.FireSessionEvent(ctx, &bossanovav1.FireSessionEventRequest{
		SessionId: sess.GetId(),
		Event:     bossanovav1.SessionEvent_SESSION_EVENT_PLAN_COMPLETE,
	}); err != nil {
		m.logger.Warn().Err(err).
			Str("session_id", sess.GetId()).
			Msg("failed to advance stuck session")
		return false
	}

	return true
}

// sweepExistingSessions queries all sessions and runs each through the repair
// logic. This catches sessions that were already in a bad state when the plugin
// started (or when a race caused the first notification to be dropped).
func (m *repairMonitor) sweepExistingSessions(ctx context.Context) {
	m.logger.Info().Msg("sweeping existing sessions for repairable state")

	resp, err := m.host.ListSessions(ctx)
	if err != nil {
		m.logger.Error().Err(err).Msg("failed to list sessions for sweep")
		return
	}

	// First pass: advance any sessions stuck in ImplementingPlan.
	var advanced int
	for _, sess := range resp.GetSessions() {
		if m.advanceStuckSession(ctx, sess) {
			advanced++
		}
	}

	// If any sessions were advanced, re-fetch so the second pass sees their
	// new state (e.g. AwaitingChecks) and can evaluate them for repair.
	if advanced > 0 {
		m.logger.Info().Int("advanced", advanced).Msg("re-fetching sessions after advancing stuck sessions")
		resp, err = m.host.ListSessions(ctx)
		if err != nil {
			m.logger.Error().Err(err).Msg("failed to re-list sessions after advancement")
			return
		}
	}

	// Second pass: evaluate each session for repair.
	for _, sess := range resp.GetSessions() {
		m.maybeRepair(sess.GetId(), sess.GetDisplayStatus(), sess.GetDisplayHasFailures())
	}

	m.logger.Info().
		Int("session_count", len(resp.GetSessions())).
		Int("advanced", advanced).
		Msg("sweep complete")
}

// periodicSweep runs sweepExistingSessions at a regular interval to catch
// sessions stuck in a repairable state that were missed by edge-triggered
// notifications (e.g. failed repair with no re-notification, or session
// transitioning to a repairable state after the initial notification).
func (m *repairMonitor) periodicSweep(ctx context.Context) {
	m.mu.Lock()
	interval := m.testSweepInterval
	if interval == 0 {
		interval = m.config.sweepInterval()
	}
	m.mu.Unlock()

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			m.sweepExistingSessions(ctx)
		}
	}
}

func repairStartChatRunRequest(sessionID string, displayStatus bossanovav1.DisplayStatus, replaceExistingReason string, replaceExistingObservedLastChatActivityAt time.Time, resumeAgentSessionID string) *bossanovav1.StartChatRunHostRequest {
	reason := ""
	switch displayStatus {
	case bossanovav1.DisplayStatus_DISPLAY_STATUS_CONFLICT:
		reason = "conflict"
	case bossanovav1.DisplayStatus_DISPLAY_STATUS_FAILING:
		reason = "checks"
	case bossanovav1.DisplayStatus_DISPLAY_STATUS_REJECTED:
		reason = "review"
	default:
		// Other statuses use the bare repair title.
	}
	req := &bossanovav1.StartChatRunHostRequest{
		SessionId:            sessionID,
		Command:              repairCommand,
		Title:                chatlabel.Title(chatlabel.RoleRepair, reason),
		ResumeAgentSessionId: resumeAgentSessionID,
		Role:                 bossanovav1.ChatRole_CHAT_ROLE_REPAIR,
	}
	if replaceExistingReason != "" {
		req.ReplaceExistingChat = true
		req.ReplaceExistingReason = replaceExistingReason
		if !replaceExistingObservedLastChatActivityAt.IsZero() {
			req.ReplaceExistingObservedLastChatActivityAt = timestamppb.New(replaceExistingObservedLastChatActivityAt)
		}
	}
	return req
}

func postRepairInputChanged(
	currentDisplayStatus bossanovav1.DisplayStatus,
	currentHasFailures bool,
	currentHeadSHA string,
	currentReviewFingerprint string,
	currentReviewFingerprintAvailable bool,
	assessment postRepairAssessment,
) bool {
	if assessment.DisplayStatus != currentDisplayStatus {
		return true
	}
	if assessment.HasFailures != currentHasFailures {
		return true
	}
	if assessment.HeadSHA != "" && assessment.HeadSHA != currentHeadSHA {
		return true
	}
	if assessment.ReviewFingerprintAvailable != currentReviewFingerprintAvailable {
		return true
	}
	if assessment.ReviewFingerprintAvailable && assessment.ReviewFingerprint != currentReviewFingerprint {
		return true
	}
	return false
}

// repairSession drives bounded repair attempts until post-repair PR checks
// settle cleanly, need another fresh repair run, or stop making safe progress.
//
// Concurrency: the caller holds the maybeRepair guard (m.repairing). The guard
// is cleared after the bounded loop exits, so no other repair goroutine can
// start for the same session while post-repair checks are still settling.
func (m *repairMonitor) repairSession(
	ctx context.Context,
	sessionID, repoName, sessionName string,
	displayStatus bossanovav1.DisplayStatus,
	hasFailures bool,
	headSHA string,
	replaceExistingReason string,
	replaceExistingObservedLastChatActivityAt time.Time,
	reviewFingerprint string,
	reviewFingerprintAvailable bool,
	baseSHA string,
) {
	log := m.logger.With().
		Str("session_id", sessionID).
		Str("session_name", sessionName).
		Str("repo", repoName).
		Logger()

	// repairFlagSet records whether any attempt in this session took the
	// single-repairer lease (SetRepairStatus IsRepairing=true). The lease is
	// held for the whole repairSession — every attempt AND its post-repair
	// assessment window (which can wait up to 30 minutes) — and released only
	// here, once the bounded loop exits. Releasing it per-attempt (inside
	// runRepairAttempt) would drop repair_active to false during the assessment
	// wait, letting a boss-epic/manual dispatcher start a second /boss-repair on
	// the same worktree.
	repairFlagSet := false

	defer func() {
		m.mu.Lock()
		delete(m.repairing, sessionID)
		m.mu.Unlock()

		if !repairFlagSet {
			return
		}
		cleanupCtx, cancel := context.WithTimeout(context.Background(), repairStatusClearTimeout)
		defer cancel()
		if _, err := m.host.SetRepairStatus(cleanupCtx, &bossanovav1.SetRepairStatusRequest{
			SessionId:    sessionID,
			IsRepairing:  false,
			DispatcherId: m.dispatcherID,
		}); err != nil {
			log.Warn().Err(err).Msg("failed to clear repair status")
		}
	}()

	currentDisplayStatus := displayStatus
	currentHasFailures := hasFailures
	currentHeadSHA := headSHA
	currentReviewFingerprint := reviewFingerprint
	currentReviewFingerprintAvailable := reviewFingerprintAvailable

	// resumeAgentSessionID carries the agent session started by the previous
	// loop iteration into the next one, so each repair attempt resumes (claude
	// `--resume`) the same conversation and the agent remembers what it already
	// tried. The first attempt starts fresh (empty). Resume targets are always
	// intra-repairSession: a daemon restart kills this goroutine and the next
	// repairSession starts at attempt 1 with an empty target, so claude's local
	// session storage always still contains the target. Cross-restart resume is
	// intentionally never attempted.
	resumeAgentSessionID := ""

	for attempt := 1; attempt <= maxRepairLoopAttempts; attempt++ {
		m.mu.Lock()
		stopped, paused := m.stopped, m.paused
		m.mu.Unlock()
		if stopped || paused {
			log.Info().
				Int("attempt", attempt).
				Bool("stopped", stopped).
				Bool("paused", paused).
				Msg("workflow stopped or paused, stopping repair loop")
			return
		}

		nextResumeAgentSessionID, ok := m.runRepairAttempt(
			ctx,
			sessionID,
			repoName,
			sessionName,
			currentDisplayStatus,
			currentHasFailures,
			currentHeadSHA,
			replaceExistingReason,
			replaceExistingObservedLastChatActivityAt,
			currentReviewFingerprint,
			currentReviewFingerprintAvailable,
			attemptBaseSHA(currentDisplayStatus, baseSHA),
			resumeAgentSessionID,
			&repairFlagSet,
		)
		if !ok {
			return
		}
		// Next iteration resumes the run we just completed. Providers such as
		// Codex may return a provider-owned resume id instead of the logical
		// bossd agent_session_id.
		resumeAgentSessionID = nextResumeAgentSessionID

		assessment := m.waitForPostRepairAssessment(
			ctx,
			sessionID,
			currentDisplayStatus,
			currentHasFailures,
			currentHeadSHA,
			currentReviewFingerprint,
			currentReviewFingerprintAvailable,
		)
		log.Info().
			Int("attempt", attempt).
			Str("post_repair_status", string(assessment.Status)).
			Str("reason", assessment.Reason).
			Int32("display_status", int32(assessment.DisplayStatus)).
			Bool("has_failures", assessment.HasFailures).
			Str("review_fingerprint", assessment.ReviewFingerprint).
			Bool("review_fingerprint_available", assessment.ReviewFingerprintAvailable).
			Msg("post-repair status assessed")

		switch assessment.Status {
		case postRepairStatusClean:
			return
		case postRepairStatusNeedsRepair:
			if !postRepairInputChanged(
				currentDisplayStatus,
				currentHasFailures,
				currentHeadSHA,
				currentReviewFingerprint,
				currentReviewFingerprintAvailable,
				assessment,
			) {
				log.Info().
					Str("reason", assessment.Reason).
					Int32("display_status", int32(assessment.DisplayStatus)).
					Bool("has_failures", assessment.HasFailures).
					Str("head_sha", currentHeadSHA).
					Str("review_fingerprint", assessment.ReviewFingerprint).
					Bool("review_fingerprint_available", assessment.ReviewFingerprintAvailable).
					Msg("post-repair input unchanged, stopping repair loop")
				return
			}
			if attempt == maxRepairLoopAttempts {
				log.Warn().
					Int("max_attempts", maxRepairLoopAttempts).
					Str("reason", assessment.Reason).
					Msg("maximum repair loop attempts reached, stopping")
				return
			}
			currentDisplayStatus = assessment.DisplayStatus
			currentHasFailures = assessment.HasFailures
			if assessment.HeadSHA != "" {
				currentHeadSHA = assessment.HeadSHA
			}
			currentReviewFingerprint = assessment.ReviewFingerprint
			currentReviewFingerprintAvailable = assessment.ReviewFingerprintAvailable
		case postRepairStatusPending:
			log.Warn().
				Int("attempt", attempt).
				Str("reason", assessment.Reason).
				Msg("post-repair checks did not settle before timeout, stopping repair loop")
			return
		case postRepairStatusUnknown:
			log.Warn().
				Int("attempt", attempt).
				Str("reason", assessment.Reason).
				Msg("post-repair status could not be determined, stopping repair loop")
			return
		default:
			log.Warn().
				Str("post_repair_status", string(assessment.Status)).
				Msg("unrecognized post-repair status, stopping")
			return
		}
	}
}

// attemptBaseSHA records the base for conflicts and failing checks, whose
// repair input can change when the base moves.
func attemptBaseSHA(displayStatus bossanovav1.DisplayStatus, baseSHA string) string {
	if displayStatus != bossanovav1.DisplayStatus_DISPLAY_STATUS_CONFLICT && displayStatus != bossanovav1.DisplayStatus_DISPLAY_STATUS_FAILING {
		return ""
	}
	return baseSHA
}
