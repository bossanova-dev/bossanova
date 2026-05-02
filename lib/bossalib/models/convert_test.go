package models

import (
	"fmt"
	"testing"
	"time"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/machine"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func TestRepoRoundTrip(t *testing.T) {
	script := "make setup"
	orig := &Repo{
		ID:                "repo-123",
		DisplayName:       "my-app",
		LocalPath:         "/home/user/my-app",
		OriginURL:         "https://github.com/user/my-app.git",
		DefaultBaseBranch: "main",
		WorktreeBaseDir:   "/home/user/.worktrees",
		SetupScript:       &script,
		CreatedAt:         time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC),
		UpdatedAt:         time.Date(2024, 6, 15, 12, 0, 0, 0, time.UTC),
	}

	proto := RepoToProto(orig)
	back := RepoFromProto(proto)

	if back.ID != orig.ID {
		t.Errorf("ID = %q, want %q", back.ID, orig.ID)
	}
	if back.DisplayName != orig.DisplayName {
		t.Errorf("DisplayName = %q, want %q", back.DisplayName, orig.DisplayName)
	}
	if back.LocalPath != orig.LocalPath {
		t.Errorf("LocalPath = %q, want %q", back.LocalPath, orig.LocalPath)
	}
	if back.OriginURL != orig.OriginURL {
		t.Errorf("OriginURL = %q, want %q", back.OriginURL, orig.OriginURL)
	}
	if back.DefaultBaseBranch != orig.DefaultBaseBranch {
		t.Errorf("DefaultBaseBranch = %q, want %q", back.DefaultBaseBranch, orig.DefaultBaseBranch)
	}
	if back.WorktreeBaseDir != orig.WorktreeBaseDir {
		t.Errorf("WorktreeBaseDir = %q, want %q", back.WorktreeBaseDir, orig.WorktreeBaseDir)
	}
	if !back.CreatedAt.Equal(orig.CreatedAt) {
		t.Errorf("CreatedAt = %v, want %v", back.CreatedAt, orig.CreatedAt)
	}
	if !back.UpdatedAt.Equal(orig.UpdatedAt) {
		t.Errorf("UpdatedAt = %v, want %v", back.UpdatedAt, orig.UpdatedAt)
	}
}

func TestSessionRoundTrip(t *testing.T) {
	claude := "claude-abc"
	prNum := 42
	prURL := "https://github.com/test/repo/pull/42"
	blocked := "max attempts reached"
	archived := time.Date(2024, 7, 1, 0, 0, 0, 0, time.UTC)

	orig := &Session{
		ID:                "sess-456",
		RepoID:            "repo-123",
		Title:             "Fix login bug",
		Plan:              "Fix the login form validation",
		WorktreePath:      "/tmp/wt/fix-login",
		BranchName:        "fix/login-bug",
		BaseBranch:        "main",
		State:             machine.Blocked,
		ClaudeSessionID:   &claude,
		PRNumber:          &prNum,
		PRURL:             &prURL,
		LastCheckState:    machine.CheckStateFailed,
		AutomationEnabled: true,
		AttemptCount:      3,
		BlockedReason:     &blocked,
		ArchivedAt:        &archived,
		CreatedAt:         time.Date(2024, 6, 1, 0, 0, 0, 0, time.UTC),
		UpdatedAt:         time.Date(2024, 6, 15, 0, 0, 0, 0, time.UTC),
	}

	proto := SessionToProto(orig)
	back := SessionFromProto(proto)

	if back.ID != orig.ID {
		t.Errorf("ID = %q, want %q", back.ID, orig.ID)
	}
	if back.State != orig.State {
		t.Errorf("State = %v, want %v", back.State, orig.State)
	}
	if back.PRNumber == nil || *back.PRNumber != *orig.PRNumber {
		t.Errorf("PRNumber = %v, want %v", back.PRNumber, orig.PRNumber)
	}
	if back.LastCheckState != orig.LastCheckState {
		t.Errorf("LastCheckState = %v, want %v", back.LastCheckState, orig.LastCheckState)
	}
	if back.AutomationEnabled != orig.AutomationEnabled {
		t.Errorf("AutomationEnabled = %v, want %v", back.AutomationEnabled, orig.AutomationEnabled)
	}
	if back.AttemptCount != orig.AttemptCount {
		t.Errorf("AttemptCount = %d, want %d", back.AttemptCount, orig.AttemptCount)
	}
	if back.BlockedReason == nil || *back.BlockedReason != *orig.BlockedReason {
		t.Errorf("BlockedReason = %v, want %v", back.BlockedReason, orig.BlockedReason)
	}
	if back.ArchivedAt == nil || !back.ArchivedAt.Equal(*orig.ArchivedAt) {
		t.Errorf("ArchivedAt = %v, want %v", back.ArchivedAt, orig.ArchivedAt)
	}
}

func TestSessionRoundTrip_NilOptionals(t *testing.T) {
	orig := &Session{
		ID:        "sess-nil",
		State:     machine.CreatingWorktree,
		CreatedAt: time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC),
		UpdatedAt: time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC),
	}

	proto := SessionToProto(orig)
	back := SessionFromProto(proto)

	if back.ClaudeSessionID != nil {
		t.Errorf("ClaudeSessionID = %v, want nil", back.ClaudeSessionID)
	}
	if back.PRNumber != nil {
		t.Errorf("PRNumber = %v, want nil", back.PRNumber)
	}
	if back.ArchivedAt != nil {
		t.Errorf("ArchivedAt = %v, want nil", back.ArchivedAt)
	}
}

func TestAttemptRoundTrip(t *testing.T) {
	errMsg := "push failed"
	orig := &Attempt{
		ID:        "att-789",
		SessionID: "sess-456",
		Trigger:   AttemptTriggerCheckFailure,
		Result:    AttemptResultFailed,
		Error:     &errMsg,
		CreatedAt: time.Date(2024, 6, 15, 10, 0, 0, 0, time.UTC),
		UpdatedAt: time.Date(2024, 6, 15, 10, 5, 0, 0, time.UTC),
	}

	proto := AttemptToProto(orig)
	back := AttemptFromProto(proto)

	if back.ID != orig.ID {
		t.Errorf("ID = %q, want %q", back.ID, orig.ID)
	}
	if back.Trigger != orig.Trigger {
		t.Errorf("Trigger = %v, want %v", back.Trigger, orig.Trigger)
	}
	if back.Result != orig.Result {
		t.Errorf("Result = %v, want %v", back.Result, orig.Result)
	}
	if back.Error == nil || *back.Error != *orig.Error {
		t.Errorf("Error = %v, want %v", back.Error, orig.Error)
	}
}

func TestStateRoundTrip(t *testing.T) {
	states := []machine.State{
		machine.CreatingWorktree,
		machine.StartingClaude,
		machine.PushingBranch,
		machine.OpeningDraftPR,
		machine.ImplementingPlan,
		machine.AwaitingChecks,
		machine.FixingChecks,
		machine.GreenDraft,
		machine.ReadyForReview,
		machine.Blocked,
		machine.Merged,
		machine.Closed,
		machine.Finalizing,
	}

	for _, s := range states {
		t.Run(s.String(), func(t *testing.T) {
			proto := stateToProto(s)
			if proto == pb.SessionState_SESSION_STATE_UNSPECIFIED {
				t.Errorf("stateToProto(%v) = UNSPECIFIED", s)
			}
			back := stateFromProto(proto)
			if back != s {
				t.Errorf("stateFromProto(stateToProto(%v)) = %v", s, back)
			}
		})
	}
}

func TestCheckStateRoundTrip(t *testing.T) {
	states := []machine.CheckState{
		machine.CheckStatePending,
		machine.CheckStatePassed,
		machine.CheckStateFailed,
	}

	for _, cs := range states {
		t.Run(fmt.Sprintf("%v", cs), func(t *testing.T) {
			proto := checkStateToProto(cs)
			back := checkStateFromProto(proto)
			if back != cs {
				t.Errorf("checkStateFromProto(checkStateToProto(%v)) = %v", cs, back)
			}
		})
	}
}

func TestAttemptTriggerRoundTrip(t *testing.T) {
	triggers := []AttemptTrigger{
		AttemptTriggerCheckFailure,
		AttemptTriggerConflict,
		AttemptTriggerReviewFeedback,
	}

	for _, tr := range triggers {
		t.Run(tr.String(), func(t *testing.T) {
			proto := attemptTriggerToProto(tr)
			back := attemptTriggerFromProto(proto)
			if back != tr {
				t.Errorf("round trip failed: %v -> %v", tr, back)
			}
		})
	}
}

func TestAttemptResultRoundTrip(t *testing.T) {
	results := []AttemptResult{
		AttemptResultSuccess,
		AttemptResultFailed,
		AttemptResultIncomplete,
	}

	for _, r := range results {
		t.Run(r.String(), func(t *testing.T) {
			proto := attemptResultToProto(r)
			back := attemptResultFromProto(proto)
			if back != r {
				t.Errorf("round trip failed: %v -> %v", r, back)
			}
		})
	}
}

func TestIntPtrConversions(t *testing.T) {
	t.Run("nil", func(t *testing.T) {
		if got := intPtrToInt32Ptr(nil); got != nil {
			t.Errorf("intPtrToInt32Ptr(nil) = %v, want nil", got)
		}
		if got := int32PtrToIntPtr(nil); got != nil {
			t.Errorf("int32PtrToIntPtr(nil) = %v, want nil", got)
		}
	})

	t.Run("value", func(t *testing.T) {
		v := 42
		got := intPtrToInt32Ptr(&v)
		if got == nil || *got != 42 {
			t.Errorf("intPtrToInt32Ptr(&42) = %v, want &42", got)
		}

		v32 := int32(99)
		gotInt := int32PtrToIntPtr(&v32)
		if gotInt == nil || *gotInt != 99 {
			t.Errorf("int32PtrToIntPtr(&99) = %v, want &99", gotInt)
		}
	})
}

func TestRepoFromProto_NilTimestamps(t *testing.T) {
	p := &pb.Repo{
		Id:          "r1",
		DisplayName: "test",
		CreatedAt:   nil,
		UpdatedAt:   nil,
	}
	r := RepoFromProto(p)
	if r.ID != "r1" {
		t.Errorf("ID = %q, want %q", r.ID, "r1")
	}
}

func TestSessionFromProto_WithArchivedAt(t *testing.T) {
	archived := timestamppb.New(time.Date(2024, 7, 1, 0, 0, 0, 0, time.UTC))
	p := &pb.Session{
		Id:         "s1",
		State:      pb.SessionState_SESSION_STATE_CLOSED,
		ArchivedAt: archived,
		CreatedAt:  timestamppb.Now(),
		UpdatedAt:  timestamppb.Now(),
	}
	s := SessionFromProto(p)
	if s.ArchivedAt == nil {
		t.Fatal("ArchivedAt should not be nil")
	}
	if !s.ArchivedAt.Equal(archived.AsTime()) {
		t.Errorf("ArchivedAt = %v, want %v", *s.ArchivedAt, archived.AsTime())
	}
}
