package db

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/recurser/bossalib/contract/cloud/sqlutil"
	"github.com/recurser/bossalib/machine"
	"github.com/recurser/bossalib/models"
	"github.com/recurser/bossd/internal/db/sqlcgen"
	"github.com/recurser/bossd/internal/domain"
)

var _ SessionStore = (*SQLiteSessionStore)(nil)

// SQLiteSessionStore implements SessionStore using SQLite.
type SQLiteSessionStore struct {
	db *sql.DB
}

// NewSessionStore creates a new SQLite-backed SessionStore.
func NewSessionStore(db *sql.DB) *SQLiteSessionStore {
	return &SQLiteSessionStore{db: db}
}

// UpdateStateConditional performs the idempotency-gate conditional UPDATE
// used by FinalizeSession: exactly one row is affected only if the session
// was still in expectedState when we arrived.
func (s *SQLiteSessionStore) UpdateStateConditional(ctx context.Context, id string, newState, expectedState int) (bool, error) {
	now := sqlutil.TimeNow()
	res, err := sqlcgen.New(s.db).UpdateSessionStateConditional(ctx, sqlcgen.UpdateSessionStateConditionalParams{
		NewState:      int64(newState),
		Now:           now,
		ID:            id,
		ExpectedState: int64(expectedState),
	})
	if err != nil {
		return false, fmt.Errorf("update state conditional: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("rows affected: %w", err)
	}
	return n == 1, nil
}

// SetTrackerIfUnlinked is the tracker linker's compare-and-set write: it
// links the session only while it is still unarchived, is not a quick chat,
// has no tracker_url, and its tracker_id still equals expectedTrackerID (NULL
// reads as ""). is_quick_chat is re-checked because CreateSession sets it in
// an Update after the insert, so a pass can read the row before it is marked.
func (s *SQLiteSessionStore) SetTrackerIfUnlinked(ctx context.Context, id, expectedTrackerID, trackerID, trackerURL string) (bool, error) {
	res, err := sqlcgen.New(s.db).UpdateSessionLinkTrackerIfUnlinked(ctx, sqlcgen.UpdateSessionLinkTrackerIfUnlinkedParams{
		TrackerID:         &trackerID,
		TrackerUrl:        &trackerURL,
		Now:               sqlutil.TimeNow(),
		ID:                id,
		ExpectedTrackerID: &expectedTrackerID,
	})
	if err != nil {
		return false, fmt.Errorf("link tracker if unlinked: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("rows affected: %w", err)
	}
	return n == 1, nil
}

// MarkHeadlessRunOrphaned atomically records the daemon-restart marker with the
// ImplementingPlan→Orphaned transition. Keeping both writes in one UPDATE
// avoids leaving a markerless Orphaned row that an auto-resume sweep cannot
// identify safely.
func (s *SQLiteSessionStore) MarkHeadlessRunOrphaned(ctx context.Context, id, reason string) (bool, error) {
	now := sqlutil.TimeNow()
	res, err := sqlcgen.New(s.db).UpdateSessionOrphanHeadlessRun(ctx, sqlcgen.UpdateSessionOrphanHeadlessRunParams{
		OrphanedState:         int64(machine.Orphaned),
		Now:                   now,
		Reason:                &reason,
		ID:                    id,
		ImplementingPlanState: int64(machine.ImplementingPlan),
	})
	if err != nil {
		return false, fmt.Errorf("orphan headless run: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("rows affected: %w", err)
	}
	return n == 1, nil
}

// MarkUnarchivedOrphanClaimed atomically advances an orphan only while it remains
// unarchived with the expected daemon-restart marker, so a user archive or
// marker change between the sweep list and this claim prevents a new headless
// run from starting.
func (s *SQLiteSessionStore) MarkUnarchivedOrphanClaimed(ctx context.Context, id, reason string) (bool, error) {
	now := sqlutil.TimeNow()
	res, err := sqlcgen.New(s.db).UpdateSessionClaimUnarchivedOrphan(ctx, sqlcgen.UpdateSessionClaimUnarchivedOrphanParams{
		ImplementingPlanState: int64(machine.ImplementingPlan),
		Now:                   now,
		ID:                    id,
		OrphanedState:         int64(machine.Orphaned),
		Reason:                &reason,
	})
	if err != nil {
		return false, fmt.Errorf("claim unarchived orphan: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("rows affected: %w", err)
	}
	return n == 1, nil
}

// SetResurrectedState clears archived_at and installs the live state in ONE
// conditional UPDATE: `SET archived_at = NULL, state = ? WHERE id = ? AND
// archived_at IS NOT NULL`. ResurrectSession (session/lifecycle.go) is the only
// caller.
//
// It also clears run_blocked_reason: the resurrected session starts a new agent
// run, which must not inherit a BLOCKED report an earlier run left unconsumed.
//
// The single statement is the whole point. Clearing archived_at and writing the
// state as two writes leaves the row wearing {archived_at NULL, state Merged}
// in between — byte-for-byte the predicate archiveMergedButUnarchived
// (session/reconcile.go) selects on — so a reconcile tick landing in that window
// dispatches a detached archive that can delete the worktree the resurrect just
// recreated. SQLite applies this UPDATE atomically, so that intermediate shape
// is never observable.
//
// The `archived_at IS NOT NULL` guard is inherited from the Resurrect method
// this replaces: without it the write would silently succeed against a live
// session and overwrite its state. Returns true when exactly one row moved;
// false means the row was no longer archived when this statement ran (a lost
// race), which the caller must treat as a failed resurrect rather than
// proceeding.
func (s *SQLiteSessionStore) SetResurrectedState(ctx context.Context, id string, newState int) (bool, error) {
	now := sqlutil.TimeNow()
	res, err := sqlcgen.New(s.db).UpdateSessionResurrectToState(ctx, sqlcgen.UpdateSessionResurrectToStateParams{
		NewState:     int64(newState),
		Now:          now,
		BlockedState: int64(machine.Blocked),
		ID:           id,
	})
	if err != nil {
		return false, fmt.Errorf("resurrect session to state: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("rows affected: %w", err)
	}
	return n == 1, nil
}

// SetFailedResurrectRolledBack undoes a resurrect whose agent start failed, also in
// one statement: `SET archived_at = ?, state = ? WHERE id = ? AND archived_at
// IS NULL AND state = ?`. ResurrectSession is the only caller.
//
// archivedAt is the timestamp the row wore before the resurrect, so the row
// returns exactly where the failed attempt found it instead of having its trash
// age silently reset. restoreState is that same pre-resurrect state;
// expectState is the state SetResurrectedState wrote (ImplementingPlan) — the two
// have opposite meanings and swapping them would silently disable the rollback.
// Re-archiving in two writes would transiently recreate the {archived_at NULL,
// state Merged} shape SetResurrectedState exists to avoid, which is why this is one
// statement too.
//
// Returns true when exactly one row was restored. A false return is NOT a
// harmless no-op: it means a concurrent writer moved the row off the shape this
// call wrote, leaving the session live, agent-less and un-retryable, so the
// caller logs the state it actually observed rather than swallowing it.
func (s *SQLiteSessionStore) SetFailedResurrectRolledBack(ctx context.Context, id string, archivedAt time.Time, restoreState, expectState int) (bool, error) {
	now := sqlutil.TimeNow()
	res, err := sqlcgen.New(s.db).UpdateSessionRollbackFailedResurrect(ctx, sqlcgen.UpdateSessionRollbackFailedResurrectParams{
		ArchivedAt:   timeText(&archivedAt),
		RestoreState: int64(restoreState),
		Now:          now,
		ID:           id,
		ExpectState:  int64(expectState),
	})
	if err != nil {
		return false, fmt.Errorf("rollback failed resurrect: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("rows affected: %w", err)
	}
	return n == 1, nil
}

// MarkOrphanResumed records a spawned replacement agent only while the
// original orphan claim remains exclusively owned by this handoff. It does not
// call Get after the UPDATE: an affected-row result is unambiguous even when a
// later read would fail, and the predicates prevent an old completion or user
// archive from being overwritten.
func (s *SQLiteSessionStore) MarkOrphanResumed(ctx context.Context, id, reason string, priorAgentSession *string, newAgentSessionID string) (bool, error) {
	now := sqlutil.TimeNow()
	res, err := sqlcgen.New(s.db).UpdateSessionCommitOrphanResume(ctx, sqlcgen.UpdateSessionCommitOrphanResumeParams{
		NewAgentSessionID:     &newAgentSessionID,
		Now:                   now,
		ID:                    id,
		ImplementingPlanState: int64(machine.ImplementingPlan),
		Reason:                &reason,
		PriorAgentSession:     priorAgentSession,
	})
	if err != nil {
		return false, fmt.Errorf("commit orphan resume: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("rows affected: %w", err)
	}
	return n == 1, nil
}

// MarkOrphanResumeClaimReleased returns a claimed orphan to its original candidate
// state when spawning failed before a replacement agent was persisted. The
// expected marker and prior agent ID prevent this cleanup from overwriting a
// winner that changed the row after the claim. RetrySession intentionally
// clears blocked_reason without starting a replacement; accepting a NULL marker
// in that one claimed shape releases the dead run while preserving Retry's
// markerless state.
func (s *SQLiteSessionStore) MarkOrphanResumeClaimReleased(ctx context.Context, id, reason string, priorAgentSession *string) (bool, error) {
	now := sqlutil.TimeNow()
	res, err := sqlcgen.New(s.db).UpdateSessionReleaseOrphanResumeClaim(ctx, sqlcgen.UpdateSessionReleaseOrphanResumeClaimParams{
		OrphanedState:         int64(machine.Orphaned),
		Now:                   now,
		ID:                    id,
		ImplementingPlanState: int64(machine.ImplementingPlan),
		Reason:                &reason,
		PriorAgentSession:     priorAgentSession,
	})
	if err != nil {
		return false, fmt.Errorf("release orphan resume claim: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("rows affected: %w", err)
	}
	return n == 1, nil
}

// MarkOrphanResumeReparked restores the pre-resume orphan shape only while this
// handoff still owns the newly persisted agent ID. A completion, archive, or
// manual intervention that won after the commit makes this a harmless no-op.
func (s *SQLiteSessionStore) MarkOrphanResumeReparked(ctx context.Context, id, reason string, priorAgentSession *string, newAgentSessionID string) (bool, error) {
	now := sqlutil.TimeNow()
	res, err := sqlcgen.New(s.db).UpdateSessionReparkOrphanResume(ctx, sqlcgen.UpdateSessionReparkOrphanResumeParams{
		OrphanedState:         int64(machine.Orphaned),
		Now:                   now,
		PriorAgentSession:     priorAgentSession,
		Reason:                &reason,
		ID:                    id,
		ImplementingPlanState: int64(machine.ImplementingPlan),
		NewAgentSessionID:     &newAgentSessionID,
	})
	if err != nil {
		return false, fmt.Errorf("repark orphan resume: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("rows affected: %w", err)
	}
	return n == 1, nil
}

// UpdateStateConditionalFrom is the from-set variant of UpdateStateConditional:
// it transitions the session to newState only when its current state is one of
// expectedStates. It backs the broadened stranded-cron reap (BOS-333), whose
// finalize entry must accept several interrupted states, while the Stop-hook
// path keeps the single-state UpdateStateConditional. An empty expectedStates
// slice is a no-op (returns false without querying) so the idempotency guard
// degenerates safely.
func (s *SQLiteSessionStore) UpdateStateConditionalFrom(ctx context.Context, id string, newState int, expectedStates []int) (bool, error) {
	if len(expectedStates) == 0 {
		return false, nil
	}
	states := make([]int64, len(expectedStates))
	for i, state := range expectedStates {
		states[i] = int64(state)
	}
	now := sqlutil.TimeNow()
	res, err := sqlcgen.New(s.db).UpdateSessionStateConditionalFrom(ctx, sqlcgen.UpdateSessionStateConditionalFromParams{
		ID:             id,
		NewState:       int64(newState),
		Now:            now,
		BlockedState:   int64(machine.Blocked),
		ExpectedStates: states,
	})
	if err != nil {
		return false, fmt.Errorf("update state conditional from: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("rows affected: %w", err)
	}
	return n > 0, nil
}

func (s *SQLiteSessionStore) Create(ctx context.Context, params CreateSessionParams) (*models.Session, error) {
	id, err := sqlutil.NewID()
	if err != nil {
		return nil, fmt.Errorf("new session id: %w", err)
	}
	now := sqlutil.TimeNow()
	agentName := params.AgentName
	if agentName == "" {
		agentName = "claude"
	}
	row, err := sqlcgen.New(s.db).CreateSession(ctx, sqlcgen.CreateSessionParams{
		ID: id, RepoID: params.RepoID, Title: params.Title, Plan: params.Plan,
		WorktreePath: params.WorktreePath, BranchName: params.BranchName, BaseBranch: params.BaseBranch,
		State: int64(machine.CreatingWorktree), StateEnteredAt: &now, AgentName: agentName,
		Model: params.Model, EffectiveModel: params.EffectiveModel, EffectiveEffort: params.EffectiveEffort,
		AccountID: params.AccountID, PrNumber: sessionInt64(params.PRNumber), PrUrl: params.PRURL,
		TrackerID: params.TrackerID, TrackerUrl: params.TrackerURL, CreatedAt: now, UpdatedAt: now,
	})
	if err != nil {
		return nil, fmt.Errorf("insert session: %w", err)
	}
	return sessionFromRow(row), nil
}

func (s *SQLiteSessionStore) Get(ctx context.Context, id string) (*models.Session, error) {
	row, err := sqlcgen.New(s.db).GetSession(ctx, id)
	if err != nil {
		return nil, notFound(err)
	}
	return sessionFromRow(row), nil
}

func (s *SQLiteSessionStore) List(ctx context.Context, repoID string) ([]*models.Session, error) {
	rows, err := sqlcgen.New(s.db).ListSessions(ctx, repoID)
	if err != nil {
		return nil, fmt.Errorf("list sessions: %w", err)
	}
	return mapRows(rows, sessionFromRow), nil
}

// ListByState returns every session currently in the given state, regardless
// of repo or archived status. Used by the daemon-startup recovery path to
// find sessions left in transient states (e.g. Finalizing) when the daemon
// crashed.
func (s *SQLiteSessionStore) ListByState(ctx context.Context, state int) ([]*models.Session, error) {
	rows, err := sqlcgen.New(s.db).ListSessionsByState(ctx, int64(state))
	if err != nil {
		return nil, fmt.Errorf("list sessions: %w", err)
	}
	return mapRows(rows, sessionFromRow), nil
}

// ListByStates returns every session whose state is one of the given states,
// regardless of repo or archived status. It is the multi-state variant of
// ListByState used by the broadened stranded-cron reap (BOS-333) to find
// unattended sessions interrupted mid-transition. An empty states slice
// returns nil without querying.
func (s *SQLiteSessionStore) ListByStates(ctx context.Context, states []int) ([]*models.Session, error) {
	if len(states) == 0 {
		return nil, nil
	}
	values := make([]int64, len(states))
	for i, state := range states {
		values[i] = int64(state)
	}
	rows, err := sqlcgen.New(s.db).ListSessionsByStates(ctx, values)
	if err != nil {
		return nil, fmt.Errorf("list sessions: %w", err)
	}
	return mapRows(rows, sessionFromRow), nil
}

// ListTmuxNames returns every non-empty sessions.tmux_session_name in
// the database, across all repos and including archived rows.
//
// It exists for the orphaned-tmux reaper (BOS-846), which needs a whitelist of
// every pane name any row can account for: every other SessionStore list is
// repo-scoped, so there was no global read. Archived rows are deliberately
// included — BOS-428 routes `--detach` runs through durable tmux so they
// outlive the daemon, and an archived-but-still-running unattended run is
// exactly the pane a repo-scoped or active-only read would leave reapable.
//
// This leg is near-dead by design: tmux.SessionName has no production callers
// and the only non-test writer clears the column, so in practice the result is
// usually empty. It is kept so a legacy row's pane is not reaped; an empty leg
// costs nothing and a missing one costs a live pane.
func (s *SQLiteSessionStore) ListTmuxNames(ctx context.Context) ([]string, error) {
	rows, err := sqlcgen.New(s.db).ListSessionTmuxNames(ctx)
	if err != nil {
		return nil, fmt.Errorf("list session tmux names: %w", err)
	}
	return mapRows(rows, textValue), nil
}

func (s *SQLiteSessionStore) ListActive(ctx context.Context, repoID string) ([]*models.Session, error) {
	rows, err := sqlcgen.New(s.db).ListSessionsByActive(ctx, repoID)
	if err != nil {
		return nil, fmt.Errorf("list sessions: %w", err)
	}
	return mapRows(rows, sessionFromRow), nil
}

// ListActiveWithRepo returns active (non-archived) sessions in one query
// that joins repos to populate RepoDisplayName. Eliminates the per-session
// Repo.Get round trip that the previous list-then-loop approach required.
func (s *SQLiteSessionStore) ListActiveWithRepo(ctx context.Context, repoID string) ([]*SessionWithRepo, error) {
	rows, err := sqlcgen.New(s.db).ListSessionsByActiveWithRepo(ctx, repoID)
	if err != nil {
		return nil, fmt.Errorf("list sessions with repo: %w", err)
	}
	return mapRows(rows, func(row sqlcgen.ListSessionsByActiveWithRepoRow) *SessionWithRepo {
		return &SessionWithRepo{Session: sessionFromRow(row.Session), RepoDisplayName: row.RepoDisplayName, RepoOriginURL: row.RepoOriginUrl}
	}), nil
}

// ListWithRepo returns both active and archived sessions with their repo
// display name populated. Used by the upstream sync adapter so archive
// transitions reach bosso (instead of an archived session just dropping out
// of the snapshot, which looks identical to deletion to the receiver).
func (s *SQLiteSessionStore) ListWithRepo(ctx context.Context, repoID string) ([]*SessionWithRepo, error) {
	rows, err := sqlcgen.New(s.db).ListSessionsWithRepo(ctx, repoID)
	if err != nil {
		return nil, fmt.Errorf("list sessions with repo: %w", err)
	}
	return mapRows(rows, func(row sqlcgen.ListSessionsWithRepoRow) *SessionWithRepo {
		return &SessionWithRepo{Session: sessionFromRow(row.Session), RepoDisplayName: row.RepoDisplayName, RepoOriginURL: row.RepoOriginUrl}
	}), nil
}

func (s *SQLiteSessionStore) ListByRepoAndPR(ctx context.Context, repoID string, prNumber int) ([]*SessionWithRepo, error) {
	number := int64(prNumber)
	rows, err := sqlcgen.New(s.db).ListSessionsByRepoAndPR(ctx, sqlcgen.ListSessionsByRepoAndPRParams{RepoID: repoID, PrNumber: &number})
	if err != nil {
		return nil, fmt.Errorf("list sessions with repo: %w", err)
	}
	return mapRows(rows, func(row sqlcgen.ListSessionsByRepoAndPRRow) *SessionWithRepo {
		return &SessionWithRepo{Session: sessionFromRow(row.Session), RepoDisplayName: row.RepoDisplayName, RepoOriginURL: row.RepoOriginUrl}
	}), nil
}

func (s *SQLiteSessionStore) ListArchived(ctx context.Context, repoID string) ([]*models.Session, error) {
	rows, err := sqlcgen.New(s.db).ListSessionsByArchived(ctx, repoID)
	if err != nil {
		return nil, fmt.Errorf("list sessions: %w", err)
	}
	return mapRows(rows, sessionFromRow), nil
}

func (s *SQLiteSessionStore) Update(ctx context.Context, id string, params UpdateSessionParams) (*domain.Session, error) {
	arg := sqlcgen.UpdateSessionParams{ID: id, UpdatedAt: sqlutil.TimeNow(), BlockedState: int64(machine.Blocked), State: sessionInt64(params.State)}
	arg.Title = params.Title
	arg.IsBranchCreated = params.IsBranchCreated
	arg.SetWorktreeCreatedAt = params.WorktreeCreatedAt != nil
	if params.WorktreeCreatedAt != nil {
		arg.WorktreeCreatedAt = *params.WorktreeCreatedAt
	}
	arg.WorktreePath = params.WorktreePath
	arg.BranchName = params.BranchName
	arg.SetAgentSessionID = params.AgentSessionID != nil
	if params.AgentSessionID != nil {
		arg.AgentSessionID = *params.AgentSessionID
	}
	arg.SetPrNumber = params.PRNumber != nil
	if params.PRNumber != nil {
		arg.PrNumber = sessionInt64(*params.PRNumber)
	}
	arg.SetPrUrl = params.PRURL != nil
	if params.PRURL != nil {
		arg.PrUrl = *params.PRURL
	}
	arg.SetTrackerID = params.TrackerID != nil
	if params.TrackerID != nil {
		arg.TrackerID = *params.TrackerID
	}
	arg.SetTrackerUrl = params.TrackerURL != nil
	if params.TrackerURL != nil {
		arg.TrackerUrl = *params.TrackerURL
	}
	arg.SetTmuxSessionName = params.TmuxSessionName != nil
	if params.TmuxSessionName != nil {
		arg.TmuxSessionName = *params.TmuxSessionName
	}
	arg.LastCheckState = sessionInt64(params.LastCheckState)
	arg.SetLastCheckStateHeadSha = params.LastCheckStateHeadSHA != nil
	if params.LastCheckStateHeadSHA != nil {
		arg.LastCheckStateHeadSha = *params.LastCheckStateHeadSHA
	}
	arg.SetLastCheckStateAt = params.LastCheckStateAt != nil
	if params.LastCheckStateAt != nil {
		arg.LastCheckStateAt = *params.LastCheckStateAt
	}
	arg.LastObservedReviewState = sessionInt64(params.LastObservedReviewState)
	arg.IsAutomationEnabled = params.IsAutomationEnabled
	arg.AttemptCount = sessionInt64(params.AttemptCount)
	arg.SetBlockedReason = params.BlockedReason != nil
	if params.BlockedReason != nil {
		arg.BlockedReason = *params.BlockedReason
	}
	arg.LastRepairAttemptCount = sessionInt64(params.LastRepairAttemptCount)
	arg.SetRunBlockedReason = params.RunBlockedReason != nil
	if params.RunBlockedReason != nil {
		arg.RunBlockedReason = *params.RunBlockedReason
	}
	arg.SetLastAttemptHeadSha = params.LastAttemptHeadSHA != nil
	if params.LastAttemptHeadSHA != nil {
		arg.LastAttemptHeadSha = *params.LastAttemptHeadSHA
	}
	arg.RotationAttemptCount = sessionInt64(params.RotationAttemptCount)
	arg.SetRotationResumeAt = params.RotationResumeAt != nil
	if params.RotationResumeAt != nil {
		arg.RotationResumeAt = *params.RotationResumeAt
	}
	arg.SetArchivedAt = params.ArchivedAt != nil
	if params.ArchivedAt != nil {
		arg.ArchivedAt = *params.ArchivedAt
	}
	arg.SetCronJobID = params.CronJobID != nil
	if params.CronJobID != nil {
		arg.CronJobID = *params.CronJobID
	}
	arg.SetHookToken = params.HookToken != nil
	if params.HookToken != nil {
		arg.HookToken = *params.HookToken
	}
	arg.SetAccountID = params.AccountID != nil
	if params.AccountID != nil {
		arg.AccountID = *params.AccountID
	}
	arg.IsTmuxUnattended = params.IsTmuxUnattended
	arg.IsDetached = params.Detach
	arg.IsQuickChat = params.IsQuickChat
	arg.DisplayLabel = params.DisplayLabel
	arg.DisplayIntent = sessionInt64(params.DisplayIntent)
	arg.HasDisplaySpinner = params.DisplaySpinner
	arg.SetupError = params.SetupError
	row, err := sqlcgen.New(s.db).UpdateSession(ctx, arg)
	if err != nil {
		return nil, fmt.Errorf("update session: %w", notFound(err))
	}
	return sessionFromRow(row), nil
}

func (s *SQLiteSessionStore) MarkArchived(ctx context.Context, id string) error {
	now := sqlutil.TimeNow()
	n, err := sqlcgen.New(s.db).MarkSessionArchived(ctx, sqlcgen.MarkSessionArchivedParams{ID: id, ArchivedAt: &now, UpdatedAt: now})
	if err != nil {
		return fmt.Errorf("archive session: %w", err)
	}
	if n == 0 {
		return notFound(sql.ErrNoRows)
	}
	return nil
}

// MarkOrphansArchived moves sessions stuck in ImplementingPlan to
// AwaitingChecks when there are no running workflows for them. This cleans
// up sessions whose driving autopilot goroutines were lost during a daemon
// restart.
//
// Quick chats (is_quick_chat = 1) are excluded: they have no branch and no
// PR lifecycle to enter (BOS-1514). A surviving quick chat stays in
// ImplementingPlan, and the stranded-cron sweep (reconcile_cron.go, whose
// strandedReapStates include ImplementingPlan) reaps a dead cron one.
func (s *SQLiteSessionStore) MarkOrphansArchived(ctx context.Context) (int64, error) {
	now := sqlutil.TimeNow()
	res, err := sqlcgen.New(s.db).UpdateSessionAdvanceOrphanedSessions(ctx, sqlcgen.UpdateSessionAdvanceOrphanedSessionsParams{
		AwaitingChecksState:   int64(machine.AwaitingChecks),
		Now:                   now,
		ImplementingPlanState: int64(machine.ImplementingPlan),
	})
	if err != nil {
		return 0, fmt.Errorf("advance orphaned sessions: %w", err)
	}
	n, _ := res.RowsAffected()
	return n, nil
}

// UpdateRepairDiagnostics writes the per-attempt outcome onto the session
// row in a single statement. The repair plugin calls this from its
// deferred cleanup so every attempt — clean or failed — leaves a
// structured trace in SQLite that survives daemon restarts and is
// queryable from the TUI.
//
// last_repair_attempt_count tracks *consecutive failures* rather than
// total attempts: a clean run resets it to 0, a failed run bumps it by
// one. That keeps the TUI's "⚠ repair failed (N×)" hint honest after a
// fail → succeed → fail sequence, where the previous all-attempts
// counter would have rendered "(3×)" when only one failure had actually
// happened since the last clean repair.
func (s *SQLiteSessionStore) UpdateRepairDiagnostics(ctx context.Context, params UpdateRepairDiagnosticsParams) error {
	if params.SessionID == "" {
		return fmt.Errorf("update repair diagnostics: session ID required")
	}
	n, err := sqlcgen.New(s.db).UpdateSessionRepairDiagnostics(ctx, sqlcgen.UpdateSessionRepairDiagnosticsParams{
		ID: params.SessionID, LastRepairStartedAt: timeText(&params.StartedAt),
		LastRepairRunnerError: params.RunnerError, LastRepairExitError: params.ExitError,
		HasError:          params.RunnerError != "" || params.ExitError != "",
		LastRepairHeadSha: params.HeadSHA, LastRepairDisplayStatus: int64(params.DisplayStatus),
		LastRepairReviewFingerprint: params.ReviewFingerprint, LastRepairBaseSha: params.BaseSHA,
	})
	if err != nil {
		return fmt.Errorf("update repair diagnostics: %w", err)
	}
	if n == 0 {
		return notFound(sql.ErrNoRows)
	}
	return nil
}

// UpdateRepairBlocked records a blocked-refusal outcome: the daemon refused
// to START a repair chat (a FailedPrecondition displace/reclaim refusal)
// rather than running one. It writes last_repair_blocked_reason +
// last_repair_blocked_at ONLY, deliberately leaving last_repair_attempt_count
// and the runner/exit error fields untouched, so a start-refusal never counts
// as a repair failure or feeds the exponential backoff. The next real repair
// outcome (via UpdateRepairDiagnostics) clears the blocked pair.
func (s *SQLiteSessionStore) UpdateRepairBlocked(ctx context.Context, sessionID string, at time.Time, reason string) error {
	if sessionID == "" {
		return fmt.Errorf("update repair blocked: session ID required")
	}
	n, err := sqlcgen.New(s.db).UpdateSessionRepairBlocked(ctx, sqlcgen.UpdateSessionRepairBlockedParams{
		ID:                      sessionID,
		LastRepairBlockedReason: reason,
		LastRepairBlockedAt:     timeText(&at),
	})
	if err != nil {
		return fmt.Errorf("update repair blocked: %w", err)
	}
	if n == 0 {
		return notFound(sql.ErrNoRows)
	}
	return nil
}

// SetRepairAlertCleared ends a latched repair alert once its condition is over
// (BOS-1535): it drops the blocked pair, the consecutive-failure counter and
// the runner/exit errors. last_repair_head_sha, last_repair_display_status,
// last_repair_started_at and the review fingerprint are history, not alert
// state, and are left untouched.
func (s *SQLiteSessionStore) SetRepairAlertCleared(ctx context.Context, sessionID string) error {
	if sessionID == "" {
		return fmt.Errorf("clear repair alert: session ID required")
	}
	n, err := sqlcgen.New(s.db).SetSessionRepairAlertCleared(ctx, sessionID)
	if err != nil {
		return fmt.Errorf("clear repair alert: %w", err)
	}
	if n == 0 {
		return notFound(sql.ErrNoRows)
	}
	return nil
}

// SetDismissedAlert records the operator's dismissal of one session alert
// (BOS-1537): it overwrites sessions.dismissed_alert_key and
// dismissed_alert_at. Dismissal is one key per session — the latest — so a
// later dismissal replaces an earlier one. It deliberately leaves updated_at
// alone: a dismissal is not a content change, and the recovery scans order by
// that column. Returns domain.ErrNotFound when no session has the id.
func (s *SQLiteSessionStore) SetDismissedAlert(ctx context.Context, id, key string, at time.Time) error {
	if id == "" {
		return fmt.Errorf("set dismissed alert: session ID required")
	}
	if key == "" {
		return fmt.Errorf("set dismissed alert: alert key required")
	}
	n, err := sqlcgen.New(s.db).SetSessionDismissedAlert(ctx, sqlcgen.SetSessionDismissedAlertParams{ID: id, DismissedAlertKey: key, DismissedAlertAt: timeText(&at)})
	if err != nil {
		return fmt.Errorf("set dismissed alert: %w", err)
	}
	if n == 0 {
		return notFound(sql.ErrNoRows)
	}
	return nil
}

func (s *SQLiteSessionStore) Delete(ctx context.Context, id string) error {
	// Use a transaction to ensure atomic cleanup of all dependent records.
	// We don't rely on ON DELETE CASCADE because the PRAGMA foreign_keys=ON
	// setting is per-connection and may not persist if database/sql recycles
	// the connection.
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	queries := sqlcgen.New(tx)

	if err := queries.SetTaskMappingsSessionDetached(ctx, &id); err != nil {
		return fmt.Errorf("detach task mappings: %w", err)
	}
	if err := queries.DeleteSessionCheckSnapshots(ctx, id); err != nil {
		return fmt.Errorf("delete check snapshots: %w", err)
	}
	if err := queries.DeleteSessionAgentRunChildren(ctx, id); err != nil {
		return fmt.Errorf("delete agent run children: %w", err)
	}
	if err := queries.DeleteSessionAgentRuns(ctx, id); err != nil {
		return fmt.Errorf("delete agent runs: %w", err)
	}
	if err := queries.DeleteSessionWorkflows(ctx, id); err != nil {
		return fmt.Errorf("delete workflows: %w", err)
	}
	if err := queries.DeleteSessionAgentChats(ctx, id); err != nil {
		return fmt.Errorf("delete claude chats: %w", err)
	}
	if err := queries.DeleteSessionFixAttempts(ctx, id); err != nil {
		return fmt.Errorf("delete attempts: %w", err)
	}
	if err := queries.DeleteSessionResumeAttempts(ctx, id); err != nil {
		return fmt.Errorf("delete resume attempts: %w", err)
	}
	if err := queries.SetCronJobsLastRunSessionDetached(ctx, &id); err != nil {
		return fmt.Errorf("detach cron job last run session: %w", err)
	}

	n, err := queries.DeleteSession(ctx, id)
	if err != nil {
		return fmt.Errorf("delete session: %w", err)
	}
	if n == 0 {
		return notFound(sql.ErrNoRows)
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit transaction: %w", err)
	}
	return nil
}

func sessionInt64[T ~int | ~int32](value *T) *int64 {
	if value == nil {
		return nil
	}
	converted := int64(*value)
	return &converted
}

func sessionFromRow(row sqlcgen.Session) *domain.Session {
	var prNumber *int
	if row.PrNumber != nil {
		value := int(*row.PrNumber)
		prNumber = &value
	}
	sess := &domain.Session{
		ID:                          row.ID,
		RepoID:                      row.RepoID,
		Title:                       row.Title,
		Plan:                        row.Plan,
		WorktreePath:                row.WorktreePath,
		IsBranchCreated:             row.IsBranchCreated,
		BranchName:                  row.BranchName,
		BaseBranch:                  row.BaseBranch,
		AgentSessionID:              row.AgentSessionID,
		PRNumber:                    prNumber,
		PRURL:                       row.PrUrl,
		TrackerID:                   row.TrackerID,
		TrackerURL:                  row.TrackerUrl,
		TmuxSessionName:             row.TmuxSessionName,
		LastCheckStateHeadSHA:       row.LastCheckStateHeadSha,
		AttemptCount:                int(row.AttemptCount),
		BlockedReason:               row.BlockedReason,
		CronJobID:                   row.CronJobID,
		HookToken:                   row.HookToken,
		DisplayLabel:                row.DisplayLabel,
		AgentName:                   row.AgentName,
		Model:                       row.Model,
		EffectiveModel:              row.EffectiveModel,
		EffectiveEffort:             row.EffectiveEffort,
		LastRepairRunnerError:       row.LastRepairRunnerError,
		LastRepairExitError:         row.LastRepairExitError,
		LastRepairAttemptCount:      int(row.LastRepairAttemptCount),
		LastRepairHeadSHA:           row.LastRepairHeadSha,
		LastRepairDisplayStatus:     clampInt32(int(row.LastRepairDisplayStatus)),
		LastRepairReviewFingerprint: row.LastRepairReviewFingerprint,
		LastRepairBaseSHA:           row.LastRepairBaseSha,
		SetupError:                  row.SetupError,
		LastRepairBlockedReason:     row.LastRepairBlockedReason,
		LastAttemptHeadSHA:          row.LastAttemptHeadSha,
		RotationAttemptCount:        int(row.RotationAttemptCount),
		AccountID:                   row.AccountID,
		ListRank:                    row.ListRank,
		DismissedAlertKey:           row.DismissedAlertKey,
		RunBlockedReason:            row.RunBlockedReason,
		State:                       machine.State(row.State),
		LastCheckState:              machine.CheckState(row.LastCheckState),
		LastObservedReviewState:     int(row.LastObservedReviewState),
		IsAutomationEnabled:         row.IsAutomationEnabled,
		IsTmuxUnattended:            row.IsTmuxUnattended,
		IsQuickChat:                 row.IsQuickChat,
		Detach:                      row.IsDetached,
		DisplayIntent:               clampInt32(int(row.DisplayIntent)),
		DisplaySpinner:              row.HasDisplaySpinner,
		WorktreeCreatedAt:           optionalTextTime(row.WorktreeCreatedAt),
		RotationResumeAt:            optionalTextTime(row.RotationResumeAt),
		LastCheckStateAt:            optionalTextTime(row.LastCheckStateAt),
		StateEnteredAt:              optionalTextTime(row.StateEnteredAt),
		CreatedAt:                   textTime(row.CreatedAt),
		UpdatedAt:                   textTime(row.UpdatedAt),
		LastRepairStartedAt:         optionalTextTime(row.LastRepairStartedAt),
		LastRepairBlockedAt:         optionalTextTime(row.LastRepairBlockedAt),
		DismissedAlertAt:            optionalTextTime(row.DismissedAlertAt),
	}
	// ArchivedAt historically preserves a non-NULL invalid timestamp as a zero time.
	if row.ArchivedAt != nil {
		value := textTime(*row.ArchivedAt)
		sess.ArchivedAt = &value
	}
	return sess
}

// MarkAgentRunResumed guards the spawn-to-persist handoff against late completion,
// archive and a competing replacement. domain.ErrNotFound means ownership was lost.
func (s *SQLiteSessionStore) MarkAgentRunResumed(ctx context.Context, id string, expectedState int, priorAgentID *string, newAgentID string) (*models.Session, error) {
	now := sqlutil.TimeNow()
	res, err := sqlcgen.New(s.db).UpdateSessionCommitRunResume(ctx, sqlcgen.UpdateSessionCommitRunResumeParams{
		ImplementingPlanState: int64(machine.ImplementingPlan),
		Now:                   &now,
		NewAgentID:            &newAgentID,
		ID:                    id,
		ExpectedState:         int64(expectedState),
		PriorAgentID:          priorAgentID,
	})
	if err != nil {
		return nil, fmt.Errorf("commit run resume: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return nil, err
	}
	if n != 1 {
		return nil, notFound(sql.ErrNoRows)
	}
	return s.Get(ctx, id)
}

// MarkAgentRunResumeAbandoned preserves concurrent completion, finalization, archive and
// replacement. domain.ErrNotFound means the exhausted run no longer owns the session.
func (s *SQLiteSessionStore) MarkAgentRunResumeAbandoned(ctx context.Context, id string, expectedState int, priorAgentID *string, reason string) error {
	now := sqlutil.TimeNow()
	res, err := sqlcgen.New(s.db).UpdateSessionGiveUpRunResume(ctx, sqlcgen.UpdateSessionGiveUpRunResumeParams{
		BlockedState:  int64(machine.Blocked),
		Now:           &now,
		Reason:        &reason,
		ID:            id,
		ExpectedState: int64(expectedState),
		PriorAgentID:  priorAgentID,
	})
	if err != nil {
		return fmt.Errorf("give up run resume: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return notFound(sql.ErrNoRows)
	}
	return nil
}

// RecordAgentRunDeath is level-triggered: the first observation survives restarts,
// while a different agent id starts a new death clock. It leaves updated_at
// untouched, so background observation is never mistaken for user activity.
func (s *SQLiteSessionStore) RecordAgentRunDeath(ctx context.Context, id, agentID string, expectedState int, at time.Time) (time.Time, error) {
	stamp := sqlutil.FormatTime(at)
	res, err := sqlcgen.New(s.db).UpdateSessionObserveRunDeath(ctx, sqlcgen.UpdateSessionObserveRunDeathParams{
		AgentID:       &agentID,
		Stamp:         &stamp,
		ID:            id,
		ExpectedState: int64(expectedState),
	})
	if err != nil {
		return time.Time{}, fmt.Errorf("observe run death: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return time.Time{}, err
	}
	if n != 1 {
		return time.Time{}, notFound(sql.ErrNoRows)
	}
	observed, err := sqlcgen.New(s.db).GetSessionRunDeathObservation(ctx, sqlcgen.GetSessionRunDeathObservationParams{ID: id, AgentID: &agentID})
	if err != nil {
		return time.Time{}, notFound(err)
	}
	return sqlutil.ParseTime(observed), nil
}
