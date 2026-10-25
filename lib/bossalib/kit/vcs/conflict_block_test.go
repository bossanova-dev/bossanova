package vcs

import "testing"

func conflictBoolPtr(v bool) *bool { return &v }

func TestPRStatusConflictBlockKind(t *testing.T) {
	tests := []struct {
		name           string
		status         *PRStatus
		rebaseStrategy bool
		want           ConflictBlockKind
		wantRepairable bool
	}{
		{
			name: "mergeable false is repairable merge conflict",
			status: &PRStatus{
				State:            PRStateOpen,
				Mergeable:        conflictBoolPtr(false),
				MergeStateStatus: MergeStateStatusDirty,
			},
			want:           ConflictBlockMerge,
			wantRepairable: true,
		},
		{
			name: "rebaseable false is repairable only for rebase strategy",
			status: &PRStatus{
				State:      PRStateOpen,
				Mergeable:  conflictBoolPtr(true),
				Rebaseable: conflictBoolPtr(false),
			},
			rebaseStrategy: true,
			want:           ConflictBlockRebase,
			wantRepairable: true,
		},
		{
			name: "rebaseable false ignored for merge strategy",
			status: &PRStatus{
				State:      PRStateOpen,
				Mergeable:  conflictBoolPtr(true),
				Rebaseable: conflictBoolPtr(false),
			},
			want: ConflictBlockNone,
		},
		{
			name: "rebaseable false does not mask unknown mergeability for merge strategy",
			status: &PRStatus{
				State:            PRStateOpen,
				Rebaseable:       conflictBoolPtr(false),
				MergeStateStatus: MergeStateStatusUnknown,
			},
			want:           ConflictBlockUnknown,
			wantRepairable: false,
		},
		{
			name: "rebaseable false does not mask unstable merge state for merge strategy",
			status: &PRStatus{
				State:            PRStateOpen,
				Mergeable:        conflictBoolPtr(true),
				Rebaseable:       conflictBoolPtr(false),
				MergeStateStatus: MergeStateStatusUnstable,
			},
			want:           ConflictBlockUnstable,
			wantRepairable: false,
		},
		{
			name: "rebaseable false does not mask unstable merge state for rebase strategy",
			status: &PRStatus{
				State:            PRStateOpen,
				Mergeable:        conflictBoolPtr(true),
				Rebaseable:       conflictBoolPtr(false),
				MergeStateStatus: MergeStateStatusUnstable,
			},
			rebaseStrategy: true,
			want:           ConflictBlockUnstable,
			wantRepairable: false,
		},
		{
			name: "review required block is not a conflict",
			status: &PRStatus{
				State:               PRStateOpen,
				Mergeable:           conflictBoolPtr(true),
				Rebaseable:          conflictBoolPtr(false),
				MergeStateStatus:    MergeStateStatusBlocked,
				ReviewDecisionState: ReviewStateRequired,
			},
			rebaseStrategy: true,
			want:           ConflictBlockReviewRequired,
			wantRepairable: false,
		},
		{
			name: "unstable is checks or policy not conflict",
			status: &PRStatus{
				State:            PRStateOpen,
				Mergeable:        conflictBoolPtr(true),
				MergeStateStatus: MergeStateStatusUnstable,
			},
			want:           ConflictBlockUnstable,
			wantRepairable: false,
		},
		{
			name: "unknown mergeability remains unknown",
			status: &PRStatus{
				State:            PRStateOpen,
				MergeStateStatus: MergeStateStatusUnknown,
			},
			want:           ConflictBlockUnknown,
			wantRepairable: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tt.status.ConflictBlockKind(tt.rebaseStrategy)
			if got != tt.want {
				t.Fatalf("ConflictBlockKind() = %v, want %v", got, tt.want)
			}
			if got.Repairable() != tt.wantRepairable {
				t.Fatalf("Repairable() = %v, want %v", got.Repairable(), tt.wantRepairable)
			}
		})
	}
}

func TestConflictBlockKindString(t *testing.T) {
	tests := []struct {
		kind ConflictBlockKind
		want string
	}{
		{ConflictBlockNone, "none"},
		{ConflictBlockMerge, "merge"},
		{ConflictBlockRebase, "rebase"},
		{ConflictBlockReviewRequired, "review-required"},
		{ConflictBlockUnstable, "unstable"},
		{ConflictBlockUnknown, "unknown"},
		{ConflictBlockKind(99), "unrecognized(99)"},
	}
	for _, tt := range tests {
		if got := tt.kind.String(); got != tt.want {
			t.Errorf("ConflictBlockKind(%d).String() = %q, want %q", int(tt.kind), got, tt.want)
		}
	}
}
