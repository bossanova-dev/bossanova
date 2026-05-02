package machine

import (
	"testing"
)

func TestHappyPath(t *testing.T) {
	m := New(CreatingWorktree)

	steps := []struct {
		event Event
		want  State
	}{
		{WorktreeCreated, StartingClaude},
		{ClaudeStarted, ImplementingPlan},
		{PlanComplete, PushingBranch},
		{BranchPushed, OpeningDraftPR},
		{PROpened, AwaitingChecks},
		{ChecksPassed, GreenDraft},
		{PlanComplete, ReadyForReview},
		{PRMerged, Merged},
	}

	for _, s := range steps {
		if err := m.Fire(s.event); err != nil {
			t.Fatalf("Fire(%s): %v", s.event, err)
		}
		if got := m.State(); got != s.want {
			t.Fatalf("after %s: got %s, want %s", s.event, got, s.want)
		}
	}
}

func TestHappyPathNoPlan(t *testing.T) {
	// No-plan PR sessions: PlanComplete skips PushingBranch/OpeningDraftPR
	// and goes straight to AwaitingChecks (PR was created during setup).
	m := New(CreatingWorktree)
	m.Context().HasPR = true

	steps := []struct {
		event Event
		want  State
	}{
		{WorktreeCreated, StartingClaude},
		{ClaudeStarted, ImplementingPlan},
		{PlanComplete, AwaitingChecks}, // skips PushingBranch → OpeningDraftPR
		{ChecksPassed, GreenDraft},
		{PlanComplete, ReadyForReview},
		{PRMerged, Merged},
	}

	for _, s := range steps {
		if err := m.Fire(s.event); err != nil {
			t.Fatalf("Fire(%s): %v", s.event, err)
		}
		if got := m.State(); got != s.want {
			t.Fatalf("after %s: got %s, want %s", s.event, got, s.want)
		}
	}
}

func TestPlanCompleteWithoutHasPR(t *testing.T) {
	// Default (HasPR=false): PlanComplete goes to PushingBranch.
	m := New(CreatingWorktree)
	for _, e := range []Event{WorktreeCreated, ClaudeStarted} {
		if err := m.Fire(e); err != nil {
			t.Fatalf("Fire(%s): %v", e, err)
		}
	}
	assertState(t, m, ImplementingPlan)

	if err := m.Fire(PlanComplete); err != nil {
		t.Fatalf("Fire(PlanComplete): %v", err)
	}
	assertState(t, m, PushingBranch)
}

func TestFixLoopChecksFailed(t *testing.T) {
	m := New(CreatingWorktree)

	// Get to awaiting_checks
	for _, e := range []Event{WorktreeCreated, ClaudeStarted, PlanComplete, BranchPushed, PROpened} {
		if err := m.Fire(e); err != nil {
			t.Fatalf("Fire(%s): %v", e, err)
		}
	}
	assertState(t, m, AwaitingChecks)

	// First failure → fixing_checks (attempt 1)
	if err := m.Fire(ChecksFailed); err != nil {
		t.Fatalf("Fire(ChecksFailed): %v", err)
	}
	assertState(t, m, FixingChecks)
	if m.Context().AttemptCount != 1 {
		t.Fatalf("attempt count: got %d, want 1", m.Context().AttemptCount)
	}

	// Fix complete → back to awaiting_checks
	if err := m.Fire(FixComplete); err != nil {
		t.Fatalf("Fire(FixComplete): %v", err)
	}
	assertState(t, m, AwaitingChecks)

	// Second failure → fixing_checks (attempt 2)
	if err := m.Fire(ChecksFailed); err != nil {
		t.Fatalf("Fire(ChecksFailed): %v", err)
	}
	assertState(t, m, FixingChecks)
	if m.Context().AttemptCount != 2 {
		t.Fatalf("attempt count: got %d, want 2", m.Context().AttemptCount)
	}
}

func TestMaxAttemptsBlocks(t *testing.T) {
	m := New(CreatingWorktree)
	m.ctx.MaxAttempts = 2

	// Get to awaiting_checks
	for _, e := range []Event{WorktreeCreated, ClaudeStarted, PlanComplete, BranchPushed, PROpened} {
		if err := m.Fire(e); err != nil {
			t.Fatalf("Fire(%s): %v", e, err)
		}
	}

	// First failure → fixing (attempt 1, under max)
	if err := m.Fire(ChecksFailed); err != nil {
		t.Fatalf("Fire(ChecksFailed): %v", err)
	}
	assertState(t, m, FixingChecks)

	// Fix complete → awaiting
	if err := m.Fire(FixComplete); err != nil {
		t.Fatalf("Fire(FixComplete): %v", err)
	}

	// Second failure → blocked (attempt count 1, +1 = 2 >= maxAttempts 2)
	if err := m.Fire(ChecksFailed); err != nil {
		t.Fatalf("Fire(ChecksFailed): %v", err)
	}
	assertState(t, m, Blocked)
	if m.Context().BlockedReason == "" {
		t.Fatal("expected blocked reason to be set")
	}
}

func TestUnblockResetsAttempts(t *testing.T) {
	m := New(CreatingWorktree)
	m.ctx.MaxAttempts = 1

	// Get to awaiting_checks
	for _, e := range []Event{WorktreeCreated, ClaudeStarted, PlanComplete, BranchPushed, PROpened} {
		if err := m.Fire(e); err != nil {
			t.Fatalf("Fire(%s): %v", e, err)
		}
	}

	// First failure → blocked immediately (maxAttempts=1, 0+1 >= 1)
	if err := m.Fire(ChecksFailed); err != nil {
		t.Fatalf("Fire(ChecksFailed): %v", err)
	}
	assertState(t, m, Blocked)

	// Unblock → implementing_plan, attempts reset
	if err := m.Fire(Unblock); err != nil {
		t.Fatalf("Fire(Unblock): %v", err)
	}
	assertState(t, m, ImplementingPlan)
	if m.Context().AttemptCount != 0 {
		t.Fatalf("attempt count after unblock: got %d, want 0", m.Context().AttemptCount)
	}
	if m.Context().BlockedReason != "" {
		t.Fatalf("blocked reason after unblock: got %q, want empty", m.Context().BlockedReason)
	}
}

func TestConflictDetected(t *testing.T) {
	m := New(CreatingWorktree)

	// Get to awaiting_checks
	for _, e := range []Event{WorktreeCreated, ClaudeStarted, PlanComplete, BranchPushed, PROpened} {
		if err := m.Fire(e); err != nil {
			t.Fatalf("Fire(%s): %v", e, err)
		}
	}

	if err := m.Fire(ConflictDetected); err != nil {
		t.Fatalf("Fire(ConflictDetected): %v", err)
	}
	assertState(t, m, FixingChecks)
}

func TestReviewSubmittedFromGreenDraft(t *testing.T) {
	m := New(CreatingWorktree)

	// Get to green_draft
	for _, e := range []Event{WorktreeCreated, ClaudeStarted, PlanComplete, BranchPushed, PROpened, ChecksPassed} {
		if err := m.Fire(e); err != nil {
			t.Fatalf("Fire(%s): %v", e, err)
		}
	}
	assertState(t, m, GreenDraft)

	if err := m.Fire(ReviewSubmitted); err != nil {
		t.Fatalf("Fire(ReviewSubmitted): %v", err)
	}
	assertState(t, m, FixingChecks)
}

func TestReviewSubmittedFromReadyForReview(t *testing.T) {
	m := New(CreatingWorktree)

	// Get to ready_for_review
	for _, e := range []Event{WorktreeCreated, ClaudeStarted, PlanComplete, BranchPushed, PROpened, ChecksPassed, PlanComplete} {
		if err := m.Fire(e); err != nil {
			t.Fatalf("Fire(%s): %v", e, err)
		}
	}
	assertState(t, m, ReadyForReview)

	if err := m.Fire(ReviewSubmitted); err != nil {
		t.Fatalf("Fire(ReviewSubmitted): %v", err)
	}
	assertState(t, m, FixingChecks)
}

func TestPRClosedFromAnyState(t *testing.T) {
	closableStates := []struct {
		initial State
		setup   []Event
	}{
		{CreatingWorktree, nil},
		{StartingClaude, []Event{WorktreeCreated}},
		{ImplementingPlan, []Event{WorktreeCreated, ClaudeStarted}},
		{PushingBranch, []Event{WorktreeCreated, ClaudeStarted, PlanComplete}},
		{OpeningDraftPR, []Event{WorktreeCreated, ClaudeStarted, PlanComplete, BranchPushed}},
		{AwaitingChecks, []Event{WorktreeCreated, ClaudeStarted, PlanComplete, BranchPushed, PROpened}},
		{GreenDraft, []Event{WorktreeCreated, ClaudeStarted, PlanComplete, BranchPushed, PROpened, ChecksPassed}},
		{ReadyForReview, []Event{WorktreeCreated, ClaudeStarted, PlanComplete, BranchPushed, PROpened, ChecksPassed, PlanComplete}},
	}

	for _, tc := range closableStates {
		t.Run(tc.initial.String(), func(t *testing.T) {
			m := New(CreatingWorktree)
			for _, e := range tc.setup {
				if err := m.Fire(e); err != nil {
					t.Fatalf("setup Fire(%s): %v", e, err)
				}
			}
			assertState(t, m, tc.initial)

			if err := m.Fire(PRClosed); err != nil {
				t.Fatalf("Fire(PRClosed): %v", err)
			}
			assertState(t, m, Closed)
		})
	}
}

func TestBlockFromImplementingPlan(t *testing.T) {
	m := New(CreatingWorktree)
	for _, e := range []Event{WorktreeCreated, ClaudeStarted} {
		if err := m.Fire(e); err != nil {
			t.Fatalf("Fire(%s): %v", e, err)
		}
	}
	assertState(t, m, ImplementingPlan)

	if err := m.Fire(Block); err != nil {
		t.Fatalf("Fire(Block): %v", err)
	}
	assertState(t, m, Blocked)
}

func TestFixFailedUnderMaxReturnsToAwaiting(t *testing.T) {
	m := New(CreatingWorktree)

	// Get to fixing_checks via conflict
	for _, e := range []Event{WorktreeCreated, ClaudeStarted, PlanComplete, BranchPushed, PROpened, ConflictDetected} {
		if err := m.Fire(e); err != nil {
			t.Fatalf("Fire(%s): %v", e, err)
		}
	}
	assertState(t, m, FixingChecks)

	// FixFailed with attempts still under max → awaiting_checks
	if err := m.Fire(FixFailed); err != nil {
		t.Fatalf("Fire(FixFailed): %v", err)
	}
	assertState(t, m, AwaitingChecks)
}

func TestFixFailedAtMaxBlocks(t *testing.T) {
	m := New(CreatingWorktree)
	m.ctx.MaxAttempts = 1

	// Get to awaiting_checks
	for _, e := range []Event{WorktreeCreated, ClaudeStarted, PlanComplete, BranchPushed, PROpened} {
		if err := m.Fire(e); err != nil {
			t.Fatalf("Fire(%s): %v", e, err)
		}
	}

	// ConflictDetected → blocked (maxAttempts=1, 0+1 >= 1)
	if err := m.Fire(ConflictDetected); err != nil {
		t.Fatalf("Fire(ConflictDetected): %v", err)
	}
	assertState(t, m, Blocked)
}

func TestCheckStateTracking(t *testing.T) {
	m := New(CreatingWorktree)

	// Get to awaiting_checks
	for _, e := range []Event{WorktreeCreated, ClaudeStarted, PlanComplete, BranchPushed, PROpened} {
		if err := m.Fire(e); err != nil {
			t.Fatalf("Fire(%s): %v", e, err)
		}
	}
	if m.Context().CheckState != CheckStatePending {
		t.Fatalf("check state in awaiting: got %d, want pending", m.Context().CheckState)
	}

	// Checks pass → green_draft
	if err := m.Fire(ChecksPassed); err != nil {
		t.Fatalf("Fire(ChecksPassed): %v", err)
	}
	if m.Context().CheckState != CheckStatePassed {
		t.Fatalf("check state after passed: got %d, want passed", m.Context().CheckState)
	}
}

func TestNewWithContext(t *testing.T) {
	sctx := &SessionContext{
		AttemptCount:  3,
		MaxAttempts:   5,
		CheckState:    CheckStateFailed,
		BlockedReason: "",
	}
	m := NewWithContext(FixingChecks, sctx)

	assertState(t, m, FixingChecks)
	if m.Context().AttemptCount != 3 {
		t.Fatalf("attempt count: got %d, want 3", m.Context().AttemptCount)
	}

	// Can fire fix complete
	if err := m.Fire(FixComplete); err != nil {
		t.Fatalf("Fire(FixComplete): %v", err)
	}
	assertState(t, m, AwaitingChecks)
}

func TestCanFireAndPermittedTriggers(t *testing.T) {
	m := New(CreatingWorktree)

	if !m.CanFire(WorktreeCreated) {
		t.Fatal("should be able to fire WorktreeCreated from CreatingWorktree")
	}
	if m.CanFire(ChecksPassed) {
		t.Fatal("should not be able to fire ChecksPassed from CreatingWorktree")
	}

	triggers := m.PermittedTriggers()
	if len(triggers) == 0 {
		t.Fatal("expected at least one permitted trigger")
	}
	found := false
	for _, tr := range triggers {
		if tr == WorktreeCreated {
			found = true
		}
	}
	if !found {
		t.Fatal("WorktreeCreated should be in permitted triggers")
	}
}

func TestAllStatesReachable(t *testing.T) {
	// Verify all 12 states can be reached via valid transitions
	reached := make(map[State]bool)

	// Happy path reaches: CreatingWorktree, StartingClaude, ImplementingPlan,
	// PushingBranch, OpeningDraftPR, AwaitingChecks, GreenDraft, ReadyForReview, Merged
	m := New(CreatingWorktree)
	reached[m.State()] = true
	for _, e := range []Event{WorktreeCreated, ClaudeStarted, PlanComplete, BranchPushed, PROpened, ChecksPassed, PlanComplete, PRMerged} {
		if err := m.Fire(e); err != nil {
			t.Fatalf("Fire(%s): %v", e, err)
		}
		reached[m.State()] = true
	}

	// FixingChecks via check failure
	m2 := New(CreatingWorktree)
	for _, e := range []Event{WorktreeCreated, ClaudeStarted, PlanComplete, BranchPushed, PROpened, ChecksFailed} {
		if err := m2.Fire(e); err != nil {
			t.Fatalf("Fire(%s): %v", e, err)
		}
	}
	reached[m2.State()] = true

	// Blocked via max attempts
	m3 := New(CreatingWorktree)
	m3.ctx.MaxAttempts = 1
	for _, e := range []Event{WorktreeCreated, ClaudeStarted, PlanComplete, BranchPushed, PROpened, ChecksFailed} {
		if err := m3.Fire(e); err != nil {
			t.Fatalf("Fire(%s): %v", e, err)
		}
	}
	reached[m3.State()] = true

	// Closed via PRClosed
	m4 := New(CreatingWorktree)
	if err := m4.Fire(PRClosed); err != nil {
		t.Fatalf("Fire(PRClosed): %v", err)
	}
	reached[m4.State()] = true

	// Finalizing via FinalizeRequested from ImplementingPlan
	m5 := New(CreatingWorktree)
	for _, e := range []Event{WorktreeCreated, ClaudeStarted, FinalizeRequested} {
		if err := m5.Fire(e); err != nil {
			t.Fatalf("Fire(%s): %v", e, err)
		}
	}
	reached[m5.State()] = true

	allStates := []State{
		CreatingWorktree, StartingClaude, PushingBranch, OpeningDraftPR,
		ImplementingPlan, AwaitingChecks, FixingChecks, GreenDraft,
		ReadyForReview, Blocked, Merged, Closed, Finalizing,
	}

	for _, s := range allStates {
		if !reached[s] {
			t.Errorf("state %s was not reached", s)
		}
	}
}

func TestPRMergedFromAwaitingChecks(t *testing.T) {
	m := New(CreatingWorktree)
	for _, e := range []Event{WorktreeCreated, ClaudeStarted, PlanComplete, BranchPushed, PROpened} {
		if err := m.Fire(e); err != nil {
			t.Fatalf("Fire(%s): %v", e, err)
		}
	}
	assertState(t, m, AwaitingChecks)

	if err := m.Fire(PRMerged); err != nil {
		t.Fatalf("Fire(PRMerged): %v", err)
	}
	assertState(t, m, Merged)
}

func TestInvalidTransitionReturnsError(t *testing.T) {
	m := New(CreatingWorktree)
	if err := m.Fire(ChecksPassed); err == nil {
		t.Fatal("expected error firing ChecksPassed from CreatingWorktree")
	}
}

func TestFinalizeRequestedFromImplementingPlan(t *testing.T) {
	m := New(CreatingWorktree)
	for _, e := range []Event{WorktreeCreated, ClaudeStarted} {
		if err := m.Fire(e); err != nil {
			t.Fatalf("Fire(%s): %v", e, err)
		}
	}
	assertState(t, m, ImplementingPlan)

	if err := m.Fire(FinalizeRequested); err != nil {
		t.Fatalf("Fire(FinalizeRequested): %v", err)
	}
	assertState(t, m, Finalizing)
}

func TestFinalizeRequestedFromNonImplementingPlanRejected(t *testing.T) {
	// FinalizeRequested is only valid from ImplementingPlan. The DB-level
	// conditional UPDATE (state=Finalizing WHERE state=ImplementingPlan) is
	// the authoritative idempotency gate, but the state machine must also
	// reject stray FinalizeRequested events from every other state.
	rejectFrom := []struct {
		initial State
		setup   []Event
	}{
		{CreatingWorktree, nil},
		{StartingClaude, []Event{WorktreeCreated}},
		{PushingBranch, []Event{WorktreeCreated, ClaudeStarted, PlanComplete}},
		{AwaitingChecks, []Event{WorktreeCreated, ClaudeStarted, PlanComplete, BranchPushed, PROpened}},
	}
	for _, tc := range rejectFrom {
		t.Run(tc.initial.String(), func(t *testing.T) {
			m := New(CreatingWorktree)
			for _, e := range tc.setup {
				if err := m.Fire(e); err != nil {
					t.Fatalf("setup Fire(%s): %v", e, err)
				}
			}
			assertState(t, m, tc.initial)
			if err := m.Fire(FinalizeRequested); err == nil {
				t.Fatalf("expected error firing FinalizeRequested from %s", tc.initial)
			}
		})
	}
}

func TestFinalizingExits(t *testing.T) {
	// Finalizing is a waiting disposition. External events (PR merged/closed by
	// the user, or an explicit Block from FinalizeSession on a fatal outcome)
	// still need to drive it to a terminal state.
	cases := []struct {
		name  string
		event Event
		want  State
	}{
		{"PRMerged", PRMerged, Merged},
		{"PRClosed", PRClosed, Closed},
		{"Block", Block, Blocked},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := New(CreatingWorktree)
			for _, e := range []Event{WorktreeCreated, ClaudeStarted, FinalizeRequested} {
				if err := m.Fire(e); err != nil {
					t.Fatalf("setup Fire(%s): %v", e, err)
				}
			}
			assertState(t, m, Finalizing)

			if err := m.Fire(tc.event); err != nil {
				t.Fatalf("Fire(%s): %v", tc.event, err)
			}
			assertState(t, m, tc.want)
		})
	}
}

func TestStateAndEventStrings(t *testing.T) {
	if CreatingWorktree.String() != "creating_worktree" {
		t.Fatalf("got %q", CreatingWorktree.String())
	}
	if WorktreeCreated.String() != "worktree_created" {
		t.Fatalf("got %q", WorktreeCreated.String())
	}
	if State(99).String() != "unknown" {
		t.Fatal("unknown state should return 'unknown'")
	}
	if Event(99).String() != "unknown" {
		t.Fatal("unknown event should return 'unknown'")
	}
}

func TestRetryOrBlock_ExactlyAtMaxAttempts(t *testing.T) {
	// Tests boundary: AttemptCount+1 >= MaxAttempts when exactly equal.
	// Catches mutation: AttemptCount+1 >= MaxAttempts changed to AttemptCount+1 > MaxAttempts.
	m := New(CreatingWorktree)
	m.ctx.MaxAttempts = 3

	// Get to FixingChecks with AttemptCount = 2
	for _, e := range []Event{WorktreeCreated, ClaudeStarted, PlanComplete, BranchPushed, PROpened} {
		if err := m.Fire(e); err != nil {
			t.Fatalf("Fire(%s): %v", e, err)
		}
	}
	// First failure: AttemptCount = 1
	if err := m.Fire(ChecksFailed); err != nil {
		t.Fatalf("Fire(ChecksFailed): %v", err)
	}
	assertState(t, m, FixingChecks)
	if err := m.Fire(FixComplete); err != nil {
		t.Fatalf("Fire(FixComplete): %v", err)
	}

	// Second failure: AttemptCount = 2
	if err := m.Fire(ChecksFailed); err != nil {
		t.Fatalf("Fire(ChecksFailed): %v", err)
	}
	assertState(t, m, FixingChecks)
	if m.Context().AttemptCount != 2 {
		t.Fatalf("AttemptCount = %d, want 2", m.Context().AttemptCount)
	}

	// FixFailed when AttemptCount = 2, MaxAttempts = 3
	// AttemptCount+1 = 3 >= 3 → should go to Blocked
	if err := m.Fire(FixFailed); err != nil {
		t.Fatalf("Fire(FixFailed): %v", err)
	}
	assertState(t, m, Blocked)
}

func TestRetryOrBlock_JustUnderMaxAttempts(t *testing.T) {
	// Tests boundary: AttemptCount+1 < MaxAttempts → should retry (AwaitingChecks).
	// Catches mutation: AttemptCount+1 >= MaxAttempts changed to AttemptCount+1 > MaxAttempts.
	m := New(CreatingWorktree)
	m.ctx.MaxAttempts = 5

	// Get to FixingChecks
	for _, e := range []Event{WorktreeCreated, ClaudeStarted, PlanComplete, BranchPushed, PROpened} {
		if err := m.Fire(e); err != nil {
			t.Fatalf("Fire(%s): %v", e, err)
		}
	}

	// Trigger 2 failures to get AttemptCount = 2
	for i := 0; i < 2; i++ {
		if err := m.Fire(ChecksFailed); err != nil {
			t.Fatalf("Fire(ChecksFailed) #%d: %v", i+1, err)
		}
		assertState(t, m, FixingChecks)
		if err := m.Fire(FixComplete); err != nil {
			t.Fatalf("Fire(FixComplete) #%d: %v", i+1, err)
		}
	}

	// One more failure: AttemptCount = 3
	if err := m.Fire(ChecksFailed); err != nil {
		t.Fatalf("Fire(ChecksFailed): %v", err)
	}
	if m.Context().AttemptCount != 3 {
		t.Fatalf("AttemptCount = %d, want 3", m.Context().AttemptCount)
	}

	// FixFailed when AttemptCount = 3, MaxAttempts = 5
	// AttemptCount+1 = 4 < 5 → should retry (AwaitingChecks), NOT block
	if err := m.Fire(FixFailed); err != nil {
		t.Fatalf("Fire(FixFailed): %v", err)
	}
	assertState(t, m, AwaitingChecks)
}

func TestFixOrBlock_ExactlyAtMaxAttempts(t *testing.T) {
	// Tests boundary: AttemptCount+1 >= MaxAttempts in fixOrBlock (ChecksFailed path).
	m := New(CreatingWorktree)
	m.ctx.MaxAttempts = 2

	// Get to AwaitingChecks
	for _, e := range []Event{WorktreeCreated, ClaudeStarted, PlanComplete, BranchPushed, PROpened} {
		if err := m.Fire(e); err != nil {
			t.Fatalf("Fire(%s): %v", e, err)
		}
	}

	// First failure: AttemptCount goes from 0 to 1
	// 1 >= 2 is false, so should go to FixingChecks
	if err := m.Fire(ChecksFailed); err != nil {
		t.Fatalf("Fire(ChecksFailed): %v", err)
	}
	assertState(t, m, FixingChecks)
	if m.Context().AttemptCount != 1 {
		t.Fatalf("AttemptCount = %d, want 1", m.Context().AttemptCount)
	}

	// Return to awaiting
	if err := m.Fire(FixComplete); err != nil {
		t.Fatalf("Fire(FixComplete): %v", err)
	}
	assertState(t, m, AwaitingChecks)

	// Second failure: AttemptCount = 1, AttemptCount+1 = 2 >= 2 → Blocked
	if err := m.Fire(ChecksFailed); err != nil {
		t.Fatalf("Fire(ChecksFailed): %v", err)
	}
	assertState(t, m, Blocked)
}

func TestNewWithContext_ZeroMaxAttempts(t *testing.T) {
	// Tests boundary: MaxAttempts == 0 changed to MaxAttempts != 0.
	// When MaxAttempts is 0, NewWithContext should set it to the default.
	sctx := &SessionContext{
		AttemptCount: 0,
		MaxAttempts:  0, // explicitly zero
		CheckState:   CheckStateUnspecified,
	}
	m := NewWithContext(ImplementingPlan, sctx)

	if m.Context().MaxAttempts != MaxAttempts {
		t.Fatalf("MaxAttempts = %d, want %d (default)", m.Context().MaxAttempts, MaxAttempts)
	}
}

func TestNewWithContext_NonZeroMaxAttempts(t *testing.T) {
	// Tests boundary: MaxAttempts == 0 should NOT override non-zero values.
	sctx := &SessionContext{
		AttemptCount: 0,
		MaxAttempts:  10, // explicitly non-zero
		CheckState:   CheckStateUnspecified,
	}
	m := NewWithContext(ImplementingPlan, sctx)

	if m.Context().MaxAttempts != 10 {
		t.Fatalf("MaxAttempts = %d, want 10 (preserved)", m.Context().MaxAttempts)
	}
}

func TestFinalizingNoProgressEvents(t *testing.T) {
	// Finalizing tracks its detailed outcome via cron_job.last_run_outcome,
	// not via the state machine. Mid-flow events (plan/check/fix, Unblock,
	// or a repeat FinalizeRequested) must not transition out of Finalizing.
	m := NewWithContext(Finalizing, &SessionContext{})
	invalidEvents := []Event{
		PlanComplete, ChecksPassed, ChecksFailed, ConflictDetected,
		ReviewSubmitted, FixComplete, FixFailed, Unblock,
		WorktreeCreated, ClaudeStarted, BranchPushed, PROpened, FinalizeRequested,
	}
	for _, e := range invalidEvents {
		if m.CanFire(e) {
			t.Errorf("Finalizing should not permit %s", e)
		}
	}
}

func TestFinalizingStateString(t *testing.T) {
	if Finalizing.String() != "finalizing" {
		t.Fatalf("got %q, want %q", Finalizing.String(), "finalizing")
	}
	if FinalizeRequested.String() != "finalize_requested" {
		t.Fatalf("got %q, want %q", FinalizeRequested.String(), "finalize_requested")
	}
}

func assertState(t *testing.T, m *Machine, want State) {
	t.Helper()
	if got := m.State(); got != want {
		t.Fatalf("state: got %s, want %s", got, want)
	}
}
