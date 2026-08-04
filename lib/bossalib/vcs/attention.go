package vcs

import (
	"time"

	"github.com/recurser/bossalib/machine"
	"github.com/recurser/bossalib/models"
)

// AttentionReason represents why a session needs human attention.
type AttentionReason int

const (
	AttentionReasonUnspecified AttentionReason = iota
	AttentionReasonBlockedMaxAttempts
	AttentionReasonAwaitingHumanInput
	AttentionReasonReviewRequested
	AttentionReasonMergeConflictUnresolvable
	// AttentionReasonAgentAuthFailed marks a session whose agent pane shows the
	// login-required terminal shape ("Not logged in" / "Please run /login").
	// Its integer value (5) matches pb.AttentionReason_ATTENTION_REASON_AGENT_AUTH_FAILED
	// so the direct cast in attentionStatusToProto stays 1:1. It is surfaced by
	// bossd's server hydration (which has the status tracker in scope), not by
	// ComputeAttentionStatus, so it is not returned here.
	AttentionReasonAgentAuthFailed
	// AttentionReasonAgentStalled marks a session whose chat reports
	// CHAT_STATUS_WORKING while the agent has made no semantic progress — no new
	// transcript record — for longer than its phase's threshold, i.e. a dead turn
	// behind a still-animating spinner. Its integer value (6) matches
	// pb.AttentionReason_ATTENTION_REASON_AGENT_STALLED so the direct cast in
	// attentionStatusToProto stays 1:1. Like AttentionReasonAgentAuthFailed it is
	// surfaced by bossd's status tracker, not by ComputeAttentionStatus, so it is
	// not returned here.
	AttentionReasonAgentStalled
)

// AttentionStatus represents whether and why a session needs human attention.
type AttentionStatus struct {
	NeedsAttention bool
	Reason         AttentionReason
	Summary        string
	Since          time.Time
}

// ComputeAttentionStatus determines whether a session needs human attention
// based on session state and repo automation flags.
func ComputeAttentionStatus(sess *models.Session, repo *models.Repo) AttentionStatus {
	switch sess.State {
	case machine.Blocked:
		summary := "blocked — needs human intervention"
		if sess.BlockedReason != nil {
			summary = *sess.BlockedReason
		}
		return AttentionStatus{
			NeedsAttention: true,
			Reason:         AttentionReasonBlockedMaxAttempts,
			Summary:        summary,
			Since:          sess.UpdatedAt,
		}

	case machine.Orphaned:
		// A headless run killed by a daemon restart: the human decides whether to
		// re-dispatch (a one-shot's prompt may have side effects), so surface it
		// loudly rather than letting the bootstrap-only PR read as done.
		summary := "orphaned — headless run killed by daemon restart; needs human"
		if sess.BlockedReason != nil && *sess.BlockedReason != "" {
			summary = *sess.BlockedReason
		}
		return AttentionStatus{
			NeedsAttention: true,
			Reason:         AttentionReasonAwaitingHumanInput,
			Summary:        summary,
			Since:          sess.UpdatedAt,
		}

	case machine.GreenDraft, machine.ReadyForReview:
		// Status column already shows this state; no attention alert needed.

	case machine.FixingChecks:
		if !repo.CanAutoRepair {
			return AttentionStatus{
				NeedsAttention: true,
				Reason:         AttentionReasonMergeConflictUnresolvable,
				Summary:        "automatic repair disabled, needs human",
				Since:          sess.UpdatedAt,
			}
		}

	case machine.CreatingWorktree, machine.StartingAgent, machine.PushingBranch,
		machine.OpeningDraftPR, machine.ImplementingPlan, machine.AwaitingChecks,
		machine.Merged, machine.Closed, machine.Finalizing:
		// These states don't require human attention.
	}

	return AttentionStatus{}
}
