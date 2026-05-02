// Package machine provides the session state machine for the Bossanova
// session lifecycle. It defines 13 states, 16 event triggers, guards, and
// actions using qmuntal/stateless.
package machine

import (
	"context"
	"fmt"

	"github.com/qmuntal/stateless"
)

// State represents a session state.
type State int

const (
	CreatingWorktree State = iota + 1
	StartingClaude
	PushingBranch
	OpeningDraftPR
	ImplementingPlan
	AwaitingChecks
	FixingChecks
	GreenDraft
	ReadyForReview
	Blocked
	Merged
	Closed
	Finalizing
)

// Event represents a session event trigger.
type Event int

const (
	WorktreeCreated Event = iota + 1
	ClaudeStarted
	BranchPushed
	PROpened
	PlanComplete
	ChecksPassed
	ChecksFailed
	ConflictDetected
	ReviewSubmitted
	FixComplete
	FixFailed
	Block
	Unblock
	PRMerged
	PRClosed
	FinalizeRequested
)

// MaxAttempts is the default maximum number of fix attempts before blocking.
const MaxAttempts = 5

// SessionContext holds mutable session metadata that the state machine
// reads and writes via guards and actions.
type SessionContext struct {
	AttemptCount  int
	MaxAttempts   int
	CheckState    CheckState
	BlockedReason string
	HasPR         bool // PR already created (e.g. no-plan PR sessions)
}

// CheckState represents the aggregate check status.
type CheckState int

const (
	CheckStateUnspecified CheckState = iota
	CheckStatePending
	CheckStatePassed
	CheckStateFailed
)

// Machine wraps a stateless.StateMachine with session-specific context.
type Machine struct {
	sm  *stateless.StateMachine
	ctx *SessionContext
}

// New creates a new session state machine starting in the given state.
func New(initial State) *Machine {
	m := &Machine{
		ctx: &SessionContext{
			MaxAttempts: MaxAttempts,
		},
	}
	m.sm = m.configure(initial)
	return m
}

// NewWithContext creates a new session state machine with pre-existing context.
// Use this when restoring a session from the database.
func NewWithContext(initial State, sctx *SessionContext) *Machine {
	m := &Machine{
		ctx: sctx,
	}
	if m.ctx.MaxAttempts == 0 {
		m.ctx.MaxAttempts = MaxAttempts
	}
	m.sm = m.configure(initial)
	return m
}

// Fire triggers a state transition with the given event.
func (m *Machine) Fire(event Event) error {
	return m.sm.Fire(event)
}

// FireCtx triggers a state transition with context.
func (m *Machine) FireCtx(ctx context.Context, event Event) error {
	return m.sm.FireCtx(ctx, event)
}

// State returns the current state.
func (m *Machine) State() State {
	return m.sm.MustState().(State)
}

// Context returns the session context for reading.
func (m *Machine) Context() *SessionContext {
	return m.ctx
}

// CanFire returns true if the given event can be fired in the current state.
func (m *Machine) CanFire(event Event) bool {
	ok, _ := m.sm.CanFire(event)
	return ok
}

// PermittedTriggers returns the events that can be fired in the current state.
func (m *Machine) PermittedTriggers() []Event {
	triggers, _ := m.sm.PermittedTriggers()
	events := make([]Event, len(triggers))
	for i, t := range triggers {
		events[i] = t.(Event)
	}
	return events
}

// fixOrBlock returns FixingChecks if under max attempts, Blocked otherwise.
func (m *Machine) fixOrBlock(_ context.Context, _ ...any) (stateless.State, error) {
	if m.ctx.AttemptCount+1 >= m.ctx.MaxAttempts {
		return Blocked, nil
	}
	return FixingChecks, nil
}

// planCompleteDestination routes PlanComplete to AwaitingChecks if the PR
// already exists (no-plan PR sessions), or PushingBranch otherwise.
func (m *Machine) planCompleteDestination(_ context.Context, _ ...any) (stateless.State, error) {
	if m.ctx.HasPR {
		return AwaitingChecks, nil
	}
	return PushingBranch, nil
}

// fixOrBlockAfterFix is the same as fixOrBlock but used for FixFailed events
// where we go back to AwaitingChecks if under max, Blocked if at max.
func (m *Machine) retryOrBlock(_ context.Context, _ ...any) (stateless.State, error) {
	if m.ctx.AttemptCount+1 >= m.ctx.MaxAttempts {
		return Blocked, nil
	}
	return AwaitingChecks, nil
}

func (m *Machine) configure(initial State) *stateless.StateMachine {
	sm := stateless.NewStateMachineWithMode(initial, stateless.FiringImmediate)

	// --- Happy path: setup states ---

	sm.Configure(CreatingWorktree).
		Permit(WorktreeCreated, StartingClaude).
		Permit(PRClosed, Closed)

	sm.Configure(StartingClaude).
		Permit(ClaudeStarted, ImplementingPlan).
		Permit(PRClosed, Closed)

	sm.Configure(ImplementingPlan).
		PermitDynamic(PlanComplete, m.planCompleteDestination).
		Permit(FinalizeRequested, Finalizing).
		Permit(Block, Blocked).
		Permit(PRClosed, Closed)

	sm.Configure(PushingBranch).
		Permit(BranchPushed, OpeningDraftPR).
		Permit(PRClosed, Closed)

	sm.Configure(OpeningDraftPR).
		Permit(PROpened, AwaitingChecks).
		Permit(PRClosed, Closed)

	// --- CI check cycle ---

	sm.Configure(AwaitingChecks).
		OnEntry(m.actionSetChecksPending).
		Permit(ChecksPassed, GreenDraft).
		PermitDynamic(ChecksFailed, m.fixOrBlock).
		PermitDynamic(ConflictDetected, m.fixOrBlock).
		Permit(PRMerged, Merged).
		Permit(PRClosed, Closed).
		Permit(Block, Blocked)

	sm.Configure(FixingChecks).
		OnEntry(m.actionOnEnterFixing).
		Permit(FixComplete, AwaitingChecks).
		PermitDynamic(FixFailed, m.retryOrBlock).
		Permit(PRClosed, Closed).
		Permit(Block, Blocked)

	// --- Review cycle ---

	sm.Configure(GreenDraft).
		OnEntry(m.actionSetChecksPassed).
		Permit(PlanComplete, ReadyForReview).
		PermitDynamic(ReviewSubmitted, m.fixOrBlock).
		PermitDynamic(ChecksFailed, m.fixOrBlock).
		Permit(PRMerged, Merged).
		Permit(PRClosed, Closed).
		Permit(Block, Blocked)

	sm.Configure(ReadyForReview).
		PermitDynamic(ReviewSubmitted, m.fixOrBlock).
		PermitDynamic(ChecksFailed, m.fixOrBlock).
		Permit(PRMerged, Merged).
		Permit(PRClosed, Closed).
		Permit(Block, Blocked)

	// --- Terminal + blocked states ---

	sm.Configure(Blocked).
		OnEntry(m.actionSetBlocked).
		OnExit(m.actionClearBlocked).
		Permit(Unblock, ImplementingPlan).
		Permit(PRClosed, Closed)

	sm.Configure(Merged)

	sm.Configure(Closed)

	// Finalizing is entered from ImplementingPlan when the Stop hook fires.
	// The detailed outcome is tracked out-of-band via cron_job.last_run_outcome.
	// Outbound transitions mirror the other non-fix states so the session can
	// still be observed to a terminal disposition (merged, closed, or blocked).
	sm.Configure(Finalizing).
		Permit(PRMerged, Merged).
		Permit(PRClosed, Closed).
		Permit(Block, Blocked)

	return sm
}

// --- Actions ---

func (m *Machine) actionOnEnterFixing(_ context.Context, _ ...any) error {
	m.ctx.AttemptCount++
	m.ctx.CheckState = CheckStateFailed
	return nil
}

func (m *Machine) actionSetChecksPassed(_ context.Context, _ ...any) error {
	m.ctx.CheckState = CheckStatePassed
	return nil
}

func (m *Machine) actionSetChecksPending(_ context.Context, _ ...any) error {
	m.ctx.CheckState = CheckStatePending
	return nil
}

func (m *Machine) actionSetBlocked(_ context.Context, _ ...any) error {
	if m.ctx.BlockedReason == "" {
		m.ctx.BlockedReason = fmt.Sprintf("max attempts reached (%d)", m.ctx.MaxAttempts)
	}
	return nil
}

func (m *Machine) actionClearBlocked(_ context.Context, _ ...any) error {
	m.ctx.BlockedReason = ""
	m.ctx.AttemptCount = 0
	return nil
}

// --- String methods ---

func (s State) String() string {
	switch s {
	case CreatingWorktree:
		return "creating_worktree"
	case StartingClaude:
		return "starting_claude"
	case PushingBranch:
		return "pushing_branch"
	case OpeningDraftPR:
		return "opening_draft_pr"
	case ImplementingPlan:
		return "implementing_plan"
	case AwaitingChecks:
		return "awaiting_checks"
	case FixingChecks:
		return "fixing_checks"
	case GreenDraft:
		return "green_draft"
	case ReadyForReview:
		return "ready_for_review"
	case Blocked:
		return "blocked"
	case Merged:
		return "merged"
	case Closed:
		return "closed"
	case Finalizing:
		return "finalizing"
	default:
		return "unknown"
	}
}

func (e Event) String() string {
	switch e {
	case WorktreeCreated:
		return "worktree_created"
	case ClaudeStarted:
		return "claude_started"
	case BranchPushed:
		return "branch_pushed"
	case PROpened:
		return "pr_opened"
	case PlanComplete:
		return "plan_complete"
	case ChecksPassed:
		return "checks_passed"
	case ChecksFailed:
		return "checks_failed"
	case ConflictDetected:
		return "conflict_detected"
	case ReviewSubmitted:
		return "review_submitted"
	case FixComplete:
		return "fix_complete"
	case FixFailed:
		return "fix_failed"
	case Block:
		return "block"
	case Unblock:
		return "unblock"
	case PRMerged:
		return "pr_merged"
	case PRClosed:
		return "pr_closed"
	case FinalizeRequested:
		return "finalize_requested"
	default:
		return "unknown"
	}
}
