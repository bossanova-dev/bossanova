package domain

import (
	"time"

	"github.com/recurser/bossalib/models"
)

type UpdateSessionParams struct {
	Title                   *string
	State                   *int
	WorktreePath            *string
	WorktreeCreatedAt       **string
	IsBranchCreated         *bool
	BranchName              *string
	AgentSessionID          **string
	PRNumber                **int
	PRURL                   **string
	TrackerID               **string
	TrackerURL              **string
	TmuxSessionName         **string
	LastCheckState          *int
	LastCheckStateHeadSHA   **string
	LastCheckStateAt        **string
	LastObservedReviewState *int
	IsAutomationEnabled     *bool
	AttemptCount            *int
	BlockedReason           **string
	// RunBlockedReason follows the nullable-string double-pointer convention:
	// nil = don't touch, *nil = clear (finalize consumed the report), *val =
	// the run reported itself BLOCKED with that reason.
	RunBlockedReason **string
	// LastAttemptHeadSHA follows the nullable-string double-pointer convention:
	// nil = don't touch, *nil = clear to NULL (reset on green / auto-unblock),
	// *val = set the SHA at which an attempt was just counted (BOS-235).
	LastAttemptHeadSHA **string
	// LastRepairAttemptCount overwrites the repair plugin's consecutive-failure
	// counter. An operator clearing a block sets it to 0, so the next sweep on
	// the same head starts a fresh repair budget instead of re-blocking.
	LastRepairAttemptCount *int

	// RotationAttemptCount and RotationResumeAt track account-rotation state
	// for the headless auto-rotation feature (BOS-174).
	RotationAttemptCount *int
	// RotationResumeAt follows the nullable-string double-pointer convention:
	// nil = don't touch, *nil = clear to NULL (resume due / rotation
	// complete), *val = set the ISO 8601 resume time (mirrors
	// LastAttemptHeadSHA).
	RotationResumeAt **string

	ArchivedAt **string // ISO 8601 string or nil
	CronJobID  **string
	HookToken  **string // double pointer: nil = don't update, *nil = clear (cleared on finalize success)
	// AccountID rebinds the session to a rotation account (BOS-171 manual
	// switch). Follows the nullable-string double-pointer convention: nil =
	// don't touch, *nil = clear to NULL (system-default account 0), *val =
	// bind to that account id.
	AccountID        **string
	IsTmuxUnattended *bool
	// Detach marks a durable, tmux-hosted --detach autonomous run (BOS-428).
	// nil = don't touch; mirrors IsTmuxUnattended (set via Update on the
	// tmux-hosted branch after create). Left false for a detach run that fell
	// back to the paneless headless path.
	Detach *bool
	// IsQuickChat marks a visible no-worktree/branch/PR planning chat (BOS-322).
	// nil = don't touch; mirrors IsTmuxUnattended (set via Update after create).
	IsQuickChat *bool

	// Composite display fields, updated by the DisplayStatusComputer (Step 2).
	// Pointer-typed so a nil value means "don't touch" and a zero value means
	// "set to empty/zero" — matching the rest of UpdateSessionParams.
	DisplayLabel   *string
	DisplayIntent  *int32
	DisplaySpinner *bool

	// SetupError flags a non-fatal setup-script failure on the session. nil
	// means "don't touch"; a value (including "") sets/clears the column.
	SetupError *string
}

type UpdateRepairDiagnosticsParams struct {
	SessionID         string
	StartedAt         time.Time
	RunnerError       string
	ExitError         string
	HeadSHA           string
	DisplayStatus     int32
	ReviewFingerprint *string
	BaseSHA           string
}

type SessionWithRepo struct {
	*models.Session
	RepoDisplayName string
	RepoOriginURL   string
}
