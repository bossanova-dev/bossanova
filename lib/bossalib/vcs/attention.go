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
		summary := "fix loop exhausted, needs human intervention"
		if sess.BlockedReason != nil {
			summary = *sess.BlockedReason
		}
		return AttentionStatus{
			NeedsAttention: true,
			Reason:         AttentionReasonBlockedMaxAttempts,
			Summary:        summary,
			Since:          sess.UpdatedAt,
		}

	case machine.GreenDraft, machine.ReadyForReview:
		// Status column already shows this state; no attention alert needed.

	case machine.FixingChecks:
		if !repo.CanAutoResolveConflicts {
			return AttentionStatus{
				NeedsAttention: true,
				Reason:         AttentionReasonMergeConflictUnresolvable,
				Summary:        "auto-resolve conflicts disabled, needs human",
				Since:          sess.UpdatedAt,
			}
		}

	case machine.CreatingWorktree, machine.StartingClaude, machine.PushingBranch,
		machine.OpeningDraftPR, machine.ImplementingPlan, machine.AwaitingChecks,
		machine.Merged, machine.Closed, machine.Finalizing:
		// These states don't require human attention.
	}

	return AttentionStatus{}
}
