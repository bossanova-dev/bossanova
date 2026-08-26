package vcs

import (
	"testing"
	"time"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/machine"
	"github.com/recurser/bossalib/models"
)

// TestAttentionReasonMirrorsProto pins every AttentionReason constant to the
// proto enum number it mirrors. The enum is hand-mirrored across
// lib/bossalib/vcs/attention.go and proto/bossanova/v1/models.proto, and
// bossd's attentionStatusToProto casts one straight to the other, so a value
// added to only one side (or renumbered on either) silently mislabels every
// session's attention reason. Compare numerically rather than by name: a cast
// is exactly what production does.
func TestAttentionReasonMirrorsProto(t *testing.T) {
	tests := []struct {
		name string
		got  AttentionReason
		want pb.AttentionReason
	}{
		{"unspecified", AttentionReasonUnspecified, pb.AttentionReason_ATTENTION_REASON_UNSPECIFIED},
		{"blocked max attempts", AttentionReasonBlockedMaxAttempts, pb.AttentionReason_ATTENTION_REASON_BLOCKED_MAX_ATTEMPTS},
		{"awaiting human input", AttentionReasonAwaitingHumanInput, pb.AttentionReason_ATTENTION_REASON_AWAITING_HUMAN_INPUT},
		{"review requested", AttentionReasonReviewRequested, pb.AttentionReason_ATTENTION_REASON_REVIEW_REQUESTED},
		{"merge conflict unresolvable", AttentionReasonMergeConflictUnresolvable, pb.AttentionReason_ATTENTION_REASON_MERGE_CONFLICT_UNRESOLVABLE},
		{"agent auth failed", AttentionReasonAgentAuthFailed, pb.AttentionReason_ATTENTION_REASON_AGENT_AUTH_FAILED},
		{"agent stalled", AttentionReasonAgentStalled, pb.AttentionReason_ATTENTION_REASON_AGENT_STALLED},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if int(tt.got) != int(tt.want) {
				t.Errorf("%s = %d, want proto value %d", tt.name, int(tt.got), int(tt.want))
			}
		})
	}
}

func TestComputeAttentionStatus(t *testing.T) {
	now := time.Now()
	stateEnteredAt := now.Add(-2 * time.Hour)
	blockedReason := "max attempts reached (5)"

	tests := []struct {
		name          string
		session       *models.Session
		repo          *models.Repo
		wantAttention bool
		wantReason    AttentionReason
		wantSummary   string
		wantSince     time.Time
	}{
		{
			name: "blocked session needs attention",
			session: &models.Session{
				State:          machine.Blocked,
				BlockedReason:  &blockedReason,
				StateEnteredAt: &stateEnteredAt,
				UpdatedAt:      now,
			},
			repo:          &models.Repo{},
			wantAttention: true,
			wantReason:    AttentionReasonBlockedMaxAttempts,
			wantSummary:   blockedReason,
			wantSince:     stateEnteredAt,
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
			wantSummary:   "blocked — needs human intervention",
		},
		{
			name: "orphaned session needs attention",
			session: &models.Session{
				State:     machine.Orphaned,
				UpdatedAt: now,
			},
			repo:          &models.Repo{},
			wantAttention: true,
			wantReason:    AttentionReasonAwaitingHumanInput,
			wantSummary:   "orphaned — headless run killed by daemon restart; needs human",
		},
		{
			name: "orphaned session with reason uses that summary",
			session: &models.Session{
				State:         machine.Orphaned,
				BlockedReason: &blockedReason,
				UpdatedAt:     now,
			},
			repo:          &models.Repo{},
			wantAttention: true,
			wantReason:    AttentionReasonAwaitingHumanInput,
			wantSummary:   blockedReason,
		},
		{
			name: "green draft with auto-merge off does not need attention",
			session: &models.Session{
				State:     machine.GreenDraft,
				UpdatedAt: now,
			},
			repo:          &models.Repo{CanAutoMerge: false},
			wantAttention: false,
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
			name: "ready for review with auto-merge off does not need attention",
			session: &models.Session{
				State:     machine.ReadyForReview,
				UpdatedAt: now,
			},
			repo:          &models.Repo{CanAutoMerge: false},
			wantAttention: false,
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
			name: "fixing checks with auto-repair off needs attention",
			session: &models.Session{
				State:     machine.FixingChecks,
				UpdatedAt: now,
			},
			repo:          &models.Repo{CanAutoRepair: false},
			wantAttention: true,
			wantReason:    AttentionReasonMergeConflictUnresolvable,
			wantSummary:   "automatic repair disabled, needs human",
		},
		{
			name: "fixing checks with auto-repair on does not need attention",
			session: &models.Session{
				State:     machine.FixingChecks,
				UpdatedAt: now,
			},
			repo:          &models.Repo{CanAutoRepair: true},
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
				if !tt.wantSince.IsZero() && !got.Since.Equal(tt.wantSince) {
					t.Errorf("Since = %v, want state_entered_at %v", got.Since, tt.wantSince)
				}
			}
		})
	}
}
