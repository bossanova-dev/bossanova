// Package models defines the core domain types for Bossanova.
// These are the Go-native types used throughout the application.
// Proto conversion functions bridge these with the generated protobuf types.
package models

import (
	"time"

	"github.com/recurser/bossalib/machine"
)

// MergeStrategy represents the merge method used when auto-merging PRs.
// Default is MergeStrategyMerge, matching GitHub's default.
type MergeStrategy string

const (
	MergeStrategyMerge  MergeStrategy = "merge"
	MergeStrategyRebase MergeStrategy = "rebase"
	MergeStrategySquash MergeStrategy = "squash"
)

// Valid reports whether m is one of the recognized merge strategies
// (merge/rebase/squash). The empty string and any unknown value are invalid.
func (m MergeStrategy) Valid() bool {
	switch m {
	case MergeStrategyMerge, MergeStrategyRebase, MergeStrategySquash:
		return true
	default:
		return false
	}
}

// MergeStrategies returns the recognized merge strategies in canonical cycle
// order (merge, rebase, squash). It is the single source of truth for the
// allowed values — the TUI cycler and any picker should source from here rather
// than duplicating bare string literals.
func MergeStrategies() []MergeStrategy {
	return []MergeStrategy{MergeStrategyMerge, MergeStrategyRebase, MergeStrategySquash}
}

// ParseMergeStrategy normalizes a stored/wire string into a validated
// MergeStrategy. A valid value maps to itself; the empty string and any unknown
// value map to the default MergeStrategyMerge (matching the DB column default
// 'merge'). This replaces unchecked MergeStrategy(s) casts at storage
// boundaries.
func ParseMergeStrategy(s string) MergeStrategy {
	m := MergeStrategy(s)
	if m.Valid() {
		return m
	}
	return MergeStrategyMerge
}

// Repo represents a registered Git repository.
type Repo struct {
	ID                     string
	DisplayName            string
	LocalPath              string
	OriginURL              string
	DefaultBaseBranch      string
	WorktreeBaseDir        string
	SetupScript            *string
	CanAutoMerge           bool
	CanAutoMergeDependabot bool
	CanAutoRepair          bool
	CanAutoRotate          bool
	// ShouldArchiveSessionsAfterMerge controls whether the daemon archives a session
	// once its PR is detected as merged. Defaults true.
	ShouldArchiveSessionsAfterMerge bool
	// CanAutoDeleteBranches controls whether the daemon deletes a session's git
	// branch once its session is archived. Defaults true.
	CanAutoDeleteBranches bool
	// ShouldKeepBranchesCurrent controls whether the daemon proactively rebases the
	// repo's other in-flight session branches onto the base branch whenever a
	// merge advances it, force-pushing with lease (BOS-521). Opt-in — every
	// rebase re-runs CI — so it defaults false.
	ShouldKeepBranchesCurrent bool
	MergeStrategy             MergeStrategy
	LinearAPIKey              string
	SentryAPIKey              string
	SentryOrg                 string
	CreatedAt                 time.Time
	UpdatedAt                 time.Time
}

// Session represents an agent coding session.
type Session struct {
	ID                      string
	RepoID                  string
	Title                   string
	Plan                    string
	WorktreePath            string
	BranchName              string
	BaseBranch              string
	State                   machine.State
	AgentSessionID          *string
	AgentName               string
	Model                   string // opaque agent model id; "" = plugin default.
	PRNumber                *int
	PRURL                   *string
	TrackerID               *string
	TrackerURL              *string
	TmuxSessionName         *string
	LastCheckState          machine.CheckState
	LastObservedReviewState int
	IsAutomationEnabled     bool
	AttemptCount            int
	BlockedReason           *string

	// LastAttemptHeadSHA is the PR head commit SHA at which the last fix-loop
	// attempt was counted. The dispatcher only consumes a new attempt when the
	// current head SHA differs from this value (a real fix pushed a new commit);
	// a settle lap at an unchanged SHA is free. Reset to nil when the PR goes
	// green (ChecksPassed) or the session is auto-unblocked, so a genuinely new
	// failure starts from a clean slate (BOS-235). Distinct from
	// LastRepairHeadSHA (the repair plugin's cooldown lane).
	LastAttemptHeadSHA *string

	// RotationAttemptCount tracks how many account rotations have been
	// consumed by this headless run (mirrors AttemptCount's per-run tally).
	RotationAttemptCount int
	// RotationResumeAt is the wall-clock time at which a parked (rotation
	// cooldown) session should be resumed. Nil means the session is not
	// currently parked awaiting rotation.
	RotationResumeAt *time.Time

	ArchivedAt *time.Time
	CronJobID  *string
	HookToken  *string
	// AccountID binds the session to a rotation account; nil/empty = the
	// system-default account 0 (no injected env, D9).
	AccountID        *string
	IsTmuxUnattended bool
	// Detach = this session's initial run is a durable, tmux-hosted --detach
	// autonomous pass; drives unattended-class recovery. False for a detach run
	// that fell back to the paneless headless path.
	Detach bool
	// IsQuickChat marks a session created via `create_session {quick_chat: true}`:
	// a visible, no-worktree/branch/PR chat (planning, recon, plan-review). Such
	// sessions have no implementation output by design, so finalize must not
	// surface their expected no-change result as a failed implementation run
	// (BOS-322). Real quick chats skip finalize entirely; this persisted flag is
	// the defensive backstop for any path that reaches FinalizeSession.
	IsQuickChat bool
	CreatedAt   time.Time
	UpdatedAt   time.Time

	// Composite display fields, persisted so every client renders the same
	// label/intent/spinner verbatim. Populated by the DisplayStatusComputer in
	// Step 2; for now they round-trip as empty/zero values.
	DisplayLabel   string
	DisplayIntent  int32
	DisplaySpinner bool

	// Last repair-attempt diagnostics, persisted by the repair plugin via
	// HostService.RecordRepairOutcome. Both error fields are empty for clean
	// runs; RunnerError means StartAgentRun never produced a process (eg.
	// claude not on PATH); ExitError means the agent ran but exited non-zero
	// or was signalled. LastRepairAttemptCount is a *consecutive-failure*
	// tally — clean runs reset it to zero, failed runs increment it — which
	// is what the TUI renders as "(3×)" so the suffix doesn't overstate
	// fail → succeed → fail sequences. LastRepairStartedAt is the
	// "an attempt has been recorded" sentinel; it stays nil until the
	// repair plugin runs for the first time on this session.
	LastRepairStartedAt         *time.Time
	LastRepairRunnerError       string
	LastRepairExitError         string
	LastRepairAttemptCount      int
	LastRepairHeadSHA           string
	LastRepairDisplayStatus     int32
	LastRepairReviewFingerprint string

	// Blocked-refusal lane: set when the daemon refused to START a repair
	// chat (a FailedPrecondition displace/reclaim refusal) rather than
	// running one. Recorded for operator visibility without bumping
	// LastRepairAttemptCount or the runner/exit error fields; cleared on
	// every real repair outcome, so a non-empty reason IS the latest
	// repair-related state.
	LastRepairBlockedReason string
	LastRepairBlockedAt     *time.Time

	// SetupError records why the repo's configured setup script failed during
	// worktree creation. A setup-script failure is non-fatal — the session
	// still starts — so this flags the degraded state for the TUI and
	// `boss show`. Empty for clean runs or repos with no setup script.
	SetupError string
}

// Attempt represents a fix attempt within a session.
type Attempt struct {
	ID        string
	SessionID string
	Trigger   AttemptTrigger
	Result    AttemptResult
	Error     *string
	CreatedAt time.Time
	UpdatedAt time.Time
}

// AgentChat represents an agent conversation associated with a session.
type AgentChat struct {
	ID                string
	SessionID         string
	AgentSessionID    string  // Agent session UUID
	ProviderSessionID *string // Provider-owned resume/session UUID, nil when same as AgentSessionID or not discovered yet
	AgentName         string  // Agent plugin name (e.g. "claude", "opencode")
	Title             string
	DaemonID          string  // Originating daemon (empty = local)
	TmuxSessionName   *string // tmux session name for this chat (nil = no tmux)
	// StartError is set when the agent failed to start for this chat
	// (e.g. SendPlan timed out, ConfigureFinalizeHook returned error).
	// nil on the happy path; non-nil rows are preserved as historical
	// "attempted but never came up" entries so the chat list can show a
	// (failed to start) badge instead of silently swallowing the
	// attempt.
	StartError *string
	// AccountID binds the chat to a rotation account; nil/empty = the
	// system-default account 0 (no injected env, D9).
	AccountID *string
	// Model is the per-chat agent model id. Empty string means "inherit the
	// agent CLI default" (BOS-381 moves model authority from the session to the
	// chat). bossd never enumerates valid models; the agent CLI validates it.
	Model     string
	CreatedAt time.Time
}

// TaskMappingStatus represents the state of a task mapping.
type TaskMappingStatus int

const (
	TaskMappingStatusPending    TaskMappingStatus = iota // Discovered, not yet acted on
	TaskMappingStatusInProgress                          // Session created or merge in progress
	TaskMappingStatusCompleted                           // Successfully completed
	TaskMappingStatusFailed                              // Failed (session failed, merge rejected, etc.)
	TaskMappingStatusSkipped                             // Skipped (e.g. previously-rejected)
	TaskMappingStatusOrphaned                            // Driving goroutine died (e.g. daemon restart); eligible for re-pickup
)

// TaskMapping tracks the relationship between an external task (e.g. a
// dependabot PR) and a bossanova session. Used for dedup and status tracking.
type TaskMapping struct {
	ID                   string
	ExternalID           string
	PluginName           string
	SessionID            *string
	RepoID               string
	Status               TaskMappingStatus
	LastError            *string
	PendingUpdateStatus  *TaskMappingStatus
	PendingUpdateDetails *string
	// RetryCount counts how many times a terminal mapping has been
	// re-processed into a fresh repair attempt. Bounds sequential re-repairs
	// of an unfixable PR (see Orchestrator.processTask).
	RetryCount int
	CreatedAt  time.Time
	UpdatedAt  time.Time
}

// AttemptTrigger represents what triggered a fix attempt.
type AttemptTrigger int

const (
	AttemptTriggerUnspecified AttemptTrigger = iota
	AttemptTriggerCheckFailure
	AttemptTriggerConflict
	AttemptTriggerReviewFeedback
)

// AttemptResult represents the outcome of a fix attempt.
type AttemptResult int

const (
	AttemptResultUnspecified AttemptResult = iota
	AttemptResultSuccess
	AttemptResultFailed
	AttemptResultIncomplete
)

func (t AttemptTrigger) String() string {
	switch t {
	case AttemptTriggerCheckFailure:
		return "check_failure"
	case AttemptTriggerConflict:
		return "conflict"
	case AttemptTriggerReviewFeedback:
		return "review_feedback"
	default:
		return "unspecified"
	}
}

func (r AttemptResult) String() string {
	switch r {
	case AttemptResultSuccess:
		return "success"
	case AttemptResultFailed:
		return "failed"
	case AttemptResultIncomplete:
		return "incomplete"
	default:
		return "unspecified"
	}
}
