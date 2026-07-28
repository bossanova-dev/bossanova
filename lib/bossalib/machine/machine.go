// Package machine provides the session state machine for the Bossanova
// session lifecycle. It defines 14 states, 17 event triggers, guards, and
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
	StartingAgent
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
	// Orphaned is a distinct terminal state for a headless (boss new --detach)
	// run that was killed mid-flight by a daemon restart: the run never
	// recorded an exit and no live agent process survives, so its bootstrap-only
	// PR must never masquerade as green. Entered only from ImplementingPlan by
	// the Bootstrap orphan sweep; deliberately no auto-re-dispatch (a one-shot's
	// prompt may have side effects — the human decides). Appended last to keep
	// the persisted integer values of the existing states stable.
	Orphaned
)

// AllStates returns every session state, in declaration order.
//
// Go cannot reflect over an iota const block, so every exhaustiveness check in
// the tree — the broadcast-subscription trigger classification, the display
// mappings — needs an enumeration to iterate, and this is the single one. Do not
// re-declare a local list: TestAllStatesMatchesTheConstBlock parses this file's
// const block and fails when the two disagree, which is what makes "a state
// added without being classified" a build failure somewhere rather than a
// silent no-op everywhere.
//
// Appending a state means adding it here in the same edit. The guard above is
// what enforces that; the previous String()-based tripwire could not, because a
// state added without a String() case simply rendered as "unknown".
func AllStates() []State {
	return []State{
		CreatingWorktree, StartingAgent, PushingBranch, OpeningDraftPR,
		ImplementingPlan, AwaitingChecks, FixingChecks, GreenDraft,
		ReadyForReview, Blocked, Merged, Closed, Finalizing, Orphaned,
	}
}

// Event represents a session event trigger.
type Event int

const (
	WorktreeCreated Event = iota + 1
	AgentStarted
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
	// Orphan transitions a stranded headless ImplementingPlan run to Orphaned
	// (see the Orphaned state). Appended last to keep existing event values stable.
	Orphan
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

	// SkipAttemptCount, when true, makes the next fix-loop lap free: the
	// FixingChecks OnEntry action does not bump AttemptCount and the
	// fixOrBlock/retryOrBlock guards never tip into Blocked. The dispatcher
	// sets it when a ChecksFailed/ConflictDetected lap is driven by an
	// unchanged PR head SHA — CI merely re-settling on the same commit, no
	// fix pushed — so no-op settle laps don't burn the fix-loop budget and
	// falsely Block a clean PR (BOS-235). The machine stays pure: it owns the
	// gate, the dispatcher owns the head-SHA comparison. Zero value (false)
	// preserves the historical always-count behaviour.
	SkipAttemptCount bool
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
	// The state machine only ever holds State values, so the assertion cannot
	// fail; use the comma-ok form to satisfy forcetypeassert without panicking.
	st, _ := m.sm.MustState().(State)
	return st
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
		// Triggers are always Event values; comma-ok satisfies forcetypeassert.
		ev, _ := t.(Event)
		events[i] = ev
	}
	return events
}

// fixOrBlock returns FixingChecks if under max attempts, Blocked otherwise.
func (m *Machine) fixOrBlock(_ context.Context, _ ...any) (stateless.State, error) {
	if !m.ctx.SkipAttemptCount && m.ctx.AttemptCount+1 >= m.ctx.MaxAttempts {
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
	if !m.ctx.SkipAttemptCount && m.ctx.AttemptCount+1 >= m.ctx.MaxAttempts {
		return Blocked, nil
	}
	return AwaitingChecks, nil
}

func (m *Machine) configure(initial State) *stateless.StateMachine {
	sm := stateless.NewStateMachineWithMode(initial, stateless.FiringImmediate)

	// --- Happy path: setup states ---

	sm.Configure(CreatingWorktree).
		Permit(WorktreeCreated, StartingAgent).
		Permit(PRClosed, Closed)

	sm.Configure(StartingAgent).
		Permit(AgentStarted, ImplementingPlan).
		Permit(PRClosed, Closed)

	sm.Configure(ImplementingPlan).
		PermitDynamic(PlanComplete, m.planCompleteDestination).
		Permit(FinalizeRequested, Finalizing).
		Permit(Block, Blocked).
		Permit(Orphan, Orphaned).
		Permit(PRMerged, Merged).
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

	// FixingChecks deliberately does NOT permit ConflictDetected. The
	// session is already being repaired in-place by the repair plugin
	// (lookupSession treats FixingChecks as repairable for any displayStatus).
	// A self-transition would re-fire actionOnEnterFixing → AttemptCount++,
	// so an unresolved conflict that the poller keeps re-detecting would
	// hit MaxAttempts in ~MaxAttempts × pollInterval and Block the session
	// even while the repair agent is still making progress.
	sm.Configure(FixingChecks).
		OnEntry(m.actionOnEnterFixing).
		Permit(FixComplete, AwaitingChecks).
		PermitDynamic(FixFailed, m.retryOrBlock).
		Permit(PRMerged, Merged).
		Permit(PRClosed, Closed).
		Permit(Block, Blocked)

	// --- Review cycle ---

	sm.Configure(GreenDraft).
		OnEntry(m.actionSetChecksPassed).
		Permit(PlanComplete, ReadyForReview).
		PermitDynamic(ReviewSubmitted, m.fixOrBlock).
		PermitDynamic(ChecksFailed, m.fixOrBlock).
		PermitDynamic(ConflictDetected, m.fixOrBlock).
		Permit(PRMerged, Merged).
		Permit(PRClosed, Closed).
		Permit(Block, Blocked)

	sm.Configure(ReadyForReview).
		PermitDynamic(ReviewSubmitted, m.fixOrBlock).
		PermitDynamic(ChecksFailed, m.fixOrBlock).
		PermitDynamic(ConflictDetected, m.fixOrBlock).
		Permit(PRMerged, Merged).
		Permit(PRClosed, Closed).
		Permit(Block, Blocked)

	// --- Terminal + blocked states ---

	sm.Configure(Blocked).
		OnEntry(m.actionSetBlocked).
		OnExit(m.actionClearBlocked).
		Permit(Unblock, ImplementingPlan).
		// A Blocked session whose PR later resolves must be able to reach its
		// terminal state so OnExit(actionClearBlocked) clears the stale block
		// reason (e.g. a non-gating "finalize failed" hint). PRClosed already
		// existed; PRMerged was missing, which wedged merged sessions in
		// Blocked forever (BOS-246).
		Permit(PRMerged, Merged).
		Permit(PRClosed, Closed)

	sm.Configure(Merged)

	sm.Configure(Closed)

	// Orphaned is terminal: a daemon restart killed the headless run, so there is
	// nothing to advance. The only outbound transition mirrors the other terminal
	// states — a closed PR moves it to Closed. Recovery is a deliberate human
	// action (a fresh run), never an automatic Unblock/re-dispatch.
	sm.Configure(Orphaned).
		Permit(PRClosed, Closed)

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
	if !m.ctx.SkipAttemptCount {
		m.ctx.AttemptCount++
	}
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
	case StartingAgent:
		return "starting_agent"
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
	case Orphaned:
		return "orphaned"
	default:
		return "unknown"
	}
}

func (e Event) String() string {
	switch e {
	case WorktreeCreated:
		return "worktree_created"
	case AgentStarted:
		return "agent_started"
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
	case Orphan:
		return "orphan"
	default:
		return "unknown"
	}
}
