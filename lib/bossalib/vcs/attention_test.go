package vcs

import (
	"testing"
	"time"

	"github.com/recurser/bossalib/machine"
	"github.com/recurser/bossalib/models"
)

func TestComputeAttentionStatus(t *testing.T) {
	now := time.Now()
	blockedReason := "max attempts reached (5)"

	tests := []struct {
		name          string
		session       *models.Session
		repo          *models.Repo
		wantAttention bool
		wantReason    AttentionReason
		wantSummary   string
	}{
		{
			name: "blocked session needs attention",
			session: &models.Session{
				State:         machine.Blocked,
				BlockedReason: &blockedReason,
				UpdatedAt:     now,
			},
			repo:          &models.Repo{},
			wantAttention: true,
			wantReason:    AttentionReasonBlockedMaxAttempts,
			wantSummary:   blockedReason,
		},
		{
			name: "blocked session without reason uses default summary",
			session: &models.Session{
				State:     machine.Blocked,
				UpdatedAt: now,
			},
			repo:          &models.Repo{},
			wantAttention: true,
			wantReason:    AttentionReasonBlockedMaxAttempts,
			wantSummary:   "fix loop exhausted, needs human intervention",
		},
		{
			name: "green draft with auto-merge off needs review",
			session: &models.Session{
				State:     machine.GreenDraft,
				UpdatedAt: now,
			},
			repo:          &models.Repo{CanAutoMerge: false},
			wantAttention: true,
			wantReason:    AttentionReasonReviewRequested,
			wantSummary:   "PR ready for human review",
		},
		{
			name: "green draft with auto-merge on does not need attention",
			session: &models.Session{
				State:     machine.GreenDraft,
				UpdatedAt: now,
			},
			repo:          &models.Repo{CanAutoMerge: true},
			wantAttention: false,
		},
		{
			name: "ready for review with auto-merge off needs review",
			session: &models.Session{
				State:     machine.ReadyForReview,
				UpdatedAt: now,
			},
			repo:          &models.Repo{CanAutoMerge: false},
			wantAttention: true,
			wantReason:    AttentionReasonReviewRequested,
			wantSummary:   "PR ready for human review",
		},
		{
			name: "ready for review with auto-merge on does not need attention",
			session: &models.Session{
				State:     machine.ReadyForReview,
				UpdatedAt: now,
			},
			repo:          &models.Repo{CanAutoMerge: true},
			wantAttention: false,
		},
		{
			name: "fixing checks with auto-resolve off needs attention",
			session: &models.Session{
				State:     machine.FixingChecks,
				UpdatedAt: now,
			},
			repo:          &models.Repo{CanAutoResolveConflicts: false},
			wantAttention: true,
			wantReason:    AttentionReasonMergeConflictUnresolvable,
			wantSummary:   "auto-resolve conflicts disabled, needs human",
		},
		{
			name: "fixing checks with auto-resolve on does not need attention",
			session: &models.Session{
				State:     machine.FixingChecks,
				UpdatedAt: now,
			},
			repo:          &models.Repo{CanAutoResolveConflicts: true},
			wantAttention: false,
		},
		{
			name: "implementing plan does not need attention",
			session: &models.Session{
				State:     machine.ImplementingPlan,
				UpdatedAt: now,
			},
			repo:          &models.Repo{},
			wantAttention: false,
		},
		{
			name: "awaiting checks does not need attention",
			session: &models.Session{
				State:     machine.AwaitingChecks,
				UpdatedAt: now,
			},
			repo:          &models.Repo{},
			wantAttention: false,
		},
		{
			name: "merged does not need attention",
			session: &models.Session{
				State:     machine.Merged,
				UpdatedAt: now,
			},
			repo:          &models.Repo{},
			wantAttention: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ComputeAttentionStatus(tt.session, tt.repo)

			if got.NeedsAttention != tt.wantAttention {
				t.Errorf("NeedsAttention = %v, want %v", got.NeedsAttention, tt.wantAttention)
			}
			if tt.wantAttention {
				if got.Reason != tt.wantReason {
					t.Errorf("Reason = %v, want %v", got.Reason, tt.wantReason)
				}
				if got.Summary != tt.wantSummary {
					t.Errorf("Summary = %q, want %q", got.Summary, tt.wantSummary)
				}
				if got.Since.IsZero() {
					t.Error("Since should not be zero when needs attention")
				}
			}
		})
	}
}
